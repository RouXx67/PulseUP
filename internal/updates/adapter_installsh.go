package updates

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// InstallShAdapter wraps the install.sh script for systemd/LXC deployments
type InstallShAdapter struct {
	history        *UpdateHistory
	installScriptURL string
	logDir         string
}

// NewInstallShAdapter creates a new install.sh adapter
func NewInstallShAdapter(history *UpdateHistory) *InstallShAdapter {
	return &InstallShAdapter{
		history:        history,
		installScriptURL: "https://raw.githubusercontent.com/RouXx67/PulseUP/main/install.sh",
		logDir:         "/var/log/pulse",
	}
}

// SupportsApply returns true for systemd and proxmoxve deployments
func (a *InstallShAdapter) SupportsApply() bool {
	return true
}

// GetDeploymentType returns the deployment type
func (a *InstallShAdapter) GetDeploymentType() string {
	return "systemd" // Can be "systemd" or "proxmoxve"
}

// PrepareUpdate returns update plan information
func (a *InstallShAdapter) PrepareUpdate(ctx context.Context, request UpdateRequest) (*UpdatePlan, error) {
	plan := &UpdatePlan{
		CanAutoUpdate:   true,
		RequiresRoot:    true,
		RollbackSupport: true,
		EstimatedTime:   "2-5 minutes",
		Instructions: []string{
			fmt.Sprintf("Download and install Pulse %s", request.Version),
			"Create backup of current installation",
			"Extract and apply update",
			"Restart Pulse service",
		},
		Prerequisites: []string{
			"Root access (sudo)",
			"Internet connection",
			"At least 100MB free disk space",
		},
	}

	return plan, nil
}

// Execute performs the update by calling install.sh
func (a *InstallShAdapter) Execute(ctx context.Context, request UpdateRequest, progressCb ProgressCallback) error {
	// Ensure log directory exists
	if err := os.MkdirAll(a.logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Create log file
	logFile := filepath.Join(a.logDir, fmt.Sprintf("update-%s.log", time.Now().Format("20060102-150405")))
	logFd, err := os.Create(logFile)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	defer logFd.Close()

	// Download install script
	progressCb(UpdateProgress{
		Stage:    "downloading",
		Progress: 10,
		Message:  "Downloading installation script...",
	})

	log.Info().
		Str("url", a.installScriptURL).
		Str("version", request.Version).
		Msg("Downloading install script")

	installScript, err := a.downloadInstallScript(ctx)
	if err != nil {
		return fmt.Errorf("failed to download install script: %w", err)
	}

	// Prepare command
	progressCb(UpdateProgress{
		Stage:    "preparing",
		Progress: 20,
		Message:  "Preparing update...",
	})

	// Validate version string to prevent command injection
	// Version must match semantic versioning format (with optional 'v' prefix)
	versionPattern := regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?(?:\+[a-zA-Z0-9.-]+)?$`)
	if !versionPattern.MatchString(request.Version) {
		return fmt.Errorf("invalid version format: %s", request.Version)
	}

	// Build command: bash install.sh --version vX.Y.Z
	args := []string{"-s", "--", "--version", request.Version}
	if request.Force {
		args = append(args, "--force")
	}

	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Stdin = strings.NewReader(installScript)

	// Create pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start install script: %w", err)
	}

	// Track backup path from output
	var backupPath string
	backupRe := regexp.MustCompile(`[Bb]ackup.*:\s*(.+)`)

	// Monitor output
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()

			// Write to log file
			fmt.Fprintln(logFd, line)

			// Parse for backup path
			if matches := backupRe.FindStringSubmatch(line); len(matches) > 1 {
				backupPath = strings.TrimSpace(matches[1])
			}

			// Emit progress based on output
			progress := a.parseProgress(line)
			if progress.Message != "" {
				progressCb(progress)
			}
		}
	}()

	// Also capture stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(logFd, "STDERR:", line)
		}
	}()

	// Wait for completion
	progressCb(UpdateProgress{
		Stage:    "installing",
		Progress: 50,
		Message:  "Installing update...",
	})

	err = cmd.Wait()

	if err != nil {
		// Read last few lines of log for error context
		errorDetails := a.readLastLines(logFile, 10)

		progressCb(UpdateProgress{
			Stage:      "failed",
			Progress:   0,
			Message:    "Update failed",
			IsComplete: true,
			Error:      errorDetails,
		})

		return fmt.Errorf("install script failed: %w\n%s", err, errorDetails)
	}

	progressCb(UpdateProgress{
		Stage:      "completed",
		Progress:   100,
		Message:    "Update completed successfully",
		IsComplete: true,
	})

	log.Info().
		Str("version", request.Version).
		Str("backup", backupPath).
		Str("log", logFile).
		Msg("Update completed successfully")

	return nil
}

// Rollback rolls back to a previous version
func (a *InstallShAdapter) Rollback(ctx context.Context, eventID string) error {
	// Get the event from history
	entry, err := a.history.GetEntry(eventID)
	if err != nil {
		return fmt.Errorf("failed to get history entry: %w", err)
	}

	if entry.BackupPath == "" {
		return fmt.Errorf("no backup path available for event %s", eventID)
	}

	// Check if backup exists
	if _, err := os.Stat(entry.BackupPath); os.IsNotExist(err) {
		return fmt.Errorf("backup not found: %s", entry.BackupPath)
	}

	targetVersion := entry.VersionFrom
	if targetVersion == "" {
		return fmt.Errorf("no target version available in history")
	}

	log.Info().
		Str("event_id", eventID).
		Str("backup", entry.BackupPath).
		Str("current_version", entry.VersionTo).
		Str("target_version", targetVersion).
		Msg("Starting rollback")

	// Create rollback history entry
	rollbackEventID, err := a.history.CreateEntry(ctx, UpdateHistoryEntry{
		Action:         ActionRollback,
		VersionFrom:    entry.VersionTo,
		VersionTo:      targetVersion,
		DeploymentType: a.GetDeploymentType(),
		InitiatedBy:    InitiatedByUser,
		InitiatedVia:   InitiatedViaCLI,
		Status:         StatusInProgress,
		RelatedEventID: eventID,
		Notes:          fmt.Sprintf("Rolling back update %s", eventID),
	})
	if err != nil {
		return fmt.Errorf("failed to create rollback history entry: %w", err)
	}

	rollbackErr := a.executeRollback(ctx, entry, targetVersion)

	// Update rollback history
	finalStatus := StatusSuccess
	var updateError *UpdateError
	if rollbackErr != nil {
		finalStatus = StatusFailed
		updateError = &UpdateError{
			Message: rollbackErr.Error(),
			Code:    "rollback_failed",
		}
	}

	_ = a.history.UpdateEntry(ctx, rollbackEventID, func(e *UpdateHistoryEntry) error {
		e.Status = finalStatus
		e.Error = updateError
		return nil
	})

	return rollbackErr
}

// executeRollback performs the actual rollback operation
func (a *InstallShAdapter) executeRollback(ctx context.Context, entry *UpdateHistoryEntry, targetVersion string) error {
	// Step 1: Detect service name
	serviceName, err := a.detectServiceName()
	if err != nil {
		return fmt.Errorf("failed to detect service name: %w", err)
	}

	log.Info().Str("service", serviceName).Msg("Detected Pulse service")

	// Step 2: Download old binary
	log.Info().Str("version", targetVersion).Msg("Downloading old binary")
	binaryPath, err := a.downloadBinary(ctx, targetVersion)
	if err != nil {
		return fmt.Errorf("failed to download binary: %w", err)
	}
	defer os.Remove(binaryPath)

	// Step 3: Stop service
	log.Info().Msg("Stopping Pulse service")
	if err := a.stopService(ctx, serviceName); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}

	// Step 4: Backup current config (safety)
	configDir := "/etc/pulse"
	safetyBackup := fmt.Sprintf("%s.rollback-safety.%s", configDir, time.Now().Format("20060102-150405"))
	log.Info().Str("backup", safetyBackup).Msg("Creating safety backup of current config")
	if err := exec.CommandContext(ctx, "cp", "-a", configDir, safetyBackup).Run(); err != nil {
		log.Warn().Err(err).Msg("Failed to create safety backup")
	}

	// Step 5: Restore config from backup
	log.Info().Str("source", entry.BackupPath).Msg("Restoring configuration")
	if err := a.restoreConfig(ctx, entry.BackupPath, configDir); err != nil {
		// Try to start service anyway
		_ = a.startService(ctx, serviceName)
		return fmt.Errorf("failed to restore config: %w", err)
	}

	// Step 6: Install old binary
	log.Info().Str("version", targetVersion).Msg("Installing old binary")
	installDir := "/opt/pulse/bin/pulse"
	if err := a.installBinary(ctx, binaryPath, installDir); err != nil {
		// Try to start service anyway
		_ = a.startService(ctx, serviceName)
		return fmt.Errorf("failed to install binary: %w", err)
	}

	// Step 7: Start service
	log.Info().Msg("Starting Pulse service")
	if err := a.startService(ctx, serviceName); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	// Step 8: Health check
	log.Info().Msg("Verifying service health")
	if err := a.waitForHealth(ctx, 30*time.Second); err != nil {
		return fmt.Errorf("service health check failed: %w", err)
	}

	log.Info().Str("version", targetVersion).Msg("Rollback completed successfully")
	return nil
}

// detectServiceName detects the active Pulse service name
func (a *InstallShAdapter) detectServiceName() (string, error) {
	candidates := []string{"pulse", "pulse-backend", "pulse-hot-dev"}

	for _, name := range candidates {
		cmd := exec.Command("systemctl", "is-active", name)
		if output, err := cmd.Output(); err == nil {
			status := strings.TrimSpace(string(output))
			if status == "active" || status == "activating" {
				return name, nil
			}
		}
	}

	// Default to "pulse" if none are active
	return "pulse", nil
}

// downloadBinary downloads a specific version binary from GitHub
func (a *InstallShAdapter) downloadBinary(ctx context.Context, version string) (string, error) {
	// Ensure version has 'v' prefix
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	// Determine architecture
	arch := "amd64"
	if _, err := os.Stat("/proc/cpuinfo"); err == nil {
		output, _ := exec.Command("uname", "-m").Output()
		machine := strings.TrimSpace(string(output))
		if machine == "aarch64" || machine == "arm64" {
			arch = "arm64"
		}
	}

	// Download URL
	url := fmt.Sprintf("https://github.com/RouXx67/PulseUP/releases/download/%s/pulse-linux-%s", version, arch)

	// Create temp file
	tmpFile, err := os.CreateTemp("", "pulse-rollback-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	// Download
	cmd := exec.CommandContext(ctx, "curl", "-fsSL", "-o", tmpPath, url)
	if err := cmd.Run(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("download failed: %w", err)
	}

	// Verify it's executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	return tmpPath, nil
}

// stopService stops the Pulse service
func (a *InstallShAdapter) stopService(ctx context.Context, serviceName string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "stop", serviceName)
	return cmd.Run()
}

// startService starts the Pulse service
func (a *InstallShAdapter) startService(ctx context.Context, serviceName string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "start", serviceName)
	return cmd.Run()
}

// restoreConfig restores configuration from backup
func (a *InstallShAdapter) restoreConfig(ctx context.Context, backupPath, targetPath string) error {
	// Remove current config
	if err := os.RemoveAll(targetPath); err != nil {
		return fmt.Errorf("failed to remove current config: %w", err)
	}

	// Copy backup to target
	cmd := exec.CommandContext(ctx, "cp", "-a", backupPath, targetPath)
	return cmd.Run()
}

// installBinary installs a binary to the target location
func (a *InstallShAdapter) installBinary(ctx context.Context, sourcePath, targetPath string) error {
	// Backup current binary
	if _, err := os.Stat(targetPath); err == nil {
		backupPath := targetPath + ".pre-rollback"
		_ = os.Rename(targetPath, backupPath)
	}

	// Copy new binary
	if err := exec.CommandContext(ctx, "cp", sourcePath, targetPath).Run(); err != nil {
		return err
	}

	// Set permissions
	if err := os.Chmod(targetPath, 0755); err != nil {
		return err
	}

	// Set ownership
	return exec.CommandContext(ctx, "chown", "pulse:pulse", targetPath).Run()
}

// waitForHealth waits for the service to become healthy
func (a *InstallShAdapter) waitForHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Try to hit health endpoint
		cmd := exec.CommandContext(ctx, "curl", "-fsS", "http://localhost:7655/api/health")
		if err := cmd.Run(); err == nil {
			return nil
		}

		// Wait before retry
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return fmt.Errorf("service did not become healthy within %v", timeout)
}

// downloadInstallScript downloads the install.sh script
func (a *InstallShAdapter) downloadInstallScript(ctx context.Context) (string, error) {
	// Use curl to download (simpler than http.Get for script execution)
	cmd := exec.CommandContext(ctx, "curl", "-fsSL", a.installScriptURL)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// parseProgress attempts to parse progress from install script output
func (a *InstallShAdapter) parseProgress(line string) UpdateProgress {
	line = strings.ToLower(line)

	// Map common install.sh output to progress stages
	patterns := map[string]UpdateProgress{
		"downloading": {Stage: "downloading", Progress: 30, Message: "Downloading update..."},
		"extracting":  {Stage: "extracting", Progress: 40, Message: "Extracting files..."},
		"installing":  {Stage: "installing", Progress: 60, Message: "Installing..."},
		"backup":      {Stage: "backing-up", Progress: 25, Message: "Creating backup..."},
		"configur":    {Stage: "configuring", Progress: 70, Message: "Configuring..."},
		"restart":     {Stage: "restarting", Progress: 90, Message: "Restarting service..."},
		"complet":     {Stage: "completed", Progress: 100, Message: "Update completed", IsComplete: true},
		"success":     {Stage: "completed", Progress: 100, Message: "Update completed", IsComplete: true},
	}

	for pattern, progress := range patterns {
		if strings.Contains(line, pattern) {
			return progress
		}
	}

	return UpdateProgress{}
}

// readLastLines reads the last N lines from a file
func (a *InstallShAdapter) readLastLines(filepath string, n int) string {
	file, err := os.Open(filepath)
	if err != nil {
		return ""
	}
	defer file.Close()

	// Read file backwards (simplified approach - read all lines and take last N)
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if len(lines) == 0 {
		return ""
	}

	start := len(lines) - n
	if start < 0 {
		start = 0
	}

	return strings.Join(lines[start:], "\n")
}

// DockerUpdater provides instructions for Docker deployments
type DockerUpdater struct{}

func NewDockerUpdater() *DockerUpdater {
	return &DockerUpdater{}
}

func (u *DockerUpdater) SupportsApply() bool {
	return false
}

func (u *DockerUpdater) GetDeploymentType() string {
	return "docker"
}

func (u *DockerUpdater) PrepareUpdate(ctx context.Context, request UpdateRequest) (*UpdatePlan, error) {
	return &UpdatePlan{
		CanAutoUpdate: false,
		Instructions: []string{
			fmt.Sprintf("docker pull RouXx67/pulseup:%s", strings.TrimPrefix(request.Version, "v")),
			"docker stop pulse",
			fmt.Sprintf("docker run -d --name pulse RouXx67/pulseup:%s", strings.TrimPrefix(request.Version, "v")),
		},
		RequiresRoot:    false,
		RollbackSupport: true,
		EstimatedTime:   "1-2 minutes",
	}, nil
}

func (u *DockerUpdater) Execute(ctx context.Context, request UpdateRequest, progressCb ProgressCallback) error {
	return fmt.Errorf("docker deployments do not support automated updates")
}

func (u *DockerUpdater) Rollback(ctx context.Context, eventID string) error {
	return fmt.Errorf("docker rollback not supported via API")
}

// AURUpdater provides instructions for Arch Linux AUR deployments
type AURUpdater struct{}

func NewAURUpdater() *AURUpdater {
	return &AURUpdater{}
}

func (u *AURUpdater) SupportsApply() bool {
	return false
}

func (u *AURUpdater) GetDeploymentType() string {
	return "aur"
}

func (u *AURUpdater) PrepareUpdate(ctx context.Context, request UpdateRequest) (*UpdatePlan, error) {
	return &UpdatePlan{
		CanAutoUpdate: false,
		Instructions: []string{
			"yay -Syu pulse-monitoring",
			"# or",
			"paru -Syu pulse-monitoring",
		},
		RequiresRoot:    false,
		RollbackSupport: false,
		EstimatedTime:   "1-2 minutes",
	}, nil
}

func (u *AURUpdater) Execute(ctx context.Context, request UpdateRequest, progressCb ProgressCallback) error {
	return fmt.Errorf("aur deployments must be updated via package manager")
}

func (u *AURUpdater) Rollback(ctx context.Context, eventID string) error {
	return fmt.Errorf("aur rollback not supported")
}

// Ensure adapters implement Updater interface
var (
	_ Updater = (*InstallShAdapter)(nil)
	_ Updater = (*DockerUpdater)(nil)
	_ Updater = (*AURUpdater)(nil)
)
