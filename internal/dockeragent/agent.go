package dockeragent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	agentsdocker "github.com/RouXx67/PulseUp/pkg/agents/docker"
	"github.com/rs/zerolog"
)

// TargetConfig describes a single Pulse backend the agent should report to.
type TargetConfig struct {
	URL                string
	Token              string
	InsecureSkipVerify bool
}

// Config describes runtime configuration for the Docker agent.
type Config struct {
	PulseURL           string
	APIToken           string
	Interval           time.Duration
	HostnameOverride   string
	AgentID            string
	InsecureSkipVerify bool
	DisableAutoUpdate  bool
	Targets            []TargetConfig
	Logger             *zerolog.Logger
}

// Agent collects Docker metrics and posts them to Pulse.
type Agent struct {
	cfg         Config
	docker      *client.Client
	httpClients map[bool]*http.Client
	logger      zerolog.Logger
	machineID   string
	hostName    string
	cpuCount    int
	targets     []TargetConfig
	hostID      string
}

// ErrStopRequested indicates the agent should terminate gracefully after acknowledging a stop command.
var ErrStopRequested = errors.New("docker host stop requested")

// New creates a new Docker agent instance.
func New(cfg Config) (*Agent, error) {
	targets, err := normalizeTargets(cfg.Targets)
	if err != nil {
		return nil, err
	}

	if len(targets) == 0 {
		url := strings.TrimSpace(cfg.PulseURL)
		token := strings.TrimSpace(cfg.APIToken)
		if url == "" || token == "" {
			return nil, errors.New("at least one Pulse target is required")
		}

		targets, err = normalizeTargets([]TargetConfig{{
			URL:                url,
			Token:              token,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		}})
		if err != nil {
			return nil, err
		}
	}

	cfg.Targets = targets
	cfg.PulseURL = targets[0].URL
	cfg.APIToken = targets[0].Token
	cfg.InsecureSkipVerify = targets[0].InsecureSkipVerify

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	hasSecure := false
	hasInsecure := false
	for _, target := range cfg.Targets {
		if target.InsecureSkipVerify {
			hasInsecure = true
		} else {
			hasSecure = true
		}
	}

	httpClients := make(map[bool]*http.Client, 2)
	if hasSecure {
		httpClients[false] = newHTTPClient(false)
	}
	if hasInsecure {
		httpClients[true] = newHTTPClient(true)
	}

	logger := cfg.Logger
	if logger == nil {
		defaultLogger := zerolog.New(os.Stdout).With().Timestamp().Str("component", "pulse-docker-agent").Logger()
		logger = &defaultLogger
	} else {
		scoped := logger.With().Str("component", "pulse-docker-agent").Logger()
		logger = &scoped
	}

	machineID, _ := readMachineID()
	hostName := cfg.HostnameOverride
	if hostName == "" {
		if h, err := os.Hostname(); err == nil {
			hostName = h
		}
	}

	agent := &Agent{
		cfg:         cfg,
		docker:      dockerClient,
		httpClients: httpClients,
		logger:      *logger,
		machineID:   machineID,
		hostName:    hostName,
		targets:     cfg.Targets,
	}

	return agent, nil
}

func normalizeTargets(raw []TargetConfig) ([]TargetConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	normalized := make([]TargetConfig, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))

	for _, target := range raw {
		url := strings.TrimSpace(target.URL)
		token := strings.TrimSpace(target.Token)
		if url == "" && token == "" {
			continue
		}

		if url == "" {
			return nil, errors.New("pulse target URL is required")
		}
		if token == "" {
			return nil, fmt.Errorf("pulse target %s is missing API token", url)
		}

		url = strings.TrimRight(url, "/")
		key := fmt.Sprintf("%s|%s|%t", url, token, target.InsecureSkipVerify)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		normalized = append(normalized, TargetConfig{
			URL:                url,
			Token:              token,
			InsecureSkipVerify: target.InsecureSkipVerify,
		})
	}

	return normalized, nil
}

// Run starts the collection loop until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	interval := a.cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
		a.cfg.Interval = interval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Check for updates on startup
	go a.checkForUpdates(ctx)

	// Check for updates daily
	updateTicker := time.NewTicker(24 * time.Hour)
	defer updateTicker.Stop()

	if err := a.collectOnce(ctx); err != nil {
		if errors.Is(err, ErrStopRequested) {
			return nil
		}
		a.logger.Error().Err(err).Msg("Failed to send initial report")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := a.collectOnce(ctx); err != nil {
				if errors.Is(err, ErrStopRequested) {
					return nil
				}
				a.logger.Error().Err(err).Msg("Failed to send docker report")
			}
		case <-updateTicker.C:
			go a.checkForUpdates(ctx)
		}
	}
}

func (a *Agent) collectOnce(ctx context.Context) error {
	report, err := a.buildReport(ctx)
	if err != nil {
		return err
	}

	return a.sendReport(ctx, report)
}

func (a *Agent) buildReport(ctx context.Context) (agentsdocker.Report, error) {
	info, err := a.docker.Info(ctx)
	if err != nil {
		return agentsdocker.Report{}, fmt.Errorf("failed to query docker info: %w", err)
	}

	a.cpuCount = info.NCPU

	agentID := a.cfg.AgentID
	if agentID == "" {
		agentID = info.ID
	}
	if agentID == "" {
		agentID = a.machineID
	}
	if agentID == "" {
		agentID = a.hostName
	}
	a.hostID = agentID

	hostName := a.hostName
	if hostName == "" {
		hostName = info.Name
	}

	uptime := readSystemUptime()

	containers, err := a.collectContainers(ctx)
	if err != nil {
		return agentsdocker.Report{}, err
	}

	report := agentsdocker.Report{
		Agent: agentsdocker.AgentInfo{
			ID:              agentID,
			Version:         Version,
			IntervalSeconds: int(a.cfg.Interval / time.Second),
		},
		Host: agentsdocker.HostInfo{
			Hostname:         hostName,
			Name:             info.Name,
			MachineID:        a.machineID,
			OS:               info.OperatingSystem,
			KernelVersion:    info.KernelVersion,
			Architecture:     info.Architecture,
			DockerVersion:    info.ServerVersion,
			TotalCPU:         info.NCPU,
			TotalMemoryBytes: info.MemTotal,
			UptimeSeconds:    uptime,
		},
		Containers: containers,
		Timestamp:  time.Now().UTC(),
	}

	if report.Agent.IntervalSeconds <= 0 {
		report.Agent.IntervalSeconds = int(30 * time.Second / time.Second)
	}

	return report, nil
}

func (a *Agent) collectContainers(ctx context.Context) ([]agentsdocker.Container, error) {
	list, err := a.docker.ContainerList(ctx, containertypes.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	containers := make([]agentsdocker.Container, 0, len(list))
	for _, summary := range list {
		container, err := a.collectContainer(ctx, summary)
		if err != nil {
			a.logger.Warn().Str("container", strings.Join(summary.Names, ",")).Err(err).Msg("Failed to collect container stats")
			continue
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (a *Agent) collectContainer(ctx context.Context, summary types.Container) (agentsdocker.Container, error) {
	const perContainerTimeout = 5 * time.Second

	containerCtx, cancel := context.WithTimeout(ctx, perContainerTimeout)
	defer cancel()

	inspect, err := a.docker.ContainerInspect(containerCtx, summary.ID)
	if err != nil {
		return agentsdocker.Container{}, fmt.Errorf("inspect: %w", err)
	}

	statsResp, err := a.docker.ContainerStatsOneShot(containerCtx, summary.ID)
	if err != nil {
		return agentsdocker.Container{}, fmt.Errorf("stats: %w", err)
	}
	defer statsResp.Body.Close()

	var stats containertypes.StatsResponse
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		return agentsdocker.Container{}, fmt.Errorf("decode stats: %w", err)
	}

	cpuPercent := calculateCPUPercent(stats, a.cpuCount)
	memUsage, memLimit, memPercent := calculateMemoryUsage(stats)

	createdAt := time.Unix(summary.Created, 0)

	startedAt := parseTime(inspect.State.StartedAt)
	finishedAt := parseTime(inspect.State.FinishedAt)

	uptimeSeconds := int64(0)
	if !startedAt.IsZero() && inspect.State.Running {
		uptimeSeconds = int64(time.Since(startedAt).Seconds())
		if uptimeSeconds < 0 {
			uptimeSeconds = 0
		}
	}

	health := ""
	if inspect.State.Health != nil {
		health = inspect.State.Health.Status
	}

	ports := make([]agentsdocker.ContainerPort, len(summary.Ports))
	for i, port := range summary.Ports {
		ports[i] = agentsdocker.ContainerPort{
			PrivatePort: int(port.PrivatePort),
			PublicPort:  int(port.PublicPort),
			Protocol:    port.Type,
			IP:          port.IP,
		}
	}

	labels := make(map[string]string, len(summary.Labels))
	for k, v := range summary.Labels {
		labels[k] = v
	}

	networks := make([]agentsdocker.ContainerNetwork, 0)
	if inspect.NetworkSettings != nil {
		for name, cfg := range inspect.NetworkSettings.Networks {
			networks = append(networks, agentsdocker.ContainerNetwork{
				Name: name,
				IPv4: cfg.IPAddress,
				IPv6: cfg.GlobalIPv6Address,
			})
		}
	}

	var startedPtr, finishedPtr *time.Time
	if !startedAt.IsZero() {
		started := startedAt
		startedPtr = &started
	}
	if !finishedAt.IsZero() && !inspect.State.Running {
		finished := finishedAt
		finishedPtr = &finished
	}

	container := agentsdocker.Container{
		ID:               summary.ID,
		Name:             trimLeadingSlash(summary.Names),
		Image:            summary.Image,
		CreatedAt:        createdAt,
		State:            summary.State,
		Status:           summary.Status,
		Health:           health,
		CPUPercent:       cpuPercent,
		MemoryUsageBytes: memUsage,
		MemoryLimitBytes: memLimit,
		MemoryPercent:    memPercent,
		UptimeSeconds:    uptimeSeconds,
		RestartCount:     inspect.RestartCount,
		ExitCode:         inspect.State.ExitCode,
		StartedAt:        startedPtr,
		FinishedAt:       finishedPtr,
		Ports:            ports,
		Labels:           labels,
		Networks:         networks,
	}

	return container, nil
}

func (a *Agent) sendReport(ctx context.Context, report agentsdocker.Report) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}

	var errs []error
	containerCount := len(report.Containers)

	for _, target := range a.targets {
		err := a.sendReportToTarget(ctx, target, payload, containerCount)
		if err == nil {
			continue
		}
		if errors.Is(err, ErrStopRequested) {
			return ErrStopRequested
		}
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	a.logger.Debug().
		Int("containers", containerCount).
		Int("targets", len(a.targets)).
		Msg("Report sent to Pulse targets")
	return nil
}

func (a *Agent) sendReportToTarget(ctx context.Context, target TargetConfig, payload []byte, containerCount int) error {
	url := fmt.Sprintf("%s/api/agents/docker/report", target.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("target %s: create request: %w", target.URL, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", target.Token)
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("User-Agent", "pulse-docker-agent/"+Version)

	client := a.httpClientFor(target)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("target %s: send report: %w", target.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("target %s: pulse responded with status %s", target.URL, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("target %s: read response: %w", target.URL, err)
	}

	if len(body) == 0 {
		return nil
	}

	var reportResp agentsdocker.ReportResponse
	if err := json.Unmarshal(body, &reportResp); err != nil {
		a.logger.Warn().Err(err).Str("target", target.URL).Msg("Failed to decode Pulse response")
		return nil
	}

	for _, command := range reportResp.Commands {
		err := a.handleCommand(ctx, target, command)
		if err == nil {
			continue
		}
		if errors.Is(err, ErrStopRequested) {
			return ErrStopRequested
		}
		return err
	}

	return nil
}

func (a *Agent) handleCommand(ctx context.Context, target TargetConfig, command agentsdocker.Command) error {
	switch strings.ToLower(command.Type) {
	case agentsdocker.CommandTypeStop:
		return a.handleStopCommand(ctx, target, command)
	default:
		a.logger.Warn().Str("command", command.Type).Msg("Received unsupported control command")
		return nil
	}
}

func (a *Agent) handleStopCommand(ctx context.Context, target TargetConfig, command agentsdocker.Command) error {
	a.logger.Info().Str("commandID", command.ID).Msg("Received stop command from Pulse")

	if err := a.disableSelf(ctx); err != nil {
		a.logger.Error().Err(err).Msg("Failed to disable pulse-docker-agent service")
		if ackErr := a.sendCommandAck(ctx, target, command.ID, agentsdocker.CommandStatusFailed, err.Error()); ackErr != nil {
			a.logger.Error().Err(ackErr).Msg("Failed to send failure acknowledgement to Pulse")
		}
		return nil
	}

	if err := a.sendCommandAck(ctx, target, command.ID, agentsdocker.CommandStatusCompleted, "Agent shutting down"); err != nil {
		return fmt.Errorf("send stop acknowledgement: %w", err)
	}

	a.logger.Info().Msg("Stop command acknowledged; terminating agent")
	return ErrStopRequested
}

func (a *Agent) disableSelf(ctx context.Context) error {
	if err := disableSystemdService(ctx, "pulse-docker-agent"); err != nil {
		return err
	}

	// Remove Unraid startup script if present to prevent restart on reboot.
	if err := removeFileIfExists("/boot/config/go.d/pulse-docker-agent.sh"); err != nil {
		a.logger.Warn().Err(err).Msg("Failed to remove Unraid startup script")
	}

	// Best-effort log cleanup (ignore errors).
	_ = removeFileIfExists("/var/log/pulse-docker-agent.log")

	return nil
}

func disableSystemdService(ctx context.Context, service string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		// Not a systemd environment; nothing to do.
		return nil
	}

	cmd := exec.CommandContext(ctx, "systemctl", "disable", service)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode := exitErr.ExitCode()
			lowerOutput := strings.ToLower(string(output))
			if exitCode == 5 || strings.Contains(lowerOutput, "could not be found") || strings.Contains(lowerOutput, "not-found") {
				return nil
			}
		}
		return fmt.Errorf("systemctl disable %s: %w (%s)", service, err, strings.TrimSpace(string(output)))
	}

	return nil
}

func removeFileIfExists(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func (a *Agent) sendCommandAck(ctx context.Context, target TargetConfig, commandID, status, message string) error {
	if a.hostID == "" {
		return fmt.Errorf("host identifier unavailable; cannot acknowledge command")
	}

	ackPayload := agentsdocker.CommandAck{
		HostID:  a.hostID,
		Status:  status,
		Message: message,
	}

	body, err := json.Marshal(ackPayload)
	if err != nil {
		return fmt.Errorf("marshal command acknowledgement: %w", err)
	}

	url := fmt.Sprintf("%s/api/agents/docker/commands/%s/ack", target.URL, commandID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create acknowledgement request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", target.Token)
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("User-Agent", "pulse-docker-agent/"+Version)

	resp, err := a.httpClientFor(target).Do(req)
	if err != nil {
		return fmt.Errorf("send acknowledgement: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pulse responded %s: %s", resp.Status, strings.TrimSpace(string(bodyBytes)))
	}

	return nil
}

func (a *Agent) primaryTarget() TargetConfig {
	if len(a.targets) == 0 {
		return TargetConfig{}
	}
	return a.targets[0]
}

func (a *Agent) httpClientFor(target TargetConfig) *http.Client {
	if client, ok := a.httpClients[target.InsecureSkipVerify]; ok {
		return client
	}
	if client, ok := a.httpClients[false]; ok {
		return client
	}
	if client, ok := a.httpClients[true]; ok {
		return client
	}
	return newHTTPClient(target.InsecureSkipVerify)
}

func newHTTPClient(insecure bool) *http.Client {
	transport := &http.Transport{}
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
	}
}

func calculateCPUPercent(stats containertypes.StatsResponse, hostCPUs int) float64 {
	totalDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)

	if totalDelta <= 0 || systemDelta <= 0 {
		return 0
	}

	onlineCPUs := stats.CPUStats.OnlineCPUs
	if onlineCPUs == 0 {
		onlineCPUs = uint32(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 && hostCPUs > 0 {
		onlineCPUs = uint32(hostCPUs)
	}

	if onlineCPUs == 0 {
		return 0
	}

	return safeFloat((totalDelta / systemDelta) * float64(onlineCPUs) * 100.0)
}

func calculateMemoryUsage(stats containertypes.StatsResponse) (usage int64, limit int64, percent float64) {
	usage = int64(stats.MemoryStats.Usage)
	if cache, ok := stats.MemoryStats.Stats["cache"]; ok {
		usage -= int64(cache)
	}
	if usage < 0 {
		usage = int64(stats.MemoryStats.Usage)
	}

	limit = int64(stats.MemoryStats.Limit)
	if limit > 0 {
		percent = (float64(usage) / float64(limit)) * 100.0
	}

	return usage, limit, safeFloat(percent)
}

func safeFloat(val float64) float64 {
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val
}

func parseTime(value string) time.Time {
	if value == "" || value == "0001-01-01T00:00:00Z" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

func trimLeadingSlash(names []string) string {
	if len(names) == 0 {
		return ""
	}
	name := names[0]
	return strings.TrimPrefix(name, "/")
}

func (a *Agent) Close() error {
	return a.docker.Close()
}

func readMachineID() (string, error) {
	paths := []string{
		"/etc/machine-id",
		"/var/lib/dbus/machine-id",
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", errors.New("machine-id not found")
}

func readSystemUptime() int64 {
	seconds, err := readProcUptime()
	if err != nil {
		return 0
	}
	return int64(seconds)
}

// checkForUpdates checks if a newer version is available and performs self-update if needed
func (a *Agent) checkForUpdates(ctx context.Context) {
	// Skip updates if disabled via config
	if a.cfg.DisableAutoUpdate {
		a.logger.Debug().Msg("Skipping update check - auto-update disabled")
		return
	}

	// Skip updates in development mode to prevent update loops
	if Version == "dev" {
		a.logger.Debug().Msg("Skipping update check - running in development mode")
		return
	}

	a.logger.Debug().Msg("Checking for agent updates")

	target := a.primaryTarget()
	if target.URL == "" {
		a.logger.Debug().Msg("Skipping update check - no Pulse target configured")
		return
	}

	// Get current version from server
	url := fmt.Sprintf("%s/api/agent/version", target.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.logger.Warn().Err(err).Msg("Failed to create version check request")
		return
	}

	if target.Token != "" {
		req.Header.Set("X-API-Token", target.Token)
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}

	client := a.httpClientFor(target)
	resp, err := client.Do(req)
	if err != nil {
		a.logger.Warn().Err(err).Msg("Failed to check for updates")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		a.logger.Warn().Int("status", resp.StatusCode).Msg("Version endpoint returned non-200 status")
		return
	}

	var versionResp struct {
		Version string `json:"version"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&versionResp); err != nil {
		a.logger.Warn().Err(err).Msg("Failed to decode version response")
		return
	}

	// Skip updates if server is also in development mode
	if versionResp.Version == "dev" {
		a.logger.Debug().Msg("Skipping update - server is in development mode")
		return
	}

	// Compare versions
	if versionResp.Version == Version {
		a.logger.Debug().Str("version", Version).Msg("Agent is up to date")
		return
	}

	a.logger.Info().
		Str("currentVersion", Version).
		Str("availableVersion", versionResp.Version).
		Msg("New agent version available, performing self-update")

	// Perform self-update
	if err := a.selfUpdate(ctx); err != nil {
		a.logger.Error().Err(err).Msg("Failed to self-update agent")
		return
	}

	a.logger.Info().Msg("Agent updated successfully, restarting...")
}

func determineSelfUpdateArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "linux-amd64"
	case "arm64":
		return "linux-arm64"
	case "arm":
		return "linux-armv7"
	}

	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return ""
	}

	normalized := strings.ToLower(strings.TrimSpace(string(out)))
	switch normalized {
	case "x86_64", "amd64":
		return "linux-amd64"
	case "aarch64", "arm64":
		return "linux-arm64"
	case "armv7l", "armhf", "armv7":
		return "linux-armv7"
	default:
		return ""
	}
}

// selfUpdate downloads the new agent binary and replaces the current one
func (a *Agent) selfUpdate(ctx context.Context) error {
	target := a.primaryTarget()
	if target.URL == "" {
		return errors.New("no Pulse target configured for self-update")
	}

	// Get path to current executable
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	downloadBase := strings.TrimRight(target.URL, "/") + "/download/pulse-docker-agent"
	archParam := determineSelfUpdateArch()

	type downloadCandidate struct {
		url  string
		arch string
	}

	candidates := make([]downloadCandidate, 0, 2)
	if archParam != "" {
		candidates = append(candidates, downloadCandidate{
			url:  fmt.Sprintf("%s?arch=%s", downloadBase, archParam),
			arch: archParam,
		})
	}
	candidates = append(candidates, downloadCandidate{url: downloadBase})

	client := a.httpClientFor(target)
	var resp *http.Response
	var lastErr error

	for _, candidate := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.url, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create download request: %w", err)
			continue
		}

		if target.Token != "" {
			req.Header.Set("X-API-Token", target.Token)
			req.Header.Set("Authorization", "Bearer "+target.Token)
		}

		response, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to download new binary: %w", err)
			continue
		}

		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("download failed with status: %s", response.Status)
			response.Body.Close()
			continue
		}

		resp = response
		if candidate.arch != "" {
			a.logger.Debug().
				Str("arch", candidate.arch).
				Msg("Self-update: downloaded architecture-specific agent binary")
		} else if archParam != "" {
			a.logger.Debug().Msg("Self-update: falling back to server default agent binary")
		}
		break
	}

	if resp == nil {
		if lastErr == nil {
			lastErr = errors.New("failed to download new binary")
		}
		return lastErr
	}
	defer resp.Body.Close()

	// Create temporary file
	tmpFile, err := os.CreateTemp("", "pulse-docker-agent-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // Clean up if something goes wrong

	// Write downloaded binary to temp file
	if _, err := tmpFile.ReadFrom(resp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write downloaded binary: %w", err)
	}
	tmpFile.Close()

	// Make temp file executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("failed to make temp file executable: %w", err)
	}

	// Create backup of current binary
	backupPath := execPath + ".backup"
	if err := os.Rename(execPath, backupPath); err != nil {
		return fmt.Errorf("failed to backup current binary: %w", err)
	}

	// Move new binary to current location
	if err := os.Rename(tmpPath, execPath); err != nil {
		// Restore backup on failure
		os.Rename(backupPath, execPath)
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	// Remove backup on success
	os.Remove(backupPath)

	// Restart agent with same arguments
	args := os.Args
	env := os.Environ()

	if err := syscall.Exec(execPath, args, env); err != nil {
		return fmt.Errorf("failed to restart agent: %w", err)
	}

	return nil
}
