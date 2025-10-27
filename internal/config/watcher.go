package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/RouXx67/PulseUP/internal/auth"
	"github.com/rs/zerolog/log"
)

// ConfigWatcher monitors the .env file for changes and updates runtime config
type ConfigWatcher struct {
	config               *Config
	envPath              string
	mockEnvPath          string
	apiTokensPath        string
	watcher              *fsnotify.Watcher
	stopChan             chan struct{}
	lastModTime          time.Time
	mockLastModTime      time.Time
	apiTokensLastModTime time.Time
	mu                   sync.RWMutex
	onMockReload         func() // Callback to trigger backend restart
}

// NewConfigWatcher creates a new config watcher
func NewConfigWatcher(config *Config) (*ConfigWatcher, error) {
	// Determine env file path
	envPath := filepath.Join(config.ConfigPath, ".env")
	if config.ConfigPath == "" {
		envPath = "/etc/pulse/.env"
	}

	// Check for Docker environment
	if _, err := os.Stat("/data/.env"); err == nil {
		envPath = "/data/.env"
	}

	// Determine mock.env path - skip in Docker or if directory doesn't exist
	mockEnvPath := ""
	isDocker := os.Getenv("PULSE_DOCKER") == "true"
	mockDir := "/opt/pulse"
	if !isDocker {
		if stat, err := os.Stat(mockDir); err == nil && stat.IsDir() {
			mockEnvPath = filepath.Join(mockDir, "mock.env")
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	apiTokensPath := filepath.Join(filepath.Dir(envPath), "api_tokens.json")

	cw := &ConfigWatcher{
		config:        config,
		envPath:       envPath,
		mockEnvPath:   mockEnvPath,
		apiTokensPath: apiTokensPath,
		watcher:       watcher,
		stopChan:      make(chan struct{}),
	}

	// Get initial mod times
	if stat, err := os.Stat(envPath); err == nil {
		cw.lastModTime = stat.ModTime()
	}
	if mockEnvPath != "" {
		if stat, err := os.Stat(mockEnvPath); err == nil {
			cw.mockLastModTime = stat.ModTime()
		}
	}
	if stat, err := os.Stat(apiTokensPath); err == nil {
		cw.apiTokensLastModTime = stat.ModTime()
	}

	return cw, nil
}

// SetMockReloadCallback sets the callback function to trigger when mock.env changes
func (cw *ConfigWatcher) SetMockReloadCallback(callback func()) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.onMockReload = callback
}

// Start begins watching the config file
func (cw *ConfigWatcher) Start() error {
	// Watch the directory for .env
	dir := filepath.Dir(cw.envPath)
	err := cw.watcher.Add(dir)
	if err != nil {
		log.Warn().Err(err).Str("path", dir).Msg("Failed to watch config directory")
	}

	// Also watch the mock.env directory if it's configured (not in Docker)
	if cw.mockEnvPath != "" {
		mockDir := filepath.Dir(cw.mockEnvPath)
		if err := cw.watcher.Add(mockDir); err != nil {
			log.Warn().Err(err).Str("path", mockDir).Msg("Failed to watch mock.env directory")
		}
	}

	if err != nil {
		log.Warn().Msg("Falling back to polling for config changes")
		go cw.pollForChanges()
		return nil
	}

	go cw.watchForChanges()
	logEvent := log.Info().Str("env_path", cw.envPath).Str("api_tokens_path", cw.apiTokensPath)
	if cw.mockEnvPath != "" {
		logEvent = logEvent.Str("mock_env_path", cw.mockEnvPath)
	}
	logEvent.Msg("Started watching config files for changes")
	return nil
}

// Stop stops the config watcher
func (cw *ConfigWatcher) Stop() {
	select {
	case <-cw.stopChan:
		// Already stopped
		return
	default:
		close(cw.stopChan)
	}
	cw.watcher.Close()
}

// ReloadConfig manually triggers a config reload (e.g., from SIGHUP)
func (cw *ConfigWatcher) ReloadConfig() {
	cw.reloadConfig()
}

// watchForChanges handles fsnotify events
func (cw *ConfigWatcher) watchForChanges() {
	for {
		select {
		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}

			// Check if the event is for our .env file
			if filepath.Base(event.Name) == ".env" || event.Name == cw.envPath {
				// Debounce - wait a bit for write to complete
				time.Sleep(100 * time.Millisecond)

				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					log.Info().Str("event", event.Op.String()).Msg("Detected .env file change")
					cw.reloadConfig()
				}
			}

			if cw.apiTokensPath != "" && (filepath.Base(event.Name) == filepath.Base(cw.apiTokensPath) || event.Name == cw.apiTokensPath) {
				// Debounce
				time.Sleep(100 * time.Millisecond)

				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					log.Info().Str("event", event.Op.String()).Msg("Detected API token file change")
					cw.reloadAPITokens()
				}
			}

			// Check if the event is for mock.env (only if mock.env watching is enabled)
			if cw.mockEnvPath != "" && (filepath.Base(event.Name) == "mock.env" || event.Name == cw.mockEnvPath) {
				// Debounce - wait a bit for write to complete
				time.Sleep(100 * time.Millisecond)

				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					log.Info().Str("event", event.Op.String()).Msg("Detected mock.env file change")
					cw.reloadMockConfig()
				}
			}

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			log.Error().Err(err).Msg("Config watcher error")

		case <-cw.stopChan:
			return
		}
	}
}

// pollForChanges is a fallback that polls for changes
func (cw *ConfigWatcher) pollForChanges() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check .env
			if stat, err := os.Stat(cw.envPath); err == nil {
				if stat.ModTime().After(cw.lastModTime) {
					log.Info().Msg("Detected .env file change via polling")
					cw.lastModTime = stat.ModTime()
					cw.reloadConfig()
				}
			}

			// Check mock.env (only if mock.env watching is enabled)
			if cw.mockEnvPath != "" {
				if stat, err := os.Stat(cw.mockEnvPath); err == nil {
					if stat.ModTime().After(cw.mockLastModTime) {
						log.Info().Msg("Detected mock.env file change via polling")
						cw.mockLastModTime = stat.ModTime()
						cw.reloadMockConfig()
					}
				}
			}

			if cw.apiTokensPath != "" {
				if stat, err := os.Stat(cw.apiTokensPath); err == nil {
					if stat.ModTime().After(cw.apiTokensLastModTime) {
						log.Info().Msg("Detected API token file change via polling")
						cw.apiTokensLastModTime = stat.ModTime()
						cw.reloadAPITokens()
					}
				}
			}

		case <-cw.stopChan:
			return
		}
	}
}

// reloadConfig reloads the config from the .env file
func (cw *ConfigWatcher) reloadConfig() {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	// Load the .env file
	envMap, err := godotenv.Read(cw.envPath)
	if err != nil {
		// File might not exist, which is fine (no auth)
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("Failed to read .env file")
			return
		}
		envMap = make(map[string]string)
	}

	// Track what changed
	var changes []string

	// Update auth settings
	oldAuthUser := cw.config.AuthUser
	oldAuthPass := cw.config.AuthPass
	oldTokenHashes := cw.config.ActiveAPITokenHashes()
	existingByHash := make(map[string]APITokenRecord, len(cw.config.APITokens))
	for _, record := range cw.config.APITokens {
		existingByHash[record.Hash] = record.Clone()
	}

	// Apply auth user
	newUser := strings.Trim(envMap["PULSE_AUTH_USER"], "'\"")
	if newUser != oldAuthUser {
		cw.config.AuthUser = newUser
		if newUser == "" {
			changes = append(changes, "auth user removed")
		} else if oldAuthUser == "" {
			changes = append(changes, "auth user added")
		} else {
			changes = append(changes, "auth user updated")
		}
	}

	// Apply auth password
	newPass := strings.Trim(envMap["PULSE_AUTH_PASS"], "'\"")
	if newPass != oldAuthPass {
		cw.config.AuthPass = newPass
		if newPass == "" {
			changes = append(changes, "auth password removed")
		} else if oldAuthPass == "" {
			changes = append(changes, "auth password added")
		} else {
			changes = append(changes, "auth password updated")
		}
	}

	// Apply API tokens if present in .env (legacy support)
	rawTokens := make([]string, 0, 4)
	if raw, ok := envMap["API_TOKENS"]; ok {
		raw = strings.Trim(raw, "'\"")
		if raw != "" {
			parts := strings.Split(raw, ",")
			for _, part := range parts {
				token := strings.TrimSpace(part)
				if token != "" {
					rawTokens = append(rawTokens, token)
				}
			}
		} else {
			// Explicit empty list clears tokens
			rawTokens = []string{}
		}
	}
	if raw, ok := envMap["API_TOKEN"]; ok {
		raw = strings.Trim(raw, "'\"")
		rawTokens = append(rawTokens, raw)
	}

	if len(rawTokens) > 0 {
		seen := make(map[string]struct{}, len(rawTokens))
		newRecords := make([]APITokenRecord, 0, len(rawTokens))
		for _, tokenValue := range rawTokens {
			tokenValue = strings.TrimSpace(tokenValue)
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
			}

			if _, exists := seen[hashed]; exists {
				continue
			}
			seen[hashed] = struct{}{}

			if existing, ok := existingByHash[hashed]; ok {
				newRecords = append(newRecords, existing)
			} else {
				newRecords = append(newRecords, APITokenRecord{
					ID:        uuid.NewString(),
					Name:      "Environment token",
					Hash:      hashed,
					Prefix:    prefix,
					Suffix:    suffix,
					CreatedAt: time.Now().UTC(),
				})
			}
		}

		cw.config.APITokens = newRecords
		cw.config.SortAPITokens()
		cw.config.APITokenEnabled = len(newRecords) > 0

		newHashes := cw.config.ActiveAPITokenHashes()
		if !reflect.DeepEqual(oldTokenHashes, newHashes) {
			switch {
			case len(newHashes) == 0:
				changes = append(changes, "API tokens removed")
			case len(oldTokenHashes) == 0:
				changes = append(changes, "API tokens added")
			default:
				changes = append(changes, "API tokens updated")
			}

			if globalPersistence != nil {
				if err := globalPersistence.SaveAPITokens(cw.config.APITokens); err != nil {
					log.Error().Err(err).Msg("Failed to persist API tokens from .env reload")
				}
			}
		}
	}

	// REMOVED: POLLING_INTERVAL from .env - now ONLY in system.json
	// This prevents confusion and ensures single source of truth

	// Log changes
	if len(changes) > 0 {
		log.Info().
			Strs("changes", changes).
			Bool("has_auth", cw.config.AuthUser != "" && cw.config.AuthPass != "").
			Bool("has_token", cw.config.HasAPITokens()).
			Msg("Applied .env file changes to runtime config")
	} else {
		log.Debug().Msg("No relevant changes detected in .env file")
	}
}

func (cw *ConfigWatcher) reloadAPITokens() {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if globalPersistence == nil {
		log.Warn().Msg("Config persistence unavailable; cannot reload API tokens")
		return
	}

	tokens, err := globalPersistence.LoadAPITokens()
	if err != nil {
		log.Error().Err(err).Msg("Failed to reload API tokens")
		return
	}

	cw.config.APITokens = tokens
	cw.config.SortAPITokens()
	cw.config.APITokenEnabled = len(tokens) > 0

	if cw.apiTokensPath != "" {
		if stat, err := os.Stat(cw.apiTokensPath); err == nil {
			cw.apiTokensLastModTime = stat.ModTime()
		}
	}

	log.Info().Int("count", len(tokens)).Msg("Reloaded API tokens from disk")
}

// reloadMockConfig handles mock.env file changes
func (cw *ConfigWatcher) reloadMockConfig() {
	// Skip if mock.env watching is disabled (Docker environment)
	if cw.mockEnvPath == "" {
		return
	}

	cw.mu.Lock()
	callback := cw.onMockReload
	cw.mu.Unlock()

	// Load the mock.env file to update environment variables
	envMap, err := godotenv.Read(cw.mockEnvPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("Failed to read mock.env file")
			return
		}
		log.Warn().Msg("mock.env file not found")
		return
	}

	// Load local overrides if they exist
	mockEnvLocalPath := cw.mockEnvPath + ".local"
	if localEnv, err := godotenv.Read(mockEnvLocalPath); err == nil {
		// Merge local overrides into envMap
		for key, value := range localEnv {
			envMap[key] = value
		}
		log.Debug().Str("path", mockEnvLocalPath).Msg("Loaded mock.env.local overrides")
	}

	// Update environment variables for the mock package to read
	for key, value := range envMap {
		if strings.HasPrefix(key, "PULSE_MOCK_") {
			os.Setenv(key, value)
		}
	}

	log.Info().
		Str("path", cw.mockEnvPath).
		Interface("config", envMap).
		Msg("Reloaded mock.env configuration")

	// Trigger callback to restart backend if set
	if callback != nil {
		log.Info().Msg("Triggering backend restart due to mock.env change")
		go callback()
	}
}
