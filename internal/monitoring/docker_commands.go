package monitoring

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rcourtman/pulse-go-rewrite/internal/models"
	"github.com/rs/zerolog/log"
)

const (
	// DockerCommandTypeStop instructs the agent to stop reporting and uninstall itself.
	DockerCommandTypeStop = "stop"

	// DockerCommandStatusQueued indicates the command is queued and waiting to be dispatched.
	DockerCommandStatusQueued = "queued"
	// DockerCommandStatusDispatched indicates the command has been delivered to the agent.
	DockerCommandStatusDispatched = "dispatched"
	// DockerCommandStatusAcknowledged indicates the agent acknowledged receipt of the command.
	DockerCommandStatusAcknowledged = "acknowledged"
	// DockerCommandStatusCompleted indicates the command completed successfully.
	DockerCommandStatusCompleted = "completed"
	// DockerCommandStatusFailed indicates the command failed.
	DockerCommandStatusFailed = "failed"
	// DockerCommandStatusExpired indicates Pulse abandoned the command.
	DockerCommandStatusExpired = "expired"
)

const (
	dockerCommandDefaultTTL = 10 * time.Minute
)

type dockerHostCommand struct {
	status  models.DockerHostCommandStatus
	payload map[string]any
	ttl     time.Time
}

func newDockerHostCommand(commandType string, message string, ttl time.Duration, payload map[string]any) dockerHostCommand {
	now := time.Now().UTC()
	var expiresAt *time.Time
	if ttl > 0 {
		expiry := now.Add(ttl)
		expiresAt = &expiry
	}
	return dockerHostCommand{
		status: models.DockerHostCommandStatus{
			ID:        uuid.NewString(),
			Type:      commandType,
			Status:    DockerCommandStatusQueued,
			Message:   message,
			CreatedAt: now,
			UpdatedAt: now,
			ExpiresAt: expiresAt,
		},
		payload: payload,
		ttl:     now.Add(ttl),
	}
}

func (cmd *dockerHostCommand) markDispatched() {
	now := time.Now().UTC()
	cmd.status.Status = DockerCommandStatusDispatched
	cmd.status.DispatchedAt = &now
	cmd.status.UpdatedAt = now
}

func (cmd *dockerHostCommand) markAcknowledged(message string) {
	now := time.Now().UTC()
	cmd.status.Status = DockerCommandStatusAcknowledged
	cmd.status.AcknowledgedAt = &now
	cmd.status.UpdatedAt = now
	if message != "" {
		cmd.status.Message = message
	}
}

func (cmd *dockerHostCommand) markCompleted(message string) {
	now := time.Now().UTC()
	cmd.status.Status = DockerCommandStatusCompleted
	cmd.status.CompletedAt = &now
	cmd.status.UpdatedAt = now
	if message != "" {
		cmd.status.Message = message
	}
}

func (cmd *dockerHostCommand) markFailed(reason string) {
	now := time.Now().UTC()
	cmd.status.Status = DockerCommandStatusFailed
	cmd.status.FailedAt = &now
	cmd.status.UpdatedAt = now
	cmd.status.FailureReason = reason
}

func (cmd *dockerHostCommand) hasExpired(now time.Time) bool {
	if cmd.status.ExpiresAt == nil {
		return false
	}
	return now.After(*cmd.status.ExpiresAt)
}

// queueDockerStopCommand enqueues a stop command for the specified docker host.
func (m *Monitor) queueDockerStopCommand(hostID string) (models.DockerHostCommandStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hostID = normalizeDockerHostID(hostID)
	if hostID == "" {
		return models.DockerHostCommandStatus{}, fmt.Errorf("docker host id is required")
	}

	// Ensure the host exists
	var hostExists bool
	for _, host := range m.state.GetDockerHosts() {
		if host.ID == hostID {
			hostExists = true
			break
		}
	}
	if !hostExists {
		return models.DockerHostCommandStatus{}, fmt.Errorf("docker host %q not found", hostID)
	}

	if existing, ok := m.dockerCommands[hostID]; ok {
		switch existing.status.Status {
		case DockerCommandStatusQueued, DockerCommandStatusDispatched, DockerCommandStatusAcknowledged:
			return existing.status, fmt.Errorf("docker host %q already has a command in progress", hostID)
		}
	}

	cmd := newDockerHostCommand(DockerCommandTypeStop, "Stopping agent", dockerCommandDefaultTTL, nil)
	if m.dockerCommands == nil {
		m.dockerCommands = make(map[string]*dockerHostCommand)
	}
	m.dockerCommands[hostID] = &cmd
	if m.dockerCommandIndex == nil {
		m.dockerCommandIndex = make(map[string]string)
	}
	m.dockerCommandIndex[cmd.status.ID] = hostID

	m.state.SetDockerHostPendingUninstall(hostID, true)
	m.state.SetDockerHostCommand(hostID, &cmd.status)
	log.Info().
		Str("dockerHostID", hostID).
		Str("commandID", cmd.status.ID).
		Msg("Queued docker host stop command")

	return cmd.status, nil
}

func (m *Monitor) getDockerCommandPayload(hostID string) (map[string]any, *models.DockerHostCommandStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hostID = normalizeDockerHostID(hostID)
	if hostID == "" {
		return nil, nil
	}

	cmd, ok := m.dockerCommands[hostID]
	if !ok {
		return nil, nil
	}
	now := time.Now().UTC()
	if cmd.hasExpired(now) {
		cmd.status.Status = DockerCommandStatusExpired
		cmd.status.UpdatedAt = now
		cmd.status.FailureReason = "command expired before agent acknowledged it"
		m.state.SetDockerHostCommand(hostID, &cmd.status)
		log.Warn().
			Str("dockerHostID", hostID).
			Str("commandID", cmd.status.ID).
			Msg("Docker command expired prior to dispatch")
		delete(m.dockerCommands, hostID)
		delete(m.dockerCommandIndex, cmd.status.ID)
		return nil, nil
	}

	// Update dispatch metadata
	if cmd.status.Status == DockerCommandStatusQueued {
		cmd.markDispatched()
		m.state.SetDockerHostCommand(hostID, &cmd.status)
	}

	statusCopy := cmd.status
	return cmd.payload, &statusCopy
}

func (m *Monitor) acknowledgeDockerCommand(commandID, hostID, status, message string) (models.DockerHostCommandStatus, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	commandID = strings.TrimSpace(commandID)
	if commandID == "" {
		return models.DockerHostCommandStatus{}, "", false, fmt.Errorf("command id is required")
	}

	resolvedHostID, ok := m.dockerCommandIndex[commandID]
	if !ok {
		return models.DockerHostCommandStatus{}, "", false, fmt.Errorf("docker host command %q not found", commandID)
	}

	if hostID != "" {
		normalized := normalizeDockerHostID(hostID)
		if normalized == "" {
			return models.DockerHostCommandStatus{}, "", false, fmt.Errorf("docker host id is required")
		}
		if normalized != resolvedHostID {
			return models.DockerHostCommandStatus{}, "", false, fmt.Errorf("command %q does not belong to host %q", commandID, normalized)
		}
	}

	cmd, ok := m.dockerCommands[resolvedHostID]
	if !ok || cmd.status.ID != commandID {
		return models.DockerHostCommandStatus{}, "", false, fmt.Errorf("docker host command %q not active", commandID)
	}

	if message != "" {
		message = strings.TrimSpace(message)
	}

	shouldRemove := false
	switch status {
	case DockerCommandStatusAcknowledged:
		cmd.markAcknowledged(message)
	case DockerCommandStatusCompleted:
		cmd.markAcknowledged(message)
		cmd.markCompleted(message)
		if cmd.status.Type == DockerCommandTypeStop {
			shouldRemove = true
		}
	case DockerCommandStatusFailed:
		cmd.markFailed(message)
		m.state.SetDockerHostPendingUninstall(resolvedHostID, false)
	default:
		return models.DockerHostCommandStatus{}, "", false, fmt.Errorf("invalid command status %q", status)
	}

	m.state.SetDockerHostCommand(resolvedHostID, &cmd.status)

	log.Info().
		Str("dockerHostID", resolvedHostID).
		Str("commandID", cmd.status.ID).
		Str("status", cmd.status.Status).
		Msg("Docker host acknowledged command")

	if status == DockerCommandStatusFailed || shouldRemove {
		delete(m.dockerCommands, resolvedHostID)
		delete(m.dockerCommandIndex, commandID)
	}

	return cmd.status, resolvedHostID, shouldRemove, nil
}

func normalizeDockerHostID(id string) string {
	return strings.TrimSpace(id)
}
