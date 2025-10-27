package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RouXx67/PulseUp/internal/alerts"
	"github.com/RouXx67/PulseUp/internal/mock"
	"github.com/RouXx67/PulseUp/internal/models"
	"github.com/RouXx67/PulseUp/internal/monitoring"
	"github.com/RouXx67/PulseUp/internal/utils"
	"github.com/RouXx67/PulseUp/internal/websocket"
	"github.com/rs/zerolog/log"
)

// AlertHandlers handles alert-related HTTP endpoints
type AlertHandlers struct {
	monitor *monitoring.Monitor
	wsHub   *websocket.Hub
}

// NewAlertHandlers creates new alert handlers
func NewAlertHandlers(monitor *monitoring.Monitor, wsHub *websocket.Hub) *AlertHandlers {
	return &AlertHandlers{
		monitor: monitor,
		wsHub:   wsHub,
	}
}

// SetMonitor updates the monitor reference for alert handlers.
func (h *AlertHandlers) SetMonitor(m *monitoring.Monitor) {
	h.monitor = m
}

// validateAlertID validates an alert ID for security
func validateAlertID(alertID string) bool {
	// Guard against empty strings or extremely large payloads that could impact memory usage.
	if len(alertID) == 0 || len(alertID) > 500 {
		return false
	}

	// Reject attempts to traverse directories via crafted path segments.
	if strings.Contains(alertID, "../") || strings.Contains(alertID, "/..") {
		return false
	}

	// Permit any printable ASCII character (including spaces) so user supplied identifiers
	// like instance names remain valid, while excluding control characters and DEL.
	for _, r := range alertID {
		if r < 32 || r == 127 {
			return false
		}
	}

	return true
}

// GetAlertConfig returns the current alert configuration
func (h *AlertHandlers) GetAlertConfig(w http.ResponseWriter, r *http.Request) {
	config := h.monitor.GetAlertManager().GetConfig()

	if err := utils.WriteJSONResponse(w, config); err != nil {
		log.Error().Err(err).Msg("Failed to write alert config response")
	}
}

// UpdateAlertConfig updates the alert configuration
func (h *AlertHandlers) UpdateAlertConfig(w http.ResponseWriter, r *http.Request) {
	var config alerts.AlertConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.monitor.GetAlertManager().UpdateConfig(config)

	// Update notification manager with schedule settings
	if config.Schedule.Cooldown > 0 {
		h.monitor.GetNotificationManager().SetCooldown(config.Schedule.Cooldown)
	}
	if config.Schedule.GroupingWindow > 0 {
		h.monitor.GetNotificationManager().SetGroupingWindow(config.Schedule.GroupingWindow)
	} else if config.Schedule.Grouping.Window > 0 {
		h.monitor.GetNotificationManager().SetGroupingWindow(config.Schedule.Grouping.Window)
	}
	h.monitor.GetNotificationManager().SetGroupingOptions(
		config.Schedule.Grouping.ByNode,
		config.Schedule.Grouping.ByGuest,
	)

	// Save to persistent storage
	if err := h.monitor.GetConfigPersistence().SaveAlertConfig(config); err != nil {
		// Log error but don't fail the request
		log.Error().Err(err).Msg("Failed to save alert configuration")
	}

	if err := utils.WriteJSONResponse(w, map[string]interface{}{
		"success": true,
		"message": "Alert configuration updated successfully",
	}); err != nil {
		log.Error().Err(err).Msg("Failed to write alert config update response")
	}
}

// ActivateAlerts activates alert notifications
func (h *AlertHandlers) ActivateAlerts(w http.ResponseWriter, r *http.Request) {
	// Get current config
	config := h.monitor.GetAlertManager().GetConfig()

	// Check if already active
	if config.ActivationState == alerts.ActivationActive {
		if err := utils.WriteJSONResponse(w, map[string]interface{}{
			"success":        true,
			"message":        "Alerts already activated",
			"state":          string(config.ActivationState),
			"activationTime": config.ActivationTime,
		}); err != nil {
			log.Error().Err(err).Msg("Failed to write activate response")
		}
		return
	}

	// Activate notifications
	now := time.Now()
	config.ActivationState = alerts.ActivationActive
	config.ActivationTime = &now

	// Update config
	h.monitor.GetAlertManager().UpdateConfig(config)

	// Save to persistent storage
	if err := h.monitor.GetConfigPersistence().SaveAlertConfig(config); err != nil {
		log.Error().Err(err).Msg("Failed to save alert configuration after activation")
		http.Error(w, "Failed to save configuration", http.StatusInternalServerError)
		return
	}

	// Notify about existing critical alerts after activation
	activeAlerts := h.monitor.GetAlertManager().GetActiveAlerts()
	criticalCount := 0
	for _, alert := range activeAlerts {
		if alert.Level == alerts.AlertLevelCritical && !alert.Acknowledged {
			// Re-dispatch critical alerts to trigger notifications
			h.monitor.GetAlertManager().NotifyExistingAlert(alert.ID)
			criticalCount++
		}
	}

	if criticalCount > 0 {
		log.Info().
			Int("criticalAlerts", criticalCount).
			Msg("Sent notifications for existing critical alerts after activation")
	}

	log.Info().Msg("Alert notifications activated")

	if err := utils.WriteJSONResponse(w, map[string]interface{}{
		"success":        true,
		"message":        "Alert notifications activated",
		"state":          string(config.ActivationState),
		"activationTime": config.ActivationTime,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to write activate response")
	}
}

// GetActiveAlerts returns all active alerts
func (h *AlertHandlers) GetActiveAlerts(w http.ResponseWriter, r *http.Request) {
	alerts := h.monitor.GetAlertManager().GetActiveAlerts()

	if err := utils.WriteJSONResponse(w, alerts); err != nil {
		log.Error().Err(err).Msg("Failed to write active alerts response")
	}
}

// GetAlertHistory returns alert history
func (h *AlertHandlers) GetAlertHistory(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	limit := 100
	if limitStr := query.Get("limit"); limitStr != "" {
		l, err := strconv.Atoi(limitStr)
		switch {
		case err != nil:
			log.Warn().Str("limit", limitStr).Msg("Invalid limit parameter, using default")
		case l < 0:
			http.Error(w, "limit must be non-negative", http.StatusBadRequest)
			return
		case l == 0:
			limit = 0
		case l > 10000:
			log.Warn().Int("limit", l).Msg("Limit exceeds maximum, capping at 10000")
			limit = 10000
		default:
			limit = l
		}
	}

	offset := 0
	if offsetStr := query.Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			if o < 0 {
				http.Error(w, "offset must be non-negative", http.StatusBadRequest)
				return
			}
			offset = o
		} else {
			log.Warn().Str("offset", offsetStr).Msg("Invalid offset parameter, ignoring")
		}
	}

	var startTime *time.Time
	if startStr := query.Get("startTime"); startStr != "" {
		parsed, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			http.Error(w, "invalid startTime parameter", http.StatusBadRequest)
			return
		}
		startTime = &parsed
	}

	var endTime *time.Time
	if endStr := query.Get("endTime"); endStr != "" {
		parsed, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			http.Error(w, "invalid endTime parameter", http.StatusBadRequest)
			return
		}
		endTime = &parsed
	}

	if startTime != nil && endTime != nil && endTime.Before(*startTime) {
		http.Error(w, "endTime must be after startTime", http.StatusBadRequest)
		return
	}

	severity := strings.ToLower(strings.TrimSpace(query.Get("severity")))
	switch severity {
	case "", "all":
		severity = ""
	case "warning", "critical":
	default:
		log.Warn().Str("severity", severity).Msg("Invalid severity filter, ignoring")
		severity = ""
	}

	resourceID := strings.TrimSpace(query.Get("resourceId"))

	matchesFilters := func(alertTime time.Time, alertLevel string, alertResourceID string) bool {
		if startTime != nil && alertTime.Before(*startTime) {
			return false
		}
		if endTime != nil && alertTime.After(*endTime) {
			return false
		}
		if severity != "" && !strings.EqualFold(alertLevel, severity) {
			return false
		}
		if resourceID != "" && alertResourceID != resourceID {
			return false
		}
		return true
	}

	trimAlerts := func(alerts []alerts.Alert) []alerts.Alert {
		if offset > 0 {
			if offset >= len(alerts) {
				return alerts[:0]
			}
			alerts = alerts[offset:]
		}
		if limit > 0 && len(alerts) > limit {
			alerts = alerts[:limit]
		}
		return alerts
	}

	trimMockAlerts := func(alerts []models.Alert) []models.Alert {
		if offset > 0 {
			if offset >= len(alerts) {
				return alerts[:0]
			}
			alerts = alerts[offset:]
		}
		if limit > 0 && len(alerts) > limit {
			alerts = alerts[:limit]
		}
		return alerts
	}

	// Check if mock mode is enabled
	mockEnabled := mock.IsMockEnabled()
	log.Debug().Bool("mockEnabled", mockEnabled).Msg("GetAlertHistory: mock mode status")

	fetchLimit := limit
	if fetchLimit > 0 && offset > 0 {
		fetchLimit += offset
	}

	if mockEnabled {
		mockHistory := mock.GetMockAlertHistory(fetchLimit)
		filtered := make([]models.Alert, 0, len(mockHistory))
		for _, alert := range mockHistory {
			if matchesFilters(alert.StartTime, alert.Level, alert.ResourceID) {
				filtered = append(filtered, alert)
			}
		}
		filtered = trimMockAlerts(filtered)
		if err := utils.WriteJSONResponse(w, filtered); err != nil {
			log.Error().Err(err).Msg("Failed to write mock alert history response")
		}
		return
	}

	if h.monitor == nil {
		http.Error(w, "monitor is not initialized", http.StatusServiceUnavailable)
		return
	}

	manager := h.monitor.GetAlertManager()
	var history []alerts.Alert
	if startTime != nil {
		history = manager.GetAlertHistorySince(*startTime, fetchLimit)
	} else {
		history = manager.GetAlertHistory(fetchLimit)
	}

	filtered := make([]alerts.Alert, 0, len(history))
	for _, alert := range history {
		if matchesFilters(alert.StartTime, string(alert.Level), alert.ResourceID) {
			filtered = append(filtered, alert)
		}
	}
	filtered = trimAlerts(filtered)

	if err := utils.WriteJSONResponse(w, filtered); err != nil {
		log.Error().Err(err).Msg("Failed to write alert history response")
	}
}

// ClearAlertHistory clears all alert history
func (h *AlertHandlers) ClearAlertHistory(w http.ResponseWriter, r *http.Request) {
	if err := h.monitor.GetAlertManager().ClearAlertHistory(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := utils.WriteJSONResponse(w, map[string]string{"status": "success", "message": "Alert history cleared"}); err != nil {
		log.Error().Err(err).Msg("Failed to write alert history clear confirmation")
	}
}

// UnacknowledgeAlert removes acknowledged status from an alert
func (h *AlertHandlers) UnacknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	// Extract alert ID from URL path: /api/alerts/{id}/unacknowledge
	path := strings.TrimPrefix(r.URL.Path, "/api/alerts/")

	const suffix = "/unacknowledge"
	if !strings.HasSuffix(path, suffix) {
		log.Error().
			Str("path", r.URL.Path).
			Msg("Path does not end with /unacknowledge")
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	// Extract alert ID by removing the suffix and decoding encoded characters
	encodedID := strings.TrimSuffix(path, suffix)
	alertID, err := url.PathUnescape(encodedID)
	if err != nil {
		log.Error().Err(err).Str("encodedID", encodedID).Msg("Failed to decode alert ID")
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}

	if !validateAlertID(alertID) {
		log.Error().
			Str("path", r.URL.Path).
			Str("alertID", alertID).
			Msg("Invalid alert ID")
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}

	// Log the unacknowledge attempt
	log.Debug().
		Str("alertID", alertID).
		Str("path", r.URL.Path).
		Msg("Attempting to unacknowledge alert")

	if err := h.monitor.GetAlertManager().UnacknowledgeAlert(alertID); err != nil {
		log.Error().
			Err(err).
			Str("alertID", alertID).
			Msg("Failed to unacknowledge alert")
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	h.monitor.SyncAlertState()

	log.Info().
		Str("alertID", alertID).
		Msg("Alert unacknowledged successfully")

	// Send response immediately
	if err := utils.WriteJSONResponse(w, map[string]bool{"success": true}); err != nil {
		log.Error().Err(err).Str("alertID", alertID).Msg("Failed to write unacknowledge response")
	}

	// Broadcast updated state to all WebSocket clients after response
	// Do this in a goroutine to avoid blocking the HTTP response
	if h.wsHub != nil {
		go func() {
			state := h.monitor.GetState()
			h.wsHub.BroadcastState(state.ToFrontend())
			log.Debug().Msg("Broadcasted state after alert unacknowledgment")
		}()
	}
}

// AcknowledgeAlert acknowledges an alert
func (h *AlertHandlers) AcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	// Extract alert ID from URL path: /api/alerts/{id}/acknowledge
	// Alert IDs can contain slashes (e.g., "pve1:qemu/101-cpu")
	// So we need to find the /acknowledge suffix and extract everything before it
	path := strings.TrimPrefix(r.URL.Path, "/api/alerts/")

	const suffix = "/acknowledge"
	if !strings.HasSuffix(path, suffix) {
		log.Error().
			Str("path", r.URL.Path).
			Msg("Path does not end with /acknowledge")
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	// Extract alert ID by removing the suffix and decoding encoded characters
	encodedID := strings.TrimSuffix(path, suffix)
	alertID, err := url.PathUnescape(encodedID)
	if err != nil {
		log.Error().Err(err).Str("encodedID", encodedID).Msg("Failed to decode alert ID")
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}

	if !validateAlertID(alertID) {
		log.Error().
			Str("path", r.URL.Path).
			Str("alertID", alertID).
			Msg("Invalid alert ID")
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}

	// Log the acknowledge attempt
	log.Debug().
		Str("alertID", alertID).
		Str("path", r.URL.Path).
		Msg("Attempting to acknowledge alert")

	// In a real implementation, you'd get the user from authentication
	user := "admin"

	log.Debug().
		Str("alertID", alertID).
		Msg("About to call AcknowledgeAlert on manager")

	if err := h.monitor.GetAlertManager().AcknowledgeAlert(alertID, user); err != nil {
		log.Error().
			Err(err).
			Str("alertID", alertID).
			Msg("Failed to acknowledge alert")
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	h.monitor.SyncAlertState()

	log.Info().
		Str("alertID", alertID).
		Str("user", user).
		Msg("Alert acknowledged successfully")

	// Send response immediately
	if err := utils.WriteJSONResponse(w, map[string]bool{"success": true}); err != nil {
		log.Error().Err(err).Str("alertID", alertID).Msg("Failed to write acknowledge response")
	}

	// Broadcast updated state to all WebSocket clients after response
	// Do this in a goroutine to avoid blocking the HTTP response
	if h.wsHub != nil {
		go func() {
			state := h.monitor.GetState()
			h.wsHub.BroadcastState(state.ToFrontend())
			log.Debug().Msg("Broadcasted state after alert acknowledgment")
		}()
	}
}

// ClearAlert manually clears an alert
func (h *AlertHandlers) ClearAlert(w http.ResponseWriter, r *http.Request) {
	// Extract alert ID from URL path: /api/alerts/{id}/clear
	// Alert IDs can contain slashes (e.g., "pve1:qemu/101-cpu")
	// So we need to find the /clear suffix and extract everything before it
	path := strings.TrimPrefix(r.URL.Path, "/api/alerts/")

	const suffix = "/clear"
	if !strings.HasSuffix(path, suffix) {
		log.Error().
			Str("path", r.URL.Path).
			Msg("Path does not end with /clear")
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	// Extract alert ID by removing the suffix and decoding encoded characters
	encodedID := strings.TrimSuffix(path, suffix)
	alertID, err := url.PathUnescape(encodedID)
	if err != nil {
		log.Error().Err(err).Str("encodedID", encodedID).Msg("Failed to decode alert ID")
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}

	if !validateAlertID(alertID) {
		log.Error().
			Str("path", r.URL.Path).
			Str("alertID", alertID).
			Msg("Invalid alert ID")
		http.Error(w, "Invalid alert ID", http.StatusBadRequest)
		return
	}

	h.monitor.GetAlertManager().ClearAlert(alertID)
	h.monitor.SyncAlertState()

	// Send response immediately
	if err := utils.WriteJSONResponse(w, map[string]bool{"success": true}); err != nil {
		log.Error().Err(err).Str("alertID", alertID).Msg("Failed to write clear alert response")
	}

	// Broadcast updated state to all WebSocket clients after response
	// Do this in a goroutine to avoid blocking the HTTP response
	if h.wsHub != nil {
		go func() {
			state := h.monitor.GetState()
			h.wsHub.BroadcastState(state.ToFrontend())
			log.Debug().Msg("Broadcasted state after alert clear")
		}()
	}
}

// BulkAcknowledgeAlerts acknowledges multiple alerts at once
func (h *AlertHandlers) BulkAcknowledgeAlerts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AlertIDs []string `json:"alertIds"`
		User     string   `json:"user,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(request.AlertIDs) == 0 {
		http.Error(w, "No alert IDs provided", http.StatusBadRequest)
		return
	}

	user := request.User
	if user == "" {
		user = "admin"
	}

	var (
		results    []map[string]interface{}
		anySuccess bool
	)
	for _, alertID := range request.AlertIDs {
		result := map[string]interface{}{
			"alertId": alertID,
			"success": true,
		}
		if err := h.monitor.GetAlertManager().AcknowledgeAlert(alertID, user); err != nil {
			result["success"] = false
			result["error"] = err.Error()
		} else {
			anySuccess = true
		}
		results = append(results, result)
	}

	if anySuccess {
		h.monitor.SyncAlertState()
	}

	// Send response immediately
	if err := utils.WriteJSONResponse(w, map[string]interface{}{
		"results": results,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to write bulk acknowledge response")
	}

	// Broadcast updated state to all WebSocket clients if any alerts were acknowledged
	// Do this in a goroutine to avoid blocking the HTTP response
	if h.wsHub != nil && anySuccess {
		go func() {
			state := h.monitor.GetState()
			h.wsHub.BroadcastState(state.ToFrontend())
			log.Debug().Msg("Broadcasted state after bulk alert acknowledgment")
		}()
	}
}

// BulkClearAlerts clears multiple alerts at once
func (h *AlertHandlers) BulkClearAlerts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		AlertIDs []string `json:"alertIds"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(request.AlertIDs) == 0 {
		http.Error(w, "No alert IDs provided", http.StatusBadRequest)
		return
	}

	var (
		results    []map[string]interface{}
		anySuccess bool
	)
	for _, alertID := range request.AlertIDs {
		result := map[string]interface{}{
			"alertId": alertID,
			"success": true,
		}
		h.monitor.GetAlertManager().ClearAlert(alertID)
		anySuccess = true
		results = append(results, result)
	}

	if anySuccess {
		h.monitor.SyncAlertState()
	}

	// Send response immediately
	if err := utils.WriteJSONResponse(w, map[string]interface{}{
		"results": results,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to write bulk clear response")
	}

	// Broadcast updated state to all WebSocket clients after response
	// Do this in a goroutine to avoid blocking the HTTP response
	if h.wsHub != nil && anySuccess {
		go func() {
			state := h.monitor.GetState()
			h.wsHub.BroadcastState(state.ToFrontend())
			log.Debug().Msg("Broadcasted state after bulk alert clear")
		}()
	}
}

// HandleAlerts routes alert requests to appropriate handlers
func (h *AlertHandlers) HandleAlerts(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/alerts/")

	log.Debug().
		Str("originalPath", r.URL.Path).
		Str("trimmedPath", path).
		Str("method", r.Method).
		Msg("HandleAlerts routing request")

	switch {
	case path == "config" && r.Method == http.MethodGet:
		h.GetAlertConfig(w, r)
	case path == "config" && r.Method == http.MethodPut:
		h.UpdateAlertConfig(w, r)
	case path == "activate" && r.Method == http.MethodPost:
		h.ActivateAlerts(w, r)
	case path == "active" && r.Method == http.MethodGet:
		h.GetActiveAlerts(w, r)
	case path == "history" && r.Method == http.MethodGet:
		h.GetAlertHistory(w, r)
	case path == "history" && r.Method == http.MethodDelete:
		h.ClearAlertHistory(w, r)
	case path == "bulk/acknowledge" && r.Method == http.MethodPost:
		h.BulkAcknowledgeAlerts(w, r)
	case path == "bulk/clear" && r.Method == http.MethodPost:
		h.BulkClearAlerts(w, r)
	case strings.HasSuffix(path, "/acknowledge") && r.Method == http.MethodPost:
		h.AcknowledgeAlert(w, r)
	case strings.HasSuffix(path, "/unacknowledge") && r.Method == http.MethodPost:
		h.UnacknowledgeAlert(w, r)
	case strings.HasSuffix(path, "/clear") && r.Method == http.MethodPost:
		h.ClearAlert(w, r)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}
