package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RouXx67/PulseUp/internal/alerts"
	"github.com/RouXx67/PulseUp/internal/crypto"
	"github.com/RouXx67/PulseUp/internal/mock"
	"github.com/RouXx67/PulseUp/internal/notifications"
	"github.com/rs/zerolog/log"
)

// ConfigPersistence handles saving and loading configuration
type ConfigPersistence struct {
	mu            sync.RWMutex
	tx            *importTransaction
	configDir     string
	alertFile     string
	emailFile     string
	webhookFile   string
	appriseFile   string
	nodesFile     string
	systemFile    string
	oidcFile      string
	apiTokensFile string
	crypto        *crypto.CryptoManager
}

// NewConfigPersistence creates a new config persistence manager
func NewConfigPersistence(configDir string) *ConfigPersistence {
	if configDir == "" {
		configDir = "/etc/pulse"
	}

	// Initialize crypto manager
	cryptoMgr, err := crypto.NewCryptoManager()
	if err != nil {
		log.Error().Err(err).Msg("Failed to initialize crypto manager, using unencrypted storage")
		cryptoMgr = nil
	}

	cp := &ConfigPersistence{
		configDir:     configDir,
		alertFile:     filepath.Join(configDir, "alerts.json"),
		emailFile:     filepath.Join(configDir, "email.enc"),
		webhookFile:   filepath.Join(configDir, "webhooks.enc"),
		appriseFile:   filepath.Join(configDir, "apprise.enc"),
		nodesFile:     filepath.Join(configDir, "nodes.enc"),
		systemFile:    filepath.Join(configDir, "system.json"),
		oidcFile:      filepath.Join(configDir, "oidc.enc"),
		apiTokensFile: filepath.Join(configDir, "api_tokens.json"),
		crypto:        cryptoMgr,
	}

	log.Debug().
		Str("configDir", configDir).
		Str("systemFile", cp.systemFile).
		Str("nodesFile", cp.nodesFile).
		Bool("encryptionEnabled", cryptoMgr != nil).
		Msg("Config persistence initialized")

	return cp
}

// EnsureConfigDir ensures the configuration directory exists
func (c *ConfigPersistence) EnsureConfigDir() error {
	return os.MkdirAll(c.configDir, 0700)
}

func (c *ConfigPersistence) beginTransaction(tx *importTransaction) {
	c.mu.Lock()
	c.tx = tx
	c.mu.Unlock()
}

func (c *ConfigPersistence) endTransaction(tx *importTransaction) {
	c.mu.Lock()
	if c.tx == tx {
		c.tx = nil
	}
	c.mu.Unlock()
}

func (c *ConfigPersistence) writeConfigFileLocked(path string, data []byte, perm os.FileMode) error {
	if c.tx != nil {
		return c.tx.StageFile(path, data, perm)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadAPITokens loads API token metadata from disk.
func (c *ConfigPersistence) LoadAPITokens() ([]APITokenRecord, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.apiTokensFile)
	if err != nil {
		if os.IsNotExist(err) {
			return []APITokenRecord{}, nil
		}
		return nil, err
	}

	if len(data) == 0 {
		return []APITokenRecord{}, nil
	}

	var tokens []APITokenRecord
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, err
	}

	return tokens, nil
}

// SaveAPITokens persists API token metadata to disk.
func (c *ConfigPersistence) SaveAPITokens(tokens []APITokenRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	// Backup previous state (best effort).
	if existing, err := os.ReadFile(c.apiTokensFile); err == nil && len(existing) > 0 {
		if err := os.WriteFile(c.apiTokensFile+".backup", existing, 0600); err != nil {
			log.Warn().Err(err).Msg("Failed to create API token backup file")
		}
	}

	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}

	return c.writeConfigFileLocked(c.apiTokensFile, data, 0600)
}

// SaveAlertConfig saves alert configuration to file
func (c *ConfigPersistence) SaveAlertConfig(config alerts.AlertConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Ensure critical defaults are set before saving
	if config.StorageDefault.Trigger <= 0 {
		config.StorageDefault.Trigger = 85
		config.StorageDefault.Clear = 80
	}
	if config.MinimumDelta <= 0 {
		margin := config.HysteresisMargin
		if margin <= 0 {
			margin = 5.0
		}
		for id, override := range config.Overrides {
			if override.Usage != nil {
				if override.Usage.Clear <= 0 {
					override.Usage.Clear = override.Usage.Trigger - margin
					if override.Usage.Clear < 0 {
						override.Usage.Clear = 0
					}
				}
				config.Overrides[id] = override
			}
		}
		config.MinimumDelta = 2.0
	}
	if config.SuppressionWindow <= 0 {
		config.SuppressionWindow = 5
	}
	if config.HysteresisMargin <= 0 {
		config.HysteresisMargin = 5.0
	}
	config.MetricTimeThresholds = alerts.NormalizeMetricTimeThresholds(config.MetricTimeThresholds)
	if config.TimeThreshold <= 0 {
		config.TimeThreshold = 5
	}
	if config.TimeThresholds == nil {
		config.TimeThresholds = make(map[string]int)
	}
	ensureDelay := func(key string) {
		if delay, ok := config.TimeThresholds[key]; !ok || delay <= 0 {
			config.TimeThresholds[key] = config.TimeThreshold
		}
	}
	ensureDelay("guest")
	ensureDelay("node")
	ensureDelay("storage")
	ensureDelay("pbs")
	if delay, ok := config.TimeThresholds["all"]; ok && delay <= 0 {
		config.TimeThresholds["all"] = config.TimeThreshold
	}
	if config.SnapshotDefaults.WarningDays < 0 {
		config.SnapshotDefaults.WarningDays = 0
	}
	if config.SnapshotDefaults.CriticalDays < 0 {
		config.SnapshotDefaults.CriticalDays = 0
	}
	if config.SnapshotDefaults.CriticalDays > 0 && config.SnapshotDefaults.WarningDays > config.SnapshotDefaults.CriticalDays {
		config.SnapshotDefaults.WarningDays = config.SnapshotDefaults.CriticalDays
	}
	if config.SnapshotDefaults.CriticalDays == 0 && config.SnapshotDefaults.WarningDays > 0 {
		config.SnapshotDefaults.CriticalDays = config.SnapshotDefaults.WarningDays
	}
	if config.SnapshotDefaults.WarningSizeGiB < 0 {
		config.SnapshotDefaults.WarningSizeGiB = 0
	}
	if config.SnapshotDefaults.CriticalSizeGiB < 0 {
		config.SnapshotDefaults.CriticalSizeGiB = 0
	}
	if config.SnapshotDefaults.CriticalSizeGiB > 0 && config.SnapshotDefaults.WarningSizeGiB > config.SnapshotDefaults.CriticalSizeGiB {
		config.SnapshotDefaults.WarningSizeGiB = config.SnapshotDefaults.CriticalSizeGiB
	}
	if config.SnapshotDefaults.CriticalSizeGiB == 0 && config.SnapshotDefaults.WarningSizeGiB > 0 {
		config.SnapshotDefaults.CriticalSizeGiB = config.SnapshotDefaults.WarningSizeGiB
	}
	if config.BackupDefaults.WarningDays < 0 {
		config.BackupDefaults.WarningDays = 0
	}
	if config.BackupDefaults.CriticalDays < 0 {
		config.BackupDefaults.CriticalDays = 0
	}
	if config.BackupDefaults.CriticalDays > 0 && config.BackupDefaults.WarningDays > config.BackupDefaults.CriticalDays {
		config.BackupDefaults.WarningDays = config.BackupDefaults.CriticalDays
	}
	config.DockerIgnoredContainerPrefixes = alerts.NormalizeDockerIgnoredPrefixes(config.DockerIgnoredContainerPrefixes)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	if err := c.writeConfigFileLocked(c.alertFile, data, 0600); err != nil {
		return err
	}

	log.Info().Str("file", c.alertFile).Msg("Alert configuration saved")
	return nil
}

// LoadAlertConfig loads alert configuration from file
func (c *ConfigPersistence) LoadAlertConfig() (*alerts.AlertConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.alertFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Return default config if file doesn't exist
			return &alerts.AlertConfig{
				Enabled: true,
				GuestDefaults: alerts.ThresholdConfig{
					CPU:    &alerts.HysteresisThreshold{Trigger: 80, Clear: 75},
					Memory: &alerts.HysteresisThreshold{Trigger: 85, Clear: 80},
					Disk:   &alerts.HysteresisThreshold{Trigger: 90, Clear: 85},
				},
				NodeDefaults: alerts.ThresholdConfig{
					CPU:         &alerts.HysteresisThreshold{Trigger: 80, Clear: 75},
					Memory:      &alerts.HysteresisThreshold{Trigger: 85, Clear: 80},
					Disk:        &alerts.HysteresisThreshold{Trigger: 90, Clear: 85},
					Temperature: &alerts.HysteresisThreshold{Trigger: 80, Clear: 75},
				},
				StorageDefault: alerts.HysteresisThreshold{Trigger: 85, Clear: 80},
				TimeThreshold:  5,
				TimeThresholds: map[string]int{
					"guest":   5,
					"node":    5,
					"storage": 5,
					"pbs":     5,
				},
				MinimumDelta:      2.0,
				SuppressionWindow: 5,
				HysteresisMargin:  5.0,
				SnapshotDefaults: alerts.SnapshotAlertConfig{
					Enabled:         false,
					WarningDays:     30,
					CriticalDays:    45,
					WarningSizeGiB:  0,
					CriticalSizeGiB: 0,
				},
				BackupDefaults: alerts.BackupAlertConfig{
					Enabled:      false,
					WarningDays:  7,
					CriticalDays: 14,
				},
				Overrides: make(map[string]alerts.ThresholdConfig),
			}, nil
		}
		return nil, err
	}

	var config alerts.AlertConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// For empty config files ({}), enable alerts by default
	// This handles the case where the file exists but is empty
	if string(data) == "{}" {
		config.Enabled = true
	}
	if config.StorageDefault.Trigger <= 0 {
		config.StorageDefault.Trigger = 85
		config.StorageDefault.Clear = 80
	}
	if config.MinimumDelta <= 0 {
		config.MinimumDelta = 2.0
	}
	if config.SuppressionWindow <= 0 {
		config.SuppressionWindow = 5
	}
	if config.HysteresisMargin <= 0 {
		config.HysteresisMargin = 5.0
	}
	if config.NodeDefaults.Temperature == nil || config.NodeDefaults.Temperature.Trigger <= 0 {
		config.NodeDefaults.Temperature = &alerts.HysteresisThreshold{Trigger: 80, Clear: 75}
	}
	if config.TimeThreshold <= 0 {
		config.TimeThreshold = 5
	}
	if config.TimeThresholds == nil {
		config.TimeThresholds = make(map[string]int)
	}
	ensureDelay := func(key string) {
		if delay, ok := config.TimeThresholds[key]; !ok || delay <= 0 {
			config.TimeThresholds[key] = config.TimeThreshold
		}
	}
	ensureDelay("guest")
	ensureDelay("node")
	ensureDelay("storage")
	ensureDelay("pbs")
	if delay, ok := config.TimeThresholds["all"]; ok && delay <= 0 {
		config.TimeThresholds["all"] = config.TimeThreshold
	}
	if config.SnapshotDefaults.WarningDays < 0 {
		config.SnapshotDefaults.WarningDays = 0
	}
	if config.SnapshotDefaults.CriticalDays < 0 {
		config.SnapshotDefaults.CriticalDays = 0
	}
	if config.SnapshotDefaults.CriticalDays > 0 && config.SnapshotDefaults.WarningDays > config.SnapshotDefaults.CriticalDays {
		config.SnapshotDefaults.WarningDays = config.SnapshotDefaults.CriticalDays
	}
	if config.SnapshotDefaults.WarningSizeGiB < 0 {
		config.SnapshotDefaults.WarningSizeGiB = 0
	}
	if config.SnapshotDefaults.CriticalSizeGiB < 0 {
		config.SnapshotDefaults.CriticalSizeGiB = 0
	}
	if config.SnapshotDefaults.CriticalSizeGiB > 0 && config.SnapshotDefaults.WarningSizeGiB > config.SnapshotDefaults.CriticalSizeGiB {
		config.SnapshotDefaults.WarningSizeGiB = config.SnapshotDefaults.CriticalSizeGiB
	}
	if config.SnapshotDefaults.CriticalSizeGiB == 0 && config.SnapshotDefaults.WarningSizeGiB > 0 {
		config.SnapshotDefaults.CriticalSizeGiB = config.SnapshotDefaults.WarningSizeGiB
	}
	if config.BackupDefaults.WarningDays < 0 {
		config.BackupDefaults.WarningDays = 0
	}
	if config.BackupDefaults.CriticalDays < 0 {
		config.BackupDefaults.CriticalDays = 0
	}
	if config.BackupDefaults.CriticalDays > 0 && config.BackupDefaults.WarningDays > config.BackupDefaults.CriticalDays {
		config.BackupDefaults.WarningDays = config.BackupDefaults.CriticalDays
	}
	config.MetricTimeThresholds = alerts.NormalizeMetricTimeThresholds(config.MetricTimeThresholds)
	config.DockerIgnoredContainerPrefixes = alerts.NormalizeDockerIgnoredPrefixes(config.DockerIgnoredContainerPrefixes)

	// Migration: Set I/O metrics to Off (0) if they have the old default values
	// This helps existing users avoid noisy I/O alerts
	if config.GuestDefaults.DiskRead != nil && config.GuestDefaults.DiskRead.Trigger == 150 {
		config.GuestDefaults.DiskRead = &alerts.HysteresisThreshold{Trigger: 0, Clear: 0}
	}
	if config.GuestDefaults.DiskWrite != nil && config.GuestDefaults.DiskWrite.Trigger == 150 {
		config.GuestDefaults.DiskWrite = &alerts.HysteresisThreshold{Trigger: 0, Clear: 0}
	}
	if config.GuestDefaults.NetworkIn != nil && config.GuestDefaults.NetworkIn.Trigger == 200 {
		config.GuestDefaults.NetworkIn = &alerts.HysteresisThreshold{Trigger: 0, Clear: 0}
	}
	if config.GuestDefaults.NetworkOut != nil && config.GuestDefaults.NetworkOut.Trigger == 200 {
		config.GuestDefaults.NetworkOut = &alerts.HysteresisThreshold{Trigger: 0, Clear: 0}
	}

	log.Info().
		Str("file", c.alertFile).
		Bool("enabled", config.Enabled).
		Msg("Alert configuration loaded")
	return &config, nil
}

// SaveEmailConfig saves email configuration to file (encrypted)
func (c *ConfigPersistence) SaveEmailConfig(config notifications.EmailConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Marshal to JSON first
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	// Encrypt if crypto manager is available
	if c.crypto != nil {
		encrypted, err := c.crypto.Encrypt(data)
		if err != nil {
			return err
		}
		data = encrypted
	}

	// Save with restricted permissions (owner read/write only)
	if err := c.writeConfigFileLocked(c.emailFile, data, 0600); err != nil {
		return err
	}

	log.Info().
		Str("file", c.emailFile).
		Bool("encrypted", c.crypto != nil).
		Msg("Email configuration saved")
	return nil
}

// LoadEmailConfig loads email configuration from file (decrypts if encrypted)
func (c *ConfigPersistence) LoadEmailConfig() (*notifications.EmailConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.emailFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty config if encrypted file doesn't exist
			return &notifications.EmailConfig{
				Enabled:  false,
				SMTPPort: 587,
				TLS:      true,
				To:       []string{},
			}, nil
		}
		return nil, err
	}

	// Decrypt if crypto manager is available
	if c.crypto != nil {
		decrypted, err := c.crypto.Decrypt(data)
		if err != nil {
			return nil, err
		}
		data = decrypted
	}

	var config notifications.EmailConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	log.Info().
		Str("file", c.emailFile).
		Bool("encrypted", c.crypto != nil).
		Msg("Email configuration loaded")
	return &config, nil
}

// SaveAppriseConfig saves Apprise configuration to file (encrypted if available)
func (c *ConfigPersistence) SaveAppriseConfig(config notifications.AppriseConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	config = notifications.NormalizeAppriseConfig(config)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	if c.crypto != nil {
		encrypted, err := c.crypto.Encrypt(data)
		if err != nil {
			return err
		}
		data = encrypted
	}

	if err := c.writeConfigFileLocked(c.appriseFile, data, 0600); err != nil {
		return err
	}

	log.Info().
		Str("file", c.appriseFile).
		Bool("encrypted", c.crypto != nil).
		Msg("Apprise configuration saved")
	return nil
}

// LoadAppriseConfig loads Apprise configuration from file (decrypts if encrypted)
func (c *ConfigPersistence) LoadAppriseConfig() (*notifications.AppriseConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.appriseFile)
	if err != nil {
		if os.IsNotExist(err) {
			defaultCfg := notifications.AppriseConfig{
				Enabled:        false,
				Mode:           notifications.AppriseModeCLI,
				Targets:        []string{},
				CLIPath:        "apprise",
				TimeoutSeconds: 15,
				APIKeyHeader:   "X-API-KEY",
			}
			return &defaultCfg, nil
		}
		return nil, err
	}

	if c.crypto != nil {
		decrypted, err := c.crypto.Decrypt(data)
		if err != nil {
			return nil, err
		}
		data = decrypted
	}

	var config notifications.AppriseConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	normalized := notifications.NormalizeAppriseConfig(config)

	log.Info().
		Str("file", c.appriseFile).
		Bool("encrypted", c.crypto != nil).
		Msg("Apprise configuration loaded")
	return &normalized, nil
}

// SaveWebhooks saves webhook configurations to file
func (c *ConfigPersistence) SaveWebhooks(webhooks []notifications.WebhookConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.MarshalIndent(webhooks, "", "  ")
	if err != nil {
		return err
	}

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	// Encrypt if crypto manager is available
	if c.crypto != nil {
		encrypted, err := c.crypto.Encrypt(data)
		if err != nil {
			return err
		}
		data = encrypted
	}

	if err := c.writeConfigFileLocked(c.webhookFile, data, 0600); err != nil {
		return err
	}

	log.Info().Str("file", c.webhookFile).
		Int("count", len(webhooks)).
		Bool("encrypted", c.crypto != nil).
		Msg("Webhooks saved")
	return nil
}

// LoadWebhooks loads webhook configurations from file (decrypts if encrypted)
func (c *ConfigPersistence) LoadWebhooks() ([]notifications.WebhookConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// First try to load from encrypted file
	data, err := os.ReadFile(c.webhookFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Check for legacy unencrypted file
			legacyFile := filepath.Join(c.configDir, "webhooks.json")
			legacyData, legacyErr := os.ReadFile(legacyFile)
			if legacyErr == nil {
				// Legacy file exists, parse it
				var webhooks []notifications.WebhookConfig
				if err := json.Unmarshal(legacyData, &webhooks); err == nil {
					log.Info().
						Str("file", legacyFile).
						Int("count", len(webhooks)).
						Msg("Found unencrypted webhooks - migration needed")

					// Return the loaded webhooks - migration will be handled by caller
					return webhooks, nil
				}
			}
			// No webhooks file exists
			return []notifications.WebhookConfig{}, nil
		}
		return nil, err
	}

	// Decrypt if crypto manager is available
	if c.crypto != nil {
		decrypted, err := c.crypto.Decrypt(data)
		if err != nil {
			// Try parsing as plain JSON (migration case)
			var webhooks []notifications.WebhookConfig
			if jsonErr := json.Unmarshal(data, &webhooks); jsonErr == nil {
				log.Info().
					Str("file", c.webhookFile).
					Int("count", len(webhooks)).
					Msg("Loaded unencrypted webhooks (will encrypt on next save)")
				return webhooks, nil
			}
			return nil, fmt.Errorf("failed to decrypt webhooks: %w", err)
		}
		data = decrypted
	}

	var webhooks []notifications.WebhookConfig
	if err := json.Unmarshal(data, &webhooks); err != nil {
		return nil, err
	}

	log.Info().
		Str("file", c.webhookFile).
		Int("count", len(webhooks)).
		Bool("encrypted", c.crypto != nil).
		Msg("Webhooks loaded")
	return webhooks, nil
}

// MigrateWebhooksIfNeeded checks for legacy webhooks.json and migrates to encrypted format
func (c *ConfigPersistence) MigrateWebhooksIfNeeded() error {
	// Check if encrypted file already exists
	if _, err := os.Stat(c.webhookFile); err == nil {
		// Encrypted file exists, no migration needed
		return nil
	}

	// Check for legacy unencrypted file
	legacyFile := filepath.Join(c.configDir, "webhooks.json")
	legacyData, err := os.ReadFile(legacyFile)
	if err != nil {
		if os.IsNotExist(err) {
			// No legacy file, nothing to migrate
			return nil
		}
		return fmt.Errorf("failed to read legacy webhooks file: %w", err)
	}

	// Parse legacy webhooks
	var webhooks []notifications.WebhookConfig
	if err := json.Unmarshal(legacyData, &webhooks); err != nil {
		return fmt.Errorf("failed to parse legacy webhooks: %w", err)
	}

	log.Info().
		Str("from", legacyFile).
		Str("to", c.webhookFile).
		Int("count", len(webhooks)).
		Msg("Migrating webhooks to encrypted format")

	// Save to encrypted file
	if err := c.SaveWebhooks(webhooks); err != nil {
		return fmt.Errorf("failed to save encrypted webhooks: %w", err)
	}

	// Create backup of original file
	backupFile := legacyFile + ".backup"
	if err := os.Rename(legacyFile, backupFile); err != nil {
		log.Warn().Err(err).Msg("Failed to rename legacy webhooks file to backup")
	} else {
		log.Info().Str("backup", backupFile).Msg("Legacy webhooks file backed up")
	}

	return nil
}

// NodesConfig represents the saved nodes configuration
type NodesConfig struct {
	PVEInstances []PVEInstance `json:"pveInstances"`
	PBSInstances []PBSInstance `json:"pbsInstances"`
	PMGInstances []PMGInstance `json:"pmgInstances"`
}

// SystemSettings represents system configuration settings
type SystemSettings struct {
	// Note: PVE polling is hardcoded to 10s since Proxmox cluster/resources endpoint only updates every 10s
	PBSPollingInterval          int             `json:"pbsPollingInterval"` // PBS polling interval in seconds
	PMGPollingInterval          int             `json:"pmgPollingInterval"` // PMG polling interval in seconds
	BackupPollingInterval       int             `json:"backupPollingInterval,omitempty"`
	BackupPollingEnabled        *bool           `json:"backupPollingEnabled,omitempty"`
	AdaptivePollingEnabled      *bool           `json:"adaptivePollingEnabled,omitempty"`
	AdaptivePollingBaseInterval int             `json:"adaptivePollingBaseInterval,omitempty"`
	AdaptivePollingMinInterval  int             `json:"adaptivePollingMinInterval,omitempty"`
	AdaptivePollingMaxInterval  int             `json:"adaptivePollingMaxInterval,omitempty"`
	BackendPort                 int             `json:"backendPort,omitempty"`
	FrontendPort                int             `json:"frontendPort,omitempty"`
	AllowedOrigins              string          `json:"allowedOrigins,omitempty"`
	ConnectionTimeout           int             `json:"connectionTimeout,omitempty"`
	UpdateChannel               string          `json:"updateChannel,omitempty"`
	AutoUpdateEnabled           bool            `json:"autoUpdateEnabled"` // Removed omitempty so false is saved
	AutoUpdateCheckInterval     int             `json:"autoUpdateCheckInterval,omitempty"`
	AutoUpdateTime              string          `json:"autoUpdateTime,omitempty"`
	LogLevel                    string          `json:"logLevel,omitempty"`
	DiscoveryEnabled            bool            `json:"discoveryEnabled"`
	DiscoverySubnet             string          `json:"discoverySubnet,omitempty"`
	DiscoveryConfig             DiscoveryConfig `json:"discoveryConfig"`
	Theme                       string          `json:"theme,omitempty"`               // User theme preference: "light", "dark", or empty for system default
	AllowEmbedding              bool            `json:"allowEmbedding"`                // Allow iframe embedding
	AllowedEmbedOrigins         string          `json:"allowedEmbedOrigins,omitempty"` // Comma-separated list of allowed origins for embedding
	// APIToken removed - now handled via .env file only
}

// DefaultSystemSettings returns a SystemSettings struct populated with sane defaults.
func DefaultSystemSettings() *SystemSettings {
	defaultDiscovery := DefaultDiscoveryConfig()
	return &SystemSettings{
		PBSPollingInterval: 60,
		PMGPollingInterval: 60,
		AutoUpdateEnabled:  false,
		DiscoveryEnabled:   false,
		DiscoverySubnet:    "auto",
		DiscoveryConfig:    defaultDiscovery,
		AllowEmbedding:     false,
	}
}

// SaveNodesConfig saves nodes configuration to file (encrypted)
func (c *ConfigPersistence) SaveNodesConfig(pveInstances []PVEInstance, pbsInstances []PBSInstance, pmgInstances []PMGInstance) error {
	return c.saveNodesConfig(pveInstances, pbsInstances, pmgInstances, false)
}

// SaveNodesConfigAllowEmpty saves nodes configuration even when all nodes are removed.
// Use sparingly for explicit administrative actions (e.g. deleting the final node).
func (c *ConfigPersistence) SaveNodesConfigAllowEmpty(pveInstances []PVEInstance, pbsInstances []PBSInstance, pmgInstances []PMGInstance) error {
	return c.saveNodesConfig(pveInstances, pbsInstances, pmgInstances, true)
}

func (c *ConfigPersistence) saveNodesConfig(pveInstances []PVEInstance, pbsInstances []PBSInstance, pmgInstances []PMGInstance, allowEmpty bool) error {
	// CRITICAL: Prevent saving empty nodes when in mock mode
	// Mock mode should NEVER modify real node configuration
	if mock.IsMockEnabled() {
		log.Warn().Msg("Skipping nodes save - mock mode is enabled")
		return nil // Silently succeed to prevent errors but don't save
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// CRITICAL: Never save empty nodes configuration
	// This prevents data loss from accidental wipes
	if !allowEmpty && len(pveInstances) == 0 && len(pbsInstances) == 0 && len(pmgInstances) == 0 {
		// Check if we're replacing existing non-empty config
		if existing, err := c.LoadNodesConfig(); err == nil && existing != nil {
			if len(existing.PVEInstances) > 0 || len(existing.PBSInstances) > 0 || len(existing.PMGInstances) > 0 {
				log.Error().
					Int("existing_pve", len(existing.PVEInstances)).
					Int("existing_pbs", len(existing.PBSInstances)).
					Int("existing_pmg", len(existing.PMGInstances)).
					Msg("BLOCKED attempt to save empty nodes config - would delete existing nodes!")
				return fmt.Errorf("refusing to save empty nodes config when %d nodes exist",
					len(existing.PVEInstances)+len(existing.PBSInstances)+len(existing.PMGInstances))
			}
		}
	}

	config := NodesConfig{
		PVEInstances: pveInstances,
		PBSInstances: pbsInstances,
		PMGInstances: pmgInstances,
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	// Create TIMESTAMPED backup of existing file before overwriting (if it exists and has content)
	// This ensures we keep multiple backups and can recover from disasters
	if info, err := os.Stat(c.nodesFile); err == nil && info.Size() > 0 {
		// Create timestamped backup
		timestampedBackup := fmt.Sprintf("%s.backup-%s", c.nodesFile, time.Now().Format("20060102-150405"))
		if backupData, err := os.ReadFile(c.nodesFile); err == nil {
			if err := os.WriteFile(timestampedBackup, backupData, 0600); err != nil {
				log.Warn().Err(err).Msg("Failed to create timestamped backup of nodes config")
			} else {
				log.Info().Str("backup", timestampedBackup).Msg("Created timestamped backup of nodes config")
			}
		}

		// Also maintain a "latest" backup for quick recovery
		latestBackup := c.nodesFile + ".backup"
		if backupData, err := os.ReadFile(c.nodesFile); err == nil {
			if err := os.WriteFile(latestBackup, backupData, 0600); err != nil {
				log.Warn().Err(err).Msg("Failed to create latest backup of nodes config")
			}
		}

		// Clean up old timestamped backups (keep last 10)
		c.cleanupOldBackups(c.nodesFile + ".backup-*")
	}

	// Encrypt if crypto manager is available
	if c.crypto != nil {
		encrypted, err := c.crypto.Encrypt(data)
		if err != nil {
			return err
		}
		data = encrypted
	}

	if err := c.writeConfigFileLocked(c.nodesFile, data, 0600); err != nil {
		return err
	}

	log.Info().Str("file", c.nodesFile).
		Int("pve", len(pveInstances)).
		Int("pbs", len(pbsInstances)).
		Int("pmg", len(pmgInstances)).
		Bool("encrypted", c.crypto != nil).
		Msg("Nodes configuration saved")
	return nil
}

// LoadNodesConfig loads nodes configuration from file (decrypts if encrypted)
func (c *ConfigPersistence) LoadNodesConfig() (*NodesConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.nodesFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty config if encrypted file doesn't exist
			log.Info().Msg("No encrypted nodes configuration found, returning empty config")
			return &NodesConfig{
				PVEInstances: []PVEInstance{},
				PBSInstances: []PBSInstance{},
				PMGInstances: []PMGInstance{},
			}, nil
		}
		return nil, err
	}

	// Decrypt if crypto manager is available
	if c.crypto != nil {
		decrypted, err := c.crypto.Decrypt(data)
		if err != nil {
			// Decryption failed - file may be corrupted
			log.Error().Err(err).Str("file", c.nodesFile).Msg("Failed to decrypt nodes config - file may be corrupted")

			// Try to restore from backup
			backupFile := c.nodesFile + ".backup"
			if backupData, backupErr := os.ReadFile(backupFile); backupErr == nil {
				log.Info().Str("backup", backupFile).Msg("Attempting to restore nodes config from backup")
				if decryptedBackup, decryptErr := c.crypto.Decrypt(backupData); decryptErr == nil {
					log.Info().Msg("Successfully decrypted backup file")
					data = decryptedBackup

					// Move corrupted file out of the way with timestamp
					corruptedFile := fmt.Sprintf("%s.corrupted-%s", c.nodesFile, time.Now().Format("20060102-150405"))
					if renameErr := os.Rename(c.nodesFile, corruptedFile); renameErr != nil {
						log.Warn().Err(renameErr).Msg("Failed to rename corrupted file")
					} else {
						log.Warn().Str("corruptedFile", corruptedFile).Msg("Moved corrupted nodes config")
					}

					// Restore backup as current file
					if writeErr := os.WriteFile(c.nodesFile, backupData, 0600); writeErr != nil {
						log.Error().Err(writeErr).Msg("Failed to restore backup as current file")
					} else {
						log.Info().Msg("Successfully restored nodes config from backup")
					}
				} else {
					log.Error().Err(decryptErr).Msg("Backup file is also corrupted or encrypted with different key")

					// CRITICAL: Don't delete the corrupted file - leave it for manual recovery
					// Create an empty config so startup can continue, but log prominently
					log.Error().
						Str("corruptedFile", c.nodesFile).
						Str("backupFile", backupFile).
						Msg("⚠️  CRITICAL: Both nodes.enc and backup are corrupted/unreadable. Encryption key may have been regenerated. Manual recovery required. Starting with empty config.")

					// Move corrupted file with timestamp for forensics
					corruptedFile := fmt.Sprintf("%s.corrupted-%s", c.nodesFile, time.Now().Format("20060102-150405"))
					os.Rename(c.nodesFile, corruptedFile)

					// Create empty but valid config so system can start
					emptyConfig := NodesConfig{PVEInstances: []PVEInstance{}, PBSInstances: []PBSInstance{}, PMGInstances: []PMGInstance{}}
					emptyData, _ := json.Marshal(emptyConfig)
					if c.crypto != nil {
						emptyData, _ = c.crypto.Encrypt(emptyData)
					}
					os.WriteFile(c.nodesFile, emptyData, 0600)

					return &emptyConfig, nil
				}
			} else {
				log.Error().Err(backupErr).Msg("No backup file available for recovery")

				// CRITICAL: Don't delete the corrupted file - leave it for manual recovery
				log.Error().
					Str("corruptedFile", c.nodesFile).
					Msg("⚠️  CRITICAL: nodes.enc is corrupted and no backup exists. Encryption key may have been regenerated. Manual recovery required. Starting with empty config.")

				// Move corrupted file with timestamp for forensics
				corruptedFile := fmt.Sprintf("%s.corrupted-%s", c.nodesFile, time.Now().Format("20060102-150405"))
				os.Rename(c.nodesFile, corruptedFile)

				// Create empty but valid config so system can start
				emptyConfig := NodesConfig{PVEInstances: []PVEInstance{}, PBSInstances: []PBSInstance{}, PMGInstances: []PMGInstance{}}
				emptyData, _ := json.Marshal(emptyConfig)
				if c.crypto != nil {
					emptyData, _ = c.crypto.Encrypt(emptyData)
				}
				os.WriteFile(c.nodesFile, emptyData, 0600)

				return &emptyConfig, nil
			}
		} else {
			data = decrypted
		}
	}

	var config NodesConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	if config.PVEInstances == nil {
		config.PVEInstances = []PVEInstance{}
	}
	if config.PBSInstances == nil {
		config.PBSInstances = []PBSInstance{}
	}
	if config.PMGInstances == nil {
		config.PMGInstances = []PMGInstance{}
	}

	// Track if any migrations were applied
	migrationApplied := false

	// Fix for bug where TokenName was incorrectly set when using password auth
	// If a PBS instance has both Password and TokenName, clear the TokenName
	for i := range config.PBSInstances {
		if config.PBSInstances[i].Password != "" && config.PBSInstances[i].TokenName != "" {
			log.Info().
				Str("instance", config.PBSInstances[i].Name).
				Msg("Fixing PBS config: clearing TokenName since Password is set")
			config.PBSInstances[i].TokenName = ""
			config.PBSInstances[i].TokenValue = ""
		}

		// Fix for missing port in PBS host
		host := config.PBSInstances[i].Host
		if host != "" {
			// Check if we need to add default port
			protocolEnd := 0
			if strings.HasPrefix(host, "https://") {
				protocolEnd = 8
			} else if strings.HasPrefix(host, "http://") {
				protocolEnd = 7
			} else if !strings.Contains(host, "://") {
				// No protocol specified, add https and check for port
				if !strings.Contains(host, ":") {
					// No port specified, add protocol and default port
					config.PBSInstances[i].Host = "https://" + host + ":8007"
					log.Info().
						Str("instance", config.PBSInstances[i].Name).
						Str("oldHost", host).
						Str("newHost", config.PBSInstances[i].Host).
						Msg("Fixed PBS host by adding protocol and default port")
				} else {
					// Port specified, just add protocol
					config.PBSInstances[i].Host = "https://" + host
					log.Info().
						Str("instance", config.PBSInstances[i].Name).
						Str("oldHost", host).
						Str("newHost", config.PBSInstances[i].Host).
						Msg("Fixed PBS host by adding protocol")
				}
			} else if protocolEnd > 0 {
				// Has protocol, check if port is missing
				hostAfterProtocol := host[protocolEnd:]
				if !strings.Contains(hostAfterProtocol, ":") {
					// No port specified, add default PBS port
					config.PBSInstances[i].Host = host + ":8007"
					log.Info().
						Str("instance", config.PBSInstances[i].Name).
						Str("oldHost", host).
						Str("newHost", config.PBSInstances[i].Host).
						Msg("Fixed PBS host by adding default port 8007")
				}
			}
		}

		// Migration: Ensure MonitorBackups is enabled for PBS instances
		// This fixes issue #411 where PBS backups weren't showing
		if !config.PBSInstances[i].MonitorBackups {
			log.Info().
				Str("instance", config.PBSInstances[i].Name).
				Msg("Enabling MonitorBackups for PBS instance (was disabled)")
			config.PBSInstances[i].MonitorBackups = true
			migrationApplied = true
		}
	}

	for i := range config.PMGInstances {
		if config.PMGInstances[i].Password != "" && config.PMGInstances[i].TokenName != "" {
			log.Info().
				Str("instance", config.PMGInstances[i].Name).
				Msg("Fixing PMG config: clearing TokenName since Password is set")
			config.PMGInstances[i].TokenName = ""
			config.PMGInstances[i].TokenValue = ""
			migrationApplied = true
		}

		host := config.PMGInstances[i].Host
		if host == "" {
			continue
		}

		protocolEnd := 0
		if strings.HasPrefix(host, "https://") {
			protocolEnd = 8
		} else if strings.HasPrefix(host, "http://") {
			protocolEnd = 7
		} else if !strings.Contains(host, "://") {
			if !strings.Contains(host, ":") {
				config.PMGInstances[i].Host = "https://" + host + ":8006"
			} else {
				config.PMGInstances[i].Host = "https://" + host
			}
			log.Info().
				Str("instance", config.PMGInstances[i].Name).
				Str("oldHost", host).
				Str("newHost", config.PMGInstances[i].Host).
				Msg("Fixed PMG host by adding protocol/port")
			migrationApplied = true
			continue
		}

		if protocolEnd > 0 {
			hostAfterProtocol := host[protocolEnd:]
			if !strings.Contains(hostAfterProtocol, ":") {
				config.PMGInstances[i].Host = host + ":8006"
				log.Info().
					Str("instance", config.PMGInstances[i].Name).
					Str("oldHost", host).
					Str("newHost", config.PMGInstances[i].Host).
					Msg("Fixed PMG host by adding default port 8006")
				migrationApplied = true
			}
		}
	}

	// If any migrations were applied, save the updated configuration
	if migrationApplied {
		log.Info().Msg("Migrations applied, saving updated configuration")
		// Need to unlock before saving to avoid deadlock
		c.mu.RUnlock()
		if err := c.SaveNodesConfig(config.PVEInstances, config.PBSInstances, config.PMGInstances); err != nil {
			log.Error().Err(err).Msg("Failed to save configuration after migration")
		}
		c.mu.RLock()
	}

	log.Info().Str("file", c.nodesFile).
		Int("pve", len(config.PVEInstances)).
		Int("pbs", len(config.PBSInstances)).
		Int("pmg", len(config.PMGInstances)).
		Bool("encrypted", c.crypto != nil).
		Msg("Nodes configuration loaded")
	return &config, nil
}

// SaveSystemSettings saves system settings to file
func (c *ConfigPersistence) SaveSystemSettings(settings SystemSettings) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	if err := c.writeConfigFileLocked(c.systemFile, data, 0600); err != nil {
		return err
	}

	// Also update the .env file if it exists
	envFile := filepath.Join(c.configDir, ".env")
	if err := c.updateEnvFile(envFile, settings); err != nil {
		log.Warn().Err(err).Msg("Failed to update .env file")
		// Don't fail the operation if .env update fails
	}

	log.Info().Str("file", c.systemFile).Msg("System settings saved")
	return nil
}

// SaveOIDCConfig stores OIDC settings, encrypting them when a crypto manager is available.
func (c *ConfigPersistence) SaveOIDCConfig(settings OIDCConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.EnsureConfigDir(); err != nil {
		return err
	}

	// Do not persist runtime-only flags.
	settings.EnvOverrides = nil

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if c.crypto != nil {
		encrypted, err := c.crypto.Encrypt(data)
		if err != nil {
			return err
		}
		data = encrypted
	}

	if err := c.writeConfigFileLocked(c.oidcFile, data, 0600); err != nil {
		return err
	}

	log.Info().Str("file", c.oidcFile).Msg("OIDC configuration saved")
	return nil
}

// LoadOIDCConfig retrieves the persisted OIDC settings. It returns nil when no configuration exists yet.
func (c *ConfigPersistence) LoadOIDCConfig() (*OIDCConfig, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.oidcFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	if c.crypto != nil {
		decrypted, err := c.crypto.Decrypt(data)
		if err != nil {
			return nil, err
		}
		data = decrypted
	}

	var settings OIDCConfig
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}

	log.Info().Str("file", c.oidcFile).Msg("OIDC configuration loaded")
	return &settings, nil
}

// LoadSystemSettings loads system settings from file
func (c *ConfigPersistence) LoadSystemSettings() (*SystemSettings, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := os.ReadFile(c.systemFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Return nil if file doesn't exist - let env vars take precedence
			return nil, nil
		}
		return nil, err
	}

	settings := DefaultSystemSettings()
	if settings == nil {
		settings = &SystemSettings{}
	}
	if err := json.Unmarshal(data, settings); err != nil {
		return nil, err
	}

	log.Info().Str("file", c.systemFile).Msg("System settings loaded")
	return settings, nil
}

// updateEnvFile updates the .env file with new system settings
func (c *ConfigPersistence) updateEnvFile(envFile string, settings SystemSettings) error {
	// Check if .env file exists
	if _, err := os.Stat(envFile); os.IsNotExist(err) {
		// File doesn't exist, nothing to update
		return nil
	}

	// Read the existing .env file
	file, err := os.Open(envFile)
	if err != nil {
		return err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip POLLING_INTERVAL lines - deprecated
		if strings.HasPrefix(line, "POLLING_INTERVAL=") {
			// Skip this line, polling interval is now hardcoded
			continue
		} else if strings.HasPrefix(line, "UPDATE_CHANNEL=") && settings.UpdateChannel != "" {
			lines = append(lines, fmt.Sprintf("UPDATE_CHANNEL=%s", settings.UpdateChannel))
		} else if strings.HasPrefix(line, "AUTO_UPDATE_ENABLED=") {
			// Always update AUTO_UPDATE_ENABLED when the line exists
			lines = append(lines, fmt.Sprintf("AUTO_UPDATE_ENABLED=%t", settings.AutoUpdateEnabled))
		} else if strings.HasPrefix(line, "AUTO_UPDATE_CHECK_INTERVAL=") && settings.AutoUpdateCheckInterval > 0 {
			lines = append(lines, fmt.Sprintf("AUTO_UPDATE_CHECK_INTERVAL=%d", settings.AutoUpdateCheckInterval))
		} else {
			lines = append(lines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Note: POLLING_INTERVAL is deprecated and no longer written

	// Write the updated content back atomically
	content := strings.Join(lines, "\n")
	if len(lines) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	// Write to temp file first
	tempFile := envFile + ".tmp"
	if err := os.WriteFile(tempFile, []byte(content), 0644); err != nil {
		return err
	}

	// Atomic rename
	return os.Rename(tempFile, envFile)
}

// cleanupOldBackups removes old backup files, keeping only the most recent N backups
func (c *ConfigPersistence) cleanupOldBackups(pattern string) {
	// Use filepath.Glob to find all backup files matching the pattern
	matches, err := filepath.Glob(pattern)
	if err != nil {
		log.Warn().Err(err).Str("pattern", pattern).Msg("Failed to find backup files for cleanup")
		return
	}

	// Keep only the last 10 backups
	const maxBackups = 10
	if len(matches) <= maxBackups {
		return
	}

	// Sort by modification time (oldest first)
	type fileInfo struct {
		path    string
		modTime time.Time
	}
	var files []fileInfo
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		files = append(files, fileInfo{path: match, modTime: info.ModTime()})
	}

	// Sort oldest first
	for i := 0; i < len(files)-1; i++ {
		for j := i + 1; j < len(files); j++ {
			if files[i].modTime.After(files[j].modTime) {
				files[i], files[j] = files[j], files[i]
			}
		}
	}

	// Delete oldest backups (keep last 10)
	toDelete := len(files) - maxBackups
	for i := 0; i < toDelete; i++ {
		if err := os.Remove(files[i].path); err != nil {
			log.Warn().Err(err).Str("file", files[i].path).Msg("Failed to delete old backup")
		} else {
			log.Debug().Str("file", files[i].path).Msg("Deleted old backup")
		}
	}
}
