package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	internalauth "github.com/RouXx67/PulseUp/internal/auth"
	"github.com/RouXx67/PulseUp/internal/config"
	"github.com/RouXx67/PulseUp/internal/updates"
	"github.com/rs/zerolog/log"
)

// detectServiceName detects the actual systemd service name being used
func detectServiceName() string {
	// Try common service names
	services := []string{"pulse-backend", "pulse", "pulse.service", "pulse-backend.service"}

	for _, service := range services {
		cmd := exec.Command("systemctl", "status", service)
		if err := cmd.Run(); err == nil {
			// Service exists
			if strings.HasSuffix(service, ".service") {
				return strings.TrimSuffix(service, ".service")
			}
			return service
		}
	}

	// Default to pulse-backend if no service found
	return "pulse-backend"
}

// validateBcryptHash ensures the hash is complete (60 characters)
func validateBcryptHash(hash string) error {
	if len(hash) != 60 {
		return fmt.Errorf("invalid bcrypt hash: expected 60 characters, got %d. Hash may be truncated", len(hash))
	}
	if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") && !strings.HasPrefix(hash, "$2y$") {
		return fmt.Errorf("invalid bcrypt hash: must start with $2a$, $2b$, or $2y$")
	}
	return nil
}

// isRunningAsRoot checks if the process has root privileges
func isRunningAsRoot() bool {
	return os.Geteuid() == 0
}

// handleQuickSecuritySetupFixed is the fixed version of the Quick Security Setup
func handleQuickSecuritySetupFixed(r *Router) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Apply rate limiting to prevent brute force attacks
		clientIP := GetClientIP(req)
		if !authLimiter.Allow(clientIP) {
			log.Warn().Str("ip", clientIP).Msg("Rate limit exceeded for security setup")
			http.Error(w, "Too many attempts. Please try again later.", http.StatusTooManyRequests)
			return
		}

		// Parse request body
		var setupRequest struct {
			Username            string `json:"username"`
			Password            string `json:"password"`
			APIToken            string `json:"apiToken"`
			EnableNotifications bool   `json:"enableNotifications"`
			DarkMode            bool   `json:"darkMode"`
			Force               bool   `json:"force"`
		}

		if err := json.NewDecoder(req.Body).Decode(&setupRequest); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if r.config.AuthUser != "" && r.config.AuthPass != "" && !setupRequest.Force {
			log.Info().Msg("Security setup skipped - password auth already configured")
			response := map[string]interface{}{
				"success": true,
				"skipped": true,
				"message": "Password authentication is already configured. Please remove existing security first if you want to reconfigure.",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}

		if setupRequest.Force {
			log.Info().Msg("Quick security setup invoked with force=true - rotating credentials")
		}

		// Validate inputs
		if setupRequest.Username == "" || setupRequest.Password == "" || setupRequest.APIToken == "" {
			http.Error(w, "Username, password, and API token are required", http.StatusBadRequest)
			return
		}

		// Validate password complexity
		if err := internalauth.ValidatePasswordComplexity(setupRequest.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Hash the password
		hashedPassword, err := internalauth.HashPassword(setupRequest.Password)
		if err != nil {
			log.Error().Err(err).Msg("Failed to hash password")
			http.Error(w, "Failed to process password", http.StatusInternalServerError)
			return
		}

		// Validate the bcrypt hash is complete
		if err := validateBcryptHash(hashedPassword); err != nil {
			log.Error().Err(err).Msg("Generated invalid bcrypt hash")
			http.Error(w, fmt.Sprintf("Password hashing error: %v", err), http.StatusInternalServerError)
			return
		}

		// Store the raw API token for displaying to the user
		rawAPIToken := setupRequest.APIToken

		tokenRecord, err := config.NewAPITokenRecord(rawAPIToken, "Primary token")
		if err != nil {
			log.Error().Err(err).Msg("Failed to construct API token record")
			http.Error(w, "Failed to process API token", http.StatusInternalServerError)
			return
		}
		primaryTokenHash := tokenRecord.Hash

		if r.config.HasAPITokens() && r.config.AuthUser == "" && r.config.AuthPass == "" {
			// We had API-only access before, now replacing with full security
			log.Info().Msg("Replacing API-only token with new secure token")
		}

		// Update runtime config immediately with hashed token - no restart needed!
		r.config.AuthUser = setupRequest.Username
		r.config.AuthPass = hashedPassword
		r.config.APITokens = []config.APITokenRecord{*tokenRecord}
		r.config.SortAPITokens()
		r.config.APITokenEnabled = true

		if r.persistence != nil {
			if err := r.persistence.SaveAPITokens(r.config.APITokens); err != nil {
				log.Warn().Err(err).Msg("Failed to persist API tokens during security setup")
			}
		}
		log.Info().Msg("Runtime config updated with new security settings - active immediately")

	// Save system settings to system.json
	systemSettings := config.DefaultSystemSettings()
	systemSettings.ConnectionTimeout = 10    // Default seconds
	systemSettings.AutoUpdateEnabled = false // Default disabled
	if err := r.persistence.SaveSystemSettings(*systemSettings); err != nil {
		log.Error().Err(err).Msg("Failed to save system settings")
		// Continue anyway - not critical for auth setup
	}

		// Detect environment
		isSystemd := os.Getenv("INVOCATION_ID") != ""
		isDocker := os.Getenv("PULSE_DOCKER") == "true"
		isRoot := isRunningAsRoot()

		// Detect actual service name if systemd
		serviceName := ""
		if isSystemd {
			serviceName = detectServiceName()
			log.Info().Str("service", serviceName).Msg("Detected systemd service name")
		}

		// Choose appropriate method based on environment
		if isDocker {
			// Docker: Save to /data/.env with proper quoting
			envPath := filepath.Join(r.config.ConfigPath, ".env")

			// CRITICAL: Use single quotes to prevent shell expansion of $ in bcrypt hash
			envContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
# IMPORTANT: Do not remove the single quotes around the password hash!
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
API_TOKEN='%s'
API_TOKENS='%s'
PULSE_AUDIT_LOG=true
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, primaryTokenHash, primaryTokenHash)

			// Ensure directory exists
			if err := os.MkdirAll(r.config.ConfigPath, 0755); err != nil {
				log.Error().Err(err).Str("path", r.config.ConfigPath).Msg("Failed to create config directory")
				http.Error(w, "Failed to prepare configuration directory", http.StatusInternalServerError)
				return
			}

			if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
				log.Error().Err(err).Str("path", envPath).Msg("Failed to write .env file in Docker")
				http.Error(w, "Failed to save security configuration", http.StatusInternalServerError)
				return
			}

			log.Info().Str("path", envPath).Msg("Docker security configuration saved")

			response := map[string]interface{}{
				"success":               true,
				"method":                "docker",
				"deploymentType":        "docker",
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  "Configuration saved to /data/.env for persistence across restarts.",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		} else if isSystemd && !isRoot {
			// Systemd but not root (ProxmoxVE script scenario)
			// Don't attempt sudo, just save config and provide instructions

			envPath := filepath.Join(r.config.ConfigPath, ".env")
			envContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
API_TOKEN='%s'
API_TOKENS='%s'
PULSE_AUDIT_LOG=true
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, primaryTokenHash, primaryTokenHash)

			// Save to config directory (usually /etc/pulse)
			if err := os.MkdirAll(r.config.ConfigPath, 0755); err != nil {
				log.Error().Err(err).Str("path", r.config.ConfigPath).Msg("Failed to create config directory")
				http.Error(w, "Failed to prepare configuration directory", http.StatusInternalServerError)
				return
			}

			if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
				// Try data directory as fallback
				envPath = filepath.Join(r.config.DataPath, ".env")
				if err := os.MkdirAll(r.config.DataPath, 0755); err != nil {
					log.Error().Err(err).Str("path", r.config.DataPath).Msg("Failed to create data directory")
					http.Error(w, "Failed to prepare configuration directory", http.StatusInternalServerError)
					return
				}
				if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
					log.Error().Err(err).Msg("Failed to write .env file")
					http.Error(w, "Failed to save security configuration", http.StatusInternalServerError)
					return
				}
			}

			// Create response - security is active immediately
			response := map[string]interface{}{
				"success":               true,
				"method":                "systemd-nonroot",
				"serviceName":           serviceName,
				"envFile":               envPath,
				"deploymentType":        updates.GetDeploymentType(),
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  fmt.Sprintf("Configuration saved to %s for persistence across restarts.", envPath),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		} else if isSystemd && isRoot {
			// Systemd with root - can apply directly

			// Create systemd override
			overridePath := fmt.Sprintf("/etc/systemd/system/%s.service.d/override.conf", serviceName)
			overrideDir := filepath.Dir(overridePath)

			if err := os.MkdirAll(overrideDir, 0755); err != nil {
				log.Error().Err(err).Msg("Failed to create override directory")
				http.Error(w, "Failed to create systemd override directory", http.StatusInternalServerError)
				return
			}

			overrideContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
[Service]
Environment="PULSE_AUTH_USER=%s"
Environment="PULSE_AUTH_PASS=%s"
Environment="API_TOKEN=%s"
Environment="API_TOKENS=%s"
Environment="PULSE_AUDIT_LOG=true"
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, primaryTokenHash, primaryTokenHash)

			if err := os.WriteFile(overridePath, []byte(overrideContent), 0644); err != nil {
				log.Error().Err(err).Msg("Failed to write systemd override")
				http.Error(w, "Failed to write systemd override", http.StatusInternalServerError)
				return
			}

			// Reload systemd
			if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
				log.Warn().Err(err).Msg("Failed to reload systemd daemon")
			}

			response := map[string]interface{}{
				"success":               true,
				"method":                "systemd-root",
				"serviceName":           serviceName,
				"deploymentType":        updates.GetDeploymentType(),
				"automatic":             true,
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  "Systemd override created for persistence across restarts.",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		} else {
			// Manual installation or development
			envPath := filepath.Join(r.config.ConfigPath, ".env")
			if r.config.ConfigPath == "" {
				envPath = "/etc/pulse/.env"
			}

			envContent := fmt.Sprintf(`# Auto-generated by Pulse Quick Security Setup
# Generated on %s
PULSE_AUTH_USER='%s'
PULSE_AUTH_PASS='%s'
API_TOKEN='%s'
API_TOKENS='%s'
PULSE_AUDIT_LOG=true
`, time.Now().Format(time.RFC3339), setupRequest.Username, hashedPassword, primaryTokenHash, primaryTokenHash)

			// Try to create directory if needed
			if err := os.MkdirAll(filepath.Dir(envPath), 0755); err != nil {
				log.Error().Err(err).Str("path", filepath.Dir(envPath)).Msg("Failed to create env directory")
				// Continue to attempt writing; error will be caught below
			}

			if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
				log.Error().Err(err).Msg("Failed to write .env file")
				// Still return success with manual instructions
			}

			// Get deployment type for restart instructions
			deploymentType := updates.GetDeploymentType()

			response := map[string]interface{}{
				"success":               true,
				"method":                "manual",
				"envFile":               envPath,
				"deploymentType":        deploymentType,
				"requiresManualRestart": false,
				"message":               "Security enabled immediately! Your settings are saved and active.",
				"note":                  fmt.Sprintf("Configuration saved to %s for persistence across restarts.", envPath),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		}
	}
}

// HandleRegenerateAPIToken generates a new API token and updates the .env file
func (r *Router) HandleRegenerateAPIToken(w http.ResponseWriter, rq *http.Request) {
	// Only require authentication if auth is already configured AND not disabled
	// This allows users to set up API-only access without password auth
	// When auth is disabled, allow API token generation for API-only access
	if !r.config.DisableAuth && (r.config.AuthUser != "" || r.config.AuthPass != "") && !CheckAuth(r.config, w, rq) {
		return
	}

	// Check if using proxy auth and if so, verify admin status
	if r.config.ProxyAuthSecret != "" {
		valid, username, isAdmin := CheckProxyAuth(r.config, rq)
		if !valid {
			// Proxy auth is configured but validation failed - reject immediately
			log.Warn().
				Str("ip", rq.RemoteAddr).
				Str("path", rq.URL.Path).
				Str("method", rq.Method).
				Msg("Proxy authentication failed for API token regeneration")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Authentication required"}`))
			return
		}
		if !isAdmin {
			// User is authenticated but not an admin
			log.Warn().
				Str("ip", rq.RemoteAddr).
				Str("path", rq.URL.Path).
				Str("method", rq.Method).
				Str("username", username).
				Msg("Non-admin user attempted to regenerate API token")

			// Return forbidden error
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"Admin privileges required"}`))
			return
		}
	}

	if rq.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Apply rate limiting to prevent abuse
	clientIP := GetClientIP(rq)
	if !authLimiter.Allow(clientIP) {
		log.Warn().Str("ip", clientIP).Msg("Rate limit exceeded for API token generation")
		http.Error(w, "Too many attempts. Please try again later.", http.StatusTooManyRequests)
		return
	}

	// Generate new token using the auth package
	rawToken, err := internalauth.GenerateAPIToken()
	if err != nil {
		log.Error().Err(err).Msg("Failed to generate API token")
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	tokenRecord, err := config.NewAPITokenRecord(rawToken, "Regenerated token")
	if err != nil {
		log.Error().Err(err).Msg("Failed to construct API token record")
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	r.config.APITokens = []config.APITokenRecord{*tokenRecord}
	r.config.SortAPITokens()
	r.config.APITokenEnabled = true
	log.Info().Msg("Runtime config updated with new API token - active immediately")

	if r.persistence != nil {
		if err := r.persistence.SaveAPITokens(r.config.APITokens); err != nil {
			log.Warn().Err(err).Msg("Failed to persist regenerated API token")
		}
	}

	// Determine env file path
	envPath := filepath.Join(r.config.ConfigPath, ".env")
	if r.config.ConfigPath == "" {
		envPath = "/etc/pulse/.env"
	}

	// Docker uses /data/.env
	if _, err := os.Stat("/data/.env"); err == nil {
		envPath = "/data/.env"
	}

	// Read existing .env file
	content, err := os.ReadFile(envPath)
	if err != nil {
		log.Error().Err(err).Str("path", envPath).Msg("Failed to read .env file")
		http.Error(w, "Security configuration not found", http.StatusNotFound)
		return
	}

	// Update the API_TOKEN / API_TOKENS lines with the hashed token
	lines := strings.Split(string(content), "\n")
	var updatedPrimary bool
	var updatedList bool
	for i, line := range lines {
		if strings.HasPrefix(line, "API_TOKEN=") {
			lines[i] = fmt.Sprintf("API_TOKEN=%s", tokenRecord.Hash)
			updatedPrimary = true
		}
		if strings.HasPrefix(line, "API_TOKENS=") {
			lines[i] = fmt.Sprintf("API_TOKENS=%s", tokenRecord.Hash)
			updatedList = true
		}
	}

	if !updatedPrimary {
		// API_TOKEN line not found, add it
		lines = append(lines, fmt.Sprintf("API_TOKEN=%s", tokenRecord.Hash))
	}
	if !updatedList {
		lines = append(lines, fmt.Sprintf("API_TOKENS=%s", tokenRecord.Hash))
	}

	// Write updated content back
	newContent := strings.Join(lines, "\n")
	if err := os.WriteFile(envPath, []byte(newContent), 0600); err != nil {
		log.Error().Err(err).Msg("Failed to update .env file")
		http.Error(w, "Failed to save new token", http.StatusInternalServerError)
		return
	}

	log.Info().Msg("API token regenerated successfully")

	// Get deployment type for restart instructions
	deploymentType := updates.GetDeploymentType()

	response := map[string]interface{}{
		"success":         true,
		"token":           rawToken, // Return the raw token to the user (only shown once!)
		"deploymentType":  deploymentType,
		"requiresRestart": false,
		"message":         "New API token generated and active immediately! Save this token - it won't be shown again.",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleValidateAPIToken validates an API token without logging it
func (r *Router) HandleValidateAPIToken(w http.ResponseWriter, rq *http.Request) {
	if rq.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Require authentication to prevent unauthenticated token guessing oracle
	// Use same auth logic as regenerate-token endpoint
	if !r.config.DisableAuth && (r.config.AuthUser != "" || r.config.AuthPass != "") && !CheckAuth(r.config, w, rq) {
		return
	}

	// Check if using proxy auth and if so, verify admin status
	if r.config.ProxyAuthSecret != "" {
		valid, username, isAdmin := CheckProxyAuth(r.config, rq)
		if !valid {
			// Proxy auth is configured but validation failed - reject immediately
			log.Warn().
				Str("ip", rq.RemoteAddr).
				Str("path", rq.URL.Path).
				Str("method", rq.Method).
				Msg("Proxy authentication failed for API token validation")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Authentication required"}`))
			return
		}
		if !isAdmin {
			// User is authenticated but not an admin
			log.Warn().
				Str("ip", rq.RemoteAddr).
				Str("path", rq.URL.Path).
				Str("method", rq.Method).
				Str("username", username).
				Msg("Non-admin user attempted to validate API token")

			// Return forbidden error
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"Admin privileges required"}`))
			return
		}
	}

	// Apply rate limiting to prevent brute force attacks
	clientIP := GetClientIP(rq)
	if !authLimiter.Allow(clientIP) {
		log.Warn().Str("ip", clientIP).Msg("Rate limit exceeded for API token validation")
		http.Error(w, "Too many attempts. Please try again later.", http.StatusTooManyRequests)
		return
	}

	// Parse request body
	var validateRequest struct {
		Token string `json:"token"`
	}

	if err := json.NewDecoder(rq.Body).Decode(&validateRequest); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if validateRequest.Token == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":   false,
			"message": "Token is required",
		})
		return
	}

	// Check if API token auth is enabled
	if !r.config.APITokenEnabled || !r.config.HasAPITokens() {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":   false,
			"message": "API token authentication is not configured",
		})
		return
	}

	// Validate the token (compare hash)
	_, isValid := r.config.ValidateAPIToken(validateRequest.Token)

	// Log validation attempt without logging the token itself
	if isValid {
		log.Debug().
			Str("ip", clientIP).
			Msg("API token validation successful")
	} else {
		log.Warn().
			Str("ip", clientIP).
			Msg("API token validation failed")
	}

	// Return validation result
	response := map[string]interface{}{
		"valid": isValid,
	}

	if isValid {
		response["message"] = "Token is valid"
	} else {
		response["message"] = "Token is invalid"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
