package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/RouXx67/PulseUp/internal/alerts"
	"github.com/RouXx67/PulseUp/internal/config"
	"github.com/RouXx67/PulseUp/internal/models"
	"github.com/RouXx67/PulseUp/internal/monitoring"
	"github.com/RouXx67/PulseUp/internal/tempproxy"
	"github.com/RouXx67/PulseUp/internal/updates"
	"github.com/RouXx67/PulseUp/pkg/pbs"
	"github.com/RouXx67/PulseUp/pkg/proxmox"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
)

// DiagnosticsInfo contains comprehensive diagnostic information
type DiagnosticsInfo struct {
	Version          string                      `json:"version"`
	Runtime          string                      `json:"runtime"`
	Uptime           float64                     `json:"uptime"`
	Nodes            []NodeDiagnostic            `json:"nodes"`
	PBS              []PBSDiagnostic             `json:"pbs"`
	System           SystemDiagnostic            `json:"system"`
	TemperatureProxy *TemperatureProxyDiagnostic `json:"temperatureProxy,omitempty"`
	APITokens        *APITokenDiagnostic         `json:"apiTokens,omitempty"`
	DockerAgents     *DockerAgentDiagnostic      `json:"dockerAgents,omitempty"`
	Alerts           *AlertsDiagnostic           `json:"alerts,omitempty"`
	Errors           []string                    `json:"errors"`
	// NodeSnapshots captures the raw memory payload and derived usage Pulse last observed per node.
	NodeSnapshots []monitoring.NodeMemorySnapshot `json:"nodeSnapshots,omitempty"`
	// GuestSnapshots captures recent per-guest memory breakdowns (VM/LXC) with the raw Proxmox fields.
	GuestSnapshots []monitoring.GuestMemorySnapshot `json:"guestSnapshots,omitempty"`
	// MemorySources summarizes how many nodes currently rely on each memory source per instance.
	MemorySources []MemorySourceStat `json:"memorySources,omitempty"`
}

// MemorySourceStat aggregates memory-source usage per instance.
type MemorySourceStat struct {
 	Instance    string `json:"instance"`
	Source      string `json:"source"`
	NodeCount   int    `json:"nodeCount"`
	LastUpdated string `json:"lastUpdated"`
	Fallback    bool   `json:"fallback"`
}

func isFallbackMemorySource(source string) bool {
	switch strings.ToLower(source) {
	case "", "unknown", "nodes-endpoint", "node-status-used", "previous-snapshot":
		return true
	default:
		return false
	}
}

const diagnosticsCacheTTL = 45 * time.Second

var (
	diagnosticsMetricsOnce sync.Once

	diagnosticsCacheMu         sync.RWMutex
	diagnosticsCache           DiagnosticsInfo
	diagnosticsCacheTimestamp  time.Time

	diagnosticsCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "pulse",
		Subsystem: "diagnostics",
		Name:      "cache_hits_total",
		Help:      "Total number of diagnostics cache hits.",
	})

	diagnosticsCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "pulse",
		Subsystem: "diagnostics",
		Name:      "cache_misses_total",
		Help:      "Total number of diagnostics cache misses.",
	})

	diagnosticsRefreshDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "pulse",
		Subsystem: "diagnostics",
		Name:      "refresh_duration_seconds",
		Help:      "Duration of diagnostics refresh operations in seconds.",
		Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30},
	})
)

// NodeDiagnostic contains diagnostic info for a Proxmox node
type NodeDiagnostic struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Host          string             `json:"host"`
	Type          string             `json:"type"`
	AuthMethod    string             `json:"authMethod"`
	Connected     bool               `json:"connected"`
	Error         string             `json:"error,omitempty"`
	Details       *NodeDetails       `json:"details,omitempty"`
	LastPoll      string             `json:"lastPoll,omitempty"`
	ClusterInfo   *ClusterInfo       `json:"clusterInfo,omitempty"`
	VMDiskCheck   *VMDiskCheckResult `json:"vmDiskCheck,omitempty"`
	PhysicalDisks *PhysicalDiskCheck `json:"physicalDisks,omitempty"`
}

// NodeDetails contains node-specific details
type NodeDetails struct {
	NodeCount int    `json:"node_count,omitempty"`
	Version   string `json:"version,omitempty"`
}

// VMDiskCheckResult contains VM disk monitoring diagnostic results
type VMDiskCheckResult struct {
	VMsFound         int                `json:"vmsFound"`
	VMsWithAgent     int                `json:"vmsWithAgent"`
	VMsWithDiskData  int                `json:"vmsWithDiskData"`
	TestVMID         int                `json:"testVMID,omitempty"`
	TestVMName       string             `json:"testVMName,omitempty"`
	TestResult       string             `json:"testResult,omitempty"`
	Permissions      []string           `json:"permissions,omitempty"`
	Recommendations  []string           `json:"recommendations,omitempty"`
	ProblematicVMs   []VMDiskIssue      `json:"problematicVMs,omitempty"`
	FilesystemsFound []FilesystemDetail `json:"filesystemsFound,omitempty"`
}

type VMDiskIssue struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Issue  string `json:"issue"`
}

type FilesystemDetail struct {
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type"`
	Total      uint64 `json:"total"`
	Used       uint64 `json:"used"`
	Filtered   bool   `json:"filtered"`
	Reason     string `json:"reason,omitempty"`
}

// PhysicalDiskCheck contains diagnostic results for physical disk detection
type PhysicalDiskCheck struct {
	NodesChecked    int              `json:"nodesChecked"`
	NodesWithDisks  int              `json:"nodesWithDisks"`
	TotalDisks      int              `json:"totalDisks"`
	NodeResults     []NodeDiskResult `json:"nodeResults"`
	TestResult      string           `json:"testResult,omitempty"`
	Recommendations []string         `json:"recommendations,omitempty"`
}

type NodeDiskResult struct {
	NodeName    string   `json:"nodeName"`
	DiskCount   int      `json:"diskCount"`
	Error       string   `json:"error,omitempty"`
	DiskDevices []string `json:"diskDevices,omitempty"`
	APIResponse string   `json:"apiResponse,omitempty"`
}

// ClusterInfo contains cluster information
type ClusterInfo struct {
	Nodes int `json:"nodes"`
}

// PBSDiagnostic contains diagnostic info for a PBS instance
type PBSDiagnostic struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Host      string      `json:"host"`
	Connected bool        `json:"connected"`
	Error     string      `json:"error,omitempty"`
	Details   *PBSDetails `json:"details,omitempty"`
}

// PBSDetails contains PBS-specific details
type PBSDetails struct {
	Version string `json:"version,omitempty"`
}

// SystemDiagnostic contains system-level diagnostic info
type SystemDiagnostic struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	GoVersion    string `json:"goVersion"`
	NumCPU       int    `json:"numCPU"`
	NumGoroutine int    `json:"numGoroutine"`
	MemoryMB     uint64 `json:"memoryMB"`
}

// TemperatureProxyDiagnostic summarizes proxy detection state
type TemperatureProxyDiagnostic struct {
	LegacySSHDetected     bool     `json:"legacySSHDetected"`
	RecommendProxyUpgrade bool     `json:"recommendProxyUpgrade"`
	SocketFound           bool     `json:"socketFound"`
	SocketPath            string   `json:"socketPath,omitempty"`
	SocketPermissions     string   `json:"socketPermissions,omitempty"`
	SocketOwner           string   `json:"socketOwner,omitempty"`
	SocketGroup           string   `json:"socketGroup,omitempty"`
	ProxyReachable        bool     `json:"proxyReachable"`
	ProxyVersion          string   `json:"proxyVersion,omitempty"`
	ProxyPublicKeySHA256  string   `json:"proxyPublicKeySha256,omitempty"`
	ProxySSHDirectory     string   `json:"proxySshDirectory,omitempty"`
	LegacySSHKeyCount     int      `json:"legacySshKeyCount,omitempty"`
	Notes                 []string `json:"notes,omitempty"`
}

// APITokenDiagnostic reports on the state of the multi-token authentication system.
type APITokenDiagnostic struct {
	Enabled                bool              `json:"enabled"`
	TokenCount             int               `json:"tokenCount"`
	HasEnvTokens           bool              `json:"hasEnvTokens"`
	HasLegacyToken         bool              `json:"hasLegacyToken"`
	RecommendTokenSetup    bool              `json:"recommendTokenSetup"`
	RecommendTokenRotation bool              `json:"recommendTokenRotation"`
	LegacyDockerHostCount  int               `json:"legacyDockerHostCount,omitempty"`
	UnusedTokenCount       int               `json:"unusedTokenCount,omitempty"`
	Notes                  []string          `json:"notes,omitempty"`
	Tokens                 []APITokenSummary `json:"tokens,omitempty"`
	Usage                  []APITokenUsage   `json:"usage,omitempty"`
}

// APITokenSummary provides sanitized token metadata for diagnostics display.
type APITokenSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hint       string `json:"hint,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
	Source     string `json:"source,omitempty"`
}

// APITokenUsage summarises how tokens are consumed by connected agents.
type APITokenUsage struct {
	TokenID   string   `json:"tokenId"`
	HostCount int      `json:"hostCount"`
	Hosts     []string `json:"hosts,omitempty"`
}

// DockerAgentDiagnostic summarizes adoption of the Docker agent command system.
type DockerAgentDiagnostic struct {
	HostsTotal               int                    `json:"hostsTotal"`
	HostsOnline              int                    `json:"hostsOnline"`
	HostsReportingVersion    int                    `json:"hostsReportingVersion"`
	HostsWithTokenBinding    int                    `json:"hostsWithTokenBinding"`
	HostsWithoutTokenBinding int                    `json:"hostsWithoutTokenBinding"`
	HostsWithoutVersion      int                    `json:"hostsWithoutVersion,omitempty"`
	HostsOutdatedVersion     int                    `json:"hostsOutdatedVersion,omitempty"`
	HostsWithStaleCommand    int                    `json:"hostsWithStaleCommand,omitempty"`
	HostsPendingUninstall    int                    `json:"hostsPendingUninstall,omitempty"`
	HostsNeedingAttention    int                    `json:"hostsNeedingAttention"`
	RecommendedAgentVersion  string                 `json:"recommendedAgentVersion,omitempty"`
	Attention                []DockerAgentAttention `json:"attention,omitempty"`
	Notes                    []string               `json:"notes,omitempty"`
}

// DockerAgentAttention captures an individual agent that requires user action.
type DockerAgentAttention struct {
	HostID       string   `json:"hostId"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	AgentVersion string   `json:"agentVersion,omitempty"`
	TokenHint    string   `json:"tokenHint,omitempty"`
	LastSeen     string   `json:"lastSeen,omitempty"`
	Issues       []string `json:"issues"`
}

// AlertsDiagnostic summarises alert configuration migration state.
type AlertsDiagnostic struct {
	LegacyThresholdsDetected bool     `json:"legacyThresholdsDetected"`
	LegacyThresholdSources   []string `json:"legacyThresholdSources,omitempty"`
	LegacyScheduleSettings   []string `json:"legacyScheduleSettings,omitempty"`
	MissingCooldown          bool     `json:"missingCooldown"`
	MissingGroupingWindow    bool     `json:"missingGroupingWindow"`
	Notes                    []string `json:"notes,omitempty"`
}

// handleDiagnostics returns comprehensive diagnostic information
func (r *Router) handleDiagnostics(w http.ResponseWriter, req *http.Request) {
	diagnosticsMetricsOnce.Do(func() {
		prometheus.MustRegister(diagnosticsCacheHits, diagnosticsCacheMisses, diagnosticsRefreshDuration)
	})

	now := time.Now()

	diagnosticsCacheMu.RLock()
	cachedDiag := diagnosticsCache
	cachedAt := diagnosticsCacheTimestamp
	diagnosticsCacheMu.RUnlock()

	if !cachedAt.IsZero() && now.Sub(cachedAt) <= diagnosticsCacheTTL {
		diagnosticsCacheHits.Inc()
		writeDiagnosticsResponse(w, cachedDiag, cachedAt)
		return
	}

	diagnosticsCacheMisses.Inc()

	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	start := time.Now()
	fresh := r.computeDiagnostics(ctx)
	diagnosticsRefreshDuration.Observe(time.Since(start).Seconds())

	diagnosticsCacheMu.Lock()
	diagnosticsCache = fresh
	diagnosticsCacheTimestamp = time.Now()
	cachedAt = diagnosticsCacheTimestamp
	diagnosticsCacheMu.Unlock()

	writeDiagnosticsResponse(w, fresh, cachedAt)
}

func writeDiagnosticsResponse(w http.ResponseWriter, diag DiagnosticsInfo, cachedAt time.Time) {
	w.Header().Set("Content-Type", "application/json")
	if !cachedAt.IsZero() {
		w.Header().Set("X-Diagnostics-Cached-At", cachedAt.UTC().Format(time.RFC3339))
	}
	if err := json.NewEncoder(w).Encode(diag); err != nil {
		log.Error().Err(err).Msg("Failed to encode diagnostics")
		http.Error(w, "Failed to generate diagnostics", http.StatusInternalServerError)
	}
}

func (r *Router) computeDiagnostics(ctx context.Context) DiagnosticsInfo {
	diag := DiagnosticsInfo{
		Errors: []string{},
	}

	// Version info
	if versionInfo, err := updates.GetCurrentVersion(); err == nil {
		diag.Version = versionInfo.Version
		diag.Runtime = versionInfo.Runtime
	} else {
		diag.Version = "unknown"
		diag.Runtime = "go"
	}

	// Uptime
	diag.Uptime = time.Since(r.monitor.GetStartTime()).Seconds()

	// System info
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	diag.System = SystemDiagnostic{
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		GoVersion:    runtime.Version(),
		NumCPU:       runtime.NumCPU(),
		NumGoroutine: runtime.NumGoroutine(),
		MemoryMB:     memStats.Alloc / 1024 / 1024,
	}

	legacySSH, recommendProxy := r.detectLegacySSH()
	diag.TemperatureProxy = buildTemperatureProxyDiagnostic(r.config, legacySSH, recommendProxy)
	diag.APITokens = buildAPITokenDiagnostic(r.config, r.monitor)

	// Test each configured node
	for _, node := range r.config.PVEInstances {
		nodeDiag := NodeDiagnostic{
			ID:   node.Name,
			Name: node.Name,
			Host: node.Host,
			Type: "pve",
		}

		// Determine auth method (sanitized - don't expose actual values)
		if node.TokenName != "" && node.TokenValue != "" {
			nodeDiag.AuthMethod = "api_token"
		} else if node.User != "" && node.Password != "" {
			nodeDiag.AuthMethod = "username_password"
		} else {
			nodeDiag.AuthMethod = "none"
			nodeDiag.Error = "No authentication configured"
		}

		// Test connection
		testCfg := proxmox.ClientConfig{
			Host:       node.Host,
			User:       node.User,
			Password:   node.Password,
			TokenName:  node.TokenName,
			TokenValue: node.TokenValue,
			VerifySSL:  node.VerifySSL,
		}

		client, err := proxmox.NewClient(testCfg)
		if err != nil {
			nodeDiag.Connected = false
			nodeDiag.Error = err.Error()
		} else {
			nodes, err := client.GetNodes(ctx)
			if err != nil {
				nodeDiag.Connected = false
				nodeDiag.Error = "Failed to connect to Proxmox API: " + err.Error()
			} else {
				nodeDiag.Connected = true

				if len(nodes) > 0 {
					nodeDiag.Details = &NodeDetails{
						NodeCount: len(nodes),
					}

					if status, err := client.GetNodeStatus(ctx, nodes[0].Node); err == nil && status != nil {
						if status.PVEVersion != "" {
							nodeDiag.Details.Version = status.PVEVersion
						}
					}
				}

				if clusterStatus, err := client.GetClusterStatus(ctx); err == nil {
					nodeDiag.ClusterInfo = &ClusterInfo{Nodes: len(clusterStatus)}
				} else {
					log.Debug().Str("node", node.Name).Msg("Cluster status not available (likely standalone node)")
					nodeDiag.ClusterInfo = &ClusterInfo{Nodes: 1}
				}

				nodeDiag.VMDiskCheck = r.checkVMDiskMonitoring(ctx, client, node.Name)
				nodeDiag.PhysicalDisks = r.checkPhysicalDisks(ctx, client, node.Name)
			}
		}

		diag.Nodes = append(diag.Nodes, nodeDiag)
	}

	// Test PBS instances
	for _, pbsNode := range r.config.PBSInstances {
		pbsDiag := PBSDiagnostic{
			ID:   pbsNode.Name,
			Name: pbsNode.Name,
			Host: pbsNode.Host,
		}

		testCfg := pbs.ClientConfig{
			Host:        pbsNode.Host,
			User:        pbsNode.User,
			Password:    pbsNode.Password,
			TokenName:   pbsNode.TokenName,
			TokenValue:  pbsNode.TokenValue,
			Fingerprint: pbsNode.Fingerprint,
			VerifySSL:   pbsNode.VerifySSL,
		}

		client, err := pbs.NewClient(testCfg)
		if err != nil {
			pbsDiag.Connected = false
			pbsDiag.Error = err.Error()
		} else {
			if version, err := client.GetVersion(ctx); err != nil {
				pbsDiag.Connected = false
				pbsDiag.Error = "Connection established but version check failed: " + err.Error()
			} else {
				pbsDiag.Connected = true
				pbsDiag.Details = &PBSDetails{Version: version.Version}
			}
		}

		diag.PBS = append(diag.PBS, pbsDiag)
	}

	diag.DockerAgents = buildDockerAgentDiagnostic(r.monitor, diag.Version)
	diag.Alerts = buildAlertsDiagnostic(r.monitor)

	if r.monitor != nil {
		snapshots := r.monitor.GetDiagnosticSnapshots()
		if len(snapshots.Nodes) > 0 {
			diag.NodeSnapshots = snapshots.Nodes

			type memorySourceAgg struct {
				stat   MemorySourceStat
				latest time.Time
			}

			sourceAverages := make(map[string]*memorySourceAgg)
			for _, snap := range snapshots.Nodes {
				source := snap.MemorySource
				if source == "" {
					source = "unknown"
				}

				key := fmt.Sprintf("%s|%s", snap.Instance, source)
				entry, ok := sourceAverages[key]
				if !ok {
					entry = &memorySourceAgg{
						stat: MemorySourceStat{
							Instance: snap.Instance,
							Source:   source,
							Fallback: isFallbackMemorySource(source),
						},
					}
					sourceAverages[key] = entry
				}

				entry.stat.NodeCount++
				if snap.RetrievedAt.After(entry.latest) {
					entry.latest = snap.RetrievedAt
				}
			}

			if len(sourceAverages) > 0 {
				diag.MemorySources = make([]MemorySourceStat, 0, len(sourceAverages))
				for _, entry := range sourceAverages {
					if !entry.latest.IsZero() {
						entry.stat.LastUpdated = entry.latest.UTC().Format(time.RFC3339)
					}
					diag.MemorySources = append(diag.MemorySources, entry.stat)
				}

				sort.Slice(diag.MemorySources, func(i, j int) bool {
					if diag.MemorySources[i].Instance == diag.MemorySources[j].Instance {
						return diag.MemorySources[i].Source < diag.MemorySources[j].Source
					}
					return diag.MemorySources[i].Instance < diag.MemorySources[j].Instance
				})
			}
		}
		if len(snapshots.Guests) > 0 {
			diag.GuestSnapshots = snapshots.Guests
		}
	}

	return diag
}

func buildTemperatureProxyDiagnostic(cfg *config.Config, legacyDetected, recommendProxy bool) *TemperatureProxyDiagnostic {
	diag := &TemperatureProxyDiagnostic{
		LegacySSHDetected:     legacyDetected,
		RecommendProxyUpgrade: recommendProxy,
	}

	appendNote := func(note string) {
		if note == "" || contains(diag.Notes, note) {
			return
		}
		diag.Notes = append(diag.Notes, note)
	}

	socketPaths := []string{
		"/mnt/pulse-proxy/pulse-sensor-proxy.sock",
		"/run/pulse-sensor-proxy/pulse-sensor-proxy.sock",
	}

	for _, path := range socketPaths {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			appendNote(fmt.Sprintf("Unable to inspect proxy socket at %s: %v", path, err))
			continue
		}

		if info.Mode()&os.ModeSocket == 0 {
			continue
		}

		diag.SocketFound = true
		diag.SocketPath = path
		diag.SocketPermissions = fmt.Sprintf("%#o", info.Mode().Perm())

		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			diag.SocketOwner = resolveUserName(stat.Uid)
			diag.SocketGroup = resolveGroupName(stat.Gid)
		}
		break
	}

	if !diag.SocketFound {
		appendNote("No proxy socket detected inside the container. Remove the affected node in Pulse, then re-add it using the installer script from Settings → Nodes to regenerate the mount (or rerun the host installer script if you prefer).")
	} else if diag.SocketPath == "/run/pulse-sensor-proxy/pulse-sensor-proxy.sock" {
		appendNote("Proxy socket is exposed via /run. Remove and re-add this node with the Settings → Nodes installer script so the managed /mnt/pulse-proxy mount is applied (advanced: rerun the host installer script).")
	}

	client := tempproxy.NewClient()
	if client != nil && client.IsAvailable() {
		diag.ProxyReachable = true
		if status, err := client.GetStatus(); err != nil {
			appendNote(fmt.Sprintf("Failed to query pulse-sensor-proxy status: %v", err))
		} else {
			if version, ok := status["version"].(string); ok {
				diag.ProxyVersion = strings.TrimSpace(version)
			}
			if sshDir, ok := status["ssh_dir"].(string); ok {
				diag.ProxySSHDirectory = sshDir
			}
			if pubKey, ok := status["public_key"].(string); ok {
				if fingerprint, err := fingerprintPublicKey(pubKey); err == nil {
					diag.ProxyPublicKeySHA256 = fingerprint
				} else {
					appendNote(fmt.Sprintf("Unable to fingerprint proxy public key: %v", err))
				}
			}
		}
	} else {
		if diag.SocketFound {
			appendNote("Proxy socket is present but the daemon did not respond. Verify pulse-sensor-proxy.service is running on the host.")
		} else {
			appendNote("pulse-sensor-proxy was not detected. Run the host installer script to harden temperature monitoring.")
		}
	}

	if cfg != nil {
		dataDir := strings.TrimSpace(cfg.DataPath)
		if dataDir == "" {
			dataDir = "/etc/pulse"
		}
		if count, err := countLegacySSHKeys(filepath.Join(dataDir, ".ssh")); err != nil {
			appendNote(fmt.Sprintf("Unable to inspect legacy SSH directory: %v", err))
		} else if count > 0 {
			diag.LegacySSHKeyCount = count
			appendNote(fmt.Sprintf("Found %d SSH key(s) inside the Pulse data directory. Remove them after migrating to the secure proxy.", count))
		}
	}

	if diag.LegacySSHDetected && diag.RecommendProxyUpgrade {
		appendNote("Legacy SSH temperature collection detected. Remove each node from Pulse and re-add it using the installer script copied from Settings → Nodes (or rerun the host installer script) to migrate to the secure proxy.")
	}

	return diag
}

func buildAPITokenDiagnostic(cfg *config.Config, monitor *monitoring.Monitor) *APITokenDiagnostic {
	if cfg == nil {
		return nil
	}

	diag := &APITokenDiagnostic{
		Enabled:    cfg.APITokenEnabled && !cfg.DisableAuth,
		TokenCount: len(cfg.APITokens),
	}

	appendNote := func(note string) {
		if note == "" || contains(diag.Notes, note) {
			return
		}
		diag.Notes = append(diag.Notes, note)
	}

	envTokens := false
	if cfg.EnvOverrides != nil && (cfg.EnvOverrides["API_TOKEN"] || cfg.EnvOverrides["API_TOKENS"]) {
		envTokens = true
	}

	legacyToken := false
	for _, record := range cfg.APITokens {
		if strings.EqualFold(record.Name, "Environment token") {
			envTokens = true
		}
		if strings.EqualFold(record.Name, "Legacy token") {
			legacyToken = true
		}
	}

	diag.HasEnvTokens = envTokens
	diag.HasLegacyToken = legacyToken
	diag.RecommendTokenSetup = len(cfg.APITokens) == 0
	diag.RecommendTokenRotation = envTokens || legacyToken

	if cfg.DisableAuth {
		appendNote("Authentication is disabled (DISABLE_AUTH=1). Re-enable it to use per-agent API tokens.")
	} else if !cfg.APITokenEnabled && len(cfg.APITokens) > 0 {
		appendNote("API token authentication is currently disabled. Enable it under Settings → Security so agents can use dedicated tokens.")
	} else if diag.RecommendTokenSetup {
		appendNote("No API tokens are configured. Open Settings → Security to generate dedicated tokens for each automation or agent.")
	}

	tokens := make([]APITokenSummary, 0, len(cfg.APITokens))
	unusedCount := 0
	for _, record := range cfg.APITokens {
		summary := APITokenSummary{
			ID:   record.ID,
			Name: record.Name,
		}

		if !record.CreatedAt.IsZero() {
			summary.CreatedAt = record.CreatedAt.UTC().Format(time.RFC3339)
		}

		if record.LastUsedAt != nil && !record.LastUsedAt.IsZero() {
			summary.LastUsedAt = record.LastUsedAt.UTC().Format(time.RFC3339)
		} else {
			unusedCount++
		}

		switch {
		case record.Prefix != "" && record.Suffix != "":
			summary.Hint = fmt.Sprintf("%s…%s", record.Prefix, record.Suffix)
		case record.Prefix != "":
			summary.Hint = record.Prefix + "…"
		case record.Suffix != "":
			summary.Hint = "…" + record.Suffix
		}

		switch {
		case strings.EqualFold(record.Name, "Environment token"):
			summary.Source = "environment"
		case strings.EqualFold(record.Name, "Legacy token"):
			summary.Source = "legacy"
		default:
			summary.Source = "user"
		}

		tokens = append(tokens, summary)
	}

	diag.Tokens = tokens
	diag.UnusedTokenCount = unusedCount

	if len(cfg.APITokens) > 0 {
		if unusedCount == len(cfg.APITokens) {
			appendNote("Configured API tokens have not been used yet. Update your agents or automations to switch to the new tokens.")
		} else if unusedCount > 0 {
			appendNote(fmt.Sprintf("%d API token(s) have never been used. Remove unused tokens or update the corresponding agents.", unusedCount))
		}
	}

	tokenUsage := make(map[string][]string)
	legacyHosts := 0
	if monitor != nil {
		for _, host := range monitor.GetDockerHosts() {
			name := preferredDockerHostName(host)
			if strings.TrimSpace(host.TokenID) == "" {
				legacyHosts++
				continue
			}
			tokenID := strings.TrimSpace(host.TokenID)
			tokenUsage[tokenID] = append(tokenUsage[tokenID], name)
		}
	}

	diag.LegacyDockerHostCount = legacyHosts
	if legacyHosts > 0 {
		appendNote(fmt.Sprintf("%d Docker host(s) still rely on the shared API token. Generate dedicated tokens and rerun the installer from Settings → Docker Agents.", legacyHosts))
	}

	if len(tokenUsage) > 0 {
		keys := make([]string, 0, len(tokenUsage))
		for tokenID := range tokenUsage {
			keys = append(keys, tokenID)
		}
		sort.Strings(keys)

		diag.Usage = make([]APITokenUsage, 0, len(keys))
		for _, tokenID := range keys {
			hosts := tokenUsage[tokenID]
			sort.Strings(hosts)
			diag.Usage = append(diag.Usage, APITokenUsage{
				TokenID:   tokenID,
				HostCount: len(hosts),
				Hosts:     hosts,
			})
		}
	}

	if envTokens {
		appendNote("Environment-based API token detected. Migrate to tokens created in the UI for per-token tracking and safer rotation.")
	}
	if legacyToken {
		appendNote("Legacy token detected. Generate new API tokens and update integrations to benefit from per-token management.")
	}

	return diag
}

func buildDockerAgentDiagnostic(m *monitoring.Monitor, serverVersion string) *DockerAgentDiagnostic {
	if m == nil {
		return nil
	}

	hosts := m.GetDockerHosts()
	diag := &DockerAgentDiagnostic{
		HostsTotal:              len(hosts),
		RecommendedAgentVersion: normalizeVersionLabel(serverVersion),
	}

	appendNote := func(note string) {
		if note == "" || contains(diag.Notes, note) {
			return
		}
		diag.Notes = append(diag.Notes, note)
	}

	if len(hosts) == 0 {
		appendNote("No Docker agents have reported in yet. Use Settings → Docker Agents to install the container-side agent and unlock remote commands.")
		return diag
	}

	var (
		serverVer        *updates.Version
		recommendedLabel = diag.RecommendedAgentVersion
	)
	if serverVersion != "" {
		if parsed, err := updates.ParseVersion(serverVersion); err == nil {
			serverVer = parsed
			recommendedLabel = normalizeVersionLabel(parsed.String())
			diag.RecommendedAgentVersion = recommendedLabel
		}
	}

	now := time.Now().UTC()
	legacyTokenHosts := 0
	for _, host := range hosts {
		status := strings.ToLower(strings.TrimSpace(host.Status))
		if status == "online" {
			diag.HostsOnline++
		}
		versionStr := strings.TrimSpace(host.AgentVersion)
		if versionStr != "" {
			diag.HostsReportingVersion++
		} else {
			diag.HostsWithoutVersion++
		}

		if strings.TrimSpace(host.TokenID) != "" {
			diag.HostsWithTokenBinding++
		} else {
			legacyTokenHosts++
		}

		issues := make([]string, 0, 4)

		if status != "online" && status != "" {
			issues = append(issues, fmt.Sprintf("Host reports status %q.", status))
		}

		if versionStr == "" {
			issues = append(issues, "Agent has not reported a version (pre v4.24). Reinstall using Settings → Docker Agents.")
		} else if serverVer != nil {
			if agentVer, err := updates.ParseVersion(versionStr); err == nil {
				if agentVer.Compare(serverVer) < 0 {
					diag.HostsOutdatedVersion++
					issues = append(issues, fmt.Sprintf("Agent version %s lags behind the recommended %s. Re-run the installer to update.", normalizeVersionLabel(versionStr), recommendedLabel))
				}
			} else {
				issues = append(issues, fmt.Sprintf("Unrecognized agent version string %q. Reinstall to ensure command support.", versionStr))
			}
		}

		if strings.TrimSpace(host.TokenID) == "" {
			issues = append(issues, "Host is still using the shared API token. Generate a dedicated token in Settings → Security and rerun the installer.")
		}

		if !host.LastSeen.IsZero() && now.Sub(host.LastSeen.UTC()) > 10*time.Minute {
			issues = append(issues, fmt.Sprintf("No heartbeat since %s. Verify the agent container is running.", host.LastSeen.UTC().Format(time.RFC3339)))
		}

		if host.Command != nil {
			cmdStatus := strings.ToLower(strings.TrimSpace(host.Command.Status))
			switch cmdStatus {
			case monitoring.DockerCommandStatusQueued, monitoring.DockerCommandStatusDispatched, monitoring.DockerCommandStatusAcknowledged:
				message := fmt.Sprintf("Command %s is still in progress.", cmdStatus)
				if !host.Command.UpdatedAt.IsZero() && now.Sub(host.Command.UpdatedAt.UTC()) > 15*time.Minute {
					diag.HostsWithStaleCommand++
					message = fmt.Sprintf("Command %s has been pending since %s; consider allowing re-enrolment.", cmdStatus, host.Command.UpdatedAt.UTC().Format(time.RFC3339))
				}
				issues = append(issues, message)
			}
		}

		if host.PendingUninstall {
			diag.HostsPendingUninstall++
			issues = append(issues, "Host is pending uninstall; confirm the agent container stopped or clear the flag.")
		}

		if len(issues) == 0 {
			continue
		}

		diag.Attention = append(diag.Attention, DockerAgentAttention{
			HostID:       host.ID,
			Name:         preferredDockerHostName(host),
			Status:       host.Status,
			AgentVersion: versionStr,
			TokenHint:    host.TokenHint,
			LastSeen:     formatTimeMaybe(host.LastSeen),
			Issues:       issues,
		})
	}

	diag.HostsWithoutTokenBinding = legacyTokenHosts
	diag.HostsNeedingAttention = len(diag.Attention)

	if legacyTokenHosts > 0 {
		appendNote(fmt.Sprintf("%d Docker host(s) still rely on the shared API token. Migrate each host to a dedicated token via Settings → Security and rerun the installer.", legacyTokenHosts))
	}
	if diag.HostsOutdatedVersion > 0 {
		appendNote(fmt.Sprintf("%d Docker host(s) run an out-of-date agent. Re-run the installer from Settings → Docker Agents to upgrade them.", diag.HostsOutdatedVersion))
	}
	if diag.HostsWithoutVersion > 0 {
		appendNote(fmt.Sprintf("%d Docker host(s) have not reported an agent version yet. Reinstall the agent to enable the new command system.", diag.HostsWithoutVersion))
	}
	if diag.HostsWithStaleCommand > 0 {
		appendNote(fmt.Sprintf("%d Docker host command(s) appear stuck. Use the 'Allow re-enroll' action in Settings → Docker Agents to reset them.", diag.HostsWithStaleCommand))
	}
	if diag.HostsPendingUninstall > 0 {
		appendNote(fmt.Sprintf("%d Docker host(s) are pending uninstall. Confirm the uninstall or clear the flag from Settings → Docker Agents.", diag.HostsPendingUninstall))
	}
	if diag.HostsNeedingAttention == 0 {
		appendNote("All Docker agents are reporting with dedicated tokens and the expected version.")
	}

	return diag
}

func buildAlertsDiagnostic(m *monitoring.Monitor) *AlertsDiagnostic {
	if m == nil {
		return nil
	}

	manager := m.GetAlertManager()
	if manager == nil {
		return nil
	}

	config := manager.GetConfig()
	diag := &AlertsDiagnostic{}

	appendNote := func(note string) {
		if note == "" || contains(diag.Notes, note) {
			return
		}
		diag.Notes = append(diag.Notes, note)
	}

	legacySources := make([]string, 0, 4)
	if hasLegacyThresholds(config.GuestDefaults) {
		diag.LegacyThresholdsDetected = true
		legacySources = append(legacySources, "guest-defaults")
	}
	if hasLegacyThresholds(config.NodeDefaults) {
		diag.LegacyThresholdsDetected = true
		legacySources = append(legacySources, "node-defaults")
	}

	overrideIndex := 0
	for _, override := range config.Overrides {
		overrideIndex++
		if hasLegacyThresholds(override) {
			diag.LegacyThresholdsDetected = true
			legacySources = append(legacySources, fmt.Sprintf("override-%d", overrideIndex))
		}
	}

	for idx, rule := range config.CustomRules {
		if hasLegacyThresholds(rule.Thresholds) {
			diag.LegacyThresholdsDetected = true
			legacySources = append(legacySources, fmt.Sprintf("custom-%d", idx+1))
		}
	}

	if len(legacySources) > 0 {
		sort.Strings(legacySources)
		diag.LegacyThresholdSources = legacySources
		appendNote("Some alert rules still rely on legacy single-value thresholds. Edit and save them to enable hysteresis-based alerts.")
	}

	legacySchedule := make([]string, 0, 2)
	if config.TimeThreshold > 0 {
		legacySchedule = append(legacySchedule, "timeThreshold")
		appendNote("Global alert delay still uses the legacy timeThreshold setting. Save the alerts configuration to migrate to per-metric delays.")
	}
	if config.Schedule.GroupingWindow > 0 && config.Schedule.Grouping.Window == 0 {
		legacySchedule = append(legacySchedule, "groupingWindow")
		appendNote("Alert grouping uses the deprecated groupingWindow value. Update the schedule to use the new grouping options.")
	}
	if len(legacySchedule) > 0 {
		sort.Strings(legacySchedule)
		diag.LegacyScheduleSettings = legacySchedule
	}

	if config.Schedule.Cooldown <= 0 {
		diag.MissingCooldown = true
		appendNote("Alert cooldown is not configured. Set a cooldown under Settings → Alerts → Schedule to prevent alert storms.")
	}
	if config.Schedule.Grouping.Window <= 0 {
		diag.MissingGroupingWindow = true
		appendNote("Alert grouping window is disabled. Configure a grouping window to bundle related alerts.")
	}

	return diag
}

func fingerprintPublicKey(pub string) (string, error) {
	pub = strings.TrimSpace(pub)
	if pub == "" {
		return "", fmt.Errorf("empty public key")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(key), nil
}

func resolveUserName(uid uint32) string {
	uidStr := strconv.FormatUint(uint64(uid), 10)
	if usr, err := user.LookupId(uidStr); err == nil && usr.Username != "" {
		return usr.Username
	}
	return "uid:" + uidStr
}

func resolveGroupName(gid uint32) string {
	gidStr := strconv.FormatUint(uint64(gid), 10)
	if grp, err := user.LookupGroupId(gidStr); err == nil && grp != nil && grp.Name != "" {
		return grp.Name
	}
	return "gid:" + gidStr
}

func countLegacySSHKeys(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "id_") {
			count++
		}
	}
	return count, nil
}

func hasLegacyThresholds(th alerts.ThresholdConfig) bool {
	return th.CPULegacy != nil ||
		th.MemoryLegacy != nil ||
		th.DiskLegacy != nil ||
		th.DiskReadLegacy != nil ||
		th.DiskWriteLegacy != nil ||
		th.NetworkInLegacy != nil ||
		th.NetworkOutLegacy != nil
}

func preferredDockerHostName(host models.DockerHost) string {
	if name := strings.TrimSpace(host.DisplayName); name != "" {
		return name
	}
	if name := strings.TrimSpace(host.Hostname); name != "" {
		return name
	}
	if name := strings.TrimSpace(host.AgentID); name != "" {
		return name
	}
	return host.ID
}

func formatTimeMaybe(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func normalizeVersionLabel(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "v") {
		return value
	}
	first := value[0]
	if first < '0' || first > '9' {
		return value
	}
	return "v" + value
}

// checkVMDiskMonitoring performs diagnostic checks for VM disk monitoring
func (r *Router) checkVMDiskMonitoring(ctx context.Context, client *proxmox.Client, nodeName string) *VMDiskCheckResult {
	result := &VMDiskCheckResult{
		Recommendations: []string{},
		Permissions:     []string{},
	}

	// Get all nodes to check
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		result.TestResult = "Failed to get nodes: " + err.Error()
		return result
	}

	if len(nodes) == 0 {
		result.TestResult = "No nodes found"
		return result
	}

	// Fetch VMs once per node and keep lookup map
	nodeVMMap := make(map[string][]proxmox.VM)
	var allVMs []proxmox.VM
	for _, node := range nodes {
		vmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		vms, err := client.GetVMs(vmCtx, node.Node)
		cancel()
		if err != nil {
			log.Debug().Err(err).Str("node", node.Node).Msg("Failed to get VMs from node")
			continue
		}
		nodeVMMap[node.Node] = vms
		allVMs = append(allVMs, vms...)
	}

	result.VMsFound = len(allVMs)
	vms := allVMs

	if len(vms) == 0 {
		result.TestResult = "No VMs found to test"
		result.Recommendations = append(result.Recommendations, "Create a test VM to verify disk monitoring")
		return result
	}

	// Check VMs for agent and disk data
	var testVM *proxmox.VM
	var testVMNode string
	result.ProblematicVMs = []VMDiskIssue{}
	for i := range vms {
		vm := vms[i]
		if vm.Template == 0 && vm.Status == "running" {
			vmNode := strings.TrimSpace(vm.Node)
			if vmNode == "" {
				continue
			}

			// Check if agent is configured
			statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
			vmStatus, err := client.GetVMStatus(statusCtx, vmNode, vm.VMID)
			statusCancel()
			if err != nil {
				errStr := err.Error()
				result.ProblematicVMs = append(result.ProblematicVMs, VMDiskIssue{
					VMID:   vm.VMID,
					Name:   vm.Name,
					Status: vm.Status,
					Issue:  "Failed to get VM status: " + errStr,
				})
			} else if vmStatus != nil && vmStatus.Agent > 0 {
				result.VMsWithAgent++

				// Try to get filesystem info
				fsCtx, fsCancel := context.WithTimeout(ctx, 10*time.Second)
				fsInfo, err := client.GetVMFSInfo(fsCtx, vmNode, vm.VMID)
				fsCancel()
				if err != nil {
					result.ProblematicVMs = append(result.ProblematicVMs, VMDiskIssue{
						VMID:   vm.VMID,
						Name:   vm.Name,
						Status: vm.Status,
						Issue:  "Agent enabled but failed to get filesystem info: " + err.Error(),
					})
					if testVM == nil {
						testVM = &vms[i]
						testVMNode = vmNode
					}
				} else if len(fsInfo) == 0 {
					result.ProblematicVMs = append(result.ProblematicVMs, VMDiskIssue{
						VMID:   vm.VMID,
						Name:   vm.Name,
						Status: vm.Status,
						Issue:  "Agent returned no filesystem info",
					})
					if testVM == nil {
						testVM = &vms[i]
						testVMNode = vmNode
					}
				} else {
					// Check if we get usable disk data
					hasUsableFS := false
					for _, fs := range fsInfo {
						if fs.Type != "tmpfs" && fs.Type != "devtmpfs" &&
							!strings.HasPrefix(fs.Mountpoint, "/dev") &&
							!strings.HasPrefix(fs.Mountpoint, "/proc") &&
							!strings.HasPrefix(fs.Mountpoint, "/sys") &&
							fs.TotalBytes > 0 {
							hasUsableFS = true
							break
						}
					}

					if hasUsableFS {
						result.VMsWithDiskData++
					} else {
						result.ProblematicVMs = append(result.ProblematicVMs, VMDiskIssue{
							VMID:   vm.VMID,
							Name:   vm.Name,
							Status: vm.Status,
							Issue:  fmt.Sprintf("Agent returned %d filesystems but none are usable for disk metrics", len(fsInfo)),
						})
					}

					if testVM == nil {
						testVM = &vms[i]
						testVMNode = vmNode
					}
				}
			} else if vmStatus != nil {
				// Agent not enabled
				result.ProblematicVMs = append(result.ProblematicVMs, VMDiskIssue{
					VMID:   vm.VMID,
					Name:   vm.Name,
					Status: vm.Status,
					Issue:  "Guest agent not enabled in VM configuration",
				})
			}
		}
	}

	// Perform detailed test on one VM
	if testVM != nil {
		result.TestVMID = testVM.VMID
		result.TestVMName = testVM.Name

		// Check VM status for agent
		statusCtx, statusCancel := context.WithTimeout(ctx, 10*time.Second)
		vmStatus, err := client.GetVMStatus(statusCtx, testVMNode, testVM.VMID)
		statusCancel()
		if err != nil {
			errStr := err.Error()
			result.TestResult = "Failed to get VM status: " + errStr
			if errors.Is(err, context.DeadlineExceeded) || strings.Contains(errStr, "context deadline exceeded") {
				result.Recommendations = append(result.Recommendations,
					"VM status request timed out; check network connectivity to the node",
					"If this persists, increase the diagnostics timeout or reduce VM load during checks",
				)
			} else if strings.Contains(errStr, "403") || strings.Contains(errStr, "401") {
				result.Recommendations = append(result.Recommendations,
					"Ensure API token has PVEAuditor role for baseline access",
					"Add VM.GuestAgent.Audit (PVE 9) or VM.Monitor (PVE 8) privileges; Pulse setup adds these via the PulseMonitor role",
					"Include Sys.Audit when available for Ceph metrics",
				)
			} else {
				result.Recommendations = append(result.Recommendations,
					"Verify the node is reachable and API token is valid",
				)
			}
		} else if vmStatus == nil || vmStatus.Agent == 0 {
			result.TestResult = "Guest agent not enabled in VM configuration"
			result.Recommendations = append(result.Recommendations,
				"Enable QEMU Guest Agent in VM Options",
				"Install qemu-guest-agent package in the VM")
		} else {
			// Try to get filesystem info
			fsCtx, fsCancel := context.WithTimeout(ctx, 10*time.Second)
			fsInfo, err := client.GetVMFSInfo(fsCtx, testVMNode, testVM.VMID)
			fsCancel()
			if err != nil {
				errStr := err.Error()
				if strings.Contains(errStr, "500") || strings.Contains(errStr, "not running") {
					result.TestResult = "Guest agent not running inside VM"
					result.Recommendations = append(result.Recommendations,
						"SSH into VM and run: systemctl status qemu-guest-agent",
						"If not installed: apt install qemu-guest-agent",
						"If installed but not running: systemctl start qemu-guest-agent")
				} else if strings.Contains(errStr, "403") || strings.Contains(errStr, "401") {
					result.TestResult = "Permission denied accessing guest agent"
					result.Recommendations = append(result.Recommendations,
						"Ensure API token has PVEAuditor role for baseline access",
						"Add VM.GuestAgent.Audit (PVE 9) or VM.Monitor (PVE 8) privileges; Pulse setup adds these via the PulseMonitor role",
						"Include Sys.Audit when available for Ceph metrics")
				} else if errors.Is(err, context.DeadlineExceeded) || strings.Contains(errStr, "context deadline exceeded") {
					result.TestResult = "Guest agent request timed out"
					result.Recommendations = append(result.Recommendations,
						"Ensure the VM responds to guest agent queries promptly",
						"Consider increasing the diagnostics timeout if the environment is large",
					)
				} else {
					result.TestResult = "Failed to get guest agent data: " + errStr
				}
			} else if len(fsInfo) == 0 {
				result.TestResult = "Guest agent returned no filesystem info"
				result.Recommendations = append(result.Recommendations,
					"Guest agent may need restart inside VM",
					"Check VM has mounted filesystems")
			} else {
				// Calculate disk usage from filesystem info
				var totalBytes, usedBytes uint64
				result.FilesystemsFound = []FilesystemDetail{}

				for _, fs := range fsInfo {
					fsDetail := FilesystemDetail{
						Mountpoint: fs.Mountpoint,
						Type:       fs.Type,
						Total:      fs.TotalBytes,
						Used:       fs.UsedBytes,
					}

					// Check if this filesystem should be filtered
					if fs.Type == "tmpfs" || fs.Type == "devtmpfs" {
						fsDetail.Filtered = true
						fsDetail.Reason = "Special filesystem type"
					} else if strings.HasPrefix(fs.Mountpoint, "/dev") ||
						strings.HasPrefix(fs.Mountpoint, "/proc") ||
						strings.HasPrefix(fs.Mountpoint, "/sys") ||
						strings.HasPrefix(fs.Mountpoint, "/run") ||
						fs.Mountpoint == "/boot/efi" {
						fsDetail.Filtered = true
						fsDetail.Reason = "System mount point"
					} else if fs.TotalBytes == 0 {
						fsDetail.Filtered = true
						fsDetail.Reason = "Zero total bytes"
					} else {
						// This filesystem counts toward disk usage
						totalBytes += fs.TotalBytes
						usedBytes += fs.UsedBytes
					}

					result.FilesystemsFound = append(result.FilesystemsFound, fsDetail)
				}

				if totalBytes > 0 {
					percent := float64(usedBytes) / float64(totalBytes) * 100
					result.TestResult = fmt.Sprintf("SUCCESS: Guest agent working! Disk usage: %.1f%% (%d/%d bytes)",
						percent, usedBytes, totalBytes)
				} else {
					result.TestResult = fmt.Sprintf("Guest agent returned %d filesystems but no usable disk data (all filtered out)", len(fsInfo))
				}
			}
		}
	} else {
		result.TestResult = "No running VMs found to test"
		result.Recommendations = append(result.Recommendations, "Start a VM to test disk monitoring")
	}

	// Add general recommendations based on results
	if result.VMsWithAgent > 0 && result.VMsWithDiskData == 0 {
		result.Recommendations = append(result.Recommendations,
			"Guest agent is configured but not providing disk data",
			"Check guest agent is running inside VMs",
			"Verify API token permissions")
	}

	return result
}

// checkPhysicalDisks performs diagnostic checks for physical disk detection
func (r *Router) checkPhysicalDisks(ctx context.Context, client *proxmox.Client, instanceName string) *PhysicalDiskCheck {
	result := &PhysicalDiskCheck{
		Recommendations: []string{},
		NodeResults:     []NodeDiskResult{},
	}

	// Get all nodes
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		result.TestResult = "Failed to get nodes: " + err.Error()
		return result
	}

	result.NodesChecked = len(nodes)

	// Check each node for physical disks
	for _, node := range nodes {
		nodeResult := NodeDiskResult{
			NodeName: node.Node,
		}

		// Skip offline nodes
		if node.Status != "online" {
			nodeResult.Error = "Node is offline"
			result.NodeResults = append(result.NodeResults, nodeResult)
			continue
		}

		// Try to get disk list
		diskCtx, diskCancel := context.WithTimeout(ctx, 10*time.Second)
		disks, err := client.GetDisks(diskCtx, node.Node)
		diskCancel()
		if err != nil {
			errStr := err.Error()
			nodeResult.Error = errStr

			// Provide specific recommendations based on error
			if strings.Contains(errStr, "401") || strings.Contains(errStr, "403") {
				nodeResult.APIResponse = "Permission denied"
				if !contains(result.Recommendations, "Check API token has sufficient permissions for disk monitoring") {
					result.Recommendations = append(result.Recommendations,
						"Check API token has sufficient permissions for disk monitoring",
						"Token needs at least PVEAuditor role on the node")
				}
			} else if errors.Is(err, context.DeadlineExceeded) || strings.Contains(errStr, "context deadline exceeded") {
				nodeResult.APIResponse = "Timeout"
				if !contains(result.Recommendations, "Disk query timed out; verify node connectivity and load") {
					result.Recommendations = append(result.Recommendations,
						"Disk query timed out; verify node connectivity and load",
						"Increase diagnostics timeout if nodes are slow to respond")
				}
			} else if strings.Contains(errStr, "404") || strings.Contains(errStr, "501") {
				nodeResult.APIResponse = "Endpoint not available"
				if !contains(result.Recommendations, "Node may be running older Proxmox version without disk API support") {
					result.Recommendations = append(result.Recommendations,
						"Node may be running older Proxmox version without disk API support",
						"Check if node is running on non-standard hardware (Raspberry Pi, etc)")
				}
			} else {
				nodeResult.APIResponse = "API error"
			}
		} else {
			nodeResult.DiskCount = len(disks)
			if len(disks) > 0 {
				result.NodesWithDisks++
				result.TotalDisks += len(disks)

				// List disk devices
				for _, disk := range disks {
					nodeResult.DiskDevices = append(nodeResult.DiskDevices, disk.DevPath)
				}
			} else {
				nodeResult.APIResponse = "Empty response (no traditional disks found)"
				// This could be normal for SD card/USB based systems
				if !contains(result.Recommendations, "Some nodes returned no disks - may be using SD cards or USB storage") {
					result.Recommendations = append(result.Recommendations,
						"Some nodes returned no disks - may be using SD cards or USB storage",
						"Proxmox disk API only returns SATA/NVMe/SAS disks, not SD cards")
				}
			}
		}

		result.NodeResults = append(result.NodeResults, nodeResult)
	}

	// Generate summary
	if result.NodesChecked == 0 {
		result.TestResult = "No nodes found to check"
	} else if result.NodesWithDisks == 0 {
		result.TestResult = fmt.Sprintf("Checked %d nodes, none returned physical disks", result.NodesChecked)
	} else {
		result.TestResult = fmt.Sprintf("Found %d disks across %d of %d nodes",
			result.TotalDisks, result.NodesWithDisks, result.NodesChecked)
	}

	return result
}

// Helper function to check if slice contains string
func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}
