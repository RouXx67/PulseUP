package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rcourtman/pulse-go-rewrite/internal/monitoring"
	"github.com/rcourtman/pulse-go-rewrite/internal/utils"
	"github.com/rcourtman/pulse-go-rewrite/internal/websocket"
	agentsdocker "github.com/rcourtman/pulse-go-rewrite/pkg/agents/docker"
	"github.com/rs/zerolog/log"
)

// DockerAgentHandlers manages ingest from the external Docker agent.
type DockerAgentHandlers struct {
	monitor *monitoring.Monitor
	wsHub   *websocket.Hub
}

type dockerCommandAckRequest struct {
	HostID  string `json:"hostId"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// NewDockerAgentHandlers constructs a new Docker agent handler group.
func NewDockerAgentHandlers(m *monitoring.Monitor, hub *websocket.Hub) *DockerAgentHandlers {
	return &DockerAgentHandlers{monitor: m, wsHub: hub}
}

// SetMonitor updates the monitor reference for docker agent handlers.
func (h *DockerAgentHandlers) SetMonitor(m *monitoring.Monitor) {
	h.monitor = m
}

// HandleReport accepts heartbeat payloads from the Docker agent.
func (h *DockerAgentHandlers) HandleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed", nil)
		return
	}

	defer r.Body.Close()

	var report agentsdocker.Report
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_json", "Failed to decode request body", map[string]string{"error": err.Error()})
		return
	}

	if report.Timestamp.IsZero() {
		report.Timestamp = time.Now()
	}

	tokenRecord := getAPITokenRecordFromRequest(r)

	host, err := h.monitor.ApplyDockerReport(report, tokenRecord)
	if err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_report", err.Error(), nil)
		return
	}

	log.Debug().
		Str("dockerHost", host.Hostname).
		Int("containers", len(host.Containers)).
		Msg("Docker agent report processed")

	// Broadcast the updated state for near-real-time UI updates
	go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

	response := map[string]any{
		"success":    true,
		"hostId":     host.ID,
		"containers": len(host.Containers),
		"lastSeen":   host.LastSeen,
	}

	if payload, cmd := h.monitor.FetchDockerCommandForHost(host.ID); cmd != nil {
		commandResponse := map[string]any{
			"id":   cmd.ID,
			"type": cmd.Type,
		}
		if payload != nil && len(payload) > 0 {
			commandResponse["payload"] = payload
		}
		response["commands"] = []map[string]any{commandResponse}
	}

	if err := utils.WriteJSONResponse(w, response); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker agent response")
	}
}

// HandleDockerHostActions routes docker host management actions based on path and method.
func (h *DockerAgentHandlers) HandleDockerHostActions(w http.ResponseWriter, r *http.Request) {
	// Check if this is an allow reenroll request
	if strings.HasSuffix(r.URL.Path, "/allow-reenroll") && r.Method == http.MethodPost {
		h.HandleAllowReenroll(w, r)
		return
	}

	// Check if this is an unhide request
	if strings.HasSuffix(r.URL.Path, "/unhide") && r.Method == http.MethodPut {
		h.HandleUnhideHost(w, r)
		return
	}

	// Check if this is a pending uninstall request
	if strings.HasSuffix(r.URL.Path, "/pending-uninstall") && r.Method == http.MethodPut {
		h.HandleMarkPendingUninstall(w, r)
		return
	}

	// Otherwise, handle as delete/hide request
	if r.Method == http.MethodDelete {
		h.HandleDeleteHost(w, r)
		return
	}

	writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", nil)
}

// HandleCommandAck processes acknowledgements from docker agents for issued commands.
func (h *DockerAgentHandlers) HandleCommandAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed", nil)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/api/agents/docker/commands/")
	if !strings.HasSuffix(trimmed, "/ack") {
		writeErrorResponse(w, http.StatusNotFound, "not_found", "Endpoint not found", nil)
		return
	}
	commandID := strings.TrimSuffix(trimmed, "/ack")
	commandID = strings.TrimSuffix(commandID, "/")
	commandID = strings.TrimSpace(commandID)
	if commandID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_command_id", "Command ID is required", nil)
		return
	}

	var req dockerCommandAckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid_json", "Failed to decode request body", map[string]string{"error": err.Error()})
		return
	}

	status := strings.ToLower(strings.TrimSpace(req.Status))
	switch status {
	case "", "ack", "acknowledged":
		status = monitoring.DockerCommandStatusAcknowledged
	case "success", "completed", "complete":
		status = monitoring.DockerCommandStatusCompleted
	case "fail", "failed", "error":
		status = monitoring.DockerCommandStatusFailed
	default:
		writeErrorResponse(w, http.StatusBadRequest, "invalid_status", "Invalid command status", nil)
		return
	}

	commandStatus, hostID, shouldRemove, err := h.monitor.AcknowledgeDockerHostCommand(commandID, req.HostID, status, req.Message)
	if err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "docker_command_ack_failed", err.Error(), nil)
		return
	}

	if shouldRemove {
		if _, removeErr := h.monitor.RemoveDockerHost(hostID); removeErr != nil {
			log.Error().Err(removeErr).Str("dockerHostID", hostID).Str("commandID", commandID).Msg("Failed to remove docker host after command completion")
		}
	}

	go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

	if err := utils.WriteJSONResponse(w, map[string]any{
		"success": true,
		"hostId":  hostID,
		"command": commandStatus,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker command acknowledgement response")
	}
}

// HandleDeleteHost removes or hides a docker host from the shared state.
// If query parameter ?hide=true is provided, the host is marked as hidden instead of deleted.
func (h *DockerAgentHandlers) HandleDeleteHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only DELETE is allowed", nil)
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, "/api/agents/docker/hosts/")
	hostID := strings.TrimSpace(trimmedPath)
	if hostID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_host_id", "Docker host ID is required", nil)
		return
	}

	// Check if we should hide instead of delete
	hideParam := r.URL.Query().Get("hide")
	shouldHide := strings.ToLower(hideParam) == "true"
	forceParam := strings.ToLower(r.URL.Query().Get("force"))
	force := forceParam == "true" || strings.ToLower(r.URL.Query().Get("mode")) == "force"

	priorHost, hostExists := h.monitor.GetDockerHost(hostID)

	if shouldHide {
		if !hostExists {
			writeErrorResponse(w, http.StatusNotFound, "docker_host_not_found", "Docker host not found", nil)
			return
		}
		host, err := h.monitor.HideDockerHost(hostID)
		if err != nil {
			writeErrorResponse(w, http.StatusNotFound, "docker_host_not_found", err.Error(), nil)
			return
		}

		go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

		if err := utils.WriteJSONResponse(w, map[string]any{
			"success": true,
			"hostId":  host.ID,
			"message": "Docker host hidden",
		}); err != nil {
			log.Error().Err(err).Msg("Failed to serialize docker host operation response")
		}
		return
	}

	if !hostExists {
		if force {
			if err := utils.WriteJSONResponse(w, map[string]any{
				"success": true,
				"hostId":  hostID,
				"message": "Docker host already removed",
			}); err != nil {
				log.Error().Err(err).Msg("Failed to serialize docker host operation response")
			}
			return
		}

		writeErrorResponse(w, http.StatusNotFound, "docker_host_not_found", "Docker host not found", nil)
		return
	}

	if !force && strings.EqualFold(priorHost.Status, "online") {
		command, err := h.monitor.QueueDockerHostStop(hostID)
		if err != nil {
			writeErrorResponse(w, http.StatusBadRequest, "docker_command_failed", err.Error(), nil)
			return
		}

		go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

		if err := utils.WriteJSONResponse(w, map[string]any{
			"success": true,
			"hostId":  hostID,
			"command": command,
			"message": "Stop command queued",
		}); err != nil {
			log.Error().Err(err).Msg("Failed to serialize docker host stop command response")
		}
		return
	}

	host, err := h.monitor.RemoveDockerHost(hostID)
	if err != nil {
		writeErrorResponse(w, http.StatusNotFound, "docker_host_not_found", err.Error(), nil)
		return
	}

	go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

	if err := utils.WriteJSONResponse(w, map[string]any{
		"success": true,
		"hostId":  host.ID,
		"message": "Docker host removed",
	}); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker host operation response")
	}
}

// HandleAllowReenroll clears the removal block for a docker host to permit future reports.
func (h *DockerAgentHandlers) HandleAllowReenroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed", nil)
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, "/api/agents/docker/hosts/")
	trimmedPath = strings.TrimSuffix(trimmedPath, "/allow-reenroll")
	hostID := strings.TrimSpace(trimmedPath)
	if hostID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_host_id", "Docker host ID is required", nil)
		return
	}

	if err := h.monitor.AllowDockerHostReenroll(hostID); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "docker_host_reenroll_failed", err.Error(), nil)
		return
	}

	if err := utils.WriteJSONResponse(w, map[string]any{
		"success": true,
		"hostId":  hostID,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker host allow reenroll response")
	}
}

// HandleUnhideHost unhides a previously hidden docker host.
func (h *DockerAgentHandlers) HandleUnhideHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only PUT is allowed", nil)
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, "/api/agents/docker/hosts/")
	trimmedPath = strings.TrimSuffix(trimmedPath, "/unhide")
	hostID := strings.TrimSpace(trimmedPath)
	if hostID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_host_id", "Docker host ID is required", nil)
		return
	}

	host, err := h.monitor.UnhideDockerHost(hostID)
	if err != nil {
		writeErrorResponse(w, http.StatusNotFound, "docker_host_not_found", err.Error(), nil)
		return
	}

	go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

	if err := utils.WriteJSONResponse(w, map[string]any{
		"success": true,
		"hostId":  host.ID,
		"message": "Docker host unhidden",
	}); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker host unhide response")
	}
}

// HandleMarkPendingUninstall marks a docker host as pending uninstall.
func (h *DockerAgentHandlers) HandleMarkPendingUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeErrorResponse(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only PUT is allowed", nil)
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, "/api/agents/docker/hosts/")
	trimmedPath = strings.TrimSuffix(trimmedPath, "/pending-uninstall")
	hostID := strings.TrimSpace(trimmedPath)
	if hostID == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing_host_id", "Docker host ID is required", nil)
		return
	}

	host, err := h.monitor.MarkDockerHostPendingUninstall(hostID)
	if err != nil {
		writeErrorResponse(w, http.StatusNotFound, "docker_host_not_found", err.Error(), nil)
		return
	}

	go h.wsHub.BroadcastState(h.monitor.GetState().ToFrontend())

	if err := utils.WriteJSONResponse(w, map[string]any{
		"success": true,
		"hostId":  host.ID,
		"message": "Docker host marked as pending uninstall",
	}); err != nil {
		log.Error().Err(err).Msg("Failed to serialize docker host pending uninstall response")
	}
}
