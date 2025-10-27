// Package config manages Pulse configuration from multiple sources.
//
// Configuration File Separation:
//   - .env: Authentication credentials ONLY (PULSE_AUTH_USER, PULSE_AUTH_PASS, API_TOKEN/API_TOKENS)
//   - system.json: Application settings (polling interval, timeouts, update settings, etc.)
//   - nodes.enc: Encrypted node credentials (PVE/PBS passwords and tokens)
//
// This separation ensures security, clarity, and proper access control.
// See docs/CONFIGURATION.md for detailed documentation.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"github.com/RouXx67/PulseUp/internal/auth"
	"github.com/RouXx67/PulseUp/internal/logging"
	"github.com/rs/zerolog/log"
)

// IsPasswordHashed checks if a string looks like a bcrypt hash
func IsPasswordHashed(password string) bool {
	// Bcrypt hashes start with $2a$, $2b$, or $2y$ and are 60 characters long
	// We check for >= 55 to catch truncated hashes and warn users
	if !strings.HasPrefix(password, "$2") {
		return false
	}

	length := len(password)
	if length == 60 {
		return true // Perfect bcrypt hash
	}

	// Warn about truncated or invalid hashes
	if length >= 55 && length < 60 {
		log.Error().
			Int("length", length).
			Str("hash_start", password[:20]+"...").
			Msg("Bcrypt hash appears truncated! Should be 60 characters. Password will be treated as plaintext.")
		return false // Treat as plaintext to force user to fix it
	}

	return false
}

// Config holds all application configuration
// NOTE: The envconfig tags are legacy and not used - configuration is loaded from encrypted JSON files
type Config struct {
	// Server settings
	BackendHost  string `envconfig:"BACKEND_HOST" default:"0.0.0.0"`
	BackendPort  int    `envconfig:"BACKEND_PORT" default:"3000"`
	FrontendHost string `envconfig:"FRONTEND_HOST" default:"0.0.0.0"`
	FrontendPort int    `envconfig:"FRONTEND_PORT" default:"7655"`
	ConfigPath   string `envconfig:"CONFIG_PATH" default:"/etc/pulse"`
	DataPath     string `envconfig:"DATA_PATH" default:"/var/lib/pulse"`
	PublicURL    string `envconfig:"PULSE_PUBLIC_URL" default:""` // Full URL to access Pulse (e.g., http://192.168.1.100:7655)

	// Proxmox VE connections
	PVEInstances []PVEInstance

	// Proxmox Backup Server connections
	PBSInstances []PBSInstance

	// Proxmox Mail Gateway connections
	PMGInstances []PMGInstance

	// Monitoring settings
	// Note: PVE polling is hardcoded to 10s since Proxmox cluster/resources endpoint only updates every 10s
	PBSPollingInterval    time.Duration `envconfig:"PBS_POLLING_INTERVAL"` // PBS polling interval (60s default)
	PMGPollingInterval    time.Duration `envconfig:"PMG_POLLING_INTERVAL"` // PMG polling interval (60s default)
	ConcurrentPolling     bool          `envconfig:"CONCURRENT_POLLING" default:"true"`
	ConnectionTimeout     time.Duration `envconfig:"CONNECTION_TIMEOUT" default:"45s"` // Increased for slow storage operations
	MetricsRetentionDays  int           `envconfig:"METRICS_RETENTION_DAYS" default:"7"`
	BackupPollingCycles   int           `envconfig:"BACKUP_POLLING_CYCLES" default:"10"`
	BackupPollingInterval time.Duration `envconfig:"BACKUP_POLLING_INTERVAL"`
	EnableBackupPolling   bool          `envconfig:"ENABLE_BACKUP_POLLING" default:"true"`
	WebhookBatchDelay     time.Duration `envconfig:"WEBHOOK_BATCH_DELAY" default:"10s"`
	AdaptivePollingEnabled      bool          `envconfig:"ADAPTIVE_POLLING_ENABLED" default:"false"`
	AdaptivePollingBaseInterval time.Duration `envconfig:"ADAPTIVE_POLLING_BASE_INTERVAL" default:"10s"`
	AdaptivePollingMinInterval  time.Duration `envconfig:"ADAPTIVE_POLLING_MIN_INTERVAL" default:"5s"`
	AdaptivePollingMaxInterval  time.Duration `envconfig:"ADAPTIVE_POLLING_MAX_INTERVAL" default:"5m"`

	// Logging settings
	LogLevel    string `envconfig:"LOG_LEVEL" default:"info"`
	LogFormat   string `envconfig:"LOG_FORMAT" default:"auto"` // "json", "console", or "auto"
	LogFile     string `envconfig:"LOG_FILE" default:""`
	LogMaxSize  int    `envconfig:"LOG_MAX_SIZE" default:"100"` // MB
	LogMaxAge   int    `envconfig:"LOG_MAX_AGE" default:"30"`   // days
	LogCompress bool   `envconfig:"LOG_COMPRESS" default:"true"`

	// Security settings
	APIToken             string           `envconfig:"API_TOKEN"`
	APITokenEnabled      bool             `envconfig:"API_TOKEN_ENABLED" default:"false"`
	APITokens            []APITokenRecord `json:"-"`
	AuthUser             string           `envconfig:"PULSE_AUTH_USER"`
	AuthPass             string           `envconfig:"PULSE_AUTH_PASS"`
	DisableAuth          bool             `envconfig:"DISABLE_AUTH" default:"false"`
	DemoMode             bool             `envconfig:"DEMO_MODE" default:"false"` // Read-only demo mode
	AllowedOrigins       string           `envconfig:"ALLOWED_ORIGINS" default:"*"`
	IframeEmbeddingAllow string           `envconfig:"IFRAME_EMBEDDING_ALLOW" default:"SAMEORIGIN"`

	// Proxy authentication settings
	ProxyAuthSecret        string `envconfig:"PROXY_AUTH_SECRET"`
	ProxyAuthUserHeader    string `envconfig:"PROXY_AUTH_USER_HEADER"`
	ProxyAuthRoleHeader    string `envconfig:"PROXY_AUTH_ROLE_HEADER"`
	ProxyAuthRoleSeparator string `envconfig:"PROXY_AUTH_ROLE_SEPARATOR" default:"|"`
	ProxyAuthAdminRole     string `envconfig:"PROXY_AUTH_ADMIN_ROLE" default:"admin"`
	ProxyAuthLogoutURL     string `envconfig:"PROXY_AUTH_LOGOUT_URL"`

	// OIDC configuration
	OIDC *OIDCConfig `json:"-"`
	// HTTPS/TLS settings
	HTTPSEnabled bool   `envconfig:"HTTPS_ENABLED" default:"false"`
	TLSCertFile  string `envconfig:"TLS_CERT_FILE" default:""`
	TLSKeyFile   string `envconfig:"TLS_KEY_FILE" default:""`

	// Update settings
	UpdateChannel           string        `envconfig:"UPDATE_CHANNEL" default:"stable"`
	AutoUpdateEnabled       bool          `envconfig:"AUTO_UPDATE_ENABLED" default:"false"`
	AutoUpdateCheckInterval time.Duration `envconfig:"AUTO_UPDATE_CHECK_INTERVAL" default:"24h"`
	AutoUpdateTime          string        `envconfig:"AUTO_UPDATE_TIME" default:"03:00"`

	// Discovery settings
	DiscoveryEnabled bool            `envconfig:"DISCOVERY_ENABLED" default:"false"`
	DiscoverySubnet  string          `envconfig:"DISCOVERY_SUBNET" default:"auto"`
	Discovery        DiscoveryConfig `json:"discoveryConfig"`

	// Deprecated - for backward compatibility
	Port  int  `envconfig:"PORT"` // Maps to BackendPort
	Debug bool `envconfig:"DEBUG" default:"false"`

	// Track which settings are overridden by environment variables
	EnvOverrides map[string]bool `json:"-"`
}

// DiscoveryConfig captures overrides for network discovery behaviour.
type DiscoveryConfig struct {
	EnvironmentOverride string   `json:"environment_override,omitempty"`
	SubnetAllowlist     []string `json:"subnet_allowlist,omitempty"`
	SubnetBlocklist     []string `json:"subnet_blocklist,omitempty"`
	MaxHostsPerScan     int      `json:"max_hosts_per_scan,omitempty"`
	MaxConcurrent       int      `json:"max_concurrent,omitempty"`
	EnableReverseDNS    bool     `json:"enable_reverse_dns"`
	ScanGateways        bool     `json:"scan_gateways"`
	DialTimeout         int      `json:"dial_timeout_ms,omitempty"`
	HTTPTimeout         int      `json:"http_timeout_ms,omitempty"`
}

// DefaultDiscoveryConfig returns opinionated defaults for discovery behaviour.
func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{
		EnvironmentOverride: "auto",
		SubnetAllowlist:     nil,
		SubnetBlocklist:     []string{"169.254.0.0/16"},
		MaxHostsPerScan:     1024,
		MaxConcurrent:       50,
		EnableReverseDNS:    true,
		ScanGateways:        true,
		DialTimeout:         1000,
		HTTPTimeout:         2000,
	}
}

// CloneDiscoveryConfig returns a deep copy of the provided discovery config.
func CloneDiscoveryConfig(cfg DiscoveryConfig) DiscoveryConfig {
	clone := cfg
	if cfg.SubnetAllowlist != nil {
		clone.SubnetAllowlist = append([]string(nil), cfg.SubnetAllowlist...)
	}
	if cfg.SubnetBlocklist != nil {
		clone.SubnetBlocklist = append([]string(nil), cfg.SubnetBlocklist...)
	}
	return clone
}

// NormalizeDiscoveryConfig ensures a discovery config contains sane values and defaults.
func NormalizeDiscoveryConfig(cfg DiscoveryConfig) DiscoveryConfig {
	defaults := DefaultDiscoveryConfig()
	normalized := CloneDiscoveryConfig(cfg)

	// Normalize environment override and ensure it's valid.
	normalized.EnvironmentOverride = strings.TrimSpace(normalized.EnvironmentOverride)
	if normalized.EnvironmentOverride == "" {
		normalized.EnvironmentOverride = defaults.EnvironmentOverride
	} else if !IsValidDiscoveryEnvironment(normalized.EnvironmentOverride) {
		log.Warn().
			Str("environment", normalized.EnvironmentOverride).
			Msg("Unknown discovery environment override detected; falling back to auto")
		normalized.EnvironmentOverride = defaults.EnvironmentOverride
	}

	normalized.SubnetAllowlist = sanitizeCIDRList(normalized.SubnetAllowlist)
	if normalized.SubnetAllowlist == nil {
		normalized.SubnetAllowlist = []string{}
	}

	normalized.SubnetBlocklist = sanitizeCIDRList(normalized.SubnetBlocklist)
	if normalized.SubnetBlocklist == nil {
		normalized.SubnetBlocklist = append([]string(nil), defaults.SubnetBlocklist...)
	}

	if normalized.MaxHostsPerScan <= 0 {
		normalized.MaxHostsPerScan = defaults.MaxHostsPerScan
	}
	if normalized.MaxConcurrent <= 0 {
		normalized.MaxConcurrent = defaults.MaxConcurrent
	}
	if normalized.DialTimeout <= 0 {
		normalized.DialTimeout = defaults.DialTimeout
	}
	if normalized.HTTPTimeout <= 0 {
		normalized.HTTPTimeout = defaults.HTTPTimeout
	}

	return normalized
}

func sanitizeCIDRList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cleaned := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		// Avoid duplicates to keep config minimal.
		if _, exists := seen[entry]; exists {
			continue
		}
		seen[entry] = struct{}{}
		cleaned = append(cleaned, entry)
	}
	if len(cleaned) == 0 {
		return []string{}
	}
	return cleaned
}

// UnmarshalJSON supports both legacy camelCase and new snake_case field names.
func (d *DiscoveryConfig) UnmarshalJSON(data []byte) error {
	type modern struct {
		EnvironmentOverride *string   `json:"environment_override"`
		SubnetAllowlist     *[]string `json:"subnet_allowlist"`
		SubnetBlocklist     *[]string `json:"subnet_blocklist"`
		MaxHostsPerScan     *int      `json:"max_hosts_per_scan"`
		MaxConcurrent       *int      `json:"max_concurrent"`
		EnableReverseDNS    *bool     `json:"enable_reverse_dns"`
		ScanGateways        *bool     `json:"scan_gateways"`
		DialTimeout         *int      `json:"dial_timeout_ms"`
		HTTPTimeout         *int      `json:"http_timeout_ms"`
	}
	type legacy struct {
		EnvironmentOverride *string   `json:"environmentOverride"`
		SubnetAllowlist     *[]string `json:"subnetAllowlist"`
		SubnetBlocklist     *[]string `json:"subnetBlocklist"`
		MaxHostsPerScan     *int      `json:"maxHostsPerScan"`
		MaxConcurrent       *int      `json:"maxConcurrent"`
		EnableReverseDNS    *bool     `json:"enableReverseDns"`
		ScanGateways        *bool     `json:"scanGateways"`
		DialTimeout         *int      `json:"dialTimeoutMs"`
		HTTPTimeout         *int      `json:"httpTimeoutMs"`
	}

	var modernPayload modern
	if err := json.Unmarshal(data, &modernPayload); err != nil {
		return err
	}

	var legacyPayload legacy
	_ = json.Unmarshal(data, &legacyPayload)

	cfg := DefaultDiscoveryConfig()

	if modernPayload.EnvironmentOverride != nil {
		cfg.EnvironmentOverride = strings.TrimSpace(*modernPayload.EnvironmentOverride)
	} else if legacyPayload.EnvironmentOverride != nil {
		cfg.EnvironmentOverride = strings.TrimSpace(*legacyPayload.EnvironmentOverride)
	}

	switch {
	case modernPayload.SubnetAllowlist != nil:
		cfg.SubnetAllowlist = sanitizeCIDRList(*modernPayload.SubnetAllowlist)
	case legacyPayload.SubnetAllowlist != nil:
		cfg.SubnetAllowlist = sanitizeCIDRList(*legacyPayload.SubnetAllowlist)
	default:
		cfg.SubnetAllowlist = []string{}
	}

	switch {
	case modernPayload.SubnetBlocklist != nil:
		cfg.SubnetBlocklist = sanitizeCIDRList(*modernPayload.SubnetBlocklist)
	case legacyPayload.SubnetBlocklist != nil:
		cfg.SubnetBlocklist = sanitizeCIDRList(*legacyPayload.SubnetBlocklist)
	}

	if modernPayload.MaxHostsPerScan != nil {
		cfg.MaxHostsPerScan = *modernPayload.MaxHostsPerScan
	} else if legacyPayload.MaxHostsPerScan != nil {
		cfg.MaxHostsPerScan = *legacyPayload.MaxHostsPerScan
	}

	if modernPayload.MaxConcurrent != nil {
		cfg.MaxConcurrent = *modernPayload.MaxConcurrent
	} else if legacyPayload.MaxConcurrent != nil {
		cfg.MaxConcurrent = *legacyPayload.MaxConcurrent
	}

	if modernPayload.EnableReverseDNS != nil {
		cfg.EnableReverseDNS = *modernPayload.EnableReverseDNS
	} else if legacyPayload.EnableReverseDNS != nil {
		cfg.EnableReverseDNS = *legacyPayload.EnableReverseDNS
	}

	if modernPayload.ScanGateways != nil {
		cfg.ScanGateways = *modernPayload.ScanGateways
	} else if legacyPayload.ScanGateways != nil {
		cfg.ScanGateways = *legacyPayload.ScanGateways
	}

	if modernPayload.DialTimeout != nil {
		cfg.DialTimeout = *modernPayload.DialTimeout
	} else if legacyPayload.DialTimeout != nil {
		cfg.DialTimeout = *legacyPayload.DialTimeout
	}

	if modernPayload.HTTPTimeout != nil {
		cfg.HTTPTimeout = *modernPayload.HTTPTimeout
	} else if legacyPayload.HTTPTimeout != nil {
		cfg.HTTPTimeout = *legacyPayload.HTTPTimeout
	}

	*d = NormalizeDiscoveryConfig(cfg)
	return nil
}

// IsValidDiscoveryEnvironment reports whether the supplied override is recognised.
func IsValidDiscoveryEnvironment(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto", "native", "docker_host", "docker_bridge", "lxc_privileged", "lxc_unprivileged":
		return true
	default:
		return false
	}
}

func splitAndTrim(value string) []string {
	if value == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// PVEInstance represents a Proxmox VE connection
type PVEInstance struct {
	Name                       string
	Host                       string // Primary endpoint (user-provided)
	User                       string
	Password                   string
	TokenName                  string
	TokenValue                 string
	Fingerprint                string
	VerifySSL                  bool
	MonitorVMs                 bool
	MonitorContainers          bool
	MonitorStorage             bool
	MonitorBackups             bool
	MonitorPhysicalDisks       *bool // Monitor physical disks (nil = enabled by default, can be explicitly disabled)
	PhysicalDiskPollingMinutes int   // How often to poll physical disks (0 = use default)

	// Cluster support
	IsCluster        bool              // True if this is a cluster
	ClusterName      string            // Cluster name if applicable
	ClusterEndpoints []ClusterEndpoint // All discovered cluster nodes
}

// ClusterEndpoint represents a single node in a cluster
type ClusterEndpoint struct {
	NodeID   string    // Node ID in cluster
	NodeName string    // Node name
	Host     string    // Full URL (e.g., https://node1.lan:8006)
	IP       string    // IP address
	Online   bool      // Current online status
	LastSeen time.Time // Last successful connection
}

// PBSInstance represents a Proxmox Backup Server connection
type PBSInstance struct {
	Name               string
	Host               string
	User               string
	Password           string
	TokenName          string
	TokenValue         string
	Fingerprint        string
	VerifySSL          bool
	MonitorBackups     bool
	MonitorDatastores  bool
	MonitorSyncJobs    bool
	MonitorVerifyJobs  bool
	MonitorPruneJobs   bool
	MonitorGarbageJobs bool
}

// PMGInstance represents a Proxmox Mail Gateway connection
type PMGInstance struct {
	Name        string
	Host        string
	User        string
	Password    string
	TokenName   string
	TokenValue  string
	Fingerprint string
	VerifySSL   bool

	MonitorMailStats   bool
	MonitorQueues      bool
	MonitorQuarantine  bool
	MonitorDomainStats bool
}

// Global persistence instance for saving
var globalPersistence *ConfigPersistence

// Load reads configuration from encrypted persistence files
func Load() (*Config, error) {
	// Get data directory from environment
	dataDir := "/etc/pulse"
	if dir := os.Getenv("PULSE_DATA_DIR"); dir != "" {
		dataDir = dir
	}

	// Load .env file if it exists (for deployment overrides)
	envFile := filepath.Join(dataDir, ".env")
	if _, err := os.Stat(envFile); err == nil {
		if err := godotenv.Load(envFile); err != nil {
			log.Warn().Err(err).Str("file", envFile).Msg("Failed to load .env file")
		} else {
			log.Info().Str("file", envFile).Msg("Loaded .env file for deployment overrides")
		}
	}

	// Also try loading from current directory for development
	if err := godotenv.Load(); err == nil {
		log.Info().Msg("Loaded configuration from .env in current directory")
	}

	// Load mock.env and mock.env.local for mock mode configuration
	mockEnv := "mock.env"
	mockEnvLocal := "mock.env.local"
	if _, err := os.Stat(mockEnv); err == nil {
		if err := godotenv.Load(mockEnv); err != nil {
			log.Warn().Err(err).Str("file", mockEnv).Msg("Failed to load mock.env file")
		} else {
			log.Info().Str("file", mockEnv).Msg("Loaded mock mode configuration")
		}
	}
	if _, err := os.Stat(mockEnvLocal); err == nil {
		if err := godotenv.Load(mockEnvLocal); err != nil {
			log.Warn().Err(err).Str("file", mockEnvLocal).Msg("Failed to load mock.env.local file")
		} else {
			log.Info().Str("file", mockEnvLocal).Msg("Loaded local mock mode overrides")
		}
	}

	// Initialize config with defaults
	cfg := &Config{
		BackendHost:           "0.0.0.0",
		BackendPort:           3000,
		FrontendHost:          "0.0.0.0",
		FrontendPort:          7655,
		ConfigPath:            dataDir,
		DataPath:              dataDir,
		ConcurrentPolling:     true,
		ConnectionTimeout:     60 * time.Second,
		MetricsRetentionDays:  7,
		BackupPollingCycles:   10,
		BackupPollingInterval: 0,
		EnableBackupPolling:   true,
		WebhookBatchDelay:     10 * time.Second,
		AdaptivePollingEnabled:      false,
		AdaptivePollingBaseInterval: 10 * time.Second,
		AdaptivePollingMinInterval:  5 * time.Second,
		AdaptivePollingMaxInterval:  5 * time.Minute,
		LogLevel:              "info",
		LogFormat:             "auto",
		LogMaxSize:            100,
		LogMaxAge:             30,
		LogCompress:           true,
		AllowedOrigins:        "", // Empty means no CORS headers (same-origin only)
		IframeEmbeddingAllow:  "SAMEORIGIN",
		PBSPollingInterval:    60 * time.Second, // Default PBS polling (slower)
		PMGPollingInterval:    60 * time.Second, // Default PMG polling (aggregated stats)
		DiscoveryEnabled:      false,
		DiscoverySubnet:       "auto",
		EnvOverrides:          make(map[string]bool),
	OIDC:                  NewOIDCConfig(),
	}

	cfg.Discovery = DefaultDiscoveryConfig()

	// Initialize persistence
	persistence := NewConfigPersistence(dataDir)
	if persistence != nil {
		// Store global persistence for saving
		globalPersistence = persistence
		// Load nodes configuration
		if nodesConfig, err := persistence.LoadNodesConfig(); err == nil && nodesConfig != nil {
			cfg.PVEInstances = nodesConfig.PVEInstances
			cfg.PBSInstances = nodesConfig.PBSInstances
			cfg.PMGInstances = nodesConfig.PMGInstances
			log.Info().
				Int("pve", len(cfg.PVEInstances)).
				Int("pbs", len(cfg.PBSInstances)).
				Int("pmg", len(cfg.PMGInstances)).
				Msg("Loaded nodes configuration")
		} else if err != nil {
			log.Warn().Err(err).Msg("Failed to load nodes configuration")
		}

		// Load system configuration
		if systemSettings, err := persistence.LoadSystemSettings(); err == nil && systemSettings != nil {
			// Load PBS polling interval if configured
			if systemSettings.PBSPollingInterval > 0 {
				cfg.PBSPollingInterval = time.Duration(systemSettings.PBSPollingInterval) * time.Second
			}
			if systemSettings.PMGPollingInterval > 0 {
				cfg.PMGPollingInterval = time.Duration(systemSettings.PMGPollingInterval) * time.Second
			}

		if systemSettings.BackupPollingInterval > 0 {
			cfg.BackupPollingInterval = time.Duration(systemSettings.BackupPollingInterval) * time.Second
		} else if systemSettings.BackupPollingInterval == 0 {
			cfg.BackupPollingInterval = 0
		}
		if systemSettings.BackupPollingEnabled != nil {
			cfg.EnableBackupPolling = *systemSettings.BackupPollingEnabled
		}
		if systemSettings.AdaptivePollingEnabled != nil {
			cfg.AdaptivePollingEnabled = *systemSettings.AdaptivePollingEnabled
		}
		if systemSettings.AdaptivePollingBaseInterval > 0 {
			cfg.AdaptivePollingBaseInterval = time.Duration(systemSettings.AdaptivePollingBaseInterval) * time.Second
		}
		if systemSettings.AdaptivePollingMinInterval > 0 {
			cfg.AdaptivePollingMinInterval = time.Duration(systemSettings.AdaptivePollingMinInterval) * time.Second
		}
		if systemSettings.AdaptivePollingMaxInterval > 0 {
			cfg.AdaptivePollingMaxInterval = time.Duration(systemSettings.AdaptivePollingMaxInterval) * time.Second
		}

			if systemSettings.UpdateChannel != "" {
				cfg.UpdateChannel = systemSettings.UpdateChannel
			}
			cfg.AutoUpdateEnabled = systemSettings.AutoUpdateEnabled
			if systemSettings.AutoUpdateCheckInterval > 0 {
				cfg.AutoUpdateCheckInterval = time.Duration(systemSettings.AutoUpdateCheckInterval) * time.Hour
			}
			if systemSettings.AutoUpdateTime != "" {
				cfg.AutoUpdateTime = systemSettings.AutoUpdateTime
			}
			if systemSettings.AllowedOrigins != "" {
				cfg.AllowedOrigins = systemSettings.AllowedOrigins
			}
			if systemSettings.ConnectionTimeout > 0 {
				cfg.ConnectionTimeout = time.Duration(systemSettings.ConnectionTimeout) * time.Second
			}
			if systemSettings.LogLevel != "" {
				cfg.LogLevel = systemSettings.LogLevel
			}
			// Always load DiscoveryEnabled even if false
			cfg.DiscoveryEnabled = systemSettings.DiscoveryEnabled
			if systemSettings.DiscoverySubnet != "" {
				cfg.DiscoverySubnet = systemSettings.DiscoverySubnet
			}
			cfg.Discovery = NormalizeDiscoveryConfig(CloneDiscoveryConfig(systemSettings.DiscoveryConfig))
			// APIToken no longer loaded from system.json - only from .env
			log.Info().
				Str("updateChannel", cfg.UpdateChannel).
				Str("logLevel", cfg.LogLevel).
				Msg("Loaded system configuration")
		} else {
			// No system.json exists - create default one
			log.Info().Msg("No system.json found, creating default")
		defaultSettings := DefaultSystemSettings()
		defaultSettings.ConnectionTimeout = int(cfg.ConnectionTimeout.Seconds())
		if err := persistence.SaveSystemSettings(*defaultSettings); err != nil {
			log.Warn().Err(err).Msg("Failed to create default system.json")
		}
		}

		if oidcSettings, err := persistence.LoadOIDCConfig(); err == nil && oidcSettings != nil {
			cfg.OIDC = oidcSettings
		} else if err != nil {
			log.Warn().Err(err).Msg("Failed to load OIDC configuration")
		}
	}

	// Load API tokens
	if tokens, err := persistence.LoadAPITokens(); err == nil {
		cfg.APITokens = tokens
		cfg.SortAPITokens()
		log.Info().Int("count", len(tokens)).Msg("Loaded API tokens from persistence")
	} else if err != nil {
		log.Warn().Err(err).Msg("Failed to load API tokens from persistence")
	}

	// Ensure PBS polling interval has default if not set
	// Note: PVE polling is hardcoded to 10s in monitor.go
	if cfg.PBSPollingInterval == 0 {
		cfg.PBSPollingInterval = 60 * time.Second
	}
	if cfg.PMGPollingInterval == 0 {
		cfg.PMGPollingInterval = 60 * time.Second
	}

	// Limited environment variable support
	// NOTE: Node configuration is NOT done via env vars - use the web UI instead

	if cyclesStr := strings.TrimSpace(os.Getenv("BACKUP_POLLING_CYCLES")); cyclesStr != "" {
		if cycles, err := strconv.Atoi(cyclesStr); err == nil {
			if cycles < 0 {
				log.Warn().Str("value", cyclesStr).Msg("Ignoring negative BACKUP_POLLING_CYCLES from environment")
			} else {
				cfg.BackupPollingCycles = cycles
				cfg.EnvOverrides["BACKUP_POLLING_CYCLES"] = true
				log.Info().Int("cycles", cycles).Msg("Overriding backup polling cycles from environment")
			}
		} else {
			log.Warn().Str("value", cyclesStr).Msg("Invalid BACKUP_POLLING_CYCLES value, ignoring")
		}
	}

	if intervalStr := strings.TrimSpace(os.Getenv("BACKUP_POLLING_INTERVAL")); intervalStr != "" {
		if dur, err := time.ParseDuration(intervalStr); err == nil {
			if dur < 0 {
				log.Warn().Str("value", intervalStr).Msg("Ignoring negative BACKUP_POLLING_INTERVAL from environment")
			} else {
				cfg.BackupPollingInterval = dur
				cfg.EnvOverrides["BACKUP_POLLING_INTERVAL"] = true
				log.Info().Dur("interval", dur).Msg("Overriding backup polling interval from environment")
			}
		} else if seconds, err := strconv.Atoi(intervalStr); err == nil {
			if seconds < 0 {
				log.Warn().Str("value", intervalStr).Msg("Ignoring negative BACKUP_POLLING_INTERVAL (seconds) from environment")
			} else {
				cfg.BackupPollingInterval = time.Duration(seconds) * time.Second
				cfg.EnvOverrides["BACKUP_POLLING_INTERVAL"] = true
				log.Info().Int("seconds", seconds).Msg("Overriding backup polling interval (seconds) from environment")
			}
		} else {
			log.Warn().Str("value", intervalStr).Msg("Invalid BACKUP_POLLING_INTERVAL value, expected duration or seconds")
		}
	}

	if enabledStr := strings.TrimSpace(os.Getenv("ENABLE_BACKUP_POLLING")); enabledStr != "" {
		switch strings.ToLower(enabledStr) {
		case "0", "false", "no", "off":
			cfg.EnableBackupPolling = false
		default:
			cfg.EnableBackupPolling = true
		}
		cfg.EnvOverrides["ENABLE_BACKUP_POLLING"] = true
		log.Info().Bool("enabled", cfg.EnableBackupPolling).Msg("Overriding backup polling enabled flag from environment")
	}

	if adaptiveEnabled := strings.TrimSpace(os.Getenv("ADAPTIVE_POLLING_ENABLED")); adaptiveEnabled != "" {
		switch strings.ToLower(adaptiveEnabled) {
		case "0", "false", "no", "off":
			cfg.AdaptivePollingEnabled = false
		default:
			cfg.AdaptivePollingEnabled = true
		}
		cfg.EnvOverrides["ADAPTIVE_POLLING_ENABLED"] = true
		log.Info().Bool("enabled", cfg.AdaptivePollingEnabled).Msg("Adaptive polling feature flag overridden by environment")
	}

	if baseInterval := strings.TrimSpace(os.Getenv("ADAPTIVE_POLLING_BASE_INTERVAL")); baseInterval != "" {
		if dur, err := time.ParseDuration(baseInterval); err == nil {
			cfg.AdaptivePollingBaseInterval = dur
			cfg.EnvOverrides["ADAPTIVE_POLLING_BASE_INTERVAL"] = true
			log.Info().Dur("interval", dur).Msg("Adaptive polling base interval overridden by environment")
		} else {
			log.Warn().Str("value", baseInterval).Msg("Invalid ADAPTIVE_POLLING_BASE_INTERVAL value, expected duration string")
		}
	}

	if minInterval := strings.TrimSpace(os.Getenv("ADAPTIVE_POLLING_MIN_INTERVAL")); minInterval != "" {
		if dur, err := time.ParseDuration(minInterval); err == nil {
			cfg.AdaptivePollingMinInterval = dur
			cfg.EnvOverrides["ADAPTIVE_POLLING_MIN_INTERVAL"] = true
			log.Info().Dur("interval", dur).Msg("Adaptive polling min interval overridden by environment")
		} else {
			log.Warn().Str("value", minInterval).Msg("Invalid ADAPTIVE_POLLING_MIN_INTERVAL value, expected duration string")
		}
	}

	if maxInterval := strings.TrimSpace(os.Getenv("ADAPTIVE_POLLING_MAX_INTERVAL")); maxInterval != "" {
		if dur, err := time.ParseDuration(maxInterval); err == nil {
			cfg.AdaptivePollingMaxInterval = dur
			cfg.EnvOverrides["ADAPTIVE_POLLING_MAX_INTERVAL"] = true
			log.Info().Dur("interval", dur).Msg("Adaptive polling max interval overridden by environment")
		} else {
			log.Warn().Str("value", maxInterval).Msg("Invalid ADAPTIVE_POLLING_MAX_INTERVAL value, expected duration string")
		}
	}

	// Support both FRONTEND_PORT (preferred) and PORT (legacy) env vars
	if frontendPort := os.Getenv("FRONTEND_PORT"); frontendPort != "" {
		if p, err := strconv.Atoi(frontendPort); err == nil {
			cfg.FrontendPort = p
			log.Info().Int("port", p).Msg("Overriding frontend port from FRONTEND_PORT env var")
		}
	} else if port := os.Getenv("PORT"); port != "" {
		// Fall back to PORT for backwards compatibility
		if p, err := strconv.Atoi(port); err == nil {
			cfg.FrontendPort = p
			log.Info().Int("port", p).Msg("Overriding frontend port from PORT env var (legacy)")
		}
	}
	envTokens := make([]string, 0, 4)
	if list := strings.TrimSpace(os.Getenv("API_TOKENS")); list != "" {
		for _, part := range strings.Split(list, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				envTokens = append(envTokens, part)
			}
		}
	}
	if token := strings.TrimSpace(os.Getenv("API_TOKEN")); token != "" {
		envTokens = append(envTokens, token)
	}

	if len(envTokens) > 0 {
		cfg.EnvOverrides["API_TOKEN"] = true
		cfg.EnvOverrides["API_TOKENS"] = true
		for _, tokenValue := range envTokens {
			if tokenValue == "" {
				continue
			}

			hashed := tokenValue
			prefix := tokenPrefix(tokenValue)
			suffix := tokenSuffix(tokenValue)

			if !auth.IsAPITokenHashed(tokenValue) {
				hashed = auth.HashAPIToken(tokenValue)
				prefix = tokenPrefix(tokenValue)
				suffix = tokenSuffix(tokenValue)
				log.Info().Msg("Auto-hashed plain text API token from environment variable")
			} else {
				log.Debug().Msg("Loaded pre-hashed API token from env var")
			}

			if cfg.HasAPITokenHash(hashed) {
				continue
			}

			record := APITokenRecord{
				ID:        uuid.NewString(),
				Name:      "Environment token",
				Hash:      hashed,
				Prefix:    prefix,
				Suffix:    suffix,
				CreatedAt: time.Now().UTC(),
			}
			cfg.APITokens = append(cfg.APITokens, record)
		}
		cfg.SortAPITokens()
	}

	// Check if API token is enabled
	if apiTokenEnabled := os.Getenv("API_TOKEN_ENABLED"); apiTokenEnabled != "" {
		cfg.APITokenEnabled = apiTokenEnabled == "true" || apiTokenEnabled == "1"
		log.Debug().Bool("enabled", cfg.APITokenEnabled).Msg("API token enabled status from env var")
	} else if cfg.HasAPITokens() {
		cfg.APITokenEnabled = true
		log.Debug().Msg("API tokens exist without explicit enabled flag, assuming enabled for backwards compatibility")
	}

	// Legacy migration: if a single token is present without metadata, wrap it.
	if !cfg.HasAPITokens() && cfg.APIToken != "" {
		if record, err := NewHashedAPITokenRecord(cfg.APIToken, "Legacy token", time.Now().UTC()); err == nil {
			cfg.APITokens = []APITokenRecord{*record}
			cfg.SortAPITokens()
			log.Info().Msg("Migrated legacy API token into token record store")
		}
	}
	// Check if auth is disabled
	disableAuthEnv := os.Getenv("DISABLE_AUTH")
	log.Debug().Str("DISABLE_AUTH_ENV", disableAuthEnv).Msg("Checking DISABLE_AUTH environment variable")
	if disableAuthEnv != "" {
		cfg.DisableAuth = disableAuthEnv == "true" || disableAuthEnv == "1"
		log.Debug().Bool("DisableAuth", cfg.DisableAuth).Msg("DisableAuth set from environment")
		if cfg.DisableAuth {
			log.Warn().Msg("⚠️  AUTHENTICATION DISABLED - Pulse is running without authentication!")
		}
	} else {
		log.Debug().Bool("DisableAuth", cfg.DisableAuth).Msg("DISABLE_AUTH not set, DisableAuth remains")
	}

	// Check if demo mode is enabled
	demoModeEnv := os.Getenv("DEMO_MODE")
	if demoModeEnv != "" {
		cfg.DemoMode = demoModeEnv == "true" || demoModeEnv == "1"
		if cfg.DemoMode {
			log.Warn().Msg("🎭 DEMO MODE - All modifications disabled (read-only)")
		}
	}

	// Load proxy authentication settings
	if proxyAuthSecret := os.Getenv("PROXY_AUTH_SECRET"); proxyAuthSecret != "" {
		cfg.ProxyAuthSecret = proxyAuthSecret
		log.Info().Msg("Proxy authentication secret configured")

		// Load other proxy auth settings
		if userHeader := os.Getenv("PROXY_AUTH_USER_HEADER"); userHeader != "" {
			cfg.ProxyAuthUserHeader = userHeader
			log.Info().Str("header", userHeader).Msg("Proxy auth user header configured")
		}
		if roleHeader := os.Getenv("PROXY_AUTH_ROLE_HEADER"); roleHeader != "" {
			cfg.ProxyAuthRoleHeader = roleHeader
			log.Info().Str("header", roleHeader).Msg("Proxy auth role header configured")
		}
		if roleSeparator := os.Getenv("PROXY_AUTH_ROLE_SEPARATOR"); roleSeparator != "" {
			cfg.ProxyAuthRoleSeparator = roleSeparator
			log.Info().Str("separator", roleSeparator).Msg("Proxy auth role separator configured")
		}
		if adminRole := os.Getenv("PROXY_AUTH_ADMIN_ROLE"); adminRole != "" {
			cfg.ProxyAuthAdminRole = adminRole
			log.Info().Str("role", adminRole).Msg("Proxy auth admin role configured")
		}
		if logoutURL := os.Getenv("PROXY_AUTH_LOGOUT_URL"); logoutURL != "" {
			cfg.ProxyAuthLogoutURL = logoutURL
			log.Info().Str("url", logoutURL).Msg("Proxy auth logout URL configured")
		}
	}

	oidcEnv := make(map[string]string)
	if val := os.Getenv("OIDC_ENABLED"); val != "" {
		oidcEnv["OIDC_ENABLED"] = val
	}
	if val := os.Getenv("OIDC_ISSUER_URL"); val != "" {
		oidcEnv["OIDC_ISSUER_URL"] = val
	}
	if val := os.Getenv("OIDC_CLIENT_ID"); val != "" {
		oidcEnv["OIDC_CLIENT_ID"] = val
	}
	if val := os.Getenv("OIDC_CLIENT_SECRET"); val != "" {
		oidcEnv["OIDC_CLIENT_SECRET"] = val
	}
	if val := os.Getenv("OIDC_REDIRECT_URL"); val != "" {
		oidcEnv["OIDC_REDIRECT_URL"] = val
	}
	if val := os.Getenv("OIDC_LOGOUT_URL"); val != "" {
		oidcEnv["OIDC_LOGOUT_URL"] = val
	}
	if val := os.Getenv("OIDC_SCOPES"); val != "" {
		oidcEnv["OIDC_SCOPES"] = val
	}
	if val := os.Getenv("OIDC_USERNAME_CLAIM"); val != "" {
		oidcEnv["OIDC_USERNAME_CLAIM"] = val
	}
	if val := os.Getenv("OIDC_EMAIL_CLAIM"); val != "" {
		oidcEnv["OIDC_EMAIL_CLAIM"] = val
	}
	if val := os.Getenv("OIDC_GROUPS_CLAIM"); val != "" {
		oidcEnv["OIDC_GROUPS_CLAIM"] = val
	}
	if val := os.Getenv("OIDC_ALLOWED_GROUPS"); val != "" {
		oidcEnv["OIDC_ALLOWED_GROUPS"] = val
	}
	if val := os.Getenv("OIDC_ALLOWED_DOMAINS"); val != "" {
		oidcEnv["OIDC_ALLOWED_DOMAINS"] = val
	}
	if val := os.Getenv("OIDC_ALLOWED_EMAILS"); val != "" {
		oidcEnv["OIDC_ALLOWED_EMAILS"] = val
	}
	if len(oidcEnv) > 0 {
		cfg.OIDC.MergeFromEnv(oidcEnv)
	}
	if authUser := os.Getenv("PULSE_AUTH_USER"); authUser != "" {
		cfg.AuthUser = authUser
		log.Info().Msg("Overriding auth user from env var")
	}
	if authPass := os.Getenv("PULSE_AUTH_PASS"); authPass != "" {
		// Auto-hash plain text passwords for security
		if !IsPasswordHashed(authPass) {
			// Plain text password - hash it immediately
			hashedPass, err := auth.HashPassword(authPass)
			if err != nil {
				log.Error().Err(err).Msg("Failed to hash password from environment variable")
				// Fall back to plain text if hashing fails (shouldn't happen)
				cfg.AuthPass = authPass
			} else {
				cfg.AuthPass = hashedPass
				log.Info().Msg("Auto-hashed plain text password from environment variable")
			}
		} else {
			// Already hashed - validate it's complete
			if len(authPass) != 60 {
				log.Error().Int("length", len(authPass)).Msg("Bcrypt hash appears truncated! Expected 60 characters. Authentication may fail.")
				log.Error().Msg("Ensure the full hash is enclosed in single quotes in your .env file or Docker environment")
			}
			cfg.AuthPass = authPass
			log.Debug().Msg("Loaded pre-hashed password from env var")
		}
	}

	// HTTPS/TLS configuration from environment
	if httpsEnabled := os.Getenv("HTTPS_ENABLED"); httpsEnabled != "" {
		cfg.HTTPSEnabled = httpsEnabled == "true" || httpsEnabled == "1"
		log.Debug().Bool("enabled", cfg.HTTPSEnabled).Msg("HTTPS enabled status from env var")
	}
	if tlsCertFile := os.Getenv("TLS_CERT_FILE"); tlsCertFile != "" {
		cfg.TLSCertFile = tlsCertFile
		log.Debug().Str("cert_file", tlsCertFile).Msg("TLS cert file from env var")
	}
	if tlsKeyFile := os.Getenv("TLS_KEY_FILE"); tlsKeyFile != "" {
		cfg.TLSKeyFile = tlsKeyFile
		log.Debug().Str("key_file", tlsKeyFile).Msg("TLS key file from env var")
	}

	// REMOVED: Update channel, auto-update, connection timeout, and allowed origins env vars
	// These settings now ONLY come from system.json to prevent confusion
	// Only keeping essential deployment/infrastructure env vars

	// Normalize PVE user fields for password authentication
	for i := range cfg.PVEInstances {
		if cfg.PVEInstances[i].TokenName == "" && cfg.PVEInstances[i].TokenValue == "" && cfg.PVEInstances[i].User != "" && !strings.Contains(cfg.PVEInstances[i].User, "@") {
			cfg.PVEInstances[i].User = cfg.PVEInstances[i].User + "@pam"
		}
	}

	if cfg.AllowedOrigins == "" {
		// If not configured and we're in development mode (different ports for frontend/backend)
		// allow localhost for development convenience
		if os.Getenv("NODE_ENV") == "development" || os.Getenv("PULSE_DEV") == "true" {
			cfg.AllowedOrigins = "http://localhost:5173,http://localhost:7655"
			log.Info().Msg("Development mode: allowing localhost origins")
		}
	}
	// Support env vars for important settings (override system.json)
	// NOTE: Environment variables always take precedence over UI/system.json settings
	if discoveryEnabled := os.Getenv("DISCOVERY_ENABLED"); discoveryEnabled != "" {
		cfg.DiscoveryEnabled = discoveryEnabled == "true" || discoveryEnabled == "1"
		cfg.EnvOverrides["discoveryEnabled"] = true
		log.Info().Bool("enabled", cfg.DiscoveryEnabled).Msg("Discovery enabled overridden by DISCOVERY_ENABLED env var")
	}
	if discoverySubnet := os.Getenv("DISCOVERY_SUBNET"); discoverySubnet != "" {
		cfg.DiscoverySubnet = discoverySubnet
		cfg.EnvOverrides["discoverySubnet"] = true
		log.Info().Str("subnet", discoverySubnet).Msg("Discovery subnet overridden by DISCOVERY_SUBNET env var")
	}
	if envOverride := strings.TrimSpace(os.Getenv("DISCOVERY_ENVIRONMENT_OVERRIDE")); envOverride != "" {
		if IsValidDiscoveryEnvironment(envOverride) {
			cfg.Discovery.EnvironmentOverride = strings.ToLower(envOverride)
			cfg.EnvOverrides["discoveryEnvironmentOverride"] = true
			log.Info().Str("environment", cfg.Discovery.EnvironmentOverride).Msg("Discovery environment override set by DISCOVERY_ENVIRONMENT_OVERRIDE")
		} else {
			log.Warn().Str("value", envOverride).Msg("Ignoring invalid DISCOVERY_ENVIRONMENT_OVERRIDE value")
		}
	}
	if allowlistEnv := strings.TrimSpace(os.Getenv("DISCOVERY_SUBNET_ALLOWLIST")); allowlistEnv != "" {
		parts := splitAndTrim(allowlistEnv)
		cfg.Discovery.SubnetAllowlist = sanitizeCIDRList(parts)
		cfg.EnvOverrides["discoverySubnetAllowlist"] = true
		log.Info().Int("allowlistCount", len(cfg.Discovery.SubnetAllowlist)).Msg("Discovery subnet allowlist overridden by DISCOVERY_SUBNET_ALLOWLIST")
	}
	if blocklistEnv := strings.TrimSpace(os.Getenv("DISCOVERY_SUBNET_BLOCKLIST")); blocklistEnv != "" {
		parts := splitAndTrim(blocklistEnv)
		cfg.Discovery.SubnetBlocklist = sanitizeCIDRList(parts)
		cfg.EnvOverrides["discoverySubnetBlocklist"] = true
		log.Info().Int("blocklistCount", len(cfg.Discovery.SubnetBlocklist)).Msg("Discovery subnet blocklist overridden by DISCOVERY_SUBNET_BLOCKLIST")
	}
	if maxHostsEnv := strings.TrimSpace(os.Getenv("DISCOVERY_MAX_HOSTS_PER_SCAN")); maxHostsEnv != "" {
		if v, err := strconv.Atoi(maxHostsEnv); err == nil && v > 0 {
			cfg.Discovery.MaxHostsPerScan = v
			cfg.EnvOverrides["discoveryMaxHostsPerScan"] = true
			log.Info().Int("maxHostsPerScan", v).Msg("Discovery max hosts per scan overridden by DISCOVERY_MAX_HOSTS_PER_SCAN")
		} else {
			log.Warn().Str("value", maxHostsEnv).Msg("Ignoring invalid DISCOVERY_MAX_HOSTS_PER_SCAN value")
		}
	}
	if maxConcurrentEnv := strings.TrimSpace(os.Getenv("DISCOVERY_MAX_CONCURRENT")); maxConcurrentEnv != "" {
		if v, err := strconv.Atoi(maxConcurrentEnv); err == nil && v > 0 {
			cfg.Discovery.MaxConcurrent = v
			cfg.EnvOverrides["discoveryMaxConcurrent"] = true
			log.Info().Int("maxConcurrent", v).Msg("Discovery concurrency overridden by DISCOVERY_MAX_CONCURRENT")
		} else {
			log.Warn().Str("value", maxConcurrentEnv).Msg("Ignoring invalid DISCOVERY_MAX_CONCURRENT value")
		}
	}
	if reverseDNSEnv := strings.TrimSpace(os.Getenv("DISCOVERY_ENABLE_REVERSE_DNS")); reverseDNSEnv != "" {
		switch strings.ToLower(reverseDNSEnv) {
		case "0", "false", "no", "off":
			cfg.Discovery.EnableReverseDNS = false
		default:
			cfg.Discovery.EnableReverseDNS = true
		}
		cfg.EnvOverrides["discoveryEnableReverseDns"] = true
		log.Info().Bool("enableReverseDNS", cfg.Discovery.EnableReverseDNS).Msg("Discovery reverse DNS setting overridden by DISCOVERY_ENABLE_REVERSE_DNS")
	}
	if scanGatewaysEnv := strings.TrimSpace(os.Getenv("DISCOVERY_SCAN_GATEWAYS")); scanGatewaysEnv != "" {
		switch strings.ToLower(scanGatewaysEnv) {
		case "0", "false", "no", "off":
			cfg.Discovery.ScanGateways = false
		default:
			cfg.Discovery.ScanGateways = true
		}
		cfg.EnvOverrides["discoveryScanGateways"] = true
		log.Info().Bool("scanGateways", cfg.Discovery.ScanGateways).Msg("Discovery gateway scanning overridden by DISCOVERY_SCAN_GATEWAYS")
	}
	if dialTimeoutEnv := strings.TrimSpace(os.Getenv("DISCOVERY_DIAL_TIMEOUT_MS")); dialTimeoutEnv != "" {
		if v, err := strconv.Atoi(dialTimeoutEnv); err == nil && v > 0 {
			cfg.Discovery.DialTimeout = v
			cfg.EnvOverrides["discoveryDialTimeoutMs"] = true
			log.Info().Int("dialTimeoutMs", v).Msg("Discovery dial timeout overridden by DISCOVERY_DIAL_TIMEOUT_MS")
		} else {
			log.Warn().Str("value", dialTimeoutEnv).Msg("Ignoring invalid DISCOVERY_DIAL_TIMEOUT_MS value")
		}
	}
	if httpTimeoutEnv := strings.TrimSpace(os.Getenv("DISCOVERY_HTTP_TIMEOUT_MS")); httpTimeoutEnv != "" {
		if v, err := strconv.Atoi(httpTimeoutEnv); err == nil && v > 0 {
			cfg.Discovery.HTTPTimeout = v
			cfg.EnvOverrides["discoveryHttpTimeoutMs"] = true
			log.Info().Int("httpTimeoutMs", v).Msg("Discovery HTTP timeout overridden by DISCOVERY_HTTP_TIMEOUT_MS")
		} else {
			log.Warn().Str("value", httpTimeoutEnv).Msg("Ignoring invalid DISCOVERY_HTTP_TIMEOUT_MS value")
		}
	}
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		cfg.LogLevel = logLevel
		cfg.EnvOverrides["logLevel"] = true
		log.Info().Str("level", logLevel).Msg("Log level overridden by LOG_LEVEL env var")
	}
	if logFormat := os.Getenv("LOG_FORMAT"); logFormat != "" {
		cfg.LogFormat = logFormat
		cfg.EnvOverrides["logFormat"] = true
		log.Info().Str("format", logFormat).Msg("Log format overridden by LOG_FORMAT env var")
	}

	cfg.Discovery = NormalizeDiscoveryConfig(cfg.Discovery)
	if connectionTimeout := os.Getenv("CONNECTION_TIMEOUT"); connectionTimeout != "" {
		if d, err := time.ParseDuration(connectionTimeout + "s"); err == nil {
			cfg.ConnectionTimeout = d
			cfg.EnvOverrides["connectionTimeout"] = true
			log.Info().Dur("timeout", d).Msg("Connection timeout overridden by CONNECTION_TIMEOUT env var")
		} else if d, err := time.ParseDuration(connectionTimeout); err == nil {
			cfg.ConnectionTimeout = d
			cfg.EnvOverrides["connectionTimeout"] = true
			log.Info().Dur("timeout", d).Msg("Connection timeout overridden by CONNECTION_TIMEOUT env var")
		}
	}
	if allowedOrigins := os.Getenv("ALLOWED_ORIGINS"); allowedOrigins != "" {
		cfg.AllowedOrigins = allowedOrigins
		cfg.EnvOverrides["allowedOrigins"] = true
		log.Info().Str("origins", allowedOrigins).Msg("Allowed origins overridden by ALLOWED_ORIGINS env var")
	}
	if publicURL := os.Getenv("PULSE_PUBLIC_URL"); publicURL != "" {
		cfg.PublicURL = publicURL
		cfg.EnvOverrides["publicURL"] = true
		log.Info().Str("url", publicURL).Msg("Public URL configured from PULSE_PUBLIC_URL env var")
	} else {
		// Try to auto-detect public URL if not explicitly configured
		if detectedURL := detectPublicURL(cfg.FrontendPort); detectedURL != "" {
			cfg.PublicURL = detectedURL
			log.Info().Str("url", detectedURL).Msg("Auto-detected public URL for webhook notifications")
		}
	}

	cfg.OIDC.ApplyDefaults(cfg.PublicURL)

	// Initialize logging with configuration values
	logging.Init(logging.Config{
		Format:    cfg.LogFormat,
		Level:     cfg.LogLevel,
		Component: "pulse-config",
	})

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, errors.Wrap(err, "load config: invalid configuration")
	}

	return cfg, nil
}

// SaveConfig saves the configuration back to encrypted files
func SaveConfig(cfg *Config) error {
	if globalPersistence == nil {
		return fmt.Errorf("config persistence not initialized")
	}

	// Save nodes configuration
	if err := globalPersistence.SaveNodesConfig(cfg.PVEInstances, cfg.PBSInstances, cfg.PMGInstances); err != nil {
		return fmt.Errorf("failed to save nodes config: %w", err)
	}

	// Save system configuration
	adaptiveEnabled := cfg.AdaptivePollingEnabled
	systemSettings := SystemSettings{
		// Note: PVE polling is hardcoded to 10s
		UpdateChannel:                 cfg.UpdateChannel,
		AutoUpdateEnabled:             cfg.AutoUpdateEnabled,
		AutoUpdateCheckInterval:       int(cfg.AutoUpdateCheckInterval.Hours()),
		AutoUpdateTime:                cfg.AutoUpdateTime,
		AllowedOrigins:                cfg.AllowedOrigins,
		ConnectionTimeout:             int(cfg.ConnectionTimeout.Seconds()),
		LogLevel:                      cfg.LogLevel,
		DiscoveryEnabled:              cfg.DiscoveryEnabled,
		DiscoverySubnet:               cfg.DiscoverySubnet,
		DiscoveryConfig:               CloneDiscoveryConfig(cfg.Discovery),
		AdaptivePollingEnabled:        &adaptiveEnabled,
		AdaptivePollingBaseInterval:   int(cfg.AdaptivePollingBaseInterval / time.Second),
		AdaptivePollingMinInterval:    int(cfg.AdaptivePollingMinInterval / time.Second),
		AdaptivePollingMaxInterval:    int(cfg.AdaptivePollingMaxInterval / time.Second),
		// APIToken removed - now handled via .env only
	}
	if err := globalPersistence.SaveSystemSettings(systemSettings); err != nil {
		return fmt.Errorf("failed to save system config: %w", err)
	}

	return nil
}

// SaveOIDCConfig persists OIDC settings using the shared config persistence layer.
func SaveOIDCConfig(settings *OIDCConfig) error {
	if globalPersistence == nil {
		return fmt.Errorf("config persistence not initialized")
	}
	if settings == nil {
		return fmt.Errorf("oidc settings cannot be nil")
	}

	clone := settings.Clone()
	if clone == nil {
		return fmt.Errorf("failed to clone oidc settings")
	}

	return globalPersistence.SaveOIDCConfig(*clone)
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	// Validate server settings
	if c.BackendPort <= 0 || c.BackendPort > 65535 {
		return fmt.Errorf("invalid backend port: %d", c.BackendPort)
	}
	if c.FrontendPort <= 0 || c.FrontendPort > 65535 {
		return fmt.Errorf("invalid frontend port: %d", c.FrontendPort)
	}

	// Validate monitoring settings
	// Note: PVE polling is hardcoded to 10s
	if c.ConnectionTimeout < time.Second {
		return fmt.Errorf("connection timeout must be at least 1 second")
	}
	if c.AdaptivePollingMinInterval <= 0 {
		return fmt.Errorf("adaptive polling min interval must be greater than 0")
	}
	if c.AdaptivePollingBaseInterval <= 0 {
		return fmt.Errorf("adaptive polling base interval must be greater than 0")
	}
	if c.AdaptivePollingMaxInterval <= 0 {
		return fmt.Errorf("adaptive polling max interval must be greater than 0")
	}
	if c.AdaptivePollingMinInterval > c.AdaptivePollingMaxInterval {
		return fmt.Errorf("adaptive polling min interval cannot exceed max interval")
	}
	if c.AdaptivePollingBaseInterval < c.AdaptivePollingMinInterval || c.AdaptivePollingBaseInterval > c.AdaptivePollingMaxInterval {
		return fmt.Errorf("adaptive polling base interval must be between min and max intervals")
	}

	// Validate PVE instances
	for i, pve := range c.PVEInstances {
		if pve.Host == "" {
			return fmt.Errorf("PVE instance %d: host is required", i+1)
		}
		if !strings.HasPrefix(pve.Host, "http://") && !strings.HasPrefix(pve.Host, "https://") {
			return fmt.Errorf("PVE instance %d: host must start with http:// or https://", i+1)
		}
		// Must have either password or token
		if pve.Password == "" && (pve.TokenName == "" || pve.TokenValue == "") {
			return fmt.Errorf("PVE instance %d: either password or token authentication is required", i+1)
		}
	}

	// Validate and auto-fix PBS instances
	validPBS := []PBSInstance{}
	for i, pbs := range c.PBSInstances {
		if pbs.Host == "" {
			log.Warn().Int("instance", i+1).Msg("PBS instance missing host, skipping")
			continue
		}
		// Auto-fix missing protocol
		if !strings.HasPrefix(pbs.Host, "http://") && !strings.HasPrefix(pbs.Host, "https://") {
			pbs.Host = "https://" + pbs.Host
			log.Info().Str("host", pbs.Host).Msg("PBS host auto-corrected to include https://")
		}
		// Check authentication
		if pbs.Password == "" && (pbs.TokenName == "" || pbs.TokenValue == "") {
			log.Warn().Int("instance", i+1).Str("host", pbs.Host).Msg("PBS instance missing authentication, skipping")
			continue
		}
		validPBS = append(validPBS, pbs)
	}
	c.PBSInstances = validPBS

	if err := c.OIDC.Validate(); err != nil {
		return err
	}

	return nil
}

// detectPublicURL attempts to automatically detect the public URL for Pulse
func detectPublicURL(port int) string {
	// When running inside Docker we can't reliably determine an externally reachable address.
	// Returning an empty string avoids surfacing container-only IPs (e.g., 172.x) in notifications.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		log.Info().Msg("Docker environment detected - skipping public URL auto-detect. Set PULSE_PUBLIC_URL to expose external links.")
		return ""
	}

	// Method 1: Check if we're in a Proxmox container (most common deployment)
	if _, err := os.Stat("/etc/pve"); err == nil {
		// We're likely in a ProxmoxVE container
		// Try to get the container's IP from hostname -I
		if output, err := exec.Command("hostname", "-I").Output(); err == nil {
			ips := strings.Fields(string(output))
			for _, ip := range ips {
				// Skip localhost and IPv6
				if !strings.HasPrefix(ip, "127.") && !strings.Contains(ip, ":") {
					return fmt.Sprintf("http://%s:%d", ip, port)
				}
			}
		}
	}

	// Method 2: Try to get the primary network interface IP
	if ip := getOutboundIP(); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, port)
	}

	// Method 3: Get all non-loopback IPs and use the first private one
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					ip := ipnet.IP.String()
					// Prefer private IPs (RFC1918)
					if strings.HasPrefix(ip, "192.168.") ||
						strings.HasPrefix(ip, "10.") ||
						strings.HasPrefix(ip, "172.") {
						return fmt.Sprintf("http://%s:%d", ip, port)
					}
				}
			}
		}
		// If no private IP found, use the first public one
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return fmt.Sprintf("http://%s:%d", ipnet.IP.String(), port)
				}
			}
		}
	}

	return ""
}

// getOutboundIP gets the preferred outbound IP of this machine
func getOutboundIP() string {
	// Try to connect to a public DNS server (doesn't actually connect, just resolves the route)
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		// Try Cloudflare DNS as fallback
		conn, err = net.Dial("udp", "1.1.1.1:80")
		if err != nil {
			return ""
		}
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
