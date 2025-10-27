package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RouXx67/PulseUP/internal/auth"
	"github.com/RouXx67/PulseUP/internal/config"
	"github.com/RouXx67/PulseUP/internal/dockeragent"
	"github.com/RouXx67/PulseUP/internal/models"
	"github.com/RouXx67/PulseUP/internal/monitoring"
	"github.com/RouXx67/PulseUP/internal/tempproxy"
	"github.com/RouXx67/PulseUP/internal/updates"
	"github.com/RouXx67/PulseUP/internal/utils"
	"github.com/RouXx67/PulseUP/internal/websocket"
	"github.com/rs/zerolog/log"
)

// Router handles HTTP routing
type Router struct {
	mux                   *http.ServeMux
	config                *config.Config
	monitor               *monitoring.Monitor
	alertHandlers         *AlertHandlers
	configHandlers        *ConfigHandlers
	notificationHandlers  *NotificationHandlers
	dockerAgentHandlers   *DockerAgentHandlers
	systemSettingsHandler *SystemSettingsHandler
	wsHub                 *websocket.Hub
	reloadFunc            func() error
	updateManager         *updates.Manager
	exportLimiter         *RateLimiter
	persistence           *config.ConfigPersistence
	oidcMu                sync.Mutex
	oidcService           *OIDCService
	wrapped               http.Handler
	projectRoot           string
	// Cached system settings to avoid loading from disk on every request
	settingsMu           sync.RWMutex
	cachedAllowEmbedding bool
	cachedAllowedOrigins string
	publicURLMu          sync.Mutex
	publicURLDetected    bool
}

// NewRouter creates a new router instance
func NewRouter(cfg *config.Config, monitor *monitoring.Monitor, wsHub *websocket.Hub, reloadFunc func() error) *Router {
	// Initialize persistent session and CSRF stores
	InitSessionStore(cfg.DataPath)
	InitCSRFStore(cfg.DataPath)

	projectRoot, err := os.Getwd()
	if err != nil {
		projectRoot = "."
	}

	r := &Router{
		mux:           http.NewServeMux(),
		config:        cfg,
		monitor:       monitor,
		wsHub:         wsHub,
		reloadFunc:    reloadFunc,
		updateManager: updates.NewManager(cfg),
		exportLimiter: NewRateLimiter(5, 1*time.Minute), // 5 attempts per minute
		persistence:   config.NewConfigPersistence(cfg.DataPath),
		projectRoot:   projectRoot,
	}

	r.setupRoutes()

	// Start forwarding update progress to WebSocket
	go r.forwardUpdateProgress()

	// Start background update checker
	go r.backgroundUpdateChecker()

	// Load system settings once at startup and cache them
	r.reloadSystemSettings()

	// Get cached values for middleware configuration
	r.settingsMu.RLock()
	allowEmbedding := r.cachedAllowEmbedding
	allowedOrigins := r.cachedAllowedOrigins
	r.settingsMu.RUnlock()

	// Apply middleware chain:
	// 1. Universal rate limiting (outermost to stop attacks early)
	// 2. Demo mode (read-only protection)
	// 3. Error handling
	// 4. Security headers with embedding configuration
	// Note: TimeoutHandler breaks WebSocket upgrades
	handler := SecurityHeadersWithConfig(r, allowEmbedding, allowedOrigins)
	handler = ErrorHandler(handler)
	handler = DemoModeMiddleware(cfg, handler)
	handler = UniversalRateLimitMiddleware(handler)
	r.wrapped = handler
	return r
}

// setupRoutes configures all routes
func (r *Router) setupRoutes() {
	// Create handlers
	r.alertHandlers = NewAlertHandlers(r.monitor, r.wsHub)
	r.notificationHandlers = NewNotificationHandlers(r.monitor)
	guestMetadataHandler := NewGuestMetadataHandler(r.config.DataPath)
	r.configHandlers = NewConfigHandlers(r.config, r.monitor, r.reloadFunc, r.wsHub, guestMetadataHandler, r.reloadSystemSettings)
	updateHandlers := NewUpdateHandlers(r.updateManager, r.config.DataPath)
	r.dockerAgentHandlers = NewDockerAgentHandlers(r.monitor, r.wsHub)

	// API routes
	r.mux.HandleFunc("/api/health", r.handleHealth)
	r.mux.HandleFunc("/api/monitoring/scheduler/health", RequireAuth(r.config, r.handleSchedulerHealth))
	r.mux.HandleFunc("/api/state", r.handleState)
	r.mux.HandleFunc("/api/agents/docker/report", RequireAuth(r.config, r.dockerAgentHandlers.HandleReport))
	r.mux.HandleFunc("/api/agents/docker/commands/", RequireAuth(r.config, r.dockerAgentHandlers.HandleCommandAck))
	r.mux.HandleFunc("/api/agents/docker/hosts/", RequireAdmin(r.config, r.dockerAgentHandlers.HandleDockerHostActions))
	r.mux.HandleFunc("/api/version", r.handleVersion)
	r.mux.HandleFunc("/api/storage/", r.handleStorage)
	r.mux.HandleFunc("/api/storage-charts", r.handleStorageCharts)
	r.mux.HandleFunc("/api/charts", r.handleCharts)
	r.mux.HandleFunc("/api/diagnostics", RequireAuth(r.config, r.handleDiagnostics))
	r.mux.HandleFunc("/api/diagnostics/temperature-proxy/register-nodes", RequireAdmin(r.config, r.handleDiagnosticsRegisterProxyNodes))
	r.mux.HandleFunc("/api/diagnostics/docker/prepare-token", RequireAdmin(r.config, r.handleDiagnosticsDockerPrepareToken))
	r.mux.HandleFunc("/api/install/pulse-sensor-proxy", r.handleDownloadPulseSensorProxy)
	r.mux.HandleFunc("/api/install/install-sensor-proxy.sh", r.handleDownloadInstallerScript)
	r.mux.HandleFunc("/api/install/install-docker.sh", r.handleDownloadDockerInstallerScript)
	r.mux.HandleFunc("/api/config", r.handleConfig)
	r.mux.HandleFunc("/api/backups", r.handleBackups)
	r.mux.HandleFunc("/api/backups/", r.handleBackups)
	r.mux.HandleFunc("/api/backups/unified", r.handleBackups)
	r.mux.HandleFunc("/api/backups/pve", r.handleBackupsPVE)
	r.mux.HandleFunc("/api/backups/pbs", r.handleBackupsPBS)
	r.mux.HandleFunc("/api/snapshots", r.handleSnapshots)

	// Guest metadata routes
	r.mux.HandleFunc("/api/guests/metadata", guestMetadataHandler.HandleGetMetadata)
	r.mux.HandleFunc("/api/guests/metadata/", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			guestMetadataHandler.HandleGetMetadata(w, req)
		case http.MethodPut, http.MethodPost:
			guestMetadataHandler.HandleUpdateMetadata(w, req)
		case http.MethodDelete:
			guestMetadataHandler.HandleDeleteMetadata(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Update routes
	r.mux.HandleFunc("/api/updates/check", updateHandlers.HandleCheckUpdates)
	r.mux.HandleFunc("/api/updates/apply", updateHandlers.HandleApplyUpdate)
	r.mux.HandleFunc("/api/updates/status", updateHandlers.HandleUpdateStatus)
	r.mux.HandleFunc("/api/updates/plan", updateHandlers.HandleGetUpdatePlan)
	r.mux.HandleFunc("/api/updates/history", updateHandlers.HandleListUpdateHistory)
	r.mux.HandleFunc("/api/updates/history/entry", updateHandlers.HandleGetUpdateHistoryEntry)

	// Config management routes
	r.mux.HandleFunc("/api/config/nodes", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			r.configHandlers.HandleGetNodes(w, req)
		case http.MethodPost:
			RequireAdmin(r.configHandlers.config, r.configHandlers.HandleAddNode)(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Test node configuration endpoint (for new nodes)
	r.mux.HandleFunc("/api/config/nodes/test-config", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			r.configHandlers.HandleTestNodeConfig(w, req)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Test connection endpoint
	r.mux.HandleFunc("/api/config/nodes/test-connection", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			r.configHandlers.HandleTestConnection(w, req)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	r.mux.HandleFunc("/api/config/nodes/", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPut:
			RequireAdmin(r.configHandlers.config, r.configHandlers.HandleUpdateNode)(w, req)
		case http.MethodDelete:
			RequireAdmin(r.configHandlers.config, r.configHandlers.HandleDeleteNode)(w, req)
		case http.MethodPost:
			// Handle test endpoint
			if strings.HasSuffix(req.URL.Path, "/test") {
				r.configHandlers.HandleTestNode(w, req)
			} else {
				http.Error(w, "Not found", http.StatusNotFound)
			}
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// System settings routes
	r.mux.HandleFunc("/api/config/system", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			r.configHandlers.HandleGetSystemSettings(w, req)
		case http.MethodPut:
			// DEPRECATED - use /api/system/settings/update instead
			RequireAdmin(r.configHandlers.config, r.configHandlers.HandleUpdateSystemSettingsOLD)(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Mock mode toggle routes
	r.mux.HandleFunc("/api/system/mock-mode", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			r.configHandlers.HandleGetMockMode(w, req)
		case http.MethodPost, http.MethodPut:
			RequireAdmin(r.configHandlers.config, r.configHandlers.HandleUpdateMockMode)(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Registration token routes removed - feature deprecated

	// Security routes
	r.mux.HandleFunc("/api/security/change-password", r.handleChangePassword)
	r.mux.HandleFunc("/api/logout", r.handleLogout)
	r.mux.HandleFunc("/api/login", r.handleLogin)
	r.mux.HandleFunc("/api/security/reset-lockout", r.handleResetLockout)
	r.mux.HandleFunc("/api/security/oidc", RequireAdmin(r.config, r.handleOIDCConfig))
	r.mux.HandleFunc("/api/oidc/login", r.handleOIDCLogin)
	r.mux.HandleFunc(config.DefaultOIDCCallbackPath, r.handleOIDCCallback)
	r.mux.HandleFunc("/api/security/tokens", RequireAdmin(r.config, func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			r.handleListAPITokens(w, req)
		case http.MethodPost:
			r.handleCreateAPIToken(w, req)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	r.mux.HandleFunc("/api/security/tokens/", RequireAdmin(r.config, r.handleDeleteAPIToken))
	r.mux.HandleFunc("/api/security/status", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")

			// Check if auth is globally disabled
			if r.config.DisableAuth {
				// Even with auth disabled, check for OIDC sessions
				oidcCfg := r.ensureOIDCConfig()
				oidcUsername := ""
				if oidcCfg != nil && oidcCfg.Enabled {
					if cookie, err := req.Cookie("pulse_session"); err == nil && cookie.Value != "" {
						if ValidateSession(cookie.Value) {
							oidcUsername = GetSessionUsername(cookie.Value)
						}
					}
				}

				// Even with auth disabled, report API token status for API access
				apiTokenHint := r.config.PrimaryAPITokenHint()

				response := map[string]interface{}{
					"configured":         false,
					"disabled":           true,
					"message":            "Authentication is disabled via DISABLE_AUTH environment variable",
					"apiTokenConfigured": r.config.HasAPITokens(),
					"apiTokenHint":       apiTokenHint,
					"hasAuthentication":  false,
				}

				// Add OIDC info if available
				if oidcCfg != nil {
					response["oidcEnabled"] = oidcCfg.Enabled
					response["oidcIssuer"] = oidcCfg.IssuerURL
					response["oidcClientId"] = oidcCfg.ClientID
					response["oidcUsername"] = oidcUsername
					response["oidcLogoutURL"] = oidcCfg.LogoutURL
					if len(oidcCfg.EnvOverrides) > 0 {
						response["oidcEnvOverrides"] = oidcCfg.EnvOverrides
					}
				}

				json.NewEncoder(w).Encode(response)
				return
			}

			// Check for basic auth configuration
			// Check both environment variables and loaded config
			oidcCfg := r.ensureOIDCConfig()
			hasAuthentication := os.Getenv("PULSE_AUTH_USER") != "" ||
				os.Getenv("REQUIRE_AUTH") == "true" ||
				r.config.AuthUser != "" ||
				r.config.AuthPass != "" ||
				(oidcCfg != nil && oidcCfg.Enabled) ||
				r.config.HasAPITokens() ||
				r.config.ProxyAuthSecret != ""

			// Check if .env file exists but hasn't been loaded yet (pending restart)
			configuredButPendingRestart := false
			envPath := filepath.Join(r.config.ConfigPath, ".env")
			if envPath == "" || r.config.ConfigPath == "" {
				envPath = "/etc/pulse/.env"
			}

			authLastModified := ""
			if stat, err := os.Stat(envPath); err == nil {
				authLastModified = stat.ModTime().UTC().Format(time.RFC3339)
				if !hasAuthentication && r.config.AuthUser == "" && r.config.AuthPass == "" {
					configuredButPendingRestart = true
				}
			}

			// Check for audit logging
			hasAuditLogging := os.Getenv("PULSE_AUDIT_LOG") == "true" || os.Getenv("AUDIT_LOG_ENABLED") == "true"

			// Credentials are always encrypted in current implementation
			credentialsEncrypted := true

			// Check network context
			clientIP := utils.GetClientIP(
				req.RemoteAddr,
				req.Header.Get("X-Forwarded-For"),
				req.Header.Get("X-Real-IP"),
			)
			isPrivateNetwork := utils.IsPrivateIP(clientIP)

			// Get trusted networks from environment
			trustedNetworks := []string{}
			if nets := os.Getenv("PULSE_TRUSTED_NETWORKS"); nets != "" {
				trustedNetworks = strings.Split(nets, ",")
			}
			isTrustedNetwork := utils.IsTrustedNetwork(clientIP, trustedNetworks)

			// Create token hint if token exists
			apiTokenHint := r.config.PrimaryAPITokenHint()

			// Check for proxy auth
			hasProxyAuth := r.config.ProxyAuthSecret != ""
			proxyAuthUsername := ""
			proxyAuthIsAdmin := false
			if hasProxyAuth {
				// Check if current request has valid proxy auth
				if valid, username, isAdmin := CheckProxyAuth(r.config, req); valid {
					proxyAuthUsername = username
					proxyAuthIsAdmin = isAdmin
				}
			}

			// Check for OIDC session
			oidcUsername := ""
			if oidcCfg != nil && oidcCfg.Enabled {
				if cookie, err := req.Cookie("pulse_session"); err == nil && cookie.Value != "" {
					if ValidateSession(cookie.Value) {
						oidcUsername = GetSessionUsername(cookie.Value)
					}
				}
			}

			requiresAuth := r.config.HasAPITokens() ||
				(r.config.AuthUser != "" && r.config.AuthPass != "") ||
				(r.config.OIDC != nil && r.config.OIDC.Enabled) ||
				r.config.ProxyAuthSecret != ""

			status := map[string]interface{}{
				"apiTokenConfigured":          r.config.HasAPITokens(),
				"apiTokenHint":                apiTokenHint,
				"requiresAuth":                requiresAuth,
				"exportProtected":             r.config.HasAPITokens() || os.Getenv("ALLOW_UNPROTECTED_EXPORT") != "true",
				"unprotectedExportAllowed":    os.Getenv("ALLOW_UNPROTECTED_EXPORT") == "true",
				"hasAuthentication":           hasAuthentication,
				"configuredButPendingRestart": configuredButPendingRestart,
				"hasAuditLogging":             hasAuditLogging,
				"credentialsEncrypted":        credentialsEncrypted,
				"hasHTTPS":                    req.TLS != nil,
				"clientIP":                    clientIP,
				"isPrivateNetwork":            isPrivateNetwork,
				"isTrustedNetwork":            isTrustedNetwork,
				"publicAccess":                !isPrivateNetwork,
				"hasProxyAuth":                hasProxyAuth,
				"proxyAuthLogoutURL":          r.config.ProxyAuthLogoutURL,
				"proxyAuthUsername":           proxyAuthUsername,
				"proxyAuthIsAdmin":            proxyAuthIsAdmin,
				"authUsername":                r.config.AuthUser,
				"authLastModified":            authLastModified,
				"oidcUsername":                oidcUsername,
			}

			if oidcCfg != nil {
				status["oidcEnabled"] = oidcCfg.Enabled
				status["oidcIssuer"] = oidcCfg.IssuerURL
				status["oidcClientId"] = oidcCfg.ClientID
				status["oidcLogoutURL"] = oidcCfg.LogoutURL
				if len(oidcCfg.EnvOverrides) > 0 {
					status["oidcEnvOverrides"] = oidcCfg.EnvOverrides
				}
			}

			json.NewEncoder(w).Encode(status)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Quick security setup route - using fixed version
	r.mux.HandleFunc("/api/security/quick-setup", handleQuickSecuritySetupFixed(r))

	// API token regeneration endpoint
	r.mux.HandleFunc("/api/security/regenerate-token", r.HandleRegenerateAPIToken)

	// API token validation endpoint
	r.mux.HandleFunc("/api/security/validate-token", r.HandleValidateAPIToken)

	// Apply security restart endpoint
	r.mux.HandleFunc("/api/security/apply-restart", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			// Only allow restart if we're running under systemd (safer)
			isSystemd := os.Getenv("INVOCATION_ID") != ""

			if !isSystemd {
				response := map[string]interface{}{
					"success": false,
					"message": "Automatic restart is only available when running under systemd. Please restart Pulse manually.",
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(response)
				return
			}

			// Write a recovery flag file before restarting
			recoveryFile := filepath.Join(r.config.DataPath, ".auth_recovery")
			recoveryContent := fmt.Sprintf("Auth setup at %s\nIf locked out, delete this file and restart to disable auth temporarily\n", time.Now().Format(time.RFC3339))
			if err := os.WriteFile(recoveryFile, []byte(recoveryContent), 0600); err != nil {
				log.Warn().Err(err).Str("path", recoveryFile).Msg("Failed to write recovery flag file")
			}

			// Schedule restart with full service restart to pick up new config
			go func() {
				time.Sleep(2 * time.Second)
				log.Info().Msg("Triggering restart to apply security settings")

				// We need to do a full systemctl restart to pick up new environment variables
				// First try daemon-reload
				cmd := exec.Command("sudo", "-n", "systemctl", "daemon-reload")
				if err := cmd.Run(); err != nil {
					log.Error().Err(err).Msg("Failed to reload systemd daemon")
				}

				// Then restart the service - this will kill us and restart with new env
				time.Sleep(500 * time.Millisecond)
				// Try to restart with the detected service name
				serviceName := detectServiceName()
				cmd = exec.Command("sudo", "-n", "systemctl", "restart", serviceName)
				if err := cmd.Run(); err != nil {
					log.Error().Err(err).Str("service", serviceName).Msg("Failed to restart service, falling back to exit")
					// Fallback to exit if restart fails
					os.Exit(0)
				}
				// If restart succeeds, we'll be killed by systemctl
			}()

			response := map[string]interface{}{
				"success": true,
				"message": "Restarting Pulse to apply security settings...",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Initialize recovery token store
	InitRecoveryTokenStore(r.config.DataPath)

	// Recovery endpoint - requires localhost access OR valid recovery token
	r.mux.HandleFunc("/api/security/recovery", func(w http.ResponseWriter, req *http.Request) {
		// Get client IP
		ip := strings.Split(req.RemoteAddr, ":")[0]
		isLocalhost := ip == "127.0.0.1" || ip == "::1" || ip == "localhost"

		// Check for recovery token in header
		recoveryToken := req.Header.Get("X-Recovery-Token")
		hasValidToken := false
		if recoveryToken != "" {
			hasValidToken = GetRecoveryTokenStore().ValidateRecoveryTokenConstantTime(recoveryToken, ip)
		}

		// Only allow from localhost OR with valid recovery token
		if !isLocalhost && !hasValidToken {
			log.Warn().
				Str("ip", ip).
				Bool("has_token", recoveryToken != "").
				Msg("Unauthorized recovery endpoint access attempt")
			http.Error(w, "Recovery endpoint requires localhost access or valid recovery token", http.StatusForbidden)
			return
		}

		if req.Method == http.MethodPost {
			// Parse action
			var recoveryRequest struct {
				Action   string `json:"action"`
				Duration int    `json:"duration,omitempty"` // Duration in minutes for token generation
			}

			if err := json.NewDecoder(req.Body).Decode(&recoveryRequest); err != nil {
				http.Error(w, "Invalid request", http.StatusBadRequest)
				return
			}

			response := map[string]interface{}{}

			switch recoveryRequest.Action {
			case "generate_token":
				// Only allow token generation from localhost
				if !isLocalhost {
					http.Error(w, "Token generation only allowed from localhost", http.StatusForbidden)
					return
				}

				// Default to 15 minutes if not specified
				duration := 15
				if recoveryRequest.Duration > 0 && recoveryRequest.Duration <= 60 {
					duration = recoveryRequest.Duration
				}

				token, err := GetRecoveryTokenStore().GenerateRecoveryToken(time.Duration(duration) * time.Minute)
				if err != nil {
					response["success"] = false
					response["message"] = fmt.Sprintf("Failed to generate recovery token: %v", err)
				} else {
					response["success"] = true
					response["token"] = token
					response["expires_in_minutes"] = duration
					response["message"] = fmt.Sprintf("Recovery token generated. Valid for %d minutes.", duration)
					log.Warn().
						Str("ip", ip).
						Int("duration_minutes", duration).
						Msg("Recovery token generated")
				}

			case "disable_auth":
				// Temporarily disable auth by creating recovery file
				recoveryFile := filepath.Join(r.config.DataPath, ".auth_recovery")
				content := fmt.Sprintf("Recovery mode enabled at %s\nAuth temporarily disabled for local access\nEnabled by: %s\n", time.Now().Format(time.RFC3339), ip)
				if err := os.WriteFile(recoveryFile, []byte(content), 0600); err != nil {
					response["success"] = false
					response["message"] = fmt.Sprintf("Failed to enable recovery mode: %v", err)
				} else {
					response["success"] = true
					response["message"] = "Recovery mode enabled. Auth disabled for localhost. Delete .auth_recovery file to re-enable."
					log.Warn().
						Str("ip", ip).
						Bool("via_token", hasValidToken).
						Msg("AUTH RECOVERY: Authentication disabled via recovery endpoint")
				}

			case "enable_auth":
				// Re-enable auth by removing recovery file
				recoveryFile := filepath.Join(r.config.DataPath, ".auth_recovery")
				if err := os.Remove(recoveryFile); err != nil {
					response["success"] = false
					response["message"] = fmt.Sprintf("Failed to disable recovery mode: %v", err)
				} else {
					response["success"] = true
					response["message"] = "Recovery mode disabled. Authentication re-enabled."
					log.Info().Msg("AUTH RECOVERY: Authentication re-enabled via recovery endpoint")
				}

			default:
				response["success"] = false
				response["message"] = "Invalid action. Use 'disable_auth' or 'enable_auth'"
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		} else if req.Method == http.MethodGet {
			// Check recovery status
			recoveryFile := filepath.Join(r.config.DataPath, ".auth_recovery")
			_, err := os.Stat(recoveryFile)
			response := map[string]interface{}{
				"recovery_mode": err == nil,
				"message":       "Recovery endpoint accessible from localhost only",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Config export/import routes (requires authentication)
	r.mux.HandleFunc("/api/config/export", r.exportLimiter.Middleware(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			// Check proxy auth first
			hasValidProxyAuth := false
			proxyAuthIsAdmin := false
			if r.config.ProxyAuthSecret != "" {
				if valid, _, isAdmin := CheckProxyAuth(r.config, req); valid {
					hasValidProxyAuth = true
					proxyAuthIsAdmin = isAdmin
				}
			}

			// Check authentication - accept proxy auth, session auth or API token
			hasValidSession := false
			if cookie, err := req.Cookie("pulse_session"); err == nil && cookie.Value != "" {
				hasValidSession = ValidateSession(cookie.Value)
			}

			validateAPIToken := func(token string) bool {
				if token == "" || !r.config.HasAPITokens() {
					return false
				}
				_, ok := r.config.ValidateAPIToken(token)
				return ok
			}

			hasValidAPIToken := validateAPIToken(req.Header.Get("X-API-Token"))

			// Check if any valid auth method is present
			hasValidAuth := hasValidProxyAuth || hasValidSession || hasValidAPIToken

			// Determine if auth is required
			authRequired := r.config.AuthUser != "" && r.config.AuthPass != "" ||
				r.config.HasAPITokens() ||
				r.config.ProxyAuthSecret != ""

			// Check admin privileges for proxy auth users
			if hasValidProxyAuth && !proxyAuthIsAdmin {
				log.Warn().
					Str("ip", req.RemoteAddr).
					Str("path", req.URL.Path).
					Msg("Non-admin proxy auth user attempted export/import")
				http.Error(w, "Admin privileges required for export/import", http.StatusForbidden)
				return
			}

			if authRequired && !hasValidAuth {
				log.Warn().
					Str("ip", req.RemoteAddr).
					Str("path", req.URL.Path).
					Bool("proxyAuth", hasValidProxyAuth).
					Bool("session", hasValidSession).
					Bool("apiToken", hasValidAPIToken).
					Msg("Unauthorized export attempt")
				http.Error(w, "Unauthorized - please log in or provide API token", http.StatusUnauthorized)
				return
			} else if !authRequired {
				// No auth configured - check if this is a homelab/private network
				clientIP := utils.GetClientIP(req.RemoteAddr,
					req.Header.Get("X-Forwarded-For"),
					req.Header.Get("X-Real-IP"))

				isPrivate := utils.IsPrivateIP(clientIP)
				allowUnprotected := os.Getenv("ALLOW_UNPROTECTED_EXPORT") == "true"

				if !isPrivate && !allowUnprotected {
					// Public network access without auth - definitely block
					log.Warn().
						Str("ip", req.RemoteAddr).
						Bool("private_network", isPrivate).
						Msg("Export blocked - public network requires authentication")
					http.Error(w, "Export requires authentication on public networks", http.StatusForbidden)
					return
				} else if isPrivate && !allowUnprotected {
					// Private network but ALLOW_UNPROTECTED_EXPORT not set - show helpful message
					log.Info().
						Str("ip", req.RemoteAddr).
						Msg("Export allowed - private network with no auth")
					// Continue - allow export on private networks for homelab users
				}
			}

			// Log successful export attempt
			log.Info().
				Str("ip", req.RemoteAddr).
				Bool("proxy_auth", hasValidProxyAuth).
				Bool("session_auth", hasValidSession).
				Bool("api_token_auth", hasValidAPIToken).
				Msg("Configuration export initiated")

			r.configHandlers.HandleExportConfig(w, req)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	r.mux.HandleFunc("/api/config/import", r.exportLimiter.Middleware(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			// Check proxy auth first
			hasValidProxyAuth := false
			proxyAuthIsAdmin := false
			if r.config.ProxyAuthSecret != "" {
				if valid, _, isAdmin := CheckProxyAuth(r.config, req); valid {
					hasValidProxyAuth = true
					proxyAuthIsAdmin = isAdmin
				}
			}

			// Check authentication - accept proxy auth, session auth or API token
			hasValidSession := false
			if cookie, err := req.Cookie("pulse_session"); err == nil && cookie.Value != "" {
				hasValidSession = ValidateSession(cookie.Value)
			}

			validateAPIToken := func(token string) bool {
				if token == "" || !r.config.HasAPITokens() {
					return false
				}
				_, ok := r.config.ValidateAPIToken(token)
				return ok
			}

			hasValidAPIToken := validateAPIToken(req.Header.Get("X-API-Token"))

			// Check if any valid auth method is present
			hasValidAuth := hasValidProxyAuth || hasValidSession || hasValidAPIToken

			// Determine if auth is required
			authRequired := r.config.AuthUser != "" && r.config.AuthPass != "" ||
				r.config.HasAPITokens() ||
				r.config.ProxyAuthSecret != ""

			// Check admin privileges for proxy auth users
			if hasValidProxyAuth && !proxyAuthIsAdmin {
				log.Warn().
					Str("ip", req.RemoteAddr).
					Str("path", req.URL.Path).
					Msg("Non-admin proxy auth user attempted export/import")
				http.Error(w, "Admin privileges required for export/import", http.StatusForbidden)
				return
			}

			if authRequired && !hasValidAuth {
				log.Warn().
					Str("ip", req.RemoteAddr).
					Str("path", req.URL.Path).
					Bool("proxyAuth", hasValidProxyAuth).
					Bool("session", hasValidSession).
					Bool("apiToken", hasValidAPIToken).
					Msg("Unauthorized import attempt")
				http.Error(w, "Unauthorized - please log in or provide API token", http.StatusUnauthorized)
				return
			} else if !authRequired {
				// No auth configured - check if this is a homelab/private network
				clientIP := utils.GetClientIP(req.RemoteAddr,
					req.Header.Get("X-Forwarded-For"),
					req.Header.Get("X-Real-IP"))

				isPrivate := utils.IsPrivateIP(clientIP)
				allowUnprotected := os.Getenv("ALLOW_UNPROTECTED_EXPORT") == "true"

				if !isPrivate && !allowUnprotected {
					// Public network access without auth - definitely block
					log.Warn().
						Str("ip", req.RemoteAddr).
						Bool("private_network", isPrivate).
						Msg("Import blocked - public network requires authentication")
					http.Error(w, "Import requires authentication on public networks", http.StatusForbidden)
					return
				} else if isPrivate && !allowUnprotected {
					// Private network but ALLOW_UNPROTECTED_EXPORT not set - show helpful message
					log.Info().
						Str("ip", req.RemoteAddr).
						Msg("Import allowed - private network with no auth")
					// Continue - allow import on private networks for homelab users
				}
			}

			// Log successful import attempt
			log.Info().
				Str("ip", req.RemoteAddr).
				Bool("session_auth", hasValidSession).
				Bool("api_token_auth", hasValidAPIToken).
				Msg("Configuration import initiated")

			r.configHandlers.HandleImportConfig(w, req)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// Discovery route

	// Setup script route
	r.mux.HandleFunc("/api/setup-script", r.configHandlers.HandleSetupScript)

	// Generate setup script URL with temporary token (for authenticated users)
	r.mux.HandleFunc("/api/setup-script-url", r.configHandlers.HandleSetupScriptURL)

	// Auto-register route for setup scripts
	r.mux.HandleFunc("/api/auto-register", r.configHandlers.HandleAutoRegister)
	// Discovery endpoint
	r.mux.HandleFunc("/api/discover", RequireAuth(r.config, r.configHandlers.HandleDiscoverServers))

	// Test endpoint for WebSocket notifications
	r.mux.HandleFunc("/api/test-notification", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Send a test auto-registration notification
		r.wsHub.BroadcastMessage(websocket.Message{
			Type: "node_auto_registered",
			Data: map[string]interface{}{
				"type":     "pve",
				"host":     "test-node.example.com",
				"name":     "Test Node",
				"tokenId":  "test-token",
				"hasToken": true,
			},
			Timestamp: time.Now().Format(time.RFC3339),
		})

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "notification sent"})
	})

	// Alert routes
	r.mux.HandleFunc("/api/alerts/", r.alertHandlers.HandleAlerts)

	// Notification routes
	r.mux.HandleFunc("/api/notifications/", r.notificationHandlers.HandleNotifications)

	// Settings routes
	r.mux.HandleFunc("/api/settings", getSettings)
	r.mux.HandleFunc("/api/settings/update", updateSettings)

	// System settings and API token management
	r.systemSettingsHandler = NewSystemSettingsHandler(r.config, r.persistence, r.wsHub, r.monitor, r.reloadSystemSettings)
	r.mux.HandleFunc("/api/system/settings", r.systemSettingsHandler.HandleGetSystemSettings)
	r.mux.HandleFunc("/api/system/settings/update", r.systemSettingsHandler.HandleUpdateSystemSettings)
	r.mux.HandleFunc("/api/system/ssh-config", r.handleSSHConfig)
	r.mux.HandleFunc("/api/system/verify-temperature-ssh", r.handleVerifyTemperatureSSH)
	r.mux.HandleFunc("/api/system/proxy-public-key", r.handleProxyPublicKey)
	// Old API token endpoints removed - now using /api/security/regenerate-token

	// Docker agent download endpoints
	r.mux.HandleFunc("/install-docker-agent.sh", r.handleDownloadInstallScript)
	r.mux.HandleFunc("/download/pulse-docker-agent", r.handleDownloadAgent)
	r.mux.HandleFunc("/api/agent/version", r.handleAgentVersion)
	r.mux.HandleFunc("/api/server/info", r.handleServerInfo)

	// WebSocket endpoint
	r.mux.HandleFunc("/ws", r.handleWebSocket)

	// Socket.io compatibility endpoints
	r.mux.HandleFunc("/socket.io/", r.handleSocketIO)

	// Simple stats page
	r.mux.HandleFunc("/simple-stats", r.handleSimpleStats)

	// Note: Frontend handler is handled manually in ServeHTTP to prevent redirect issues
	// See issue #334 - ServeMux redirects empty path to "./" which breaks reverse proxies

}

func (r *Router) handleVerifyTemperatureSSH(w http.ResponseWriter, req *http.Request) {
	if r.configHandlers == nil {
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	if token := extractSetupToken(req); token != "" {
		if r.configHandlers.ValidateSetupToken(token) {
			r.configHandlers.HandleVerifyTemperatureSSH(w, req)
			return
		}
	}

	if CheckAuth(r.config, w, req) {
		r.configHandlers.HandleVerifyTemperatureSSH(w, req)
		return
	}

	log.Warn().
		Str("ip", req.RemoteAddr).
		Str("path", req.URL.Path).
		Str("method", req.Method).
		Msg("Unauthorized access attempt (verify-temperature-ssh)")

	if strings.HasPrefix(req.URL.Path, "/api/") || strings.Contains(req.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Authentication required"}`))
	} else {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

// handleSSHConfig handles SSH config writes with setup token or API auth
func (r *Router) handleSSHConfig(w http.ResponseWriter, req *http.Request) {
	if r.systemSettingsHandler == nil {
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Check setup token first (for setup scripts)
	if token := extractSetupToken(req); token != "" {
		if r.configHandlers != nil && r.configHandlers.ValidateSetupToken(token) {
			r.systemSettingsHandler.HandleSSHConfig(w, req)
			return
		}
	}

	// Fall back to standard API authentication
	if CheckAuth(r.config, w, req) {
		r.systemSettingsHandler.HandleSSHConfig(w, req)
		return
	}

	log.Warn().
		Str("ip", req.RemoteAddr).
		Str("path", req.URL.Path).
		Str("method", req.Method).
		Msg("Unauthorized access attempt (ssh-config)")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"Authentication required"}`))
}

// handleProxyPublicKey returns the temperature proxy's public SSH key (public endpoint)
func (r *Router) handleProxyPublicKey(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Try to get the proxy's public key
	proxyClient := tempproxy.NewClient()
	if !proxyClient.IsAvailable() {
		// Proxy not available - return empty response
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(""))
		return
	}

	// Get proxy status which includes the public key
	status, err := proxyClient.GetStatus()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get proxy status")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(""))
		return
	}

	// Extract public key
	publicKey, ok := status["public_key"].(string)
	if !ok || publicKey == "" {
		log.Warn().Msg("Public key not found in proxy status")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(""))
		return
	}

	// Return the public key as plain text
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(publicKey))
}

func extractSetupToken(req *http.Request) string {
	if token := strings.TrimSpace(req.Header.Get("X-Setup-Token")); token != "" {
		return token
	}
	if token := extractBearerToken(req.Header.Get("Authorization")); token != "" {
		return token
	}
	if token := strings.TrimSpace(req.URL.Query().Get("auth_token")); token != "" {
		return token
	}
	return ""
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}

	trimmed := strings.TrimSpace(header)
	if len(trimmed) < 7 {
		return ""
	}

	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		return strings.TrimSpace(trimmed[7:])
	}

	return ""
}

// Handler returns the router wrapped with middleware.
func (r *Router) Handler() http.Handler {
	if r.wrapped != nil {
		return r.wrapped
	}
	return r
}

// SetMonitor updates the router and associated handlers with a new monitor instance.
func (r *Router) SetMonitor(m *monitoring.Monitor) {
	r.monitor = m
	if r.alertHandlers != nil {
		r.alertHandlers.SetMonitor(m)
	}
	if r.configHandlers != nil {
		r.configHandlers.SetMonitor(m)
	}
	if r.notificationHandlers != nil {
		r.notificationHandlers.SetMonitor(m)
	}
	if r.dockerAgentHandlers != nil {
		r.dockerAgentHandlers.SetMonitor(m)
	}
	if r.systemSettingsHandler != nil {
		r.systemSettingsHandler.SetMonitor(m)
	}
	if m != nil {
		if url := strings.TrimSpace(r.config.PublicURL); url != "" {
			if mgr := m.GetNotificationManager(); mgr != nil {
				mgr.SetPublicURL(url)
			}
		}
	}
}

// SetConfig refreshes the configuration reference used by the router and dependent handlers.
func (r *Router) SetConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}

	if r.config == nil {
		r.config = cfg
	} else {
		*r.config = *cfg
	}

	if r.configHandlers != nil {
		r.configHandlers.SetConfig(r.config)
	}
	if r.systemSettingsHandler != nil {
		r.systemSettingsHandler.SetConfig(r.config)
	}
}

// reloadSystemSettings loads system settings from disk and caches them
func (r *Router) reloadSystemSettings() {
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()

	// Load from disk
	if systemSettings, err := r.persistence.LoadSystemSettings(); err == nil && systemSettings != nil {
		r.cachedAllowEmbedding = systemSettings.AllowEmbedding
		r.cachedAllowedOrigins = systemSettings.AllowedEmbedOrigins
	} else {
		// On error, use safe defaults
		r.cachedAllowEmbedding = false
		r.cachedAllowedOrigins = ""
	}
}

// ServeHTTP implements http.Handler
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Prevent path traversal attacks by cleaning the path
	cleanPath := filepath.Clean(req.URL.Path)
	// Reject requests with path traversal attempts
	if strings.Contains(req.URL.Path, "..") || cleanPath != req.URL.Path {
		// Return 401 for API paths to match expected test behavior
		if strings.HasPrefix(req.URL.Path, "/api/") {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		} else {
			http.Error(w, "Invalid path", http.StatusBadRequest)
		}
		log.Warn().
			Str("ip", req.RemoteAddr).
			Str("path", req.URL.Path).
			Str("clean_path", cleanPath).
			Msg("Path traversal attempt blocked")
		return
	}

	// Get cached system settings (loaded once at startup, not from disk every request)
	r.capturePublicURLFromRequest(req)
	r.settingsMu.RLock()
	allowEmbedding := r.cachedAllowEmbedding
	allowedEmbedOrigins := r.cachedAllowedOrigins
	r.settingsMu.RUnlock()

	// Apply security headers with embedding configuration
	SecurityHeadersWithConfig(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Add CORS headers if configured
		if r.config.AllowedOrigins != "" {
			w.Header().Set("Access-Control-Allow-Origin", r.config.AllowedOrigins)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Token, X-CSRF-Token")
		}

		// Handle preflight requests
		if req.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Check if we need authentication
		needsAuth := true

		// Check if auth is globally disabled
		// BUT still check for API tokens if provided (for API access when auth is disabled)
		if r.config.DisableAuth {
			// Check if an API token was provided
			providedToken := req.Header.Get("X-API-Token")
			if providedToken == "" {
				providedToken = req.URL.Query().Get("token")
			}

			// If a valid API token is provided, allow access even with DisableAuth
			if providedToken != "" && r.config.HasAPITokens() {
				if _, ok := r.config.ValidateAPIToken(providedToken); ok {
					needsAuth = false
					w.Header().Set("X-Auth-Method", "api-token")
				} else {
					http.Error(w, "Invalid API token", http.StatusUnauthorized)
					return
				}
			} else {
				// No API token provided with DisableAuth - allow open access
				needsAuth = false
				w.Header().Set("X-Auth-Disabled", "true")
			}
		}

		// Recovery mechanism: Check if recovery mode is enabled
		recoveryFile := filepath.Join(r.config.DataPath, ".auth_recovery")
		if _, err := os.Stat(recoveryFile); err == nil {
			// Recovery mode is enabled - allow local access only
			ip := strings.Split(req.RemoteAddr, ":")[0]
			log.Debug().
				Str("recovery_file", recoveryFile).
				Str("remote_ip", ip).
				Str("path", req.URL.Path).
				Bool("file_exists", err == nil).
				Msg("Checking auth recovery mode")
			if ip == "127.0.0.1" || ip == "::1" || ip == "localhost" {
				log.Warn().
					Str("recovery_file", recoveryFile).
					Msg("AUTH RECOVERY MODE: Allowing local access without authentication")
				// Allow access but add a warning header
				w.Header().Set("X-Auth-Recovery", "true")
				// Recovery mode bypasses auth for localhost
				needsAuth = false
			}
		}

		if needsAuth {
			// Normal authentication check
			// Normalize path to handle double slashes (e.g., //download -> /download)
			// This prevents auth bypass failures when URLs have trailing slashes
			normalizedPath := path.Clean(req.URL.Path)

			// Skip auth for certain public endpoints and static assets
			publicPaths := []string{
				"/api/health",
				"/api/security/status",
				"/api/version",
				"/api/login", // Add login endpoint as public
				"/api/oidc/login",
				config.DefaultOIDCCallbackPath,
				"/install-docker-agent.sh",             // Docker agent bootstrap script must be public
				"/download/pulse-docker-agent",         // Agent binary download should not require auth
				"/api/agent/version",                   // Agent update checks need to work before auth
				"/api/server/info",                     // Server info for installer script
				"/api/install/install-sensor-proxy.sh", // Temperature proxy installer fallback
				"/api/install/pulse-sensor-proxy",      // Temperature proxy binary fallback
				"/api/install/install-docker.sh",       // Docker turnkey installer
				"/api/system/proxy-public-key",         // Temperature proxy public key for setup script
			}

			// Also allow static assets without auth (JS, CSS, etc)
			// These MUST be accessible for the login page to work
			isStaticAsset := strings.HasPrefix(req.URL.Path, "/assets/") ||
				strings.HasPrefix(req.URL.Path, "/@vite/") ||
				strings.HasPrefix(req.URL.Path, "/@solid-refresh") ||
				strings.HasPrefix(req.URL.Path, "/src/") ||
				strings.HasPrefix(req.URL.Path, "/node_modules/") ||
				req.URL.Path == "/" ||
				req.URL.Path == "/index.html" ||
				req.URL.Path == "/favicon.ico" ||
				req.URL.Path == "/logo.svg" ||
				strings.HasSuffix(req.URL.Path, ".js") ||
				strings.HasSuffix(req.URL.Path, ".css") ||
				strings.HasSuffix(req.URL.Path, ".map") ||
				strings.HasSuffix(req.URL.Path, ".ts") ||
				strings.HasSuffix(req.URL.Path, ".tsx") ||
				strings.HasSuffix(req.URL.Path, ".mjs") ||
				strings.HasSuffix(req.URL.Path, ".jsx")

			isPublic := isStaticAsset
			for _, path := range publicPaths {
				if normalizedPath == path {
					isPublic = true
					break
				}
			}

			// Special case: setup-script should be public (uses setup codes for auth)
			if normalizedPath == "/api/setup-script" {
				// The script itself prompts for a setup code
				isPublic = true
			}

			// Allow temperature verification endpoint when a setup token is provided
			if normalizedPath == "/api/system/verify-temperature-ssh" && r.configHandlers != nil {
				if token := extractSetupToken(req); token != "" && r.configHandlers.ValidateSetupToken(token) {
					isPublic = true
				}
			}

			// Allow SSH config endpoint when a setup token is provided
			if normalizedPath == "/api/system/ssh-config" && r.configHandlers != nil {
				if token := extractSetupToken(req); token != "" && r.configHandlers.ValidateSetupToken(token) {
					isPublic = true
				}
			}

			// Auto-register endpoint needs to be public (validates tokens internally)
			// BUT the tokens must be generated by authenticated users via setup-script-url
			if normalizedPath == "/api/auto-register" {
				isPublic = true
			}

			// Special case: quick-setup should be accessible to check if already configured
			// The handler itself will verify if setup should be skipped
			if normalizedPath == "/api/security/quick-setup" && req.Method == http.MethodPost {
				isPublic = true
			}
			// Dev mode bypass for admin endpoints (disabled by default)
			if os.Getenv("ALLOW_ADMIN_BYPASS") == "1" {
				log.Info().
					Str("path", req.URL.Path).
					Msg("=== ADMIN BYPASS ENABLED - SKIPPING GLOBAL AUTH ===")
				needsAuth = false
			}

			// Check auth for protected routes (only if auth is needed)
			if needsAuth && !isPublic && !CheckAuth(r.config, w, req) {
				// Never send WWW-Authenticate - use custom login page
				// For API requests, return JSON
				if strings.HasPrefix(req.URL.Path, "/api/") || strings.Contains(req.Header.Get("Accept"), "application/json") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte(`{"error":"Authentication required"}`))
				} else {
					http.Error(w, "Authentication required", http.StatusUnauthorized)
				}
				log.Warn().
					Str("ip", req.RemoteAddr).
					Str("path", req.URL.Path).
					Msg("Unauthorized access attempt")
				return
			}
		}
		// Check CSRF for state-changing requests
		// CSRF is only needed when using session-based auth
		// Only skip CSRF for initial setup when no auth is configured
		skipCSRF := false
		if (req.URL.Path == "/api/security/quick-setup" || req.URL.Path == "/api/security/apply-restart") &&
			r.config.AuthUser == "" && r.config.AuthPass == "" {
			// Only skip CSRF for initial setup and restart when no auth exists
			skipCSRF = true
		}
		// Skip CSRF for setup-script-url endpoint (generates temporary tokens, not a state change)
		if req.URL.Path == "/api/setup-script-url" {
			skipCSRF = true
		}
		if strings.HasPrefix(req.URL.Path, "/api/") && !skipCSRF && !CheckCSRF(w, req) {
			http.Error(w, "CSRF token validation failed", http.StatusForbidden)
			LogAuditEvent("csrf_failure", "", GetClientIP(req), req.URL.Path, false, "Invalid CSRF token")
			return
		}

		// Rate limiting is now handled by UniversalRateLimitMiddleware
		// No need for duplicate rate limiting logic here

		// Log request
		start := time.Now()

		// Fix for issue #334: Custom routing to prevent ServeMux's "./" redirect
		// When accessing without trailing slash, ServeMux redirects to "./" which is wrong
		// We handle routing manually to avoid this issue

		// Check if this is an API or WebSocket route
		if strings.HasPrefix(req.URL.Path, "/api/") ||
			strings.HasPrefix(req.URL.Path, "/ws") ||
			strings.HasPrefix(req.URL.Path, "/socket.io/") ||
			strings.HasPrefix(req.URL.Path, "/download/") ||
			req.URL.Path == "/simple-stats" ||
			req.URL.Path == "/install-docker-agent.sh" {
			// Use the mux for API and special routes
			r.mux.ServeHTTP(w, req)
		} else {
			// Serve frontend for all other paths (including root)
			handler := serveFrontendHandler()
			handler(w, req)
		}

		log.Debug().
			Str("method", req.Method).
			Str("path", req.URL.Path).
			Dur("duration", time.Since(start)).
			Msg("Request handled")
	}), allowEmbedding, allowedEmbedOrigins).ServeHTTP(w, req)
}

func (r *Router) capturePublicURLFromRequest(req *http.Request) {
	if req == nil || r == nil || r.config == nil {
		return
	}

	if r.config.EnvOverrides != nil && r.config.EnvOverrides["publicURL"] {
		return
	}

	rawHost := firstForwardedValue(req.Header.Get("X-Forwarded-Host"))
	if rawHost == "" {
		rawHost = req.Host
	}
	hostWithPort, hostOnly := sanitizeForwardedHost(rawHost)
	if hostWithPort == "" {
		return
	}
	if isLoopbackHost(hostOnly) {
		return
	}

	rawProto := firstForwardedValue(req.Header.Get("X-Forwarded-Proto"))
	if rawProto == "" {
		rawProto = firstForwardedValue(req.Header.Get("X-Forwarded-Scheme"))
	}
	scheme := strings.ToLower(strings.TrimSpace(rawProto))
	switch scheme {
	case "https", "http":
		// supported values
	default:
		if req.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	if scheme == "" {
		scheme = "http"
	}

	if _, _, err := net.SplitHostPort(hostWithPort); err != nil {
		if forwardedPort := firstForwardedValue(req.Header.Get("X-Forwarded-Port")); forwardedPort != "" {
			if shouldAppendForwardedPort(forwardedPort, scheme) {
				if strings.Contains(hostWithPort, ":") && !strings.HasPrefix(hostWithPort, "[") {
					hostWithPort = fmt.Sprintf("[%s]", hostWithPort)
				} else if strings.HasPrefix(hostWithPort, "[") && !strings.Contains(hostWithPort, "]") {
					hostWithPort = fmt.Sprintf("[%s]", strings.TrimPrefix(hostWithPort, "["))
				}
				hostWithPort = fmt.Sprintf("%s:%s", hostWithPort, forwardedPort)
			}
		}
	}

	candidate := fmt.Sprintf("%s://%s", scheme, hostWithPort)
	normalizedCandidate := strings.TrimRight(strings.TrimSpace(candidate), "/")

	r.publicURLMu.Lock()
	if r.publicURLDetected {
		r.publicURLMu.Unlock()
		return
	}

	current := strings.TrimRight(strings.TrimSpace(r.config.PublicURL), "/")
	if current != "" && current == normalizedCandidate {
		r.publicURLDetected = true
		r.publicURLMu.Unlock()
		return
	}

	r.config.PublicURL = normalizedCandidate
	r.publicURLDetected = true
	r.publicURLMu.Unlock()

	log.Info().
		Str("publicURL", normalizedCandidate).
		Msg("Detected public URL from inbound request; using for notifications")

	if r.monitor != nil {
		if mgr := r.monitor.GetNotificationManager(); mgr != nil {
			mgr.SetPublicURL(normalizedCandidate)
		}
	}
}

func firstForwardedValue(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.Split(header, ",")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func sanitizeForwardedHost(raw string) (string, string) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", ""
	}

	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimSpace(strings.TrimSuffix(host, "/"))
	if host == "" {
		return "", ""
	}

	hostOnly := host
	if h, _, err := net.SplitHostPort(hostOnly); err == nil {
		hostOnly = h
	}
	hostOnly = strings.Trim(hostOnly, "[]")

	return host, hostOnly
}

func isLoopbackHost(host string) bool {
	if host == "" {
		return true
	}
	lower := strings.ToLower(host)
	if lower == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}

func shouldAppendForwardedPort(port, scheme string) bool {
	if port == "" {
		return false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return false
	}
	if scheme == "https" && port == "443" {
		return false
	}
	if scheme == "http" && port == "80" {
		return false
	}
	return true
}

// detectLegacySSH checks if Pulse is using legacy SSH for temperature monitoring
//
// ⚠️ MIGRATION SCAFFOLDING - TEMPORARY CODE
// This detection exists only to handle migration from legacy SSH-in-container
// to the secure pulse-sensor-proxy architecture introduced in v4.23.0.
//
// REMOVAL CRITERIA: Remove after v5.0 or when banner telemetry shows <1% fire rate
// for 30+ days. This code serves no functional purpose beyond migration assistance.
//
// Can be disabled via environment variable: PULSE_LEGACY_DETECTION=false
func (r *Router) detectLegacySSH() (legacyDetected, recommendProxy bool) {
	// Check if detection is disabled via environment variable
	if os.Getenv("PULSE_LEGACY_DETECTION") == "false" {
		return false, false
	}

	// Check if running in a container using multiple detection methods
	inContainer := false

	// Method 1: Check for /.dockerenv (Docker)
	if _, err := os.Stat("/.dockerenv"); err == nil {
		inContainer = true
	}

	// Method 2: Check /proc/1/cgroup (Docker/LXC)
	if !inContainer {
		if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
			cgroupStr := string(data)
			if strings.Contains(cgroupStr, "docker") ||
				strings.Contains(cgroupStr, "lxc") ||
				strings.Contains(cgroupStr, "/docker/") ||
				strings.Contains(cgroupStr, "/lxc/") {
				inContainer = true
			}
		}
	}

	// Method 3: Check /run/systemd/container (systemd containers)
	if !inContainer {
		if _, err := os.Stat("/run/systemd/container"); err == nil {
			inContainer = true
		}
	}

	// Method 4: Check /proc/1/environ for container indicators
	if !inContainer {
		if data, err := os.ReadFile("/proc/1/environ"); err == nil {
			environStr := string(data)
			if strings.Contains(environStr, "container=") ||
				strings.Contains(environStr, "DOCKER") ||
				strings.Contains(environStr, "LXC") {
				inContainer = true
			}
		}
	}

	// If not in container, no need for proxy
	if !inContainer {
		return false, false
	}

	// Check if SSH keys are configured in the data directory
	sshKeysConfigured := false

	// Check for SSH keys in the configured data directory
	dataDir := r.config.DataPath
	if dataDir == "" {
		dataDir = "/etc/pulse"
	}

	sshPrivKeyPath := filepath.Join(dataDir, ".ssh", "id_ed25519")
	if _, err := os.Stat(sshPrivKeyPath); err == nil {
		sshKeysConfigured = true
	}

	// Also check for RSA keys
	sshPrivKeyPathRSA := filepath.Join(dataDir, ".ssh", "id_rsa")
	if _, err := os.Stat(sshPrivKeyPathRSA); err == nil {
		sshKeysConfigured = true
	}

	// If SSH keys exist, check if proxy is configured vs just temporarily down
	if sshKeysConfigured {
		// Check if pulse-sensor-proxy is available via unix socket
		proxySocket := "/run/pulse-sensor-proxy/sensor.sock"
		proxyBinary := "/usr/local/bin/pulse-sensor-proxy"

		// If socket doesn't exist, need to distinguish:
		// - Legacy setup: proxy never installed (binary doesn't exist)
		// - Migrated setup: proxy installed but temporarily down
		if _, err := os.Stat(proxySocket); err != nil {
			// Socket missing - check if proxy was ever installed
			if _, err := os.Stat(proxyBinary); err != nil {
				// Proxy binary doesn't exist - this is legacy SSH setup
				// User needs to remove nodes and re-add them
				return true, true
			}
			// Proxy binary exists but socket is missing - likely just restarting/down
			// Don't show banner - this is a transient issue, not a configuration problem
			return false, false
		}
	}

	return false, false
}

// handleHealth handles health check requests
func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Detect legacy SSH setup
	legacySSH, recommendProxy := r.detectLegacySSH()

	// Log when legacy SSH is detected for telemetry/metrics
	// This helps track removal criteria: <1% detection rate for 30+ days
	if legacySSH && recommendProxy {
		log.Warn().
			Str("detection_type", "legacy_ssh_migration").
			Msg("Legacy SSH configuration detected - user should migrate to proxy architecture")
	}

	// Check for dev mode SSH override (FOR TESTING ONLY - NEVER in production)
	devModeSSH := os.Getenv("PULSE_DEV_ALLOW_CONTAINER_SSH") == "true"

	response := HealthResponse{
		Status:                      "healthy",
		Timestamp:                   time.Now().Unix(),
		Uptime:                      time.Since(r.monitor.GetStartTime()).Seconds(),
		LegacySSHDetected:           legacySSH,
		RecommendProxyUpgrade:       recommendProxy,
		ProxyInstallScriptAvailable: true, // Install script is always available
		DevModeSSH:                  devModeSSH,
	}

	if err := utils.WriteJSONResponse(w, response); err != nil {
		log.Error().Err(err).Msg("Failed to write health response")
	}
}

// handleSchedulerHealth returns scheduler health status for adaptive polling
func (r *Router) handleSchedulerHealth(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.monitor == nil {
		http.Error(w, "Monitor not available", http.StatusServiceUnavailable)
		return
	}

	health := r.monitor.SchedulerHealth()
	if err := utils.WriteJSONResponse(w, health); err != nil {
		log.Error().Err(err).Msg("Failed to write scheduler health response")
	}
}

// handleChangePassword handles password change requests
func (r *Router) handleChangePassword(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only POST method is allowed", nil)
		return
	}

	// Check if using proxy auth and if so, verify admin status
	if r.config.ProxyAuthSecret != "" {
		if valid, username, isAdmin := CheckProxyAuth(r.config, req); valid {
			if !isAdmin {
				// User is authenticated but not an admin
				log.Warn().
					Str("ip", req.RemoteAddr).
					Str("path", req.URL.Path).
					Str("method", req.Method).
					Str("username", username).
					Msg("Non-admin user attempted to change password")

				// Return forbidden error
				writeErrorResponse(w, http.StatusForbidden, "forbidden",
					"Admin privileges required", nil)
				return
			}
		}
	}

	// Parse request
	var changeReq struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}

	if err := json.NewDecoder(req.Body).Decode(&changeReq); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_request",
			"Invalid request body", nil)
		return
	}

	// Validate new password complexity
	if err := auth.ValidatePasswordComplexity(changeReq.NewPassword); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_password",
			err.Error(), nil)
		return
	}

	// Verify current password matches
	// When behind a proxy with Basic Auth, the proxy may overwrite the Authorization header
	// So we verify the current password from the JSON body instead

	// First, validate that currentPassword was provided
	if changeReq.CurrentPassword == "" {
		writeErrorResponse(w, http.StatusUnauthorized, "unauthorized",
			"Current password required", nil)
		return
	}

	// Check if we should use Basic Auth header or JSON body for verification
	// If there's an Authorization header AND it's not from a proxy, use it
	authHeader := req.Header.Get("Authorization")
	useAuthHeader := false
	username := r.config.AuthUser // Default to configured username

	if authHeader != "" {
		const basicPrefix = "Basic "
		if strings.HasPrefix(authHeader, basicPrefix) {
			decoded, err := base64.StdEncoding.DecodeString(authHeader[len(basicPrefix):])
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					// Check if this looks like Pulse credentials (matching username)
					if parts[0] == r.config.AuthUser {
						// This is likely from Pulse's own auth, not a proxy
						username = parts[0]
						useAuthHeader = true
						// Verify the password from the header matches
						if !auth.CheckPasswordHash(parts[1], r.config.AuthPass) {
							log.Warn().
								Str("ip", req.RemoteAddr).
								Str("username", username).
								Msg("Failed password change attempt - incorrect current password in auth header")
							writeErrorResponse(w, http.StatusUnauthorized, "unauthorized",
								"Current password is incorrect", nil)
							return
						}
					}
					// If username doesn't match, this is likely proxy auth - ignore it
				}
			}
		}
	}

	// If we didn't use the auth header, or need to double-check, verify from JSON body
	if !useAuthHeader || changeReq.CurrentPassword != "" {
		// Verify current password from JSON body
		if !auth.CheckPasswordHash(changeReq.CurrentPassword, r.config.AuthPass) {
			log.Warn().
				Str("ip", req.RemoteAddr).
				Str("username", username).
				Msg("Failed password change attempt - incorrect current password")
			writeErrorResponse(w, http.StatusUnauthorized, "unauthorized",
				"Current password is incorrect", nil)
			return
		}
	}

	// Hash the new password before storing
	hashedPassword, err := auth.HashPassword(changeReq.NewPassword)
	if err != nil {
		log.Error().Err(err).Msg("Failed to hash new password")
		writeErrorResponse(w, http.StatusInternalServerError, "hash_error",
			"Failed to process new password", nil)
		return
	}

	// Check if we're running in Docker
	isDocker := os.Getenv("PULSE_DOCKER") == "true"

	if isDocker {
		// For Docker, update the .env file in the data directory
		envPath := filepath.Join(r.config.ConfigPath, ".env")

		// Read existing .env file to preserve other settings
		envContent := ""
		existingContent, err := os.ReadFile(envPath)
		if err == nil {
			// Parse existing content and update password
			scanner := bufio.NewScanner(strings.NewReader(string(existingContent)))
			for scanner.Scan() {
				line := scanner.Text()
				// Skip empty lines and comments
				if line == "" || strings.HasPrefix(line, "#") {
					envContent += line + "\n"
					continue
				}
				// Update password line, keep others
				if strings.HasPrefix(line, "PULSE_AUTH_PASS=") {
					envContent += fmt.Sprintf("PULSE_AUTH_PASS='%s'\n", hashedPassword)
				} else {
					envContent += line + "\n"
				}
			}
		} else {
			// Create new .env file if it doesn't exist
			envContent = fmt.Sprintf(`# Auto-generated by Pulse password change
# Generated on %s
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
`, time.Now().Format(time.RFC3339), r.config.AuthUser, hashedPassword)

			// Include API token if configured
			if r.config.HasAPITokens() {
				hashes := make([]string, len(r.config.APITokens))
				for i, t := range r.config.APITokens {
					hashes[i] = t.Hash
				}
				envContent += fmt.Sprintf("API_TOKEN='%s'\n", r.config.PrimaryAPITokenHash())
				envContent += fmt.Sprintf("API_TOKENS='%s'\n", strings.Join(hashes, ","))
			}
		}

		// Write the updated .env file
		if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
			log.Error().Err(err).Str("path", envPath).Msg("Failed to write .env file")
			writeErrorResponse(w, http.StatusInternalServerError, "config_error",
				"Failed to save new password", nil)
			return
		}

		// Update the running config
		r.config.AuthPass = hashedPassword

		log.Info().Msg("Password changed successfully in Docker environment")

		// Invalidate all sessions
		InvalidateUserSessions(r.config.AuthUser)

		// Audit log
		LogAuditEvent("password_change", r.config.AuthUser, GetClientIP(req), req.URL.Path, true, "Password changed (Docker)")

		// Return success with Docker-specific message
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Password changed successfully. Please restart your Docker container to apply changes.",
		})

	} else {
		// For non-Docker (systemd/manual), save to .env file
		envPath := filepath.Join(r.config.ConfigPath, ".env")
		if r.config.ConfigPath == "" {
			envPath = "/etc/pulse/.env"
		}

		// Read existing .env file to preserve other settings
		envContent := ""
		existingContent, err := os.ReadFile(envPath)
		if err == nil {
			// Parse and update existing content
			scanner := bufio.NewScanner(strings.NewReader(string(existingContent)))
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" || strings.HasPrefix(line, "#") {
					envContent += line + "\n"
					continue
				}
				// Update password line, keep others
				if strings.HasPrefix(line, "PULSE_AUTH_PASS=") {
					envContent += fmt.Sprintf("PULSE_AUTH_PASS='%s'\n", hashedPassword)
				} else {
					envContent += line + "\n"
				}
			}
		} else {
			// Create new .env if doesn't exist
			envContent = fmt.Sprintf(`# Auto-generated by Pulse password change
# Generated on %s
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
`, time.Now().Format(time.RFC3339), r.config.AuthUser, hashedPassword)

			if r.config.HasAPITokens() {
				hashes := make([]string, len(r.config.APITokens))
				for i, t := range r.config.APITokens {
					hashes[i] = t.Hash
				}
				envContent += fmt.Sprintf("API_TOKEN='%s'\n", r.config.PrimaryAPITokenHash())
				envContent += fmt.Sprintf("API_TOKENS='%s'\n", strings.Join(hashes, ","))
			}
		}

		// Try to write the .env file
		if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
			log.Error().Err(err).Str("path", envPath).Msg("Failed to write .env file")
			writeErrorResponse(w, http.StatusInternalServerError, "config_error",
				"Failed to save new password. You may need to update the password manually.", nil)
			return
		}

		// Update the running config
		r.config.AuthPass = hashedPassword

		log.Info().Msg("Password changed successfully")

		// Invalidate all sessions
		InvalidateUserSessions(r.config.AuthUser)

		// Audit log
		LogAuditEvent("password_change", r.config.AuthUser, GetClientIP(req), req.URL.Path, true, "Password changed")

		// Detect service name for restart instructions
		serviceName := detectServiceName()

		// Return success with manual restart instructions
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":         true,
			"message":         fmt.Sprintf("Password changed. Restart the service to apply: sudo systemctl restart %s", serviceName),
			"requiresRestart": true,
			"serviceName":     serviceName,
		})
	}
}

// handleLogout handles logout requests
func (r *Router) handleLogout(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only POST method is allowed", nil)
		return
	}

	// Get session token from cookie
	var sessionToken string
	if cookie, err := req.Cookie("pulse_session"); err == nil {
		sessionToken = cookie.Value
	}

	// Delete the session if it exists
	if sessionToken != "" {
		GetSessionStore().DeleteSession(sessionToken)

		// Also delete CSRF token if exists
		GetCSRFStore().DeleteCSRFToken(sessionToken)
	}

	// Get appropriate cookie settings based on proxy detection (consistent with login)
	isSecure, sameSitePolicy := getCookieSettings(req)

	// Clear the session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "pulse_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecure,
		SameSite: sameSitePolicy,
	})

	// Audit log logout (use admin as username since we have single user for now)
	LogAuditEvent("logout", "admin", GetClientIP(req), req.URL.Path, true, "User logged out")

	log.Info().
		Str("user", "admin").
		Str("ip", GetClientIP(req)).
		Msg("User logged out")

	// Return success
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Successfully logged out",
	})
}

func (r *Router) establishSession(w http.ResponseWriter, req *http.Request, username string) error {
	token := generateSessionToken()
	if token == "" {
		return fmt.Errorf("failed to generate session token")
	}

	userAgent := req.Header.Get("User-Agent")
	clientIP := GetClientIP(req)
	GetSessionStore().CreateSession(token, 24*time.Hour, userAgent, clientIP)

	if username != "" {
		TrackUserSession(username, token)
	}

	csrfToken := generateCSRFToken(token)
	isSecure, sameSitePolicy := getCookieSettings(req)

	http.SetCookie(w, &http.Cookie{
		Name:     "pulse_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecure,
		SameSite: sameSitePolicy,
		MaxAge:   86400,
	})

	http.SetCookie(w, &http.Cookie{
		Name:     "pulse_csrf",
		Value:    csrfToken,
		Path:     "/",
		Secure:   isSecure,
		SameSite: sameSitePolicy,
		MaxAge:   86400,
	})

	return nil
}

// handleLogin handles login requests and provides detailed feedback about lockouts
func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only POST method is allowed", nil)
		return
	}

	// Parse request
	var loginReq struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(req.Body).Decode(&loginReq); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_request",
			"Invalid request body", nil)
		return
	}

	clientIP := GetClientIP(req)

	// Check if account is locked out before attempting login
	_, userLockedUntil, userLocked := GetLockoutInfo(loginReq.Username)
	_, ipLockedUntil, ipLocked := GetLockoutInfo(clientIP)

	if userLocked || ipLocked {
		lockedUntil := userLockedUntil
		if ipLocked && ipLockedUntil.After(lockedUntil) {
			lockedUntil = ipLockedUntil
		}

		remainingMinutes := int(time.Until(lockedUntil).Minutes())
		if remainingMinutes < 1 {
			remainingMinutes = 1
		}

		LogAuditEvent("login", loginReq.Username, clientIP, req.URL.Path, false, "Account locked")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":            "account_locked",
			"message":          fmt.Sprintf("Too many failed attempts. Account is locked for %d more minutes.", remainingMinutes),
			"lockedUntil":      lockedUntil.Format(time.RFC3339),
			"remainingMinutes": remainingMinutes,
		})
		return
	}

	// Check rate limiting
	if !authLimiter.Allow(clientIP) {
		LogAuditEvent("login", loginReq.Username, clientIP, req.URL.Path, false, "Rate limited")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "rate_limit",
			"message": "Too many requests. Please wait before trying again.",
		})
		return
	}

	// Verify credentials
	if loginReq.Username == r.config.AuthUser && auth.CheckPasswordHash(loginReq.Password, r.config.AuthPass) {
		// Clear failed login attempts
		ClearFailedLogins(loginReq.Username)
		ClearFailedLogins(clientIP)

		// Create session
		token := generateSessionToken()
		if token == "" {
			writeErrorResponse(w, http.StatusInternalServerError, "session_error",
				"Failed to create session", nil)
			return
		}

		// Store session persistently
		userAgent := req.Header.Get("User-Agent")
		GetSessionStore().CreateSession(token, 24*time.Hour, userAgent, clientIP)

		// Track session for user
		TrackUserSession(loginReq.Username, token)

		// Generate CSRF token
		csrfToken := generateCSRFToken(token)

		// Get appropriate cookie settings based on proxy detection
		isSecure, sameSitePolicy := getCookieSettings(req)

		// Set session cookie
		http.SetCookie(w, &http.Cookie{
			Name:     "pulse_session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   isSecure,
			SameSite: sameSitePolicy,
			MaxAge:   86400, // 24 hours
		})

		// Set CSRF cookie (not HttpOnly so JS can read it)
		http.SetCookie(w, &http.Cookie{
			Name:     "pulse_csrf",
			Value:    csrfToken,
			Path:     "/",
			Secure:   isSecure,
			SameSite: sameSitePolicy,
			MaxAge:   86400, // 24 hours
		})

		// Audit log successful login
		LogAuditEvent("login", loginReq.Username, clientIP, req.URL.Path, true, "Successful login")

		// Return success
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Successfully logged in",
		})
	} else {
		// Failed login
		RecordFailedLogin(loginReq.Username)
		RecordFailedLogin(clientIP)
		LogAuditEvent("login", loginReq.Username, clientIP, req.URL.Path, false, "Invalid credentials")

		// Get updated attempt counts
		newUserAttempts, _, _ := GetLockoutInfo(loginReq.Username)
		newIPAttempts, _, _ := GetLockoutInfo(clientIP)

		// Use the higher count for warning
		attempts := newUserAttempts
		if newIPAttempts > attempts {
			attempts = newIPAttempts
		}

		// Prepare response with attempt information
		remaining := maxFailedAttempts - attempts

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)

		if remaining > 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":       "invalid_credentials",
				"message":     fmt.Sprintf("Invalid username or password. You have %d attempts remaining.", remaining),
				"attempts":    attempts,
				"remaining":   remaining,
				"maxAttempts": maxFailedAttempts,
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":           "invalid_credentials",
				"message":         "Invalid username or password. Account is now locked for 15 minutes.",
				"locked":          true,
				"lockoutDuration": "15 minutes",
			})
		}
	}
}

// handleResetLockout allows administrators to manually reset account lockouts
func (r *Router) handleResetLockout(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only POST method is allowed", nil)
		return
	}

	// Parse request
	var resetReq struct {
		Identifier string `json:"identifier"` // Can be username or IP
	}

	if err := json.NewDecoder(req.Body).Decode(&resetReq); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_request",
			"Invalid request body", nil)
		return
	}

	if resetReq.Identifier == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_identifier",
			"Identifier (username or IP) is required", nil)
		return
	}

	// Reset the lockout
	ResetLockout(resetReq.Identifier)

	// Also clear failed login attempts
	ClearFailedLogins(resetReq.Identifier)

	// Audit log the reset
	LogAuditEvent("lockout_reset", "admin", GetClientIP(req), req.URL.Path, true,
		fmt.Sprintf("Lockout reset for: %s", resetReq.Identifier))

	log.Info().
		Str("identifier", resetReq.Identifier).
		Str("reset_by", "admin").
		Str("ip", GetClientIP(req)).
		Msg("Account lockout manually reset")

	// Return success
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Lockout reset for %s", resetReq.Identifier),
	})
}

// handleState handles state requests
func (r *Router) handleState(w http.ResponseWriter, req *http.Request) {
	log.Debug().Msg("[DEBUG] handleState: START")
	if req.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only GET method is allowed", nil)
		return
	}

	log.Debug().Msg("[DEBUG] handleState: Before auth check")
	// Use standard auth check (supports both basic auth and API tokens) unless auth is disabled
	if !r.config.DisableAuth && !CheckAuth(r.config, w, req) {
		writeErrorResponse(w, http.StatusUnauthorized, "unauthorized",
			"Authentication required", nil)
		return
	}

	log.Debug().Msg("[DEBUG] handleState: Before GetState")
	state := r.monitor.GetState()
	log.Debug().Msg("[DEBUG] handleState: After GetState, before ToFrontend")
	frontendState := state.ToFrontend()

	log.Debug().Msg("[DEBUG] handleState: Before WriteJSONResponse")
	if err := utils.WriteJSONResponse(w, frontendState); err != nil {
		log.Error().Err(err).Msg("Failed to encode state response")
		writeErrorResponse(w, http.StatusInternalServerError, "encoding_error",
			"Failed to encode state data", nil)
	}
	log.Debug().Msg("[DEBUG] handleState: END")
}

// handleVersion handles version requests
func (r *Router) handleVersion(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	versionInfo, err := updates.GetCurrentVersion()
	if err != nil {
		// Fallback to VERSION file
		versionBytes, _ := os.ReadFile("VERSION")
		response := VersionResponse{
			Version:       strings.TrimSpace(string(versionBytes)),
			BuildTime:     "development",
			Build:         "development",
			GoVersion:     runtime.Version(),
			Runtime:       runtime.Version(),
			Channel:       "stable",
			IsDocker:      false,
			IsSourceBuild: false,
			IsDevelopment: true,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	// Convert to typed response
	response := VersionResponse{
		Version:        versionInfo.Version,
		BuildTime:      versionInfo.Build,
		Build:          versionInfo.Build,
		GoVersion:      runtime.Version(),
		Runtime:        versionInfo.Runtime,
		Channel:        versionInfo.Channel,
		IsDocker:       versionInfo.IsDocker,
		IsSourceBuild:  versionInfo.IsSourceBuild,
		IsDevelopment:  versionInfo.IsDevelopment,
		DeploymentType: versionInfo.DeploymentType,
	}

	// Detect containerization (LXC/Docker)
	if containerType, err := os.ReadFile("/run/systemd/container"); err == nil {
		response.Containerized = true

		// Try to get container ID from hostname (LXC containers often use CTID as hostname)
		if hostname, err := os.Hostname(); err == nil {
			// For LXC, try to extract numeric ID from hostname or use full hostname
			response.ContainerId = hostname
		}

		// Add container type to deployment type if not already set
		if response.DeploymentType == "" {
			response.DeploymentType = string(containerType)
		}
	}

	// Add cached update info if available
	if cachedUpdate := r.updateManager.GetCachedUpdateInfo(); cachedUpdate != nil {
		response.UpdateAvailable = cachedUpdate.Available
		response.LatestVersion = cachedUpdate.LatestVersion
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleAgentVersion returns the current Docker agent version for update checks
func (r *Router) handleAgentVersion(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Current agent version - matches the version baked into the Docker agent binary
	version := strings.TrimSpace(dockeragent.Version)
	if version == "" {
		version = "dev"
	}

	response := AgentVersionResponse{
		Version: version,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
func (r *Router) handleServerInfo(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	versionInfo, err := updates.GetCurrentVersion()
	isDev := true
	if err == nil {
		isDev = versionInfo.IsDevelopment
	}

	response := map[string]interface{}{
		"isDevelopment": isDev,
		"version":       dockeragent.Version,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleStorage handles storage detail requests
func (r *Router) handleStorage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only GET method is allowed", nil)
		return
	}

	// Extract storage ID from path
	path := strings.TrimPrefix(req.URL.Path, "/api/storage/")
	if path == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_storage_id",
			"Storage ID is required", nil)
		return
	}

	// Get current state
	state := r.monitor.GetState()

	// Find the storage by ID
	var storageDetail *models.Storage
	for _, storage := range state.Storage {
		if storage.ID == path {
			storageDetail = &storage
			break
		}
	}

	if storageDetail == nil {
		writeErrorResponse(w, http.StatusNotFound, "storage_not_found",
			fmt.Sprintf("Storage with ID '%s' not found", path), nil)
		return
	}

	// Return storage details
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"data":      storageDetail,
		"timestamp": time.Now().Unix(),
	}); err != nil {
		log.Error().Err(err).Str("storage_id", path).Msg("Failed to encode storage details")
		writeErrorResponse(w, http.StatusInternalServerError, "encoding_error",
			"Failed to encode response", nil)
	}
}

// handleCharts handles chart data requests
func (r *Router) handleCharts(w http.ResponseWriter, req *http.Request) {
	log.Debug().Str("method", req.Method).Str("url", req.URL.String()).Msg("Charts endpoint hit")

	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get time range from query parameters
	query := req.URL.Query()
	timeRange := query.Get("range")
	if timeRange == "" {
		timeRange = "1h"
	}

	// Convert time range to duration
	var duration time.Duration
	switch timeRange {
	case "5m":
		duration = 5 * time.Minute
	case "15m":
		duration = 15 * time.Minute
	case "30m":
		duration = 30 * time.Minute
	case "1h":
		duration = time.Hour
	case "4h":
		duration = 4 * time.Hour
	case "12h":
		duration = 12 * time.Hour
	case "24h":
		duration = 24 * time.Hour
	case "7d":
		duration = 7 * 24 * time.Hour
	default:
		duration = time.Hour
	}

	// Get current state from monitor
	state := r.monitor.GetState()

	// Create chart data structure that matches frontend expectations
	chartData := make(map[string]VMChartData)
	nodeData := make(map[string]NodeChartData)

	currentTime := time.Now().Unix() * 1000 // JavaScript timestamp format
	oldestTimestamp := currentTime

	// Process VMs - get historical data
	for _, vm := range state.VMs {
		if chartData[vm.ID] == nil {
			chartData[vm.ID] = make(VMChartData)
		}

		// Get historical metrics
		metrics := r.monitor.GetGuestMetrics(vm.ID, duration)

		// Convert metric points to API format
		for metricType, points := range metrics {
			chartData[vm.ID][metricType] = make([]MetricPoint, len(points))
			for i, point := range points {
				ts := point.Timestamp.Unix() * 1000
				if ts < oldestTimestamp {
					oldestTimestamp = ts
				}
				chartData[vm.ID][metricType][i] = MetricPoint{
					Timestamp: ts,
					Value:     point.Value,
				}
			}
		}

		// If no historical data, add current value
		if len(chartData[vm.ID]["cpu"]) == 0 {
			chartData[vm.ID]["cpu"] = []MetricPoint{
				{Timestamp: currentTime, Value: vm.CPU * 100},
			}
			chartData[vm.ID]["memory"] = []MetricPoint{
				{Timestamp: currentTime, Value: vm.Memory.Usage},
			}
			chartData[vm.ID]["disk"] = []MetricPoint{
				{Timestamp: currentTime, Value: vm.Disk.Usage},
			}
			chartData[vm.ID]["diskread"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(vm.DiskRead)},
			}
			chartData[vm.ID]["diskwrite"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(vm.DiskWrite)},
			}
			chartData[vm.ID]["netin"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(vm.NetworkIn)},
			}
			chartData[vm.ID]["netout"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(vm.NetworkOut)},
			}
		}
	}

	// Process Containers - get historical data
	for _, ct := range state.Containers {
		if chartData[ct.ID] == nil {
			chartData[ct.ID] = make(VMChartData)
		}

		// Get historical metrics
		metrics := r.monitor.GetGuestMetrics(ct.ID, duration)

		// Convert metric points to API format
		for metricType, points := range metrics {
			chartData[ct.ID][metricType] = make([]MetricPoint, len(points))
			for i, point := range points {
				ts := point.Timestamp.Unix() * 1000
				if ts < oldestTimestamp {
					oldestTimestamp = ts
				}
				chartData[ct.ID][metricType][i] = MetricPoint{
					Timestamp: ts,
					Value:     point.Value,
				}
			}
		}

		// If no historical data, add current value
		if len(chartData[ct.ID]["cpu"]) == 0 {
			chartData[ct.ID]["cpu"] = []MetricPoint{
				{Timestamp: currentTime, Value: ct.CPU * 100},
			}
			chartData[ct.ID]["memory"] = []MetricPoint{
				{Timestamp: currentTime, Value: ct.Memory.Usage},
			}
			chartData[ct.ID]["disk"] = []MetricPoint{
				{Timestamp: currentTime, Value: ct.Disk.Usage},
			}
			chartData[ct.ID]["diskread"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(ct.DiskRead)},
			}
			chartData[ct.ID]["diskwrite"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(ct.DiskWrite)},
			}
			chartData[ct.ID]["netin"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(ct.NetworkIn)},
			}
			chartData[ct.ID]["netout"] = []MetricPoint{
				{Timestamp: currentTime, Value: float64(ct.NetworkOut)},
			}
		}
	}

	// Process Storage - get historical data
	storageData := make(map[string]StorageChartData)
	for _, storage := range state.Storage {
		if storageData[storage.ID] == nil {
			storageData[storage.ID] = make(StorageChartData)
		}

		// Get historical metrics
		metrics := r.monitor.GetStorageMetrics(storage.ID, duration)

		// Convert usage metrics to chart format
		if usagePoints, ok := metrics["usage"]; ok && len(usagePoints) > 0 {
			// Convert MetricPoint slice to chart format
			storageData[storage.ID]["disk"] = make([]MetricPoint, len(usagePoints))
			for i, point := range usagePoints {
				ts := point.Timestamp.Unix() * 1000
				if ts < oldestTimestamp {
					oldestTimestamp = ts
				}
				storageData[storage.ID]["disk"][i] = MetricPoint{
					Timestamp: ts,
					Value:     point.Value,
				}
			}
		} else {
			// Add current value if no historical data
			usagePercent := float64(0)
			if storage.Total > 0 {
				usagePercent = (float64(storage.Used) / float64(storage.Total)) * 100
			}
			storageData[storage.ID]["disk"] = []MetricPoint{
				{Timestamp: currentTime, Value: usagePercent},
			}
		}
	}

	// Process Nodes - get historical data
	for _, node := range state.Nodes {
		if nodeData[node.ID] == nil {
			nodeData[node.ID] = make(NodeChartData)
		}

		// Get historical metrics for each type
		for _, metricType := range []string{"cpu", "memory", "disk"} {
			points := r.monitor.GetNodeMetrics(node.ID, metricType, duration)
			nodeData[node.ID][metricType] = make([]MetricPoint, len(points))
			for i, point := range points {
				ts := point.Timestamp.Unix() * 1000
				if ts < oldestTimestamp {
					oldestTimestamp = ts
				}
				nodeData[node.ID][metricType][i] = MetricPoint{
					Timestamp: ts,
					Value:     point.Value,
				}
			}

			// If no historical data, add current value
			if len(nodeData[node.ID][metricType]) == 0 {
				var value float64
				switch metricType {
				case "cpu":
					value = node.CPU * 100
				case "memory":
					value = node.Memory.Usage
				case "disk":
					value = node.Disk.Usage
				}
				nodeData[node.ID][metricType] = []MetricPoint{
					{Timestamp: currentTime, Value: value},
				}
			}
		}
	}

	response := ChartResponse{
		ChartData:   chartData,
		NodeData:    nodeData,
		StorageData: storageData,
		Timestamp:   currentTime,
		Stats: ChartStats{
			OldestDataTimestamp: oldestTimestamp,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error().Err(err).Msg("Failed to encode chart data response")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	log.Debug().
		Int("guests", len(chartData)).
		Int("nodes", len(nodeData)).
		Int("storage", len(storageData)).
		Str("range", timeRange).
		Msg("Chart data response sent")
}

// handleStorageCharts handles storage chart data requests
func (r *Router) handleStorageCharts(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	query := req.URL.Query()
	rangeMinutes := 60 // default 1 hour
	if rangeStr := query.Get("range"); rangeStr != "" {
		if _, err := fmt.Sscanf(rangeStr, "%d", &rangeMinutes); err != nil {
			log.Warn().Err(err).Str("range", rangeStr).Msg("Invalid range parameter; using default")
		}
	}

	duration := time.Duration(rangeMinutes) * time.Minute
	state := r.monitor.GetState()

	// Build storage chart data
	storageData := make(StorageChartsResponse)

	for _, storage := range state.Storage {
		metrics := r.monitor.GetStorageMetrics(storage.ID, duration)

		storageData[storage.ID] = StorageMetrics{
			Usage: metrics["usage"],
			Used:  metrics["used"],
			Total: metrics["total"],
			Avail: metrics["avail"],
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(storageData); err != nil {
		log.Error().Err(err).Msg("Failed to encode storage chart data")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// handleConfig handles configuration requests
func (r *Router) handleConfig(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Return public configuration
	config := map[string]interface{}{
		"csrfProtection":    false, // Not implemented yet
		"autoUpdateEnabled": r.config.AutoUpdateEnabled,
		"updateChannel":     r.config.UpdateChannel,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

// handleBackups handles backup requests
func (r *Router) handleBackups(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get current state
	state := r.monitor.GetState()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Backups     models.Backups         `json:"backups"`
		PVEBackups  models.PVEBackups      `json:"pveBackups"`
		PBSBackups  []models.PBSBackup     `json:"pbsBackups"`
		PMGBackups  []models.PMGBackup     `json:"pmgBackups"`
		BackupTasks []models.BackupTask    `json:"backupTasks"`
		Storage     []models.StorageBackup `json:"storageBackups"`
		GuestSnaps  []models.GuestSnapshot `json:"guestSnapshots"`
	}{
		Backups:     state.Backups,
		PVEBackups:  state.PVEBackups,
		PBSBackups:  state.PBSBackups,
		PMGBackups:  state.PMGBackups,
		BackupTasks: state.PVEBackups.BackupTasks,
		Storage:     state.PVEBackups.StorageBackups,
		GuestSnaps:  state.PVEBackups.GuestSnapshots,
	})
}

// handleBackupsPVE handles PVE backup requests
func (r *Router) handleBackupsPVE(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get state and extract PVE backups
	state := r.monitor.GetState()

	// Return PVE backup data in expected format
	backups := state.PVEBackups.StorageBackups
	if backups == nil {
		backups = []models.StorageBackup{}
	}

	pveBackups := map[string]interface{}{
		"backups": backups,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pveBackups); err != nil {
		log.Error().Err(err).Msg("Failed to encode PVE backups response")
		// Return empty array as fallback
		w.Write([]byte(`{"backups":[]}`))
	}
}

// handleBackupsPBS handles PBS backup requests
func (r *Router) handleBackupsPBS(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get state and extract PBS backups
	state := r.monitor.GetState()

	// Return PBS backup data in expected format
	instances := state.PBSInstances
	if instances == nil {
		instances = []models.PBSInstance{}
	}

	pbsData := map[string]interface{}{
		"instances": instances,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pbsData); err != nil {
		log.Error().Err(err).Msg("Failed to encode PBS response")
		// Return empty array as fallback
		w.Write([]byte(`{"instances":[]}`))
	}
}

// handleSnapshots handles snapshot requests
func (r *Router) handleSnapshots(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get state and extract guest snapshots
	state := r.monitor.GetState()

	// Return snapshot data
	snaps := state.PVEBackups.GuestSnapshots
	if snaps == nil {
		snaps = []models.GuestSnapshot{}
	}

	snapshots := map[string]interface{}{
		"snapshots": snaps,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snapshots); err != nil {
		log.Error().Err(err).Msg("Failed to encode snapshots response")
		// Return empty array as fallback
		w.Write([]byte(`{"snapshots":[]}`))
	}
}

// handleWebSocket handles WebSocket connections
func (r *Router) handleWebSocket(w http.ResponseWriter, req *http.Request) {
	r.wsHub.HandleWebSocket(w, req)
}

// handleSimpleStats serves a simple stats page
func (r *Router) handleSimpleStats(w http.ResponseWriter, req *http.Request) {
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Simple Pulse Stats</title>
    <style>
        body {
            font-family: Arial, sans-serif;
            margin: 20px;
            background: #f5f5f5;
        }
        table {
            width: 100%;
            border-collapse: collapse;
            background: white;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        th, td {
            padding: 12px;
            text-align: left;
            border-bottom: 1px solid #ddd;
        }
        th {
            background: #333;
            color: white;
            font-weight: bold;
            position: sticky;
            top: 0;
        }
        tr:hover {
            background: #f5f5f5;
        }
        .status {
            padding: 4px 8px;
            border-radius: 4px;
            color: white;
            font-size: 12px;
        }
        .running { background: #28a745; }
        .stopped { background: #dc3545; }
        #status {
            margin-bottom: 20px;
            padding: 10px;
            background: #e9ecef;
            border-radius: 4px;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        .update-indicator {
            display: inline-block;
            width: 10px;
            height: 10px;
            background: #28a745;
            border-radius: 50%;
            animation: pulse 0.5s ease-out;
        }
        @keyframes pulse {
            0% { transform: scale(1); opacity: 1; }
            50% { transform: scale(1.5); opacity: 0.7; }
            100% { transform: scale(1); opacity: 1; }
        }
        .update-timer {
            font-family: monospace;
            font-size: 14px;
            color: #666;
        }
        .metric {
            font-family: monospace;
            text-align: right;
        }
    </style>
</head>
<body>
    <h1>Simple Pulse Stats</h1>
    <div id="status">
        <div>
            <span id="status-text">Connecting...</span>
            <span class="update-indicator" id="update-indicator" style="display:none"></span>
        </div>
        <div class="update-timer" id="update-timer"></div>
    </div>
    
    <h2>Containers</h2>
    <table id="containers">
        <thead>
            <tr>
                <th>Name</th>
                <th>Status</th>
                <th>CPU %</th>
                <th>Memory</th>
                <th>Disk Read</th>
                <th>Disk Write</th>
                <th>Net In</th>
                <th>Net Out</th>
            </tr>
        </thead>
        <tbody></tbody>
    </table>

    <script>
        let ws;
        let lastUpdateTime = null;
        let updateCount = 0;
        let updateInterval = null;
        
        function formatBytes(bytes) {
            if (!bytes || bytes < 0) return '0 B/s';
            const units = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
            let i = 0;
            let value = bytes;
            while (value >= 1024 && i < units.length - 1) {
                value /= 1024;
                i++;
            }
            return value.toFixed(1) + ' ' + units[i];
        }
        
        function formatMemory(used, total) {
            const usedGB = (used / 1024 / 1024 / 1024).toFixed(1);
            const totalGB = (total / 1024 / 1024 / 1024).toFixed(1);
            const percent = ((used / total) * 100).toFixed(0);
            return usedGB + '/' + totalGB + ' GB (' + percent + '%)';
        }
        
        function updateTable(containers) {
            const tbody = document.querySelector('#containers tbody');
            tbody.innerHTML = '';
            
            containers.sort((a, b) => a.name.localeCompare(b.name));
            
            containers.forEach(ct => {
                const row = document.createElement('tr');
                row.innerHTML = 
                    '<td><strong>' + ct.name + '</strong></td>' +
                    '<td><span class="status ' + ct.status + '">' + ct.status + '</span></td>' +
                    '<td class="metric">' + (ct.cpu ? ct.cpu.toFixed(1) : '0.0') + '%</td>' +
                    '<td class="metric">' + formatMemory(ct.mem || 0, ct.maxmem || 1) + '</td>' +
                    '<td class="metric">' + formatBytes(ct.diskread) + '</td>' +
                    '<td class="metric">' + formatBytes(ct.diskwrite) + '</td>' +
                    '<td class="metric">' + formatBytes(ct.netin) + '</td>' +
                    '<td class="metric">' + formatBytes(ct.netout) + '</td>';
                tbody.appendChild(row);
            });
        }
        
        function updateTimer() {
            if (lastUpdateTime) {
                const secondsSince = Math.floor((Date.now() - lastUpdateTime) / 1000);
                document.getElementById('update-timer').textContent = 'Next update in: ' + (2 - (secondsSince % 2)) + 's';
            }
        }
        
        function connect() {
            const statusText = document.getElementById('status-text');
            const indicator = document.getElementById('update-indicator');
            statusText.textContent = 'Connecting to WebSocket...';
            
            ws = new WebSocket('ws://' + window.location.host + '/ws');
            
            ws.onopen = function() {
                statusText.textContent = 'Connected! Updates every 2 seconds';
                console.log('WebSocket connected');
                // Start the countdown timer
                if (updateInterval) clearInterval(updateInterval);
                updateInterval = setInterval(updateTimer, 100);
            };
            
            ws.onmessage = function(event) {
                try {
                    const msg = JSON.parse(event.data);
                    
                    if (msg.type === 'initialState' || msg.type === 'rawData') {
                        if (msg.data && msg.data.containers) {
                            updateCount++;
                            lastUpdateTime = Date.now();
                            
                            // Show update indicator with animation
                            indicator.style.display = 'inline-block';
                            indicator.style.animation = 'none';
                            setTimeout(() => {
                                indicator.style.animation = 'pulse 0.5s ease-out';
                            }, 10);
                            
                            statusText.textContent = 'Update #' + updateCount + ' at ' + new Date().toLocaleTimeString();
                            updateTable(msg.data.containers);
                        }
                    }
                } catch (err) {
                    console.error('Parse error:', err);
                }
            };
            
            ws.onclose = function(event) {
                statusText.textContent = 'Disconnected: ' + event.code + ' ' + event.reason + '. Reconnecting in 3s...';
                indicator.style.display = 'none';
                if (updateInterval) clearInterval(updateInterval);
                setTimeout(connect, 3000);
            };
            
            ws.onerror = function(error) {
                statusText.textContent = 'Connection error. Retrying...';
                console.error('WebSocket error:', error);
            };
        }
        
        // Start connection
        connect();
    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// handleSocketIO handles socket.io requests
func (r *Router) handleSocketIO(w http.ResponseWriter, req *http.Request) {
	// For socket.io.js, redirect to CDN
	if strings.Contains(req.URL.Path, "socket.io.js") {
		http.Redirect(w, req, "https://cdn.socket.io/4.8.1/socket.io.min.js", http.StatusFound)
		return
	}

	// For other socket.io endpoints, use our WebSocket
	// This provides basic compatibility
	if strings.Contains(req.URL.RawQuery, "transport=websocket") {
		r.wsHub.HandleWebSocket(w, req)
		return
	}

	// For polling transport, return proper socket.io response
	// Socket.io v4 expects specific format
	if strings.Contains(req.URL.RawQuery, "transport=polling") {
		if strings.Contains(req.URL.RawQuery, "sid=") {
			// Already connected, return empty poll
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("6"))
		} else {
			// Initial handshake
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(http.StatusOK)
			// Send open packet with session ID and config
			sessionID := fmt.Sprintf("%d", time.Now().UnixNano())
			response := fmt.Sprintf(`0{"sid":"%s","upgrades":["websocket"],"pingInterval":25000,"pingTimeout":60000}`, sessionID)
			w.Write([]byte(response))
		}
		return
	}

	// Default: redirect to WebSocket
	http.Redirect(w, req, "/ws", http.StatusFound)
}

// forwardUpdateProgress forwards update progress to WebSocket clients
func (r *Router) forwardUpdateProgress() {
	progressChan := r.updateManager.GetProgressChannel()

	for status := range progressChan {
		// Create update event for WebSocket
		message := websocket.Message{
			Type:      "update:progress",
			Data:      status,
			Timestamp: time.Now().Format(time.RFC3339),
		}

		// Broadcast to all connected clients
		r.wsHub.BroadcastMessage(message)

		// Log progress
		log.Debug().
			Str("status", status.Status).
			Int("progress", status.Progress).
			Str("message", status.Message).
			Msg("Update progress")
	}
}

// backgroundUpdateChecker periodically checks for updates and caches the result
func (r *Router) backgroundUpdateChecker() {
	// Delay initial check to allow WebSocket clients to receive welcome messages first
	time.Sleep(1 * time.Second)

	ctx := context.Background()
	if _, err := r.updateManager.CheckForUpdates(ctx); err != nil {
		log.Debug().Err(err).Msg("Initial update check failed")
	} else {
		log.Info().Msg("Initial update check completed")
	}

	// Then check every hour
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		if _, err := r.updateManager.CheckForUpdates(ctx); err != nil {
			log.Debug().Err(err).Msg("Periodic update check failed")
		} else {
			log.Debug().Msg("Periodic update check completed")
		}
	}
}

// handleDownloadInstallScript serves the Docker agent installation script
func (r *Router) handleDownloadInstallScript(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Prevent caching - always serve the latest version
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	scriptPath := "/opt/pulse/scripts/install-docker-agent.sh"
	http.ServeFile(w, req, scriptPath)
}

// handleDownloadAgent serves the Docker agent binary
func (r *Router) handleDownloadAgent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Prevent caching - always serve the latest version
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	archParam := strings.TrimSpace(req.URL.Query().Get("arch"))
	searchPaths := make([]string, 0, 6)

	if normalized := normalizeDockerAgentArch(archParam); normalized != "" {
		searchPaths = append(searchPaths,
			filepath.Join("/opt/pulse/bin", "pulse-docker-agent-"+normalized),
			filepath.Join("/opt/pulse", "pulse-docker-agent-"+normalized),
			filepath.Join("/app", "pulse-docker-agent-"+normalized), // legacy Docker image layout
		)
	}

	// Default locations (host architecture)
	searchPaths = append(searchPaths,
		filepath.Join("/opt/pulse/bin", "pulse-docker-agent"),
		"/opt/pulse/pulse-docker-agent",
		filepath.Join("/app", "pulse-docker-agent"), // legacy Docker image layout
	)

	for _, candidate := range searchPaths {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			http.ServeFile(w, req, candidate)
			return
		}
	}

	http.Error(w, "Agent binary not found", http.StatusNotFound)
}

func (r *Router) handleDiagnosticsRegisterProxyNodes(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed", nil)
		return
	}

	client := tempproxy.NewClient()
	if client == nil || !client.IsAvailable() {
		writeErrorResponse(w, http.StatusServiceUnavailable, "proxy_unavailable", "pulse-sensor-proxy socket not detected inside the container", nil)
		return
	}

	nodes, err := client.RegisterNodes()
	if err != nil {
		log.Error().Err(err).Msg("Failed to request proxy node registration status")
		writeErrorResponse(w, http.StatusBadGateway, "proxy_error", err.Error(), nil)
		return
	}

	if err := utils.WriteJSONResponse(w, map[string]any{
		"success": true,
		"nodes":   nodes,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to encode proxy register nodes response")
	}
}

func (r *Router) handleDiagnosticsDockerPrepareToken(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed", nil)
		return
	}

	var payload struct {
		HostID    string `json:"hostId"`
		TokenName string `json:"tokenName"`
	}

	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_json", "Failed to decode request body", nil)
		return
	}

	hostID := strings.TrimSpace(payload.HostID)
	if hostID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_host_id", "hostId is required", nil)
		return
	}

	host, ok := r.monitor.GetDockerHost(hostID)
	if !ok {
		writeErrorResponse(w, http.StatusNotFound, "host_not_found", "Docker host not found", nil)
		return
	}

	name := strings.TrimSpace(payload.TokenName)
	if name == "" {
		displayName := preferredDockerHostName(host)
		name = fmt.Sprintf("Docker host: %s", displayName)
	}

	rawToken, err := auth.GenerateAPIToken()
	if err != nil {
		log.Error().Err(err).Msg("Failed to generate docker migration token")
		writeErrorResponse(w, http.StatusInternalServerError, "token_generation_failed", "Failed to generate API token", nil)
		return
	}

	record, err := config.NewAPITokenRecord(rawToken, name)
	if err != nil {
		log.Error().Err(err).Msg("Failed to construct token record for docker migration")
		writeErrorResponse(w, http.StatusInternalServerError, "token_generation_failed", "Failed to generate API token", nil)
		return
	}

	r.config.APITokens = append(r.config.APITokens, *record)
	r.config.SortAPITokens()
	r.config.APITokenEnabled = true

	if r.persistence != nil {
		if err := r.persistence.SaveAPITokens(r.config.APITokens); err != nil {
			r.config.RemoveAPIToken(record.ID)
			log.Error().Err(err).Msg("Failed to persist API tokens after docker migration generation")
			writeErrorResponse(w, http.StatusInternalServerError, "token_persist_failed", "Failed to persist API token", nil)
			return
		}
	}

	baseURL := strings.TrimRight(r.resolvePublicURL(req), "/")
	installCommand := fmt.Sprintf("curl -fsSL %s/install-docker-agent.sh | bash -s -- --url %s --token %s", baseURL, baseURL, rawToken)
	systemdSnippet := fmt.Sprintf("[Service]\nType=simple\nEnvironment=\"PULSE_URL=%s\"\nEnvironment=\"PULSE_TOKEN=%s\"\nExecStart=/usr/local/bin/pulse-docker-agent --url %s --interval 30s\nRestart=always\nRestartSec=5s\nUser=root", baseURL, rawToken, baseURL)

	response := map[string]any{
		"success": true,
		"token":   rawToken,
		"record":  toAPITokenDTO(*record),
		"host": map[string]any{
			"id":   host.ID,
			"name": preferredDockerHostName(host),
		},
		"installCommand":        installCommand,
		"systemdServiceSnippet": systemdSnippet,
		"pulseURL":              baseURL,
	}

	if err := utils.WriteJSONResponse(w, response); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker token migration response")
	}
}

func (r *Router) handleDownloadPulseSensorProxy(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET is allowed", nil)
		return
	}

	// Get requested architecture from query param
	arch := strings.TrimSpace(req.URL.Query().Get("arch"))
	if arch == "" {
		arch = "linux-amd64" // Default to amd64
	}

	var binaryPath string
	var filename string

	// Map architecture to binary filename
	switch arch {
	case "linux-amd64", "amd64":
		filename = "pulse-sensor-proxy-linux-amd64"
	case "linux-arm64", "arm64":
		filename = "pulse-sensor-proxy-linux-arm64"
	case "linux-armv7", "armv7", "armhf":
		filename = "pulse-sensor-proxy-linux-armv7"
	default:
		writeErrorResponse(w, http.StatusBadRequest, "unsupported_arch", fmt.Sprintf("Unsupported architecture: %s", arch), nil)
		return
	}

	// Try pre-built architecture-specific binary first (in container)
	binaryPath = filepath.Join("/opt/pulse/bin", filename)
	content, err := os.ReadFile(binaryPath)
	if err != nil {
		// Try generic pulse-sensor-proxy binary (built for host arch)
		genericPath := "/opt/pulse/bin/pulse-sensor-proxy"
		content, err = os.ReadFile(genericPath)
		if err == nil {
			log.Info().
				Str("arch", arch).
				Str("path", genericPath).
				Int("size", len(content)).
				Msg("Serving generic pulse-sensor-proxy binary (built for host arch)")
			binaryPath = genericPath
		}
	}

	if err != nil {
		// Fallback: Try to build on-the-fly for dev environments
		log.Info().
			Str("arch", arch).
			Str("tried_path", binaryPath).
			Msg("Pre-built binary not found, attempting to build on-the-fly (dev mode)")

		if !strings.HasPrefix(arch, "linux-amd64") && runtime.GOARCH != strings.TrimPrefix(arch, "linux-") {
			writeErrorResponse(w, http.StatusBadRequest, "cross_compile_unsupported", "Cross-compilation not supported in dev mode", nil)
			return
		}

		tmpFile, err := os.CreateTemp("", "pulse-sensor-proxy-*.bin")
		if err != nil {
			log.Error().Err(err).Msg("Failed to create temp file for on-the-fly build")
			writeErrorResponse(w, http.StatusInternalServerError, "tempfile_error", "Binary not available and build failed", nil)
			return
		}
		tmpFileName := tmpFile.Name()
		tmpFile.Close()
		defer os.Remove(tmpFileName)

		cmd := exec.Command("go", "build", "-o", tmpFileName, "./cmd/pulse-sensor-proxy")
		cmd.Dir = r.projectRoot
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
		)

		buildOutput, err := cmd.CombinedOutput()
		if err != nil {
			log.Error().Err(err).Bytes("output", buildOutput).Msg("Failed to build pulse-sensor-proxy binary on-the-fly")
			writeErrorResponse(w, http.StatusInternalServerError, "build_failed", "Binary not available and on-the-fly build failed", nil)
			return
		}

		// Read the built binary
		content, err = os.ReadFile(tmpFileName)
		if err != nil {
			log.Error().Err(err).Msg("Failed to read built binary")
			writeErrorResponse(w, http.StatusInternalServerError, "read_error", "Failed to read built binary", nil)
			return
		}

		log.Info().
			Str("arch", arch).
			Int("size", len(content)).
			Msg("Successfully built pulse-sensor-proxy binary on-the-fly")
	} else {
		log.Info().
			Str("path", binaryPath).
			Str("arch", arch).
			Int("size", len(content)).
			Msg("Serving pre-built pulse-sensor-proxy binary")
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))

	if _, err := w.Write(content); err != nil {
		log.Error().Err(err).Msg("Failed to write proxy binary to client")
	}
}

func (r *Router) handleDownloadInstallerScript(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET is allowed", nil)
		return
	}

	// Try pre-built location first (in container)
	scriptPath := "/opt/pulse/scripts/install-sensor-proxy.sh"
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		// Fallback to project root (dev environment)
		scriptPath = filepath.Join(r.projectRoot, "scripts", "install-sensor-proxy.sh")
		content, err = os.ReadFile(scriptPath)
		if err != nil {
			log.Error().Err(err).Str("path", scriptPath).Msg("Failed to read installer script")
			writeErrorResponse(w, http.StatusInternalServerError, "read_error", "Failed to read installer script", nil)
			return
		}
	}

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "attachment; filename=install-sensor-proxy.sh")
	if _, err := w.Write(content); err != nil {
		log.Error().Err(err).Msg("Failed to write installer script to client")
	}
}

func (r *Router) handleDownloadDockerInstallerScript(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET is allowed", nil)
		return
	}

	// Try pre-built location first (in container)
	scriptPath := "/opt/pulse/scripts/install-docker.sh"
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		// Fallback to project root (dev environment)
		scriptPath = filepath.Join(r.projectRoot, "scripts", "install-docker.sh")
		content, err = os.ReadFile(scriptPath)
		if err != nil {
			log.Error().Err(err).Str("path", scriptPath).Msg("Failed to read Docker installer script")
			writeErrorResponse(w, http.StatusInternalServerError, "read_error", "Failed to read Docker installer script", nil)
			return
		}
	}

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "attachment; filename=install-docker.sh")
	if _, err := w.Write(content); err != nil {
		log.Error().Err(err).Msg("Failed to write Docker installer script to client")
	}
}

func (r *Router) resolvePublicURL(req *http.Request) string {
	if publicURL := strings.TrimSpace(r.config.PublicURL); publicURL != "" {
		return strings.TrimRight(publicURL, "/")
	}

	scheme := "http"
	if req != nil {
		if req.TLS != nil {
			scheme = "https"
		} else if proto := req.Header.Get("X-Forwarded-Proto"); strings.EqualFold(proto, "https") {
			scheme = "https"
		}
	}

	host := ""
	if req != nil {
		host = strings.TrimSpace(req.Host)
	}
	if host == "" {
		if r.config.FrontendPort > 0 {
			host = fmt.Sprintf("localhost:%d", r.config.FrontendPort)
		} else {
			host = "localhost:7655"
		}
	}

	return fmt.Sprintf("%s://%s", scheme, host)
}

func normalizeDockerAgentArch(arch string) string {
	if arch == "" {
		return ""
	}

	arch = strings.ToLower(strings.TrimSpace(arch))
	switch arch {
	case "linux-amd64", "amd64", "x86_64", "x86-64":
		return "linux-amd64"
	case "linux-arm64", "arm64", "aarch64":
		return "linux-arm64"
	case "linux-armv7", "armv7", "armv7l", "armhf":
		return "linux-armv7"
	default:
		return ""
	}
}
