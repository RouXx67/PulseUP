#!/usr/bin/env bash

# Pulse Installer Script
# Supports: Ubuntu 20.04+, Debian 11+, Proxmox VE 7+

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
INSTALL_DIR="/opt/pulse"
CONFIG_DIR="/etc/pulse"  # All config and data goes here for manual installs
SERVICE_NAME="pulse"
GITHUB_REPO="rcourtman/Pulse"
BUILD_FROM_SOURCE=false
SKIP_DOWNLOAD=false
IN_CONTAINER=false
IN_DOCKER=false
ENABLE_AUTO_UPDATES=false
FORCE_VERSION=""
FORCE_CHANNEL=""
SOURCE_BRANCH="main"
PROXY_MODE=""  # Can be: yes, no, auto, or empty (will prompt)
PROXY_USER_CHOICE=""
PROXY_PREPARE_MOUNT=false
CURRENT_INSTALL_CTID=""
CONTAINER_CREATED_FOR_CLEANUP=false
BUILD_FROM_SOURCE_MARKER="$INSTALL_DIR/BUILD_FROM_SOURCE"
DETECTED_CTID=""

DEBIAN_TEMPLATE_FALLBACK="debian-12-standard_12.12-1_amd64.tar.zst"
DEBIAN_TEMPLATE=""

detect_lxc_ctid() {
    local ctid=""

    if [[ -r /proc/1/cgroup ]]; then
        ctid=$(sed 's/\\x2d/-/g' /proc/1/cgroup 2>/dev/null | grep -Eo '(lxc|machine-lxc)-[0-9]+' | tail -n1 | grep -Eo '[0-9]+' | tail -n1)
        if [[ -n "$ctid" ]]; then
            echo "$ctid"
            return
        fi
    fi

    if command -v hostname >/dev/null 2>&1; then
        ctid=$(hostname 2>/dev/null || true)
        if [[ "$ctid" =~ ^[0-9]+$ ]]; then
            echo "$ctid"
            return
        fi
    fi

    if command -v hostnamectl >/dev/null 2>&1; then
        ctid=$(hostnamectl hostname 2>/dev/null || true)
        if [[ "$ctid" =~ ^[0-9]+$ ]]; then
            echo "$ctid"
            return
        fi
    fi

    if command -v findmnt >/dev/null 2>&1; then
        local mount_src
        mount_src=$(findmnt -no SOURCE / 2>/dev/null || true)
        if [[ "$mount_src" =~ -([0-9]+)-disk ]]; then
            echo "${BASH_REMATCH[1]}"
            return
        fi
    fi

    if command -v df >/dev/null 2>&1; then
        local root_src
        root_src=$(df -P / 2>/dev/null | awk 'NR==2 {print $1}')
        if [[ "$root_src" =~ -([0-9]+)-disk ]]; then
            echo "${BASH_REMATCH[1]}"
            return
        fi
    fi
}

auto_detect_container_environment() {
    if [[ "$IN_CONTAINER" == "true" ]]; then
        if [[ -z "$DETECTED_CTID" ]]; then
            DETECTED_CTID=$(detect_lxc_ctid 2>/dev/null || true)
        fi
        return
    fi

    local virt_type=""
    if command -v systemd-detect-virt >/dev/null 2>&1; then
        virt_type=$(systemd-detect-virt --container 2>/dev/null || true)
        if [[ -n "$virt_type" && "$virt_type" != "none" ]]; then
            IN_CONTAINER=true
            if [[ "$virt_type" == "docker" ]]; then
                IN_DOCKER=true
            fi
        fi
    fi

    if [[ "$IN_CONTAINER" != "true" ]]; then
        if [[ -f /.dockerenv ]] || grep -qa docker /proc/1/cgroup 2>/dev/null || grep -qa docker /proc/self/cgroup 2>/dev/null; then
            IN_CONTAINER=true
            IN_DOCKER=true
        elif grep -qaE '(lxc|machine-lxc)' /proc/1/cgroup 2>/dev/null; then
            IN_CONTAINER=true
        elif [[ -r /proc/1/environ ]] && grep -qa 'container=lxc' /proc/1/environ 2>/dev/null; then
            IN_CONTAINER=true
        fi
    fi

    if [[ "$IN_CONTAINER" == "true" ]]; then
        if [[ -z "$DETECTED_CTID" ]]; then
            DETECTED_CTID=$(detect_lxc_ctid 2>/dev/null || true)
        fi
    fi
}

handle_install_interrupt() {
    echo ""
    print_error "Installation cancelled"
    if [[ -n "$CURRENT_INSTALL_CTID" ]] && [[ "$CONTAINER_CREATED_FOR_CLEANUP" == "true" ]]; then
        print_info "Cleaning up container $CURRENT_INSTALL_CTID..."
        pct stop "$CURRENT_INSTALL_CTID" 2>/dev/null || true
        sleep 2
        pct destroy "$CURRENT_INSTALL_CTID" 2>/dev/null || true
    fi
    exit 1
}

# Wrapper for systemctl commands that might hang in unprivileged containers
safe_systemctl() {
    local action="$1"
    shift
    timeout 5 systemctl "$action" "$@" 2>/dev/null || {
        if [[ "$action" == "daemon-reload" ]]; then
            # daemon-reload hanging is common in unprivileged containers, silent fail is OK
            return 0
        elif [[ "$action" == "start" || "$action" == "enable" ]]; then
            print_info "Note: systemctl $action failed (may be in unprivileged container)"
            return 1
        else
            return 1
        fi
    }
}

# Detect existing service name (pulse or pulse-backend)
detect_service_name() {
    if systemctl list-unit-files --no-legend | grep -q "^pulse-backend.service"; then
        echo "pulse-backend"
    elif systemctl list-unit-files --no-legend | grep -q "^pulse.service"; then
        echo "pulse"
    else
        echo "pulse"  # Default for new installations
    fi
}

# Functions
print_header() {
    echo -e "${BLUE}=================================================${NC}"
    echo -e "${BLUE}           Pulse Installation Script${NC}"
    echo -e "${BLUE}=================================================${NC}"
    echo
}

# Safe read function that works with or without TTY
safe_read() {
    local prompt="$1"
    local var_name="$2"
    shift 2
    local read_args="$@"  # Allow passing additional args like -n 1
    
    # When script is piped (curl | bash), stdin is the pipe, not the terminal
    # We need to read from /dev/tty for user input
    if [[ -t 0 ]]; then
        # stdin is a terminal, read normally
        echo -n "$prompt"
        IFS= read -r $read_args "$var_name"
        return 0
    else
        # stdin is not a terminal (piped), try /dev/tty if available
        if { exec 3< /dev/tty; } 2>/dev/null; then
            # /dev/tty is available and usable
            echo -n "$prompt"
            IFS= read -r $read_args "$var_name" <&3
            exec 3<&-
            return 0
        else
            # No TTY available at all - truly non-interactive
            # Don't try to read from piped stdin as it will hang
            return 1
        fi
    fi
}

# Wrapper that handles safe_read with automatic error handling
safe_read_with_default() {
    local prompt="$1"
    local var_name="$2"
    local default_value="$3"
    shift 3
    local read_args="$@"
    
    # Temporarily disable errexit
    set +e
    safe_read "$prompt" "$var_name" $read_args
    local read_result=$?
    set -e
    
    if [[ $read_result -ne 0 ]]; then
        # Failed to read - use default
        eval "$var_name='$default_value'"
        # Only print default message in truly non-interactive mode
        if [[ ! -t 0 ]] && [[ -n "$default_value" ]]; then
            print_info "Using default: $default_value"
        fi
        return 0  # Return success since we handled it with a default
    fi
    
    # Check if empty and use default
    local current_value
    eval "current_value=\$$var_name"
    if [[ -z "$current_value" ]]; then
        eval "$var_name='$default_value'"
    fi
    
    return 0
}

print_error() {
    echo -e "${RED}[ERROR] $1${NC}" >&2
}

print_success() {
    echo -e "${GREEN}[SUCCESS] $1${NC}"
}

print_info() {
    echo -e "${YELLOW}[INFO] $1${NC}"
}

print_warn() {
    echo -e "${YELLOW}[WARN] $1${NC}"
}

# Prompt user about proxy installation
# Returns 0 if user wants proxy, 1 if not
prompt_proxy_installation() {
    local docker_detected="$1"
    local default_choice="${2:-n}"

    # Normalize default to lowercase single character
    default_choice=$(echo "$default_choice" | tr '[:upper:]' '[:lower:]')
    case "$default_choice" in
        y|yes) default_choice="y" ;;
        *) default_choice="n" ;;
    esac

    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  Temperature Monitoring Setup"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "Pulse can install a secure proxy service on the host to enable"
    echo "temperature monitoring from within the container."
    echo ""
    echo "This will:"
    echo "  • Install pulse-sensor-proxy service on the Proxmox host"
    echo "  • Configure container with temperature monitoring bind mount"
    echo "  • Distribute SSH keys to cluster nodes"
    echo "  • Enable temperature monitoring from first boot"
    echo ""

    if [[ "$docker_detected" == "true" ]]; then
        echo "${YELLOW}Docker detected - proxy is recommended for temperature monitoring${NC}"
        echo ""
    fi

    local prompt_text
    if [[ "$default_choice" == "y" ]]; then
        prompt_text="Enable temperature monitoring? [Y/n]: "
    else
        prompt_text="Enable temperature monitoring? [y/N]: "
    fi

    local response=""
    safe_read_with_default "$prompt_text" response "$default_choice"

    # Normalize to lowercase
    response=$(echo "$response" | tr '[:upper:]' '[:lower:]')

    if [[ "$response" =~ ^(y|yes)$ ]]; then
        echo ""
        print_info "Temperature proxy will be installed"
        return 0
    else
        echo ""
        print_info "Skipping proxy installation"
        print_info "Temperature monitoring from container will be unavailable"
        print_info "To enable later, run: curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh | bash -s -- --ctid <CTID>"
        return 1
    fi
}

ensure_debian_template() {
    if [[ -n "$DEBIAN_TEMPLATE" ]]; then
        return
    fi

    local candidate=""
    if command -v pveam >/dev/null 2>&1; then
        local available_output=""
        available_output=$(pveam available --section system 2>/dev/null || true)
        candidate=$(echo "$available_output" | awk '$2 ~ /^debian-12-standard_[0-9]+\.[0-9]+-[0-9]+_amd64\.tar\.zst$/ {print $2}' | sort -V | tail -1)

        if [[ -z "$candidate" ]]; then
            available_output=$(pveam available 2>/dev/null || true)
            candidate=$(echo "$available_output" | awk '$2 ~ /^debian-12-standard_[0-9]+\.[0-9]+-[0-9]+_amd64\.tar\.zst$/ {print $2}' | sort -V | tail -1)
        fi
    fi

    if [[ -z "$candidate" ]]; then
        candidate="$DEBIAN_TEMPLATE_FALLBACK"
        print_info "Using fallback Debian template version: $candidate"
    else
        print_info "Detected latest Debian template version: $candidate"
    fi

    DEBIAN_TEMPLATE="$candidate"
}

check_root() {
    if [[ $EUID -ne 0 ]]; then
        print_error "This script must be run as root"
        exit 1
    fi
}

# V3 is deprecated - no longer checking for it

detect_os() {
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        OS=$ID
        VER=$VERSION_ID
    else
        print_error "Cannot detect OS"
        exit 1
    fi
}

check_proxmox_host() {
    # Check if this is a Proxmox VE host
    if command -v pvesh &> /dev/null && [[ -d /etc/pve ]]; then
        return 0
    fi
    return 1
}

check_docker_environment() {
    # Detect if we're running inside Docker (multiple detection methods)
    if [[ -f /.dockerenv ]] || \
       grep -q docker /proc/1/cgroup 2>/dev/null || \
       grep -q docker /proc/self/cgroup 2>/dev/null || \
       [[ -f /run/.containerenv ]] || \
       [[ "${container:-}" == "docker" ]]; then
        print_error "Docker environment detected"
        echo "Please use the Docker image directly: docker run -d -p 7655:7655 rcourtman/pulse:latest"
        echo "See: https://github.com/rcourtman/Pulse/blob/main/docs/DOCKER.md"
        exit 1
    fi
}

# Discover host bridge interfaces, including Open vSwitch bridges.
detect_network_bridges() {
    local preferred=()
    local fallback=()
    local ip_output=""
    ip_output=$(ip -o link show type bridge 2>/dev/null || true)
    if [[ -n "$ip_output" ]]; then
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            local iface=${line#*: }
            iface=${iface%%:*}
            iface=${iface%%@*}
            case "$iface" in
                docker*|br-*|virbr*|cni*|cilium*|flannel*|kube-*|veth*|tap*|ovs-system)
                    continue
                    ;;
                vmbr*|vnet*|ovs*)
                    preferred+=("$iface")
                    ;;
                *)
                    fallback+=("$iface")
                    ;;
            esac
        done <<< "$ip_output"
    fi
    
    if command -v ovs-vsctl >/dev/null 2>&1; then
        local ovs_output=""
        ovs_output=$(ovs-vsctl list-br 2>/dev/null || true)
        if [[ -n "$ovs_output" ]]; then
            while IFS= read -r iface; do
                [[ -z "$iface" ]] && continue
                case "$iface" in
                    docker*|br-*|virbr*|cni*|cilium*|flannel*|kube-*|veth*|tap*|ovs-system)
                        continue
                        ;;
                    vmbr*|vnet*|ovs*)
                        preferred+=("$iface")
                        ;;
                    *)
                        fallback+=("$iface")
                        ;;
                esac
            done <<< "$ovs_output"
        fi
    fi
    
    local combined=()
    if [[ ${#preferred[@]} -gt 0 ]]; then
        combined=("${preferred[@]}")
    fi
    if [[ ${#fallback[@]} -gt 0 ]]; then
        combined+=("${fallback[@]}")
    fi
    
    if [[ ${#combined[@]} -gt 0 ]]; then
        printf '%s\n' "${combined[@]}" | awk '!seen[$0]++' | paste -sd' ' -
    fi
}

is_bridge_interface() {
    local iface="$1"
    [[ -z "$iface" ]] && return 1
    
    local bridge_info=""
    bridge_info=$(ip -o link show "$iface" type bridge 2>/dev/null || true)
    if [[ -n "$bridge_info" ]]; then
        return 0
    fi
    
    if command -v ovs-vsctl >/dev/null 2>&1 && ovs-vsctl br-exists "$iface" &>/dev/null; then
        return 0
    fi
    
    return 1
}

create_lxc_container() {
    CURRENT_INSTALL_CTID=""
    CONTAINER_CREATED_FOR_CLEANUP=false
    trap handle_install_interrupt INT TERM
    
    print_header
    echo "Proxmox VE detected. Installing Pulse in a container."
    echo
    
    # Check if we can interact with the user
    # Try to read from /dev/tty to test if we have terminal access
    if test -e /dev/tty && (echo -n "" > /dev/tty) 2>/dev/null; then
        # We have terminal access, show menu
        echo "Installation mode:"
        echo "  1) Quick (recommended)"
        echo "  2) Advanced"  
        echo "  3) Cancel"
        safe_read_with_default "Select [1-3]: " mode "1"
    else
        # No terminal access - truly non-interactive
        echo "Non-interactive mode detected. Using Quick installation."
        mode="1"
    fi
    
    case $mode in
        3)
            print_info "Installation cancelled"
            exit 0
            ;;
        2)
            ADVANCED_MODE=true
            ;;
        *)
            ADVANCED_MODE=false
            ;;
    esac
    
    # Get next available container ID from Proxmox
    local CTID=$(pvesh get /cluster/nextid 2>/dev/null || echo "100")
    
    # If pvesh failed, fallback to manual search
    if [[ "$CTID" == "100" ]]; then
        while pct status $CTID &>/dev/null 2>&1 || qm status $CTID &>/dev/null 2>&1; do
            ((CTID++))
        done
    fi
    
    if [[ "$ADVANCED_MODE" == "true" ]]; then
        echo
        # Ask for port configuration
        safe_read_with_default "Frontend port [7655]: " frontend_port "7655"
        if [[ ! "$frontend_port" =~ ^[0-9]+$ ]] || [[ "$frontend_port" -lt 1 ]] || [[ "$frontend_port" -gt 65535 ]]; then
            print_error "Invalid port number. Using default port 7655."
            frontend_port=7655
        fi
        
        auto_updates_flag=""
        if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
            echo
            print_info "Skipping auto-update configuration: source builds don't support automatic release updates."
        else
            echo
            # Ask about auto-updates
            echo "Enable automatic updates?"
            echo "Pulse can automatically install stable updates daily (between 2-6 AM)"
            safe_read_with_default "Enable auto-updates? [y/N]: " enable_updates "n"
            if [[ "$enable_updates" =~ ^[Yy]$ ]]; then
                auto_updates_flag="--enable-auto-updates"
                ENABLE_AUTO_UPDATES=true  # Set the global variable for host installations
            fi
        fi

        if [[ -z "$PROXY_MODE" ]]; then
            echo
            if prompt_proxy_installation "false" "n"; then
                PROXY_USER_CHOICE="yes"
            else
                PROXY_USER_CHOICE="no"
            fi
        fi
        
        echo
        # Try to get cluster-wide IDs, fall back to local
        # Disable error exit for this section as commands may fail on fresh PVE installs without VMs
        set +e
        local USED_IDS=""
        if command -v pvesh &>/dev/null; then
            # Parse JSON output using grep and sed (works without jq)
            USED_IDS=$(pvesh get /cluster/resources --type vm --output-format json 2>/dev/null | \
                      grep -o '"vmid":[0-9]*' | \
                      sed 's/"vmid"://' | \
                      sort -n | \
                      paste -sd',' -)
        fi
        
        if [[ -z "$USED_IDS" ]]; then
            # Fallback: get local containers and VMs
            local LOCAL_CTS=$(pct list 2>/dev/null | tail -n +2 | awk '{print $1}' | sort -n)
            local LOCAL_VMS=$(qm list 2>/dev/null | tail -n +2 | awk '{print $1}' | sort -n)
            USED_IDS=$(echo -e "$LOCAL_CTS\n$LOCAL_VMS" | grep -v '^$' | sort -n | paste -sd',' -)
        fi
        # Re-enable error exit
        set -e
        
        echo "Container/VM IDs in use: ${USED_IDS:-none}"
        safe_read_with_default "Container ID [$CTID]: " custom_ctid "$CTID"
        if [[ "$custom_ctid" != "$CTID" ]] && [[ "$custom_ctid" =~ ^[0-9]+$ ]]; then
            # Check if ID is in use
            if pct status $custom_ctid &>/dev/null 2>&1 || qm status $custom_ctid &>/dev/null 2>&1; then
                print_error "Container/VM ID $custom_ctid is already in use"
                exit 1
            fi
            # Also check cluster if possible
            if command -v pvesh &>/dev/null; then
                if pvesh get /cluster/resources --type vm 2>/dev/null | jq -e ".[] | select(.vmid == $custom_ctid)" &>/dev/null; then
                    print_error "Container/VM ID $custom_ctid is already in use in the cluster"
                    exit 1
                fi
            fi
            CTID=$custom_ctid
        fi
    fi

    print_info "Using container ID: $CTID"
    CURRENT_INSTALL_CTID="$CTID"
    
    if [[ "$ADVANCED_MODE" == "true" ]]; then
        echo
        echo -e "${BLUE}Advanced Mode - Customize all settings${NC}"
        echo -e "${YELLOW}Defaults shown are suitable for monitoring 10-20 nodes${NC}"
        echo
        
        # Container settings
        safe_read_with_default "Container hostname [pulse]: " hostname "pulse"
        
        safe_read_with_default "Memory (MB) [1024]: " memory "1024"
        
        safe_read_with_default "Disk size (GB) [4]: " disk "4"
        
        safe_read_with_default "CPU cores [2]: " cores "2"
        
        safe_read_with_default "CPU limit (0=unlimited) [2]: " cpulimit "2"
        
        safe_read_with_default "Swap (MB) [256]: " swap "256"
        
        safe_read_with_default "Start on boot? [Y/n]: " onboot "Y" -n 1 -r
        echo
        if [[ "$onboot" =~ ^[Nn]$ ]]; then
            onboot=0
        else
            onboot=1
        fi
        
        safe_read_with_default "Enable firewall? [Y/n]: " firewall "Y" -n 1 -r
        echo
        if [[ "$firewall" =~ ^[Nn]$ ]]; then
            firewall=0
        else
            firewall=1
        fi
        
        safe_read_with_default "Unprivileged container? [Y/n]: " unprivileged "Y" -n 1 -r
        echo
        if [[ "$unprivileged" =~ ^[Nn]$ ]]; then
            unprivileged=0
        else
            unprivileged=1
        fi
    else
        # Quick mode - just use defaults silently
        
        # Use optimized defaults
        hostname="pulse"
        memory=1024
        disk=4
        cores=2
        cpulimit=2
        swap=256
        onboot=1
        firewall=1
        unprivileged=1
        # Ask for port even in quick mode
        echo
        safe_read_with_default "Port [7655]: " frontend_port "7655"
        if [[ ! "$frontend_port" =~ ^[0-9]+$ ]] || [[ "$frontend_port" -lt 1 ]] || [[ "$frontend_port" -gt 65535 ]]; then
            print_info "Using default: 7655"
            frontend_port=7655
        fi
        auto_updates_flag=""
        if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
            echo
            print_info "Skipping auto-update configuration: source builds don't support automatic release updates."
        else
            # Quick mode should ask about auto-updates too
            echo
            echo "Enable automatic updates?"
            echo "Pulse can automatically install stable updates daily (between 2-6 AM)"
            safe_read_with_default "Enable auto-updates? [y/N]: " enable_updates "n"
            if [[ "$enable_updates" =~ ^[Yy]$ ]]; then
                auto_updates_flag="--enable-auto-updates"
                ENABLE_AUTO_UPDATES=true  # Set the global variable for host installations
            fi
        fi

        if [[ -z "$PROXY_MODE" ]]; then
            echo
            if prompt_proxy_installation "false" "y"; then
                PROXY_USER_CHOICE="yes"
            else
                PROXY_USER_CHOICE="no"
            fi
        fi

        # Optional VLAN configuration - defaults to empty (no VLAN) for regular users
        echo
        safe_read_with_default "VLAN ID (press Enter for no VLAN): " vlan_id ""
        if [[ -n "$vlan_id" ]]; then
            # Validate VLAN ID (1-4094)
            if [[ ! "$vlan_id" =~ ^[0-9]+$ ]] || [[ "$vlan_id" -lt 1 ]] || [[ "$vlan_id" -gt 4094 ]]; then
                print_error "Invalid VLAN ID. Must be between 1 and 4094"
                print_info "Proceeding without VLAN"
                vlan_id=""
            fi
        fi
    fi
    
    # Get available network bridges
    echo
    print_info "Detecting available resources..."
    
    # Get available bridges
    local BRIDGES=$(detect_network_bridges)
    
    # First try to find the default network interface (could be bridge or regular interface)
    local DEFAULT_INTERFACE=$(ip route | grep default | head -1 | grep -oP 'dev \K\S+')
    
    # Check if the default interface is a bridge
    local DEFAULT_BRIDGE=""
    if [[ -n "$DEFAULT_INTERFACE" ]]; then
        if is_bridge_interface "$DEFAULT_INTERFACE"; then
            # Default interface is a bridge, use it
            DEFAULT_BRIDGE="$DEFAULT_INTERFACE"
        fi
    fi
    
    # If no default bridge found, try to use the first available bridge
    if [[ -z "$DEFAULT_BRIDGE" && -n "$BRIDGES" ]]; then
        DEFAULT_BRIDGE=$(echo "$BRIDGES" | cut -d' ' -f1)
    fi
    
    # If still no bridge found, we'll need to ask the user
    if [[ -z "$DEFAULT_BRIDGE" ]]; then
        if [[ -n "$DEFAULT_INTERFACE" ]]; then
            # We have a default interface but it's not a bridge
            print_info "Default network interface is $DEFAULT_INTERFACE (not a bridge)"
        fi
        DEFAULT_BRIDGE="vmbr0"  # Fallback suggestion only
    fi
    
    # Get available storage with usage info
    local STORAGE_INFO=$(pvesm status -content rootdir 2>/dev/null | tail -n +2)
    local DEFAULT_STORAGE=$(echo "$STORAGE_INFO" | awk '{print $1}' | head -1)
    DEFAULT_STORAGE=${DEFAULT_STORAGE:-local-lvm}
    
    if [[ "$ADVANCED_MODE" == "true" ]]; then
        # Show available bridges
        echo
        if [[ -n "$BRIDGES" ]]; then
            echo "Available network bridges:"
            local bridge_array=($BRIDGES)
            local default_idx=0
            for i in "${!bridge_array[@]}"; do
                local idx=$((i+1))
                echo "  $idx) ${bridge_array[$i]}"
                if [[ "${bridge_array[$i]}" == "$DEFAULT_BRIDGE" ]]; then
                    default_idx=$idx
                fi
            done
            
            set +e
            safe_read "Select network bridge [${default_idx}]: " bridge_choice
            local read_result=$?
            set -e
            
            if [[ $read_result -eq 0 ]]; then
                bridge_choice=${bridge_choice:-$default_idx}
                if [[ "$bridge_choice" =~ ^[0-9]+$ ]] && [[ "$bridge_choice" -ge 1 ]] && [[ "$bridge_choice" -le ${#bridge_array[@]} ]]; then
                    bridge="${bridge_array[$((bridge_choice-1))]}"
                elif [[ -n "$bridge_choice" ]]; then
                    # User typed a bridge name directly
                    bridge="$bridge_choice"
                else
                    bridge="$DEFAULT_BRIDGE"
                fi
            else
                # Non-interactive, use default
                bridge="$DEFAULT_BRIDGE"
            fi
        else
            echo "No network bridges detected"
            echo "You may need to create a Linux bridge or Open vSwitch bridge first (e.g., vmbr0)"
            set +e
            safe_read "Enter network bridge name: " bridge
            set -e
        fi
        
        # Show available storage with usage details
        echo
        if [[ -n "$STORAGE_INFO" ]]; then
            echo "Available storage pools:"
            local storage_names=()
            local default_idx=0
            local idx=1
            
            while IFS= read -r line; do
                local storage_name storage_type avail_gb total_gb used_pct parsed_line
                parsed_line=$(LC_NUMERIC=C awk '{printf "%s,%s,%.1f,%.1f,%s", $1, $2, $6/1048576, $4/1048576, $7}' <<< "$line")
                [[ -z "$parsed_line" ]] && continue
                IFS=',' read -r storage_name storage_type avail_gb total_gb used_pct <<< "$parsed_line" || continue

                storage_names+=("$storage_name")
                LC_ALL=C printf "  %d) %-15s %-8s %6.1f GB free of %6.1f GB (%s used)\n" \
                    "$idx" "$storage_name" "$storage_type" "$avail_gb" "$total_gb" "$used_pct"
                
                if [[ "$storage_name" == "$DEFAULT_STORAGE" ]]; then
                    default_idx=$idx
                fi
                ((idx++))
            done <<< "$STORAGE_INFO"
            
            set +e
            safe_read "Select storage pool [${default_idx}]: " storage_choice
            local read_result=$?
            set -e
            
            if [[ $read_result -eq 0 ]]; then
                storage_choice=${storage_choice:-$default_idx}
                if [[ "$storage_choice" =~ ^[0-9]+$ ]] && [[ "$storage_choice" -ge 1 ]] && [[ "$storage_choice" -le ${#storage_names[@]} ]]; then
                    storage="${storage_names[$((storage_choice-1))]}"
                elif [[ -n "$storage_choice" ]]; then
                    # User typed a storage name directly
                    storage="$storage_choice"
                else
                    storage="$DEFAULT_STORAGE"
                fi
            else
                # Non-interactive, use default
                storage="$DEFAULT_STORAGE"
            fi
        else
            echo "  No storage pools found"
            set +e
            safe_read "Enter storage pool name [$DEFAULT_STORAGE]: " storage
            set -e
            storage=${storage:-$DEFAULT_STORAGE}
        fi
        
        safe_read_with_default "Static IP with CIDR (e.g. 192.168.1.100/24, leave empty for DHCP): " static_ip ""
        
        # If static IP is provided, we need gateway
        if [[ -n "$static_ip" ]]; then
            # Validate IP format
            if [[ ! "$static_ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}/[0-9]{1,2}$ ]]; then
                print_error "Invalid IP format. Please use CIDR notation (e.g., 192.168.1.100/24)"
                print_info "Using DHCP instead"
                static_ip=""
            else
                safe_read_with_default "Gateway IP address: " gateway_ip ""
                if [[ -z "$gateway_ip" ]]; then
                    # Try to guess gateway from the IP (use .1 of the subnet)
                    local ip_base="${static_ip%.*}"
                    gateway_ip="${ip_base}.1"
                    print_info "No gateway specified, using $gateway_ip"
                elif [[ ! "$gateway_ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
                    print_error "Invalid gateway format. Using DHCP instead"
                    static_ip=""
                    gateway_ip=""
                fi
            fi
        fi
        
        safe_read_with_default "DNS servers (space-separated, empty for host settings): " nameserver ""
        
        # VLAN configuration
        safe_read_with_default "VLAN ID (leave empty for no VLAN): " vlan_id ""
        if [[ -n "$vlan_id" ]]; then
            # Validate VLAN ID (1-4094)
            if [[ ! "$vlan_id" =~ ^[0-9]+$ ]] || [[ "$vlan_id" -lt 1 ]] || [[ "$vlan_id" -gt 4094 ]]; then
                print_error "Invalid VLAN ID. Must be between 1 and 4094"
                print_info "Proceeding without VLAN"
                vlan_id=""
            fi
        fi
        
        safe_read_with_default "Startup order [99]: " startup "99"
    else
        # Quick mode - but still need to verify critical settings
        
        # Network bridge selection
        echo
        if [[ -n "$BRIDGES" ]]; then
            echo "Available network bridges:"
            local bridge_array=($BRIDGES)
            local default_idx=0
            for i in "${!bridge_array[@]}"; do
                local idx=$((i+1))
                echo "  $idx) ${bridge_array[$i]}"
                if [[ "${bridge_array[$i]}" == "$DEFAULT_BRIDGE" ]]; then
                    default_idx=$idx
                fi
            done
            
            if [[ ${#bridge_array[@]} -eq 1 ]]; then
                # Only one bridge available, use it
                bridge="${bridge_array[0]}"
                print_info "Using network bridge: $bridge"
            else
                set +e
                safe_read "Select network bridge [${default_idx}]: " bridge_choice
                local read_result=$?
                set -e
                
                if [[ $read_result -eq 0 ]]; then
                    bridge_choice=${bridge_choice:-$default_idx}
                    if [[ "$bridge_choice" =~ ^[0-9]+$ ]] && [[ "$bridge_choice" -ge 1 ]] && [[ "$bridge_choice" -le ${#bridge_array[@]} ]]; then
                        bridge="${bridge_array[$((bridge_choice-1))]}"
                    else
                        bridge="$DEFAULT_BRIDGE"
                        print_info "Invalid selection, using default: $bridge"
                    fi
                else
                    # Non-interactive, use default
                    bridge="$DEFAULT_BRIDGE"
                fi
            fi
        else
            print_error "No network bridges detected on this system"
            print_info "You may need to create a Linux bridge or Open vSwitch bridge first (e.g., vmbr0)"
            set +e
            safe_read "Enter network bridge name to use: " bridge
            set -e
        fi
        
        # Storage selection
        echo
        if [[ -n "$STORAGE_INFO" ]]; then
            echo "Available storage pools:"
            # Create arrays for storage info
            local storage_names=()
            local storage_info_lines=()
            local default_idx=0
            local idx=1
            
            while IFS= read -r line; do
                local storage_name storage_type avail_gb total_gb used_pct parsed_line
                parsed_line=$(LC_NUMERIC=C awk '{printf "%s,%s,%.1f,%.1f,%s", $1, $2, $6/1048576, $4/1048576, $7}' <<< "$line")
                [[ -z "$parsed_line" ]] && continue
                IFS=',' read -r storage_name storage_type avail_gb total_gb used_pct <<< "$parsed_line" || continue

                storage_names+=("$storage_name")
                LC_ALL=C printf "  %d) %-15s %-8s %6.1f GB free of %6.1f GB (%s used)\n" \
                    "$idx" "$storage_name" "$storage_type" "$avail_gb" "$total_gb" "$used_pct"
                
                if [[ "$storage_name" == "$DEFAULT_STORAGE" ]]; then
                    default_idx=$idx
                fi
                ((idx++))
            done <<< "$STORAGE_INFO"
            
            if [[ ${#storage_names[@]} -eq 1 ]]; then
                # Only one storage available, use it
                storage="${storage_names[0]}"
                print_info "Using storage pool: $storage"
            else
                set +e
                safe_read "Select storage pool [${default_idx}]: " storage_choice
                local read_result=$?
                set -e
                
                if [[ $read_result -eq 0 ]]; then
                    storage_choice=${storage_choice:-$default_idx}
                    if [[ "$storage_choice" =~ ^[0-9]+$ ]] && [[ "$storage_choice" -ge 1 ]] && [[ "$storage_choice" -le ${#storage_names[@]} ]]; then
                        storage="${storage_names[$((storage_choice-1))]}"
                    else
                        storage="$DEFAULT_STORAGE"
                        print_info "Invalid selection, using default: $storage"
                    fi
                else
                    # Non-interactive, use default
                    storage="$DEFAULT_STORAGE"
                fi
            fi
        else
            print_error "No storage pools detected"
            set +e
            safe_read "Enter storage pool name to use: " storage
            set -e
        fi
        
        static_ip=""
        gateway_ip=""
        nameserver=""
        startup=99
    fi
    
    # Handle OS template selection
    echo
    if [[ "$ADVANCED_MODE" == "true" ]]; then
        # Get ALL storages that can contain templates
        local TEMPLATE_STORAGES=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | awk '{print $1}' | paste -sd' ' -)
        
        # Collect templates from ALL template-capable storages
        local ALL_TEMPLATES=""
        for tmpl_storage in $TEMPLATE_STORAGES; do
            # pveam list output already includes the storage prefix in the path
            local STORAGE_TEMPLATES=$(pveam list "$tmpl_storage" 2>/dev/null | tail -n +2 | awk '{print $1}' || true)
            if [[ -n "$STORAGE_TEMPLATES" ]]; then
                if [[ -n "$ALL_TEMPLATES" ]]; then
                    ALL_TEMPLATES="${ALL_TEMPLATES}\n${STORAGE_TEMPLATES}"
                else
                    ALL_TEMPLATES="$STORAGE_TEMPLATES"
                fi
            fi
        done
        
        echo "Available OS templates across all storages:"
        # Format templates with numbers
        local TEMPLATES=$(echo -e "$ALL_TEMPLATES" | nl -w2 -s') ')
        if [[ -n "$TEMPLATES" ]]; then
            echo "$TEMPLATES"
            echo
            echo "Or download a new template:"
            echo "  d) Download Debian 12 (recommended)"
            echo "  u) Download Ubuntu 22.04 LTS"
            echo "  a) Download Alpine Linux (minimal)"
            echo
            safe_read_with_default "Select template number or option [Enter for Debian 12]: " template_choice ""
            if [[ -n "$template_choice" ]]; then
                case "$template_choice" in
                    d|D)
                        # Find best storage for templates (prefer one with most free space)
                        local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
                        BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
                        ensure_debian_template
                        print_info "Downloading Debian 12 to storage '$BEST_TEMPLATE_STORAGE'..."
                        pveam download "$BEST_TEMPLATE_STORAGE" "$DEBIAN_TEMPLATE"
                        TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/${DEBIAN_TEMPLATE}"
                        ;;
                    u|U)
                        # Find best storage for templates (prefer one with most free space)
                        local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
                        BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
                        print_info "Downloading Ubuntu 22.04 to storage '$BEST_TEMPLATE_STORAGE'..."
                        pveam download "$BEST_TEMPLATE_STORAGE" ubuntu-22.04-standard_22.04-1_amd64.tar.zst
                        TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst"
                        ;;
                    a|A)
                        # Find best storage for templates (prefer one with most free space)
                        local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
                        BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
                        print_info "Downloading Alpine Linux to storage '$BEST_TEMPLATE_STORAGE'..."
                        pveam download "$BEST_TEMPLATE_STORAGE" alpine-3.18-default_20230607_amd64.tar.xz
                        TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/alpine-3.18-default_20230607_amd64.tar.xz"
                        ;;
                    [0-9]*)
                        # Extract the full template path from numbered list
                        TEMPLATE=$(echo -e "$ALL_TEMPLATES" | sed -n "${template_choice}p")
                        if [[ -n "$TEMPLATE" ]]; then
                            print_info "Using template: $TEMPLATE"
                        else
                            # Find best storage for templates (prefer one with most free space)
                            local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
                            BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
                            ensure_debian_template
                            TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/${DEBIAN_TEMPLATE}"
                            print_info "Invalid selection, using Debian 12"
                        fi
                        ;;
                    *)
                        # Find best storage for templates (prefer one with most free space)
                        local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
                        BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
                        ensure_debian_template
                        TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/${DEBIAN_TEMPLATE}"
                        ;;
                esac
            else
                # Find best storage for templates (prefer one with most free space)
                local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
                BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
                ensure_debian_template
                TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/${DEBIAN_TEMPLATE}"
            fi
        else
            # Find best storage for templates (prefer one with most free space)
            local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
            BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
            ensure_debian_template
            TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/${DEBIAN_TEMPLATE}"
        fi
    else
        # Quick mode - find best storage for templates
        local BEST_TEMPLATE_STORAGE=$(pvesm status -content vztmpl 2>/dev/null | tail -n +2 | sort -k6 -rn | head -1 | awk '{print $1}')
        BEST_TEMPLATE_STORAGE=${BEST_TEMPLATE_STORAGE:-$storage}
        ensure_debian_template
        TEMPLATE="${BEST_TEMPLATE_STORAGE}:vztmpl/${DEBIAN_TEMPLATE}"
    fi
    
    # Download template if it doesn't exist
    # Check if template exists - pveam list shows full paths like storage:vztmpl/file.tar.zst
    local TEMPLATE_EXISTS=false
    if [[ "$TEMPLATE" =~ ^([^:]+):vztmpl/(.+)$ ]]; then
        local STORAGE_NAME="${BASH_REMATCH[1]}"
        local TEMPLATE_FILE="${BASH_REMATCH[2]}"
        # Check if this exact template exists in pveam list
        if pveam list "$STORAGE_NAME" 2>/dev/null | grep -q "$TEMPLATE_FILE"; then
            TEMPLATE_EXISTS=true
        fi
    fi
    
    if [[ "$TEMPLATE_EXISTS" == "false" ]]; then
        # Extract storage name from template path
        local TEMPLATE_STORAGE="${TEMPLATE%%:*}"
        print_info "Template not found, downloading Debian 12 to storage '$TEMPLATE_STORAGE'..."
        ensure_debian_template
        if ! pveam download "$TEMPLATE_STORAGE" "$DEBIAN_TEMPLATE"; then
            print_error "Failed to download template. Please check your internet connection and try again."
            print_info "You can manually download with: pveam download $TEMPLATE_STORAGE $DEBIAN_TEMPLATE"
            exit 1
        fi
        # Verify it was downloaded
        if ! pveam list "$TEMPLATE_STORAGE" 2>/dev/null | grep -q "$DEBIAN_TEMPLATE"; then
            print_error "Template download succeeded but file not found in storage"
            exit 1
        fi
    fi
    
    print_info "Creating container..."
    
    # Build network configuration
    if [[ -n "$static_ip" ]]; then
        # Include gateway in network config for static IP
        if [[ -n "$gateway_ip" ]]; then
            NET_CONFIG="name=eth0,bridge=${bridge},ip=${static_ip},gw=${gateway_ip},firewall=${firewall}"
        else
            # This shouldn't happen but handle it gracefully
            NET_CONFIG="name=eth0,bridge=${bridge},ip=${static_ip},firewall=${firewall}"
        fi
    else
        NET_CONFIG="name=eth0,bridge=${bridge},ip=dhcp,firewall=${firewall}"
    fi
    
    # Add VLAN tag if specified
    if [[ -n "$vlan_id" ]]; then
        NET_CONFIG="${NET_CONFIG},tag=${vlan_id}"
    fi
    
    # Build container create command using array to avoid eval issues
    local CREATE_ARGS=(pct create "$CTID" "$TEMPLATE")
    CREATE_ARGS+=("--hostname" "$hostname")
    CREATE_ARGS+=("--memory" "$memory")
    CREATE_ARGS+=("--cores" "$cores")

    if [[ "$cpulimit" != "0" ]]; then
        CREATE_ARGS+=("--cpulimit" "$cpulimit")
    fi

    CREATE_ARGS+=("--rootfs" "${storage}:${disk}")
    CREATE_ARGS+=("--net0" "$NET_CONFIG")
    CREATE_ARGS+=("--unprivileged" "$unprivileged")
    CREATE_ARGS+=("--features" "nesting=1")
    CREATE_ARGS+=("--onboot" "$onboot")
    CREATE_ARGS+=("--startup" "order=$startup")
    CREATE_ARGS+=("--protection" "0")
    CREATE_ARGS+=("--swap" "$swap")

    if [[ -n "$nameserver" ]]; then
        CREATE_ARGS+=("--nameserver" "$nameserver")
    fi

    if [[ "$PROXY_MODE" == "yes" || "$PROXY_MODE" == "auto" ]]; then
        PROXY_PREPARE_MOUNT=true
    elif [[ -z "$PROXY_MODE" ]] && [[ "$PROXY_USER_CHOICE" == "yes" ]]; then
        PROXY_PREPARE_MOUNT=true
    else
        PROXY_PREPARE_MOUNT=false
    fi

    if [[ "$PROXY_PREPARE_MOUNT" == "true" ]]; then
        local PROXY_HOST_PATH="/run/pulse-sensor-proxy"
        local PROXY_CONTAINER_PATH="/mnt/pulse-proxy"
        mkdir -p "$PROXY_HOST_PATH"
        CREATE_ARGS+=("--mp0" "${PROXY_HOST_PATH},mp=${PROXY_CONTAINER_PATH},replicate=0")
    fi

    # Execute container creation (suppress verbose output)
    if ! "${CREATE_ARGS[@]}" >/dev/null 2>&1; then
        print_error "Failed to create container"
        exit 1
    fi
    CONTAINER_CREATED_FOR_CLEANUP=true
    
    # From this point on, cleanup container if we fail
    cleanup_on_error() {
        print_error "Installation failed, cleaning up container $CTID..."
        CURRENT_INSTALL_CTID="$CTID"
        CONTAINER_CREATED_FOR_CLEANUP=true
        pct stop $CTID 2>/dev/null || true
        sleep 2
        pct destroy $CTID 2>/dev/null || true
        exit 1
    }
    
    # Start container
    print_info "Starting container..."
    if ! pct start $CTID >/dev/null 2>&1; then
        print_error "Failed to start container"
        cleanup_on_error
    fi
    sleep 3
    
    # Wait for network to provide an IP address
    print_info "Waiting for network..."
    local network_ready=false
    for i in {1..60}; do
        local container_ip=""
        container_ip=$(pct exec $CTID -- hostname -I 2>/dev/null | awk '{print $1}') || true
        if [[ -n "$container_ip" ]]; then
            network_ready=true
            break
        fi
        sleep 1
    done

    if [[ "$network_ready" != "true" ]]; then
        print_error "Container network failed to obtain an IP address after 60 seconds"
        cleanup_on_error
    fi
    
    # Install dependencies and optimize container
    print_info "Installing dependencies..."
    if ! pct exec $CTID -- bash -c "
        apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq curl wget ca-certificates >/dev/null 2>&1
        # Set timezone to UTC for consistent logging
        ln -sf /usr/share/zoneinfo/UTC /etc/localtime 2>/dev/null
        # Optimize sysctl for monitoring workload
        echo 'net.core.somaxconn=1024' >> /etc/sysctl.conf
        echo 'net.ipv4.tcp_keepalive_time=60' >> /etc/sysctl.conf
        sysctl -p >/dev/null 2>&1
        
        # Note: We don't create /usr/local/bin/update to avoid conflicts with Community Scripts
        # Native installations should update using: curl -fsSL ... | bash
        
        # Ensure /usr/local/bin is in PATH for all users
        if ! grep -q '/usr/local/bin' /etc/profile 2>/dev/null; then
            echo 'export PATH="/usr/local/bin:$PATH"' >> /etc/profile
        fi
        
        # Also add to bash profile if it exists
        if [[ -f /etc/bash.bashrc ]] && ! grep -q '/usr/local/bin' /etc/bash.bashrc 2>/dev/null; then
            echo 'export PATH="/usr/local/bin:$PATH"' >> /etc/bash.bashrc
        fi
    "; then
        print_error "Failed to install dependencies in container"
        cleanup_on_error
    fi
    
    # Install Pulse inside container
    print_info "Installing Pulse..."
    
    # When piped through curl, $0 is "bash" not the script. Download fresh copy.
    local script_source="/tmp/pulse_install_$$.sh"
    if [[ "$0" == "bash" ]] || [[ ! -f "$0" ]]; then
        # We're being piped, download the script
        # Use timeout to prevent hanging
        if command -v timeout >/dev/null 2>&1; then
            if ! timeout 15 curl -fsSL --connect-timeout 5 --max-time 15 https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh > "$script_source" 2>/dev/null; then
                print_error "Failed to download install script"
                cleanup_on_error
            fi
        else
            if ! curl -fsSL --connect-timeout 5 --max-time 15 https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh > "$script_source" 2>/dev/null; then
                print_error "Failed to download install script"
                cleanup_on_error
            fi
        fi
    else
        # We have a local script file
        script_source="$0"
    fi
    
    # Copy this script to container and run it
    if ! pct push $CTID "$script_source" /tmp/install.sh >/dev/null 2>&1; then
        print_error "Failed to copy install script to container"
        cleanup_on_error
    fi
    
    # Clean up temp file if we created one
    if [[ "$script_source" == "/tmp/pulse_install_"* ]]; then
        rm -f "$script_source"
    fi
    
    # Run installation with visible progress
    local install_cmd="bash /tmp/install.sh --in-container"
    if [[ -n "$auto_updates_flag" ]]; then
        install_cmd="$install_cmd $auto_updates_flag"
    fi
    if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
        install_cmd="$install_cmd --source '$SOURCE_BRANCH'"
    fi
    if [[ "$frontend_port" != "7655" ]]; then
        install_cmd="FRONTEND_PORT=$frontend_port $install_cmd"
    fi
    
    # Run installation showing output in real-time so users can see progress/errors
    # Use timeout wrapper if available
    local install_status
    local timeout_duration=300

    if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
        timeout_duration=1200
    fi

    if [[ -n "${PULSE_CONTAINER_TIMEOUT:-}" ]]; then
        if [[ "${PULSE_CONTAINER_TIMEOUT}" =~ ^[0-9]+$ ]]; then
            timeout_duration=${PULSE_CONTAINER_TIMEOUT}
        else
            print_warn "Ignoring invalid PULSE_CONTAINER_TIMEOUT value '${PULSE_CONTAINER_TIMEOUT}'"
        fi
    fi

    if command -v timeout >/dev/null 2>&1 && [[ "$timeout_duration" -ne 0 ]]; then
        # Show output in real-time with timeout
        timeout "$timeout_duration" pct exec $CTID -- bash -c "$install_cmd"
        install_status=$?
        if [[ $install_status -eq 124 ]]; then
            print_error "Installation timed out after ${timeout_duration}s"
            if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
                print_info "Building from source can take more than 15 minutes in containers, especially on first run."
            else
                print_info "This usually happens due to network issues or GitHub rate limiting."
            fi
            print_info "You can increase or disable the timeout by setting PULSE_CONTAINER_TIMEOUT (set to 0 to disable)."
            print_info "Then enter the container and run 'bash /tmp/install.sh' manually:"
            print_info "  pct enter $CTID"
            cleanup_on_error
        fi
    else
        # Show output in real-time without timeout
        pct exec $CTID -- bash -c "$install_cmd"
        install_status=$?
    fi
    
    if [[ $install_status -ne 0 ]]; then
        print_error "Failed to install Pulse inside container"
        print_info "You can enter the container to investigate:"
        print_info "  pct enter $CTID"
        print_info "  bash /tmp/install.sh"
        cleanup_on_error
    fi
    
    # Get container IP
    local IP=$(pct exec $CTID -- hostname -I | awk '{print $1}')

    if [[ "$IN_CONTAINER" != "true" ]] && [[ "$PROXY_PREPARE_MOUNT" == "true" ]]; then
        print_info "Configuring pulse-sensor-proxy socket inside container..."
        if ! pct exec $CTID -- bash -c 'set -e
ENV_FILE="/etc/pulse/.env"
SOCKET_LINE="PULSE_SENSOR_PROXY_SOCKET=/mnt/pulse-proxy/pulse-sensor-proxy.sock"
mkdir -p /etc/pulse
if [[ -f "$ENV_FILE" ]] && grep -q "^PULSE_SENSOR_PROXY_SOCKET=" "$ENV_FILE" 2>/dev/null; then
  sed -i "s|^PULSE_SENSOR_PROXY_SOCKET=.*|$SOCKET_LINE|" "$ENV_FILE"
else
  echo "$SOCKET_LINE" >> "$ENV_FILE"
fi'; then
            print_error "Failed to configure pulse-sensor-proxy socket inside container"
            cleanup_on_error
        fi

        if ! pct exec $CTID -- systemctl restart $SERVICE_NAME >/dev/null 2>&1; then
            print_warn "Unable to restart $SERVICE_NAME inside container; please restart manually"
        fi
    fi

    # Determine if we should install temperature proxy
    local install_proxy=false
    local docker_in_container=false

    # Check if Docker is installed in the container
    if pct exec $CTID -- command -v docker >/dev/null 2>&1; then
        docker_in_container=true
    fi

    # Decide based on PROXY_MODE
    case "$PROXY_MODE" in
        yes)
            install_proxy=true
            ;;
        no)
            install_proxy=false
            ;;
        auto)
            # Auto-detect: install if Docker is present
            if [[ "$docker_in_container" == "true" ]]; then
                install_proxy=true
            fi
            ;;
        *)
            # Empty/unset - reuse earlier user choice (defaults handled already)
            if [[ "$PROXY_USER_CHOICE" == "yes" ]]; then
                install_proxy=true
            fi
            ;;
    esac

    if [[ "$PROXY_MODE" == "auto" ]] && [[ "$install_proxy" != "true" ]]; then
        print_info "Docker not detected inside container; skipping temperature proxy installation (auto mode)."
    fi

    # Install temperature proxy on host for secure monitoring (if enabled)
    if [[ "$install_proxy" == "true" ]]; then
        echo
        print_info "Installing temperature monitoring proxy on host..."
        local proxy_script="/tmp/install-sensor-proxy-$$.sh"

        # Download proxy installer
        if command -v timeout >/dev/null 2>&1; then
            if ! timeout 15 curl -fsSL --connect-timeout 5 --max-time 15 \
                "https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh" \
                > "$proxy_script" 2>/dev/null; then
                print_warn "Failed to download proxy installer - temperature monitoring unavailable"
                print_info "Run manually later: curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh | bash -s -- --ctid $CTID"
            fi
        else
            if ! curl -fsSL --connect-timeout 5 --max-time 15 \
                "https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh" \
                > "$proxy_script" 2>/dev/null; then
                print_warn "Failed to download proxy installer - temperature monitoring unavailable"
                print_info "Run manually later: curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh | bash -s -- --ctid $CTID"
            fi
        fi

        # Run proxy installer if downloaded
        if [[ -f "$proxy_script" ]]; then
            chmod +x "$proxy_script"

            # Stop existing pulse-sensor-proxy service if running to allow binary update
            if systemctl is-active --quiet pulse-sensor-proxy 2>/dev/null; then
                print_info "Stopping existing pulse-sensor-proxy service to update binary..."
                systemctl stop pulse-sensor-proxy 2>/dev/null || true
                sleep 1
            fi

            # If building from source, copy the binary from the LXC instead of downloading
            local proxy_install_args=(--ctid "$CTID" --skip-restart)
            if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
                local local_proxy_binary="/tmp/pulse-sensor-proxy-$CTID"
                print_info "Copying locally-built pulse-sensor-proxy binary from container..."
                if pct pull $CTID /opt/pulse/bin/pulse-sensor-proxy "$local_proxy_binary" 2>/dev/null; then
                    proxy_install_args=(--ctid "$CTID" --local-binary "$local_proxy_binary" --skip-restart)
                    print_info "Using locally-built binary from container"
                else
                    print_warn "Failed to copy binary from container, will try fallback download"
                    export PULSE_SENSOR_PROXY_FALLBACK_URL="http://${IP}:${frontend_port}/api/install/pulse-sensor-proxy"
                fi
            else
                # For release installs, set fallback URL to download from Pulse server inside the LXC
                export PULSE_SENSOR_PROXY_FALLBACK_URL="http://${IP}:${frontend_port}/api/install/pulse-sensor-proxy"
            fi

            if bash "$proxy_script" "${proxy_install_args[@]}" 2>&1 | tee /tmp/proxy-install-${CTID}.log; then
                print_info "Temperature proxy installed successfully"
                # Clean up temporary binary if it was copied
                [[ -f "$local_proxy_binary" ]] && rm -f "$local_proxy_binary"
            else
                print_warn "Proxy installation failed - temperature monitoring will use fallback method"
                print_info "Check logs: /tmp/proxy-install-${CTID}.log"
                print_info "Or run manually: curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh | bash -s -- --ctid $CTID"
            fi
            rm -f "$proxy_script"
        fi
    fi

    # Clean final output
    echo
    print_success "Pulse installation complete!"
    echo
    echo "  Web UI:     http://${IP}:${frontend_port}"
    echo "  Container:  $CTID"
    echo
    echo "  Commands:"
    echo "    pct enter $CTID              # Enter container"
    echo "    pct exec $CTID -- update     # Update Pulse"
    echo
    
    CONTAINER_CREATED_FOR_CLEANUP=false
    CURRENT_INSTALL_CTID=""
    trap - INT TERM
    
    exit 0
}

# Compare two version strings
# Returns: 0 if equal, 1 if first > second, 2 if first < second
compare_versions() {
    local v1="${1#v}"  # Remove 'v' prefix
    local v2="${2#v}"
    
    # Strip any pre-release suffix (e.g., -rc.1, -beta, etc.)
    local base_v1="${v1%%-*}"
    local base_v2="${v2%%-*}"
    local suffix_v1="${v1#*-}"
    local suffix_v2="${v2#*-}"
    
    # If no suffix, suffix equals the full version
    [[ "$suffix_v1" == "$v1" ]] && suffix_v1=""
    [[ "$suffix_v2" == "$v2" ]] && suffix_v2=""
    
    # Split base versions into parts
    IFS='.' read -ra V1_PARTS <<< "$base_v1"
    IFS='.' read -ra V2_PARTS <<< "$base_v2"
    
    # Compare major.minor.patch
    for i in 0 1 2; do
        local p1="${V1_PARTS[$i]:-0}"
        local p2="${V2_PARTS[$i]:-0}"
        if [[ "$p1" -gt "$p2" ]]; then
            return 1
        elif [[ "$p1" -lt "$p2" ]]; then
            return 2
        fi
    done
    
    # Base versions are equal, now compare suffixes
    # No suffix (stable) > rc suffix
    if [[ -z "$suffix_v1" ]] && [[ -n "$suffix_v2" ]]; then
        return 1  # v1 (stable) > v2 (rc)
    elif [[ -n "$suffix_v1" ]] && [[ -z "$suffix_v2" ]]; then
        return 2  # v1 (rc) < v2 (stable)
    elif [[ -n "$suffix_v1" ]] && [[ -n "$suffix_v2" ]]; then
        # Both have suffixes, compare them lexicographically
        if [[ "$suffix_v1" > "$suffix_v2" ]]; then
            return 1
        elif [[ "$suffix_v1" < "$suffix_v2" ]]; then
            return 2
        fi
    fi
    
    return 0  # versions are equal
}

check_existing_installation() {
    CURRENT_VERSION=""  # Make it global so we can use it later
    local BINARY_PATH=""
    
    # Check for the binary in expected locations
    if [[ -f "$INSTALL_DIR/bin/pulse" ]]; then
        BINARY_PATH="$INSTALL_DIR/bin/pulse"
    elif [[ -f "$INSTALL_DIR/pulse" ]]; then
        BINARY_PATH="$INSTALL_DIR/pulse"
    fi
    
    # Try to get version if binary exists
    if [[ -n "$BINARY_PATH" ]]; then
        CURRENT_VERSION=$($BINARY_PATH --version 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9\.]+)?' | head -1 || echo "unknown")
    fi
    
    if systemctl is-active --quiet $SERVICE_NAME 2>/dev/null; then
        if [[ -n "$CURRENT_VERSION" && "$CURRENT_VERSION" != "unknown" ]]; then
            print_info "Pulse $CURRENT_VERSION is currently running"
        else
            print_info "Pulse is currently running"
        fi
        return 0
    elif [[ -n "$BINARY_PATH" ]]; then
        if [[ -n "$CURRENT_VERSION" && "$CURRENT_VERSION" != "unknown" ]]; then
            print_info "Pulse $CURRENT_VERSION is installed but not running"
        else
            print_info "Pulse is installed but not running"
        fi
        return 0
    else
        return 1
    fi
}

install_dependencies() {
    print_info "Installing dependencies..."
    
    apt-get update -qq >/dev/null 2>&1
    # Install essential dependencies plus jq for reliable JSON handling
    apt-get install -y -qq curl wget jq >/dev/null 2>&1 || {
        # If jq fails to install, just install the essentials
        apt-get install -y -qq curl wget >/dev/null 2>&1
    }
}

create_user() {
    if ! id -u pulse &>/dev/null; then
        print_info "Creating pulse user..."
        useradd --system --home-dir $INSTALL_DIR --shell /bin/false pulse
    fi
}

backup_existing() {
    if [[ -d "$CONFIG_DIR" ]]; then
        print_info "Backing up existing configuration..."
        cp -a "$CONFIG_DIR" "${CONFIG_DIR}.backup.$(date +%Y%m%d-%H%M%S)"
    fi
}

download_pulse() {
    # Check if we should build from source - do this FIRST to avoid confusing version messages
    if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
        if ! build_from_source "$SOURCE_BRANCH"; then
            print_error "Source build failed"
            exit 1
        fi
        return 0
    fi

    print_info "Downloading Pulse..."

    # Check for forced version first
    if [[ -n "${FORCE_VERSION}" ]]; then
        LATEST_RELEASE="${FORCE_VERSION}"
        print_info "Installing specific version: $LATEST_RELEASE"

        # Verify the version exists (with timeout)
        if command -v timeout >/dev/null 2>&1; then
            if ! timeout 15 curl -fsS --connect-timeout 10 --max-time 30 "https://api.github.com/repos/$GITHUB_REPO/releases/tags/$LATEST_RELEASE" > /dev/null 2>&1; then
                print_warn "Could not verify version $LATEST_RELEASE, proceeding anyway..."
            fi
        else
            if ! curl -fsS --connect-timeout 10 --max-time 30 "https://api.github.com/repos/$GITHUB_REPO/releases/tags/$LATEST_RELEASE" > /dev/null 2>&1; then
                print_warn "Could not verify version $LATEST_RELEASE, proceeding anyway..."
            fi
        fi
    else
        # UPDATE_CHANNEL should already be set by main(), but set default if not
        if [[ -z "${UPDATE_CHANNEL:-}" ]]; then
            UPDATE_CHANNEL="stable"

            # Allow override via command line
            if [[ -n "${FORCE_CHANNEL}" ]]; then
                UPDATE_CHANNEL="${FORCE_CHANNEL}"
                print_info "Using $UPDATE_CHANNEL channel from command line"
            elif [[ -f "$CONFIG_DIR/system.json" ]]; then
                CONFIGURED_CHANNEL=$(cat "$CONFIG_DIR/system.json" 2>/dev/null | grep -o '"updateChannel"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/')
                if [[ "$CONFIGURED_CHANNEL" == "rc" ]]; then
                    UPDATE_CHANNEL="rc"
                    print_info "RC channel detected in configuration"
                fi
            fi
        fi

        # Get appropriate release based on channel (with timeout)
        if [[ "$UPDATE_CHANNEL" == "rc" ]]; then
            # Get all releases and find the latest (including pre-releases)
            # Use timeout command for extra protection against hanging
            if command -v timeout >/dev/null 2>&1; then
                LATEST_RELEASE=$(timeout 15 curl -s --connect-timeout 10 --max-time 30 https://api.github.com/repos/$GITHUB_REPO/releases 2>/dev/null | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
            else
                LATEST_RELEASE=$(curl -s --connect-timeout 10 --max-time 30 https://api.github.com/repos/$GITHUB_REPO/releases 2>/dev/null | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
            fi
        else
            # Get latest stable release only
            # Use timeout command for extra protection against hanging
            if command -v timeout >/dev/null 2>&1; then
                LATEST_RELEASE=$(timeout 15 curl -s --connect-timeout 10 --max-time 30 https://api.github.com/repos/$GITHUB_REPO/releases/latest 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)
            else
                LATEST_RELEASE=$(curl -s --connect-timeout 10 --max-time 30 https://api.github.com/repos/$GITHUB_REPO/releases/latest 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)
            fi
        fi

        # Fallback: Try direct GitHub redirect if API fails
        if [[ -z "$LATEST_RELEASE" ]]; then
            print_info "GitHub API unavailable, trying alternative method..."
            if command -v timeout >/dev/null 2>&1; then
                LATEST_RELEASE=$(timeout 10 curl -sI --connect-timeout 5 --max-time 10 https://github.com/$GITHUB_REPO/releases/latest 2>/dev/null | grep -i '^location:' | sed -E 's|.*tag/([^[:space:]]+).*|\1|' | tr -d '\r' || true)
            else
                LATEST_RELEASE=$(curl -sI --connect-timeout 5 --max-time 10 https://github.com/$GITHUB_REPO/releases/latest 2>/dev/null | grep -i '^location:' | sed -E 's|.*tag/([^[:space:]]+).*|\1|' | tr -d '\r' || true)
            fi
        fi

        # Final fallback: Use a known good version
        if [[ -z "$LATEST_RELEASE" ]]; then
            print_warn "Could not determine latest release from GitHub, using fallback version"
            LATEST_RELEASE="v4.5.1"  # Known stable version as fallback
        fi

        print_info "Latest version: $LATEST_RELEASE"
    fi
    
    if [[ "$BUILD_FROM_SOURCE" == "true" && "$SKIP_DOWNLOAD" != "true" ]]; then
        print_error "Source build requested but download path was reached (internal error)"
        exit 1
    fi
    
    # Only do download if not building from source
    if [[ "$SKIP_DOWNLOAD" != "true" ]]; then
        rm -f "$BUILD_FROM_SOURCE_MARKER"
        # Detect architecture
        ARCH=$(uname -m)
        case $ARCH in
            x86_64)
                PULSE_ARCH="amd64"
                ;;
            aarch64)
                PULSE_ARCH="arm64"
                ;;
            armv7l)
                PULSE_ARCH="armv7"
                ;;
            *)
                print_error "Unsupported architecture: $ARCH"
                exit 1
                ;;
        esac
        
        print_info "Detected architecture: $ARCH ($PULSE_ARCH)"
        
        # Download architecture-specific release
        DOWNLOAD_URL="https://github.com/$GITHUB_REPO/releases/download/$LATEST_RELEASE/pulse-${LATEST_RELEASE}-linux-${PULSE_ARCH}.tar.gz"
        print_info "Downloading from: $DOWNLOAD_URL"
        
        # Detect and stop existing service BEFORE downloading (to free the binary)
        EXISTING_SERVICE=$(detect_service_name)
        if timeout 5 systemctl is-active --quiet $EXISTING_SERVICE 2>/dev/null; then
            print_info "Stopping existing Pulse service ($EXISTING_SERVICE)..."
            safe_systemctl stop $EXISTING_SERVICE || true
            sleep 2  # Give the process time to fully stop and release the binary
        fi
        
        cd /tmp
        # Download with timeout (60 seconds should be enough for ~5MB file)
        if ! wget -q --timeout=60 --tries=2 -O pulse.tar.gz "$DOWNLOAD_URL"; then
            print_error "Failed to download Pulse release"
            print_info "This can happen due to network issues or GitHub rate limiting"
            print_info "You can try downloading manually from: $DOWNLOAD_URL"
            exit 1
        fi
        
        # Extract to temporary directory first
        TEMP_EXTRACT="/tmp/pulse-extract-$$"
        mkdir -p "$TEMP_EXTRACT"
        tar -xzf pulse.tar.gz -C "$TEMP_EXTRACT"
    
        # Ensure install directory and bin subdirectory exist
        mkdir -p "$INSTALL_DIR/bin"
    
        # Copy Pulse binary to the correct location (/opt/pulse/bin/pulse)
        # First, backup the old binary if it exists
        if [[ -f "$INSTALL_DIR/bin/pulse" ]]; then
            mv "$INSTALL_DIR/bin/pulse" "$INSTALL_DIR/bin/pulse.old" 2>/dev/null || true
        fi
    
        if [[ -f "$TEMP_EXTRACT/bin/pulse" ]]; then
            if ! cp "$TEMP_EXTRACT/bin/pulse" "$INSTALL_DIR/bin/pulse"; then
                print_error "Failed to copy new binary to $INSTALL_DIR/bin/pulse"
                # Try to restore old binary
                if [[ -f "$INSTALL_DIR/bin/pulse.old" ]]; then
                    mv "$INSTALL_DIR/bin/pulse.old" "$INSTALL_DIR/bin/pulse"
                fi
                exit 1
            fi
        elif [[ -f "$TEMP_EXTRACT/pulse" ]]; then
            # Fallback for old archives (pre-v4.3.1)
            if ! cp "$TEMP_EXTRACT/pulse" "$INSTALL_DIR/bin/pulse"; then
                print_error "Failed to copy new binary to $INSTALL_DIR/bin/pulse"
                # Try to restore old binary
                if [[ -f "$INSTALL_DIR/bin/pulse.old" ]]; then
                    mv "$INSTALL_DIR/bin/pulse.old" "$INSTALL_DIR/bin/pulse"
                fi
                exit 1
            fi
        else
            print_error "Pulse binary not found in archive"
            # Try to restore old binary
            if [[ -f "$INSTALL_DIR/bin/pulse.old" ]]; then
                mv "$INSTALL_DIR/bin/pulse.old" "$INSTALL_DIR/bin/pulse"
            fi
            exit 1
        fi
    
        # Verify the new binary was copied and is executable
        if [[ ! -f "$INSTALL_DIR/bin/pulse" ]]; then
            print_error "Binary installation failed - file not found after copy"
            # Try to restore old binary
            if [[ -f "$INSTALL_DIR/bin/pulse.old" ]]; then
                mv "$INSTALL_DIR/bin/pulse.old" "$INSTALL_DIR/bin/pulse"
            fi
            exit 1
        fi

        # Install Docker agent binary for distribution
        if [[ -f "$TEMP_EXTRACT/bin/pulse-docker-agent" ]]; then
            cp -f "$TEMP_EXTRACT/bin/pulse-docker-agent" "$INSTALL_DIR/pulse-docker-agent"
            cp -f "$TEMP_EXTRACT/bin/pulse-docker-agent" "$INSTALL_DIR/bin/pulse-docker-agent"
            chmod +x "$INSTALL_DIR/pulse-docker-agent" "$INSTALL_DIR/bin/pulse-docker-agent"
            chown pulse:pulse "$INSTALL_DIR/pulse-docker-agent" "$INSTALL_DIR/bin/pulse-docker-agent"
            ln -sf "$INSTALL_DIR/bin/pulse-docker-agent" /usr/local/bin/pulse-docker-agent
            print_success "Docker agent binary installed"
        else
            print_warn "Docker agent binary not found in archive; skipping installation"
        fi

        install_additional_agent_binaries "$LATEST_RELEASE"

        # Install Docker agent convenience script
        mkdir -p "$INSTALL_DIR/scripts"
        if [[ -f "$TEMP_EXTRACT/scripts/install-docker-agent.sh" ]]; then
            cp "$TEMP_EXTRACT/scripts/install-docker-agent.sh" "$INSTALL_DIR/scripts/install-docker-agent.sh"
            chmod 755 "$INSTALL_DIR/scripts/install-docker-agent.sh"
            chown pulse:pulse "$INSTALL_DIR/scripts/install-docker-agent.sh"
            print_success "Docker agent install script deployed"
        else
            print_warn "Docker agent install script not found in archive; skipping installation"
        fi
        
        chmod +x "$INSTALL_DIR/bin/pulse"
        chown -R pulse:pulse "$INSTALL_DIR"
        
        # Clean up old binary backup if everything succeeded
        rm -f "$INSTALL_DIR/bin/pulse.old"
        
        # Create symlink in /usr/local/bin for PATH convenience
        ln -sf "$INSTALL_DIR/bin/pulse" /usr/local/bin/pulse
        print_success "Pulse binary installed to $INSTALL_DIR/bin/pulse"
        print_success "Symlink created at /usr/local/bin/pulse"
    
        # Copy VERSION file if present
        if [[ -f "$TEMP_EXTRACT/VERSION" ]]; then
            cp "$TEMP_EXTRACT/VERSION" "$INSTALL_DIR/VERSION"
            chown pulse:pulse "$INSTALL_DIR/VERSION"
        fi
    
        # Verify the installed version matches what we expected
        INSTALLED_VERSION=$("$INSTALL_DIR/bin/pulse" --version 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9\.]+)?' | head -1 || echo "unknown")
        if [[ "$INSTALLED_VERSION" != "$LATEST_RELEASE" ]]; then
            print_warn "Version verification issue: Expected $LATEST_RELEASE but binary reports $INSTALLED_VERSION"
            print_info "This can happen if the binary wasn't properly replaced. Trying to fix..."
            
            # Force remove and recopy
            rm -f "$INSTALL_DIR/bin/pulse"
            if [[ -f "/tmp/pulse.tar.gz" ]]; then
                # Re-extract and try again
                TEMP_EXTRACT2="/tmp/pulse-extract2-$$"
                mkdir -p "$TEMP_EXTRACT2"
                tar -xzf /tmp/pulse.tar.gz -C "$TEMP_EXTRACT2"
                
                if [[ -f "$TEMP_EXTRACT2/bin/pulse" ]]; then
                    cp -f "$TEMP_EXTRACT2/bin/pulse" "$INSTALL_DIR/bin/pulse"
                elif [[ -f "$TEMP_EXTRACT2/pulse" ]]; then
                    cp -f "$TEMP_EXTRACT2/pulse" "$INSTALL_DIR/bin/pulse"
                fi

                if [[ -f "$TEMP_EXTRACT2/bin/pulse-docker-agent" ]]; then
                    cp -f "$TEMP_EXTRACT2/bin/pulse-docker-agent" "$INSTALL_DIR/pulse-docker-agent"
                    cp -f "$TEMP_EXTRACT2/bin/pulse-docker-agent" "$INSTALL_DIR/bin/pulse-docker-agent"
                    chmod +x "$INSTALL_DIR/pulse-docker-agent" "$INSTALL_DIR/bin/pulse-docker-agent"
                    ln -sf "$INSTALL_DIR/bin/pulse-docker-agent" /usr/local/bin/pulse-docker-agent
                fi

                install_additional_agent_binaries "$LATEST_RELEASE"

                if [[ -f "$TEMP_EXTRACT2/scripts/install-docker-agent.sh" ]]; then
                    mkdir -p "$INSTALL_DIR/scripts"
                    cp "$TEMP_EXTRACT2/scripts/install-docker-agent.sh" "$INSTALL_DIR/scripts/install-docker-agent.sh"
                    chmod 755 "$INSTALL_DIR/scripts/install-docker-agent.sh"
                    chown pulse:pulse "$INSTALL_DIR/scripts/install-docker-agent.sh"
                fi
                
                chmod +x "$INSTALL_DIR/bin/pulse"
                chown -R pulse:pulse "$INSTALL_DIR"
                rm -rf "$TEMP_EXTRACT2"
                
                # Check version again
                INSTALLED_VERSION=$("$INSTALL_DIR/bin/pulse" --version 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9\.]+)?' | head -1 || echo "unknown")
                if [[ "$INSTALLED_VERSION" == "$LATEST_RELEASE" ]]; then
                    print_success "Version issue resolved - now running $INSTALLED_VERSION"
                else
                    print_warn "Version mismatch persists. You may need to restart the service or reboot."
                fi
            fi
        else
            print_success "Version verified: $INSTALLED_VERSION"
        fi
    
        # Cleanup
        rm -rf "$TEMP_EXTRACT" pulse.tar.gz
    fi  # End of SKIP_DOWNLOAD check
}

install_additional_agent_binaries() {
    local version="$1"

    if [[ -z "$version" ]]; then
        return
    fi

    local targets=("linux-amd64" "linux-arm64" "linux-armv7")
    local missing=0
    for target in "${targets[@]}"; do
        if [[ ! -f "$INSTALL_DIR/bin/pulse-docker-agent-$target" ]]; then
            missing=1
            break
        fi
    done

    if [[ $missing -eq 0 ]]; then
        return
    fi

    local universal_url="https://github.com/$GITHUB_REPO/releases/download/$version/pulse-${version}.tar.gz"
    local universal_tar="/tmp/pulse-universal-${version}.tar.gz"

    print_info "Downloading universal agent bundle for cross-architecture support..."

    if command -v curl >/dev/null 2>&1; then
        if ! curl -fsSL --connect-timeout 10 --max-time 300 -o "$universal_tar" "$universal_url"; then
            print_warn "Failed to download universal agent bundle"
            rm -f "$universal_tar"
            return
        fi
    elif command -v wget >/dev/null 2>&1; then
        if ! wget -q --timeout=300 -O "$universal_tar" "$universal_url"; then
            print_warn "Failed to download universal agent bundle"
            rm -f "$universal_tar"
            return
        fi
    else
        print_warn "Cannot download universal agent bundle (curl or wget not available)"
        return
    fi

    local temp_dir
    temp_dir=$(mktemp -d -t pulse-universal-XXXXXX)
    if ! tar -xzf "$universal_tar" -C "$temp_dir"; then
        print_warn "Failed to extract universal agent bundle"
        rm -f "$universal_tar"
        rm -rf "$temp_dir"
        return
    fi

    local installed=0
    for agent_file in "$temp_dir"/bin/pulse-docker-agent-linux-*; do
        if [[ -f "$agent_file" ]]; then
            local base
            base=$(basename "$agent_file")
            cp -f "$agent_file" "$INSTALL_DIR/bin/$base"
            cp -f "$agent_file" "$INSTALL_DIR/$base"
            chmod +x "$INSTALL_DIR/bin/$base" "$INSTALL_DIR/$base"
            chown pulse:pulse "$INSTALL_DIR/bin/$base" "$INSTALL_DIR/$base"
            installed=1
        fi
    done

    if [[ $installed -eq 1 ]]; then
        print_success "Additional Docker agent binaries installed"
    else
        print_warn "No agent binaries found in universal bundle"
    fi

    rm -f "$universal_tar"
    rm -rf "$temp_dir"
}

build_agent_binaries_from_source() {
    local agent_version="$1"

    if ! command -v go >/dev/null 2>&1; then
        print_warn "Go not available for cross-compiling additional agent binaries"
        return
    fi

    local targets=(
        "linux-amd64:amd64:"
        "linux-arm64:arm64:"
        "linux-armv7:arm:7"
    )

    for entry in "${targets[@]}"; do
        IFS=':' read -r suffix goarch goarm <<< "$entry"

        local output="/tmp/pulse-docker-agent-$suffix-$$"

        # Skip if already exists
        if [[ -f "$INSTALL_DIR/bin/pulse-docker-agent-$suffix" ]]; then
            continue
        fi

        print_info "Cross-compiling Docker agent for $suffix"
        local env_cmd=(env GOOS=linux GOARCH="$goarch")
        if [[ -n "$goarm" ]]; then
            env_cmd+=(GOARM="$goarm")
        fi

        if ! "${env_cmd[@]}" go build -ldflags="-X github.com/rcourtman/pulse-go-rewrite/internal/dockeragent.Version=${agent_version}" -o "$output" ./cmd/pulse-docker-agent >/dev/null 2>&1; then
            print_warn "Failed to build Docker agent for $suffix"
            rm -f "$output"
            continue
        fi

        cp -f "$output" "$INSTALL_DIR/bin/pulse-docker-agent-$suffix"
        cp -f "$output" "$INSTALL_DIR/pulse-docker-agent-$suffix"
        chmod +x "$INSTALL_DIR/bin/pulse-docker-agent-$suffix" "$INSTALL_DIR/pulse-docker-agent-$suffix"
        chown pulse:pulse "$INSTALL_DIR/bin/pulse-docker-agent-$suffix" "$INSTALL_DIR/pulse-docker-agent-$suffix"
        rm -f "$output"
    done
}

build_from_source() {
    local branch="${1:-main}"
    local original_dir
    original_dir=$(pwd)
    local temp_build=""
    local GO_MIN_VERSION="1.24"
    local GO_INSTALLED=false
    local arch=""
    local go_arch=""
    local agent_version=""
    local service_name=""

    print_info "Building Pulse from source (branch: $branch)..."

    print_info "Installing build dependencies..."
    if ! (apt-get update >/dev/null 2>&1 && apt-get install -y git make nodejs npm wget >/dev/null 2>&1); then
        print_error "Failed to install build dependencies"
        return 1
    fi

    if command -v go >/dev/null 2>&1; then
        local GO_VERSION
        GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
        if [[ "$(printf '%s\n' "$GO_MIN_VERSION" "$GO_VERSION" | sort -V | head -n1)" == "$GO_MIN_VERSION" ]]; then
            GO_INSTALLED=true
            print_info "Go $GO_VERSION is installed (meets minimum $GO_MIN_VERSION)"
        else
            print_info "Go $GO_VERSION is too old. Installing Go $GO_MIN_VERSION..."
        fi
    fi

    if [[ "$GO_INSTALLED" != "true" ]]; then
        arch=$(uname -m)
        case "$arch" in
            x86_64)
                go_arch="amd64"
                ;;
            aarch64)
                go_arch="arm64"
                ;;
            armv7l)
                go_arch="armv6l"
                ;;
            *)
                print_error "Unsupported architecture for Go: $arch"
                return 1
                ;;
        esac

        if ! cd /tmp; then
            print_error "Failed to prepare Go installation directory"
            cd "$original_dir" >/dev/null 2>&1 || true
            return 1
        fi

        if ! wget -q "https://go.dev/dl/go1.24.7.linux-${go_arch}.tar.gz"; then
            print_error "Failed to download Go toolchain"
            cd "$original_dir" >/dev/null 2>&1 || true
            return 1
        fi

        rm -rf /usr/local/go
        if ! tar -C /usr/local -xzf "go1.24.7.linux-${go_arch}.tar.gz"; then
            print_error "Failed to extract Go toolchain"
            rm -f "go1.24.7.linux-${go_arch}.tar.gz"
            cd "$original_dir" >/dev/null 2>&1 || true
            return 1
        fi
        rm -f "go1.24.7.linux-${go_arch}.tar.gz"
        cd "$original_dir" >/dev/null 2>&1 || true
    fi

    export PATH=/usr/local/go/bin:$PATH

    temp_build=$(mktemp -d /tmp/pulse-build-XXXXXX)
    if [[ ! -d "$temp_build" ]]; then
        print_error "Failed to create temporary build directory"
        return 1
    fi

    if ! git clone --depth 1 --branch "$branch" "https://github.com/$GITHUB_REPO.git" "$temp_build/Pulse" >/dev/null 2>&1; then
        print_error "Failed to clone repository (branch: $branch)"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi

    if ! cd "$temp_build/Pulse"; then
        print_error "Failed to enter source checkout"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi

    if ! cd frontend-modern; then
        print_error "Frontend directory missing in repository"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi

    if ! npm ci >/dev/null 2>&1; then
        print_warn "npm ci failed, falling back to npm install..."
        if ! npm install >/dev/null 2>&1; then
            print_error "Failed to install frontend dependencies"
            cd "$original_dir" >/dev/null 2>&1 || true
            rm -rf "$temp_build"
            return 1
        fi
    fi

    if ! npm run build >/dev/null 2>&1; then
        print_error "Failed to build frontend"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi

    cd ..

    if ! make build >/dev/null 2>&1; then
        print_error "Failed to build backend"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi

    service_name=$(detect_service_name)
    if timeout 5 systemctl is-active --quiet "$service_name" 2>/dev/null; then
        print_info "Stopping existing Pulse service ($service_name)..."
        safe_systemctl stop "$service_name" || true
        sleep 2
    fi

    mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/scripts"

    if [[ -f "$INSTALL_DIR/bin/pulse" ]]; then
        mv "$INSTALL_DIR/bin/pulse" "$INSTALL_DIR/bin/pulse.old" 2>/dev/null || true
    fi

    if ! cp pulse "$INSTALL_DIR/bin/pulse"; then
        print_error "Failed to copy built Pulse binary"
        [[ -f "$INSTALL_DIR/bin/pulse.old" ]] && mv "$INSTALL_DIR/bin/pulse.old" "$INSTALL_DIR/bin/pulse"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi
    chmod +x "$INSTALL_DIR/bin/pulse"

    agent_version=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
    if ! go build -ldflags="-X github.com/rcourtman/pulse-go-rewrite/internal/dockeragent.Version=${agent_version}" -o pulse-docker-agent ./cmd/pulse-docker-agent >/dev/null 2>&1; then
        print_error "Failed to build Docker agent binary"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi

    cp -f pulse-docker-agent "$INSTALL_DIR/pulse-docker-agent"
    cp -f pulse-docker-agent "$INSTALL_DIR/bin/pulse-docker-agent"
    chmod +x "$INSTALL_DIR/pulse-docker-agent" "$INSTALL_DIR/bin/pulse-docker-agent"
    ln -sf "$INSTALL_DIR/bin/pulse-docker-agent" /usr/local/bin/pulse-docker-agent
    rm -f pulse-docker-agent

    if ! go build -o pulse-sensor-proxy ./cmd/pulse-sensor-proxy >/dev/null 2>&1; then
        print_error "Failed to build temperature proxy binary"
        cd "$original_dir" >/dev/null 2>&1 || true
        rm -rf "$temp_build"
        return 1
    fi
    cp -f pulse-sensor-proxy "$INSTALL_DIR/bin/pulse-sensor-proxy"
    chmod +x "$INSTALL_DIR/bin/pulse-sensor-proxy"
    rm -f pulse-sensor-proxy

    build_agent_binaries_from_source "$agent_version"

    for script_name in install-docker-agent.sh install-docker.sh install-sensor-proxy.sh; do
        if [[ -f "scripts/$script_name" ]]; then
            cp "scripts/$script_name" "$INSTALL_DIR/scripts/$script_name"
            chmod 755 "$INSTALL_DIR/scripts/$script_name"
        fi
    done

    ln -sf "$INSTALL_DIR/bin/pulse" /usr/local/bin/pulse

    echo "$branch-$(git rev-parse --short HEAD)" > "$INSTALL_DIR/VERSION"
    echo "$branch" > "$BUILD_FROM_SOURCE_MARKER"

    chown -R pulse:pulse "$INSTALL_DIR" 2>/dev/null || true
    chown pulse:pulse "$BUILD_FROM_SOURCE_MARKER" 2>/dev/null || true
    rm -f "$INSTALL_DIR/bin/pulse.old"

    cd "$original_dir" >/dev/null 2>&1 || true
    rm -rf "$temp_build"

    SKIP_DOWNLOAD=true
    print_success "Successfully built and installed Pulse from source (branch: $branch)"
    return 0
}

setup_directories() {
    print_info "Setting up directories..."

    # Create directories (only if they don't exist)
    mkdir -p "$CONFIG_DIR"
    mkdir -p "$INSTALL_DIR"

    # Set permissions (preserve existing files)
    # Use chown without -R on CONFIG_DIR to avoid changing existing file permissions
    chown pulse:pulse "$CONFIG_DIR"
    chown -R pulse:pulse "$INSTALL_DIR"
    chmod 700 "$CONFIG_DIR"

    # Ensure critical config files retain proper permissions if they exist
    for config_file in "$CONFIG_DIR"/alerts.json "$CONFIG_DIR"/system.json "$CONFIG_DIR"/*.enc; do
        if [[ -f "$config_file" ]]; then
            chown pulse:pulse "$config_file"
        fi
    done

    # Create .env file with mock mode explicitly disabled (unless it already exists)
    if [[ ! -f "$CONFIG_DIR/.env" ]]; then
        cat > "$CONFIG_DIR/.env" << 'EOF'
# Pulse Environment Configuration
# This file is loaded by systemd when starting Pulse

# Mock mode - set to "true" to enable mock/demo data for testing
# WARNING: Only enable this on demo/test servers, never in production!
PULSE_MOCK_MODE=false

# Mock configuration (only used when PULSE_MOCK_MODE=true)
#PULSE_MOCK_NODES=7
#PULSE_MOCK_VMS_PER_NODE=5
#PULSE_MOCK_LXCS_PER_NODE=8
#PULSE_MOCK_RANDOM_METRICS=true
#PULSE_MOCK_STOPPED_PERCENT=20
EOF
        chown pulse:pulse "$CONFIG_DIR/.env"
        chmod 600 "$CONFIG_DIR/.env"
    fi
}

setup_update_command() {
    # Create update command at /bin/update for ProxmoxVE LXC detection
    # This allows the backend to detect ProxmoxVE installations
    cat > /bin/update <<'EOF'
#!/usr/bin/env bash
# Pulse update command
# This script re-runs the Pulse installer to update to the latest version

set -e

INSTALL_ROOT="/opt/pulse"
MARKER_FILE="${INSTALL_ROOT}/BUILD_FROM_SOURCE"

extra_args=()
if [[ -f "$MARKER_FILE" ]]; then
    branch=$(tr -d '\r\n' <"$MARKER_FILE" 2>/dev/null || true)
    if [[ -n "$branch" ]]; then
        extra_args+=(--source "$branch")
    fi
fi

echo "Updating Pulse..."
if [[ ${#extra_args[@]} -gt 0 ]]; then
    curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh | bash -s -- "${extra_args[@]}"
else
    curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh | bash
fi

echo ""
echo "Update complete! Pulse will restart automatically."
EOF

    chmod +x /bin/update

    # Ensure /usr/local/bin is in PATH for all users
    if ! grep -q '/usr/local/bin' /etc/profile 2>/dev/null; then
        echo 'export PATH="/usr/local/bin:$PATH"' >> /etc/profile
    fi

    # Also add to bash profile if it exists
    if [[ -f /etc/bash.bashrc ]] && ! grep -q '/usr/local/bin' /etc/bash.bashrc 2>/dev/null; then
        echo 'export PATH="/usr/local/bin:$PATH"' >> /etc/bash.bashrc
    fi
}

setup_auto_updates() {
    print_info "Setting up automatic updates..."
    
    # Copy auto-update script if it exists in the release
    if [[ -f "$INSTALL_DIR/scripts/pulse-auto-update.sh" ]]; then
        cp "$INSTALL_DIR/scripts/pulse-auto-update.sh" /usr/local/bin/pulse-auto-update.sh
        chmod +x /usr/local/bin/pulse-auto-update.sh
    else
        # Download from GitHub if not in release
        print_info "Downloading auto-update script..."
        # Use timeout to prevent hanging
        if command -v timeout >/dev/null 2>&1; then
            if ! timeout 15 curl -fsSL --connect-timeout 5 --max-time 15 "https://raw.githubusercontent.com/$GITHUB_REPO/main/scripts/pulse-auto-update.sh" -o /usr/local/bin/pulse-auto-update.sh; then
                print_error "Failed to download auto-update script"
                return 1
            fi
        else
            if ! curl -fsSL --connect-timeout 5 --max-time 15 "https://raw.githubusercontent.com/$GITHUB_REPO/main/scripts/pulse-auto-update.sh" -o /usr/local/bin/pulse-auto-update.sh; then
                print_error "Failed to download auto-update script"
                return 1
            fi
        fi
        chmod +x /usr/local/bin/pulse-auto-update.sh
    fi
    
    # Install systemd timer and service
    cat > /etc/systemd/system/pulse-update.service << 'EOF'
[Unit]
Description=Automatic Pulse update check and install
Documentation=https://github.com/rcourtman/Pulse
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
Group=root
ExecStart=/usr/local/bin/pulse-auto-update.sh
Restart=no
TimeoutStartSec=600
StandardOutput=journal
StandardError=journal
SyslogIdentifier=pulse-update
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=strict
ReadWritePaths=/opt/pulse /etc/pulse /tmp
PrivateNetwork=no
Nice=10

[Install]
WantedBy=multi-user.target
EOF

    cat > /etc/systemd/system/pulse-update.timer << 'EOF'
[Unit]
Description=Daily check for Pulse updates
Documentation=https://github.com/rcourtman/Pulse
After=network-online.target
Wants=network-online.target

[Timer]
OnCalendar=daily
OnCalendar=02:00
RandomizedDelaySec=4h
Persistent=true
AccuracySec=1h

[Install]
WantedBy=timers.target
EOF

    # Reload systemd daemon
    safe_systemctl daemon-reload
    
    # Enable timer but don't start it yet  
    safe_systemctl enable pulse-update.timer || true
    
    # Update system.json to enable auto-updates
    if [[ -f "$CONFIG_DIR/system.json" ]]; then
        # Update existing file
        local temp_file="/tmp/system_$$.json"
        if command -v jq &> /dev/null; then
            jq '.autoUpdateEnabled = true' "$CONFIG_DIR/system.json" > "$temp_file" && mv "$temp_file" "$CONFIG_DIR/system.json"
        else
            # Fallback to sed if jq not available
            # First check if autoUpdateEnabled already exists in the file
            if grep -q '"autoUpdateEnabled"' "$CONFIG_DIR/system.json"; then
                # Field exists, update its value
                sed -i 's/"autoUpdateEnabled":[^,}]*/"autoUpdateEnabled":true/' "$CONFIG_DIR/system.json"
            else
                # Field doesn't exist, add it after the opening brace
                sed -i 's/^{/{\"autoUpdateEnabled\":true,/' "$CONFIG_DIR/system.json"
            fi
        fi
    else
        # Create new file with auto-updates enabled
        echo '{"autoUpdateEnabled":true,"pollingInterval":5}' > "$CONFIG_DIR/system.json"
    fi
    
    chown pulse:pulse "$CONFIG_DIR/system.json" 2>/dev/null || true
    
    # Start the timer
    safe_systemctl start pulse-update.timer || true
    
    print_success "Automatic updates enabled (daily check with 2-6 hour random delay)"
}

install_systemd_service() {
    print_info "Installing systemd service..."
    
    # Use existing service name if found, otherwise use default
    EXISTING_SERVICE=$(detect_service_name)
    if [[ "$EXISTING_SERVICE" == "pulse-backend" ]] && [[ -f "/etc/systemd/system/pulse-backend.service" ]]; then
        # Keep using pulse-backend for compatibility (ProxmoxVE)
        SERVICE_NAME="pulse-backend"
        print_info "Using existing service name: pulse-backend"
    fi
    
    cat > /etc/systemd/system/$SERVICE_NAME.service << EOF
[Unit]
Description=Pulse Monitoring Server
After=network.target

[Service]
Type=simple
User=pulse
Group=pulse
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/bin/pulse
Restart=always
RestartSec=3
StandardOutput=journal
StandardError=journal
Environment="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
Environment="PULSE_DATA_DIR=$CONFIG_DIR"
EnvironmentFile=-$CONFIG_DIR/.env
EOF

    # Add port configuration if not default
    if [[ "${FRONTEND_PORT:-7655}" != "7655" ]]; then
        cat >> /etc/systemd/system/$SERVICE_NAME.service << EOF
Environment="FRONTEND_PORT=$FRONTEND_PORT"
EOF
    fi

    cat >> /etc/systemd/system/$SERVICE_NAME.service << EOF

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$INSTALL_DIR $CONFIG_DIR

[Install]
WantedBy=multi-user.target
EOF

    # Reload systemd daemon
    safe_systemctl daemon-reload
}

start_pulse() {
    print_info "Starting Pulse..."
    
    # Try to enable/start service (may fail in unprivileged containers)
    if ! safe_systemctl enable $SERVICE_NAME; then
        print_info "Note: systemctl enable failed (common in unprivileged containers)"
    fi
    
    if ! safe_systemctl start $SERVICE_NAME; then
        print_info "Note: systemctl start failed (common in unprivileged containers)"
        print_info "The service will start automatically when the container starts"
        return 0
    fi
    
    # Wait for service to start
    sleep 3
    
    if timeout 5 systemctl is-active --quiet $SERVICE_NAME 2>/dev/null; then
        print_success "Pulse started successfully"
    else
        print_error "Failed to start Pulse"
        journalctl -u $SERVICE_NAME -n 20 2>/dev/null || true
        # Don't exit, just warn
        print_info "Service may not be running. You might need to start it manually."
    fi
}

create_marker_file() {
    # Create marker file for version tracking (helps with Community Scripts compatibility)
    touch ~/.pulse 2>/dev/null || true
}

print_completion() {
    local IP=$(hostname -I | awk '{print $1}')
    
    # Get the port from the service file or use default
    local PORT="${FRONTEND_PORT:-7655}"
    if [[ -z "${FRONTEND_PORT:-}" ]] && [[ -f "/etc/systemd/system/$SERVICE_NAME.service" ]]; then
        # Try to extract port from service file
        PORT=$(grep -oP 'FRONTEND_PORT=\K\d+' "/etc/systemd/system/$SERVICE_NAME.service" 2>/dev/null || echo "7655")
    fi
    
    echo
    print_header
    print_success "Pulse installation completed!"
    echo
    local PULSE_URL="http://${IP}:${PORT}"

    echo -e "${GREEN}Access Pulse at:${NC} ${PULSE_URL}"
    echo
    echo -e "${YELLOW}Quick commands:${NC}"
    echo "  systemctl status $SERVICE_NAME    - Check status"
    echo "  systemctl restart $SERVICE_NAME   - Restart"
    echo "  journalctl -u $SERVICE_NAME -f    - View logs"
    echo
    echo -e "${YELLOW}Management:${NC}"
    echo "  Update:     curl -sSL https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh | bash"
    echo "  Reset:      curl -sSL https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh | bash -s -- --reset"
    echo "  Uninstall:  curl -sSL https://raw.githubusercontent.com/rcourtman/Pulse/main/install.sh | bash -s -- --uninstall"

    if [[ "$IN_CONTAINER" == "true" ]]; then
        local proxy_ctid="${DETECTED_CTID:-<your-container-id>}"
        echo
        echo -e "${YELLOW}Temperature monitoring:${NC}"
        echo "  Run on the Proxmox host to enable secure temperature collection:"
        echo "    curl -fsSL https://raw.githubusercontent.com/rcourtman/Pulse/main/scripts/install-sensor-proxy.sh | \\"
        echo "      bash -s -- --ctid ${proxy_ctid} --pulse-server ${PULSE_URL}"
        echo "  See docs/TEMPERATURE_MONITORING.md for details."
    fi
    
    # Show auto-update status if timer exists
    if systemctl list-unit-files --no-legend | grep -q "^pulse-update.timer"; then
        echo
        echo -e "${YELLOW}Auto-updates:${NC}"
        if systemctl is-enabled --quiet pulse-update.timer 2>/dev/null; then
            echo -e "  Status:     ${GREEN}Enabled${NC} (daily check between 2-6 AM)"
            echo "  Disable:    systemctl disable --now pulse-update.timer"
        else
            echo -e "  Status:     ${YELLOW}Disabled${NC}"
            echo "  Enable:     systemctl enable --now pulse-update.timer"
        fi
    fi
    
    echo
}

# Main installation flow
main() {
    # Skip Proxmox host check if we're already inside a container
    if [[ "$IN_CONTAINER" != "true" ]] && check_proxmox_host; then
        create_lxc_container
        exit 0
    fi
    
    print_header
    check_root
    detect_os
    check_docker_environment
    
    # Check for existing installation FIRST before asking for configuration
    if check_existing_installation; then
        # If building from source was requested, skip the update prompt
        if [[ "$BUILD_FROM_SOURCE" == "true" ]]; then
            create_user
            setup_directories

            if ! build_from_source "$SOURCE_BRANCH"; then
                exit 1
            fi

            setup_update_command
            install_systemd_service

            if [[ "$ENABLE_AUTO_UPDATES" == "true" ]]; then
                setup_auto_updates
            fi

            start_pulse
            create_marker_file
            print_completion
            exit 0
        fi
        # If a specific version was requested, just update to it
        if [[ -n "${FORCE_VERSION}" ]]; then
            # Determine if this is an upgrade, downgrade, or reinstall
            local action_word="Installing"
            if [[ -n "$CURRENT_VERSION" ]] && [[ "$CURRENT_VERSION" != "unknown" ]]; then
                local compare_result
                compare_versions "$FORCE_VERSION" "$CURRENT_VERSION" && compare_result=$? || compare_result=$?
                case $compare_result in
                    0) action_word="Reinstalling" ;;
                    1) action_word="Updating to" ;;
                    2) action_word="Downgrading to" ;;
                esac
            fi
            print_info "${action_word} version ${FORCE_VERSION}..."
            LATEST_RELEASE="${FORCE_VERSION}"
            
            # Check if auto-updates should be offered when using --version
            # Same logic as update/reinstall paths
            if [[ "$ENABLE_AUTO_UPDATES" != "true" ]] && [[ "$IN_DOCKER" != "true" ]]; then
                local should_ask_about_updates=false
                local prompt_reason=""
                
                if ! systemctl list-unit-files --no-legend 2>/dev/null | grep -q "^pulse-update.timer"; then
                    # Timer doesn't exist - new feature
                    should_ask_about_updates=true
                    prompt_reason="new"
                elif [[ -f "$CONFIG_DIR/system.json" ]]; then
                    # Timer exists, check if it's properly configured
                    if grep -q '"autoUpdateEnabled":\s*false' "$CONFIG_DIR/system.json" 2>/dev/null; then
                        should_ask_about_updates=true
                        prompt_reason="disabled"
                    fi
                fi
                
                if [[ "$should_ask_about_updates" == "true" ]]; then
                    echo
                    if [[ "$prompt_reason" == "disabled" ]]; then
                        echo -e "${YELLOW}Auto-updates are currently disabled.${NC}"
                        echo "Would you like to enable automatic updates?"
                    else
                        echo -e "${YELLOW}New feature: Automatic updates!${NC}"
                    fi
                    echo "Pulse can automatically install stable updates daily (between 2-6 AM)"
                    echo "This keeps your installation secure and up-to-date."
                    safe_read_with_default "Enable auto-updates? [Y/n]: " enable_updates "y"
                    # Default to yes for this prompt since they're already updating
                    if [[ ! "$enable_updates" =~ ^[Nn]$ ]]; then
                        ENABLE_AUTO_UPDATES=true
                    fi
                fi
            fi
            
            # Detect the actual service name before trying to stop it
            SERVICE_NAME=$(detect_service_name)
            
            backup_existing
            systemctl stop $SERVICE_NAME || true
            create_user
            download_pulse
            setup_update_command
            
            # Setup auto-updates if requested
            if [[ "$ENABLE_AUTO_UPDATES" == "true" ]]; then
                setup_auto_updates
            fi
            
            start_pulse
            create_marker_file
            print_completion
            return 0
        fi
        
        # Get both stable and RC versions
        # Try GitHub API first, but have a fallback - with timeout protection
        local STABLE_VERSION=""
        if command -v timeout >/dev/null 2>&1; then
            STABLE_VERSION=$(timeout 10 curl -s --connect-timeout 5 --max-time 10 https://api.github.com/repos/$GITHUB_REPO/releases/latest 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)
        else
            STABLE_VERSION=$(curl -s --connect-timeout 5 --max-time 10 https://api.github.com/repos/$GITHUB_REPO/releases/latest 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)
        fi
        
        # If rate limited or failed, try direct GitHub latest URL
        if [[ -z "$STABLE_VERSION" ]] || [[ "$STABLE_VERSION" == *"rate limit"* ]]; then
            # Use the GitHub latest release redirect to get version
            if command -v timeout >/dev/null 2>&1; then
                STABLE_VERSION=$(timeout 10 curl -sI --connect-timeout 5 --max-time 10 https://github.com/$GITHUB_REPO/releases/latest 2>/dev/null | grep -i '^location:' | sed -E 's|.*tag/([^[:space:]]+).*|\1|' | tr -d '\r' || true)
            else
                STABLE_VERSION=$(curl -sI --connect-timeout 5 --max-time 10 https://github.com/$GITHUB_REPO/releases/latest 2>/dev/null | grep -i '^location:' | sed -E 's|.*tag/([^[:space:]]+).*|\1|' | tr -d '\r' || true)
            fi
        fi
        
        # For RC, we need the API, so if it fails just use empty
        local RC_VERSION=""
        if command -v timeout >/dev/null 2>&1; then
            RC_VERSION=$(timeout 10 curl -s --connect-timeout 5 --max-time 10 https://api.github.com/repos/$GITHUB_REPO/releases 2>/dev/null | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
        else
            RC_VERSION=$(curl -s --connect-timeout 5 --max-time 10 https://api.github.com/repos/$GITHUB_REPO/releases 2>/dev/null | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
        fi
        
        # Determine default update channel
        UPDATE_CHANNEL="stable"
        
        # Allow override via command line
        if [[ -n "${FORCE_CHANNEL}" ]]; then
            UPDATE_CHANNEL="${FORCE_CHANNEL}"
        elif [[ -f "$CONFIG_DIR/system.json" ]]; then
            CONFIGURED_CHANNEL=$(cat "$CONFIG_DIR/system.json" 2>/dev/null | grep -o '"updateChannel"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/' || true)
            if [[ "$CONFIGURED_CHANNEL" == "rc" ]]; then
                UPDATE_CHANNEL="rc"
            fi
        fi
        
        echo
        echo "What would you like to do?"
        
        # Show update options based on available versions
        local menu_option=1
        if [[ -n "$STABLE_VERSION" ]] && [[ "$STABLE_VERSION" != "$CURRENT_VERSION" ]]; then
            echo "${menu_option}) Update to $STABLE_VERSION (stable)"
            ((menu_option++))
        fi
        
        if [[ -n "$RC_VERSION" ]] && [[ "$RC_VERSION" != "$STABLE_VERSION" ]] && [[ "$RC_VERSION" != "$CURRENT_VERSION" ]]; then
            echo "${menu_option}) Update to $RC_VERSION (release candidate)"
            ((menu_option++))
        fi
        
        echo "${menu_option}) Reinstall current version"
        ((menu_option++))
        echo "${menu_option}) Remove Pulse"
        ((menu_option++))
        echo "${menu_option}) Cancel"
        local max_option=$menu_option
        
        # Try to read user choice interactively
        # safe_read handles both normal and piped input (via /dev/tty)
        if [[ "$IN_DOCKER" == "true" ]]; then
            # In Docker, always auto-select
            print_info "Docker environment detected. Auto-selecting update option."
            if [[ "$UPDATE_CHANNEL" == "rc" ]] && [[ -n "$RC_VERSION" ]] && [[ "$RC_VERSION" != "$STABLE_VERSION" ]]; then
                choice=2  # RC version
            else
                choice=1  # Stable version
            fi
        elif safe_read "Select option [1-${max_option}]: " choice; then
            # Successfully read user choice (either from stdin or /dev/tty)
            : # Do nothing, choice was set
        else
            # safe_read failed - truly non-interactive
            print_info "Non-interactive mode detected. Auto-selecting update option."
            if [[ "$UPDATE_CHANNEL" == "rc" ]] && [[ -n "$RC_VERSION" ]] && [[ "$RC_VERSION" != "$STABLE_VERSION" ]]; then
                choice=2  # RC version
            else
                choice=1  # Stable version
            fi
        fi
        
        # Debug: Check if choice was read correctly
        if [[ -z "$choice" ]]; then
            print_error "No option selected. Exiting."
            exit 1
        fi
        
        # Debug output to see what's happening
        # print_info "DEBUG: You selected option $choice"
        
        # Determine what action to take based on the dynamic menu
        local action=""
        local target_version=""
        local current_choice=1
        
        # Check if user selected stable update
        if [[ -n "$STABLE_VERSION" ]] && [[ "$STABLE_VERSION" != "$CURRENT_VERSION" ]]; then
            if [[ "$choice" == "$current_choice" ]]; then
                action="update"
                target_version="$STABLE_VERSION"
                UPDATE_CHANNEL="stable"
            fi
            ((current_choice++))
        fi
        
        # Check if user selected RC update
        if [[ -n "$RC_VERSION" ]] && [[ "$RC_VERSION" != "$STABLE_VERSION" ]] && [[ "$RC_VERSION" != "$CURRENT_VERSION" ]]; then
            if [[ "$choice" == "$current_choice" ]]; then
                action="update"
                target_version="$RC_VERSION"
                UPDATE_CHANNEL="rc"
            fi
            ((current_choice++))
        fi
        
        # Check if user selected reinstall
        if [[ "$choice" == "$current_choice" ]]; then
            action="reinstall"
        fi
        ((current_choice++))
        
        # Check if user selected remove
        if [[ "$choice" == "$current_choice" ]]; then
            action="remove"
        fi
        ((current_choice++))
        
        # Check if user selected cancel
        if [[ "$choice" == "$current_choice" ]]; then
            action="cancel"
        fi
        
        # Debug: Show what action was determined
        # print_info "DEBUG: Action determined: ${action:-'none'}"
        
        case $action in
            update)
                # Determine if this is an upgrade or downgrade
                local action_word="Installing"
                if [[ -n "$CURRENT_VERSION" ]] && [[ "$CURRENT_VERSION" != "unknown" ]]; then
                    local cmp_result=0
                    compare_versions "$target_version" "$CURRENT_VERSION" || cmp_result=$?
                    case $cmp_result in
                        0) action_word="Reinstalling" ;;
                        1) action_word="Updating to" ;;
                        2) action_word="Downgrading to" ;;
                    esac
                fi
                print_info "${action_word} $target_version..."
                LATEST_RELEASE="$target_version"
                
                # Check if auto-updates should be offered to the user
                # Offer if: not already forced by flag, not in Docker, and either:
                # 1. Timer doesn't exist (new feature), OR
                # 2. Timer exists but autoUpdateEnabled is false (misconfigured)
                if [[ "$ENABLE_AUTO_UPDATES" != "true" ]] && [[ "$IN_DOCKER" != "true" ]]; then
                    local should_ask_about_updates=false
                    local prompt_reason=""
                    
                    if ! systemctl list-unit-files --no-legend 2>/dev/null | grep -q "^pulse-update.timer"; then
                        # Timer doesn't exist - new feature
                        should_ask_about_updates=true
                        prompt_reason="new"
                    elif [[ -f "$CONFIG_DIR/system.json" ]]; then
                        # Timer exists, check if it's properly configured
                        if grep -q '"autoUpdateEnabled":\s*false' "$CONFIG_DIR/system.json" 2>/dev/null; then
                            should_ask_about_updates=true
                            prompt_reason="disabled"
                        fi
                    fi
                    
                    if [[ "$should_ask_about_updates" == "true" ]]; then
                        echo
                        if [[ "$prompt_reason" == "disabled" ]]; then
                            echo -e "${YELLOW}Auto-updates are currently disabled.${NC}"
                            echo "Would you like to enable automatic updates?"
                        else
                            echo -e "${YELLOW}New feature: Automatic updates!${NC}"
                        fi
                        echo "Pulse can automatically install stable updates daily (between 2-6 AM)"
                        echo "This keeps your installation secure and up-to-date."
                        safe_read_with_default "Enable auto-updates? [Y/n]: " enable_updates "y"
                        # Default to yes for this prompt since they're already updating
                        if [[ ! "$enable_updates" =~ ^[Nn]$ ]]; then
                            ENABLE_AUTO_UPDATES=true
                        fi
                    fi
                fi
                
                backup_existing
                systemctl stop $SERVICE_NAME || true
                create_user
                download_pulse
                setup_update_command
                
                # Setup auto-updates if requested during update
                if [[ "$ENABLE_AUTO_UPDATES" == "true" ]]; then
                    setup_auto_updates
                fi
                
                start_pulse
                create_marker_file
                print_completion
                exit 0
                ;;
            reinstall)
                # Check if auto-updates should be offered to the user
                # Offer if: not already forced by flag, not in Docker, and either:
                # 1. Timer doesn't exist (new feature), OR
                # 2. Timer exists but autoUpdateEnabled is false (misconfigured)
                if [[ "$ENABLE_AUTO_UPDATES" != "true" ]] && [[ "$IN_DOCKER" != "true" ]]; then
                    local should_ask_about_updates=false
                    local prompt_reason=""
                    
                    if ! systemctl list-unit-files --no-legend 2>/dev/null | grep -q "^pulse-update.timer"; then
                        # Timer doesn't exist - new feature
                        should_ask_about_updates=true
                        prompt_reason="new"
                    elif [[ -f "$CONFIG_DIR/system.json" ]]; then
                        # Timer exists, check if it's properly configured
                        if grep -q '"autoUpdateEnabled":\s*false' "$CONFIG_DIR/system.json" 2>/dev/null; then
                            should_ask_about_updates=true
                            prompt_reason="disabled"
                        fi
                    fi
                    
                    if [[ "$should_ask_about_updates" == "true" ]]; then
                        echo
                        if [[ "$prompt_reason" == "disabled" ]]; then
                            echo -e "${YELLOW}Auto-updates are currently disabled.${NC}"
                            echo "Would you like to enable automatic updates?"
                        else
                            echo -e "${YELLOW}New feature: Automatic updates!${NC}"
                        fi
                        echo "Pulse can automatically install stable updates daily (between 2-6 AM)"
                        echo "This keeps your installation secure and up-to-date."
                        safe_read_with_default "Enable auto-updates? [Y/n]: " enable_updates "y"
                        # Default to yes for this prompt
                        if [[ ! "$enable_updates" =~ ^[Nn]$ ]]; then
                            ENABLE_AUTO_UPDATES=true
                        fi
                    fi
                fi
                
                backup_existing
                systemctl stop $SERVICE_NAME || true
                create_user
                download_pulse
                setup_directories
                setup_update_command
                install_systemd_service
                
                # Setup auto-updates if requested during reinstall
                if [[ "$ENABLE_AUTO_UPDATES" == "true" ]]; then
                    setup_auto_updates
                fi
                
                start_pulse
                create_marker_file
                print_completion
                exit 0
                ;;
            remove)
                # Stop and disable service
                systemctl stop $SERVICE_NAME 2>/dev/null || true
                systemctl disable $SERVICE_NAME 2>/dev/null || true
                
                # Remove service files
                rm -f /etc/systemd/system/$SERVICE_NAME.service
                rm -f /etc/systemd/system/pulse.service
                rm -f /etc/systemd/system/pulse-backend.service
                # Reload systemd daemon
    safe_systemctl daemon-reload
                
                # Remove installation directory
                rm -rf "$INSTALL_DIR"
                
                # Remove symlink
                rm -f /usr/local/bin/pulse
                
                # Ask about config/data removal
                echo
                print_info "Config and data files exist in $CONFIG_DIR"
                safe_read_with_default "Remove all configuration and data? (y/N): " remove_config "n"
                if [[ "$remove_config" =~ ^[Yy]$ ]]; then
                    rm -rf "$CONFIG_DIR"
                    print_success "Configuration and data removed"
                else
                    print_info "Configuration preserved in $CONFIG_DIR"
                fi
                
                # Ask about user removal
                if id "pulse" &>/dev/null; then
                    safe_read_with_default "Remove pulse user account? (y/N): " remove_user "n"
                    if [[ "$remove_user" =~ ^[Yy]$ ]]; then
                        userdel pulse 2>/dev/null || true
                        print_success "User account removed"
                    else
                        print_info "User account preserved"
                    fi
                fi
                
                # Remove any log files
                rm -f /var/log/pulse*.log 2>/dev/null || true
                rm -f /opt/pulse.log 2>/dev/null || true
                
                print_success "Pulse removed successfully"
                ;;
            cancel)
                print_info "Installation cancelled"
                exit 0
                ;;
            *)
                print_error "Invalid option"
                exit 1
                ;;
        esac
    else
        # Check if this is truly a fresh installation or an update
        # Check for existing installation BEFORE we create directories
        # If binary exists OR system.json exists OR --version was specified, it's likely an update
        if [[ -f "$INSTALL_DIR/bin/pulse" ]] || [[ -f "$INSTALL_DIR/pulse" ]] || [[ -f "$CONFIG_DIR/system.json" ]] || [[ -n "${FORCE_VERSION}" ]]; then
            # This is an update/reinstall, don't prompt for port
            FRONTEND_PORT=${FRONTEND_PORT:-7655}
        else
            # Fresh installation - ask for port configuration and auto-updates
            FRONTEND_PORT=${FRONTEND_PORT:-}
            if [[ -z "$FRONTEND_PORT" ]]; then
                if [[ "$IN_DOCKER" == "true" ]] || [[ "$IN_CONTAINER" == "true" ]]; then
                    # In Docker/container mode, use default port without prompting
                    FRONTEND_PORT=7655
                else
                    echo
                    safe_read_with_default "Frontend port [7655]: " FRONTEND_PORT "7655"
                    if [[ ! "$FRONTEND_PORT" =~ ^[0-9]+$ ]] || [[ "$FRONTEND_PORT" -lt 1 ]] || [[ "$FRONTEND_PORT" -gt 65535 ]]; then
                        print_error "Invalid port number. Using default port 7655."
                        FRONTEND_PORT=7655
                    fi
                fi
            fi
            
            # Ask about auto-updates for fresh installation
            # Skip if: already set by flag, in Docker, or being installed from host (IN_CONTAINER=true)
            if [[ "$ENABLE_AUTO_UPDATES" != "true" ]] && [[ "$IN_DOCKER" != "true" ]] && [[ "$IN_CONTAINER" != "true" ]]; then
                echo
                echo "Enable automatic updates?"
                echo "Pulse can automatically install stable updates daily (between 2-6 AM)"
                safe_read_with_default "Enable auto-updates? [y/N]: " enable_updates "n"
                if [[ "$enable_updates" =~ ^[Yy]$ ]]; then
                    ENABLE_AUTO_UPDATES=true
                fi
            fi
        fi
        
        install_dependencies
        create_user
        setup_directories
        download_pulse
        setup_update_command
        install_systemd_service
        
        # Setup auto-updates if requested
        if [[ "$ENABLE_AUTO_UPDATES" == "true" ]]; then
            setup_auto_updates
        fi
        
        start_pulse
        create_marker_file
        print_completion
    fi
}

# Uninstall function
uninstall_pulse() {
    check_root
    print_header
    echo -e "\033[0;33mUninstalling Pulse...\033[0m"
    echo
    
    # Detect service name
    local SERVICE_NAME=$(detect_service_name)
    
    # Stop and disable service
    if systemctl is-active --quiet $SERVICE_NAME 2>/dev/null; then
        echo "Stopping $SERVICE_NAME..."
        systemctl stop $SERVICE_NAME
    fi
    
    if systemctl is-enabled --quiet $SERVICE_NAME 2>/dev/null; then
        echo "Disabling $SERVICE_NAME..."
        systemctl disable $SERVICE_NAME
    fi
    
    # Stop and disable auto-update timer if it exists
    if systemctl is-enabled --quiet pulse-update.timer 2>/dev/null; then
        echo "Disabling auto-update timer..."
        systemctl disable --now pulse-update.timer
    fi
    
    # Remove files
    echo "Removing Pulse files..."
    rm -rf /opt/pulse
    rm -rf /etc/pulse
    rm -f /etc/systemd/system/pulse.service
    rm -f /etc/systemd/system/pulse-backend.service
    rm -f /etc/systemd/system/pulse-update.service
    rm -f /etc/systemd/system/pulse-update.timer
    rm -f /usr/local/bin/pulse
    rm -f /usr/local/bin/pulse-auto-update.sh
    # Don't remove update commands - might be from community scripts
    # rm -f /usr/local/bin/update
    
    # Remove user (if it exists and isn't being used by other services)
    if id "pulse" &>/dev/null; then
        echo "Removing pulse user..."
        userdel pulse 2>/dev/null || true
    fi
    
    # Reload systemd
    systemctl daemon-reload
    
    echo
    echo -e "\033[0;32m✓ Pulse has been completely uninstalled\033[0m"
    exit 0
}

# Reset function
reset_pulse() {
    check_root
    print_header
    echo -e "\033[0;33mResetting Pulse configuration...\033[0m"
    echo
    
    # Detect service name
    local SERVICE_NAME=$(detect_service_name)
    
    # Stop service
    if systemctl is-active --quiet $SERVICE_NAME 2>/dev/null; then
        echo "Stopping $SERVICE_NAME..."
        systemctl stop $SERVICE_NAME
    fi
    
    # Remove config but keep binary
    echo "Removing configuration and data..."
    rm -rf /etc/pulse/*
    
    # Restart service
    echo "Starting $SERVICE_NAME with fresh configuration..."
    systemctl start $SERVICE_NAME
    
    echo
    echo -e "\033[0;32m✓ Pulse has been reset to fresh configuration\033[0m"
    echo "Access Pulse at: http://$(hostname -I | awk '{print $1}'):7655"
    exit 0
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --uninstall)
            uninstall_pulse
            ;;
        --reset)
            reset_pulse
            ;;
        --rc|--pre|--prerelease)
            FORCE_CHANNEL="rc"
            shift
            ;;
        --stable)
            FORCE_CHANNEL="stable"
            shift
            ;;
        --version)
            FORCE_VERSION="$2"
            shift 2
            ;;
        --in-container)
            IN_CONTAINER=true
            # Check if this is a Docker container (multiple detection methods)
            if [[ -f /.dockerenv ]] || \
               grep -q docker /proc/1/cgroup 2>/dev/null || \
               grep -q docker /proc/self/cgroup 2>/dev/null || \
               [[ -f /run/.containerenv ]] || \
               [[ "${container:-}" == "docker" ]]; then
                IN_DOCKER=true
            fi
            shift
            ;;
        --enable-auto-updates)
            ENABLE_AUTO_UPDATES=true
            shift
            ;;
        --proxy)
            PROXY_MODE="$2"
            if [[ ! "$PROXY_MODE" =~ ^(yes|no|auto)$ ]]; then
                print_error "Invalid --proxy value: $PROXY_MODE (must be yes, no, or auto)"
                exit 1
            fi
            shift 2
            ;;
        --source|--from-source|--branch)
            BUILD_FROM_SOURCE=true
            # Optional: specify branch
            if [[ $# -gt 1 ]] && [[ -n "$2" ]] && [[ ! "$2" =~ ^-- ]]; then
                SOURCE_BRANCH="$2"
                shift 2
            else
                SOURCE_BRANCH="main"
                shift
            fi
            ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Installation options:"
            echo "  --rc, --pre        Install latest RC/pre-release version"
            echo "  --stable           Install latest stable version (default)"
            echo "  --version VERSION  Install specific version (e.g., v4.4.0-rc.1)"
            echo "  --source [BRANCH]  Build and install from source (default: main)"
            echo "  --enable-auto-updates  Enable automatic stable updates (via systemd timer)"
            echo "  --proxy MODE       Control temperature proxy installation (yes/no/auto)"
            echo "                     yes: Install without prompting"
            echo "                     no: Skip proxy installation"
            echo "                     auto: Auto-detect (install if Docker present)"
            echo "                     (default: prompt user for LXC installations)"
            echo ""
            echo "Management options:"
            echo "  --reset            Reset Pulse to fresh configuration"
            echo "  --uninstall        Completely remove Pulse from system"
            echo ""
            echo "  -h, --help         Show this help message"
            exit 0
            ;;
        *)
            print_error "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

auto_detect_container_environment

# Export for use in download_pulse function
export FORCE_VERSION FORCE_CHANNEL

# Run main function
main
