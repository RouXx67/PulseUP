package monitoring

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/rcourtman/pulse-go-rewrite/internal/alerts"
	"github.com/rcourtman/pulse-go-rewrite/internal/config"
	"github.com/rcourtman/pulse-go-rewrite/internal/discovery"
	"github.com/rcourtman/pulse-go-rewrite/internal/errors"
	"github.com/rcourtman/pulse-go-rewrite/internal/logging"
	"github.com/rcourtman/pulse-go-rewrite/internal/mock"
	"github.com/rcourtman/pulse-go-rewrite/internal/models"
	"github.com/rcourtman/pulse-go-rewrite/internal/notifications"
	"github.com/rcourtman/pulse-go-rewrite/internal/websocket"
	agentsdocker "github.com/rcourtman/pulse-go-rewrite/pkg/agents/docker"
	agentshost "github.com/rcourtman/pulse-go-rewrite/pkg/agents/host"
	"github.com/rcourtman/pulse-go-rewrite/pkg/pbs"
	"github.com/rcourtman/pulse-go-rewrite/pkg/pmg"
	"github.com/rcourtman/pulse-go-rewrite/pkg/proxmox"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// PVEClientInterface defines the interface for PVE clients (both regular and cluster)
type PVEClientInterface interface {
	GetNodes(ctx context.Context) ([]proxmox.Node, error)
	GetNodeStatus(ctx context.Context, node string) (*proxmox.NodeStatus, error)
	GetNodeRRDData(ctx context.Context, node string, timeframe string, cf string, ds []string) ([]proxmox.NodeRRDPoint, error)
	GetVMs(ctx context.Context, node string) ([]proxmox.VM, error)
	GetContainers(ctx context.Context, node string) ([]proxmox.Container, error)
	GetStorage(ctx context.Context, node string) ([]proxmox.Storage, error)
	GetAllStorage(ctx context.Context) ([]proxmox.Storage, error)
	GetBackupTasks(ctx context.Context) ([]proxmox.Task, error)
	GetReplicationStatus(ctx context.Context) ([]proxmox.ReplicationJob, error)
	GetStorageContent(ctx context.Context, node, storage string) ([]proxmox.StorageContent, error)
	GetVMSnapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error)
	GetContainerSnapshots(ctx context.Context, node string, vmid int) ([]proxmox.Snapshot, error)
	GetVMStatus(ctx context.Context, node string, vmid int) (*proxmox.VMStatus, error)
	GetContainerStatus(ctx context.Context, node string, vmid int) (*proxmox.Container, error)
	GetContainerConfig(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	GetContainerInterfaces(ctx context.Context, node string, vmid int) ([]proxmox.ContainerInterface, error)
	GetClusterResources(ctx context.Context, resourceType string) ([]proxmox.ClusterResource, error)
	IsClusterMember(ctx context.Context) (bool, error)
	GetVMFSInfo(ctx context.Context, node string, vmid int) ([]proxmox.VMFileSystem, error)
	GetVMNetworkInterfaces(ctx context.Context, node string, vmid int) ([]proxmox.VMNetworkInterface, error)
	GetVMAgentInfo(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	GetVMAgentVersion(ctx context.Context, node string, vmid int) (string, error)
	GetZFSPoolStatus(ctx context.Context, node string) ([]proxmox.ZFSPoolStatus, error)
	GetZFSPoolsWithDetails(ctx context.Context, node string) ([]proxmox.ZFSPoolInfo, error)
	GetDisks(ctx context.Context, node string) ([]proxmox.Disk, error)
	GetCephStatus(ctx context.Context) (*proxmox.CephStatus, error)
	GetCephDF(ctx context.Context) (*proxmox.CephDF, error)
}

func getNodeDisplayName(instance *config.PVEInstance, nodeName string) string {
	baseName := strings.TrimSpace(nodeName)
	if baseName == "" {
		baseName = "unknown-node"
	}

	if instance == nil {
		return baseName
	}

	friendly := strings.TrimSpace(instance.Name)

	if instance.IsCluster {
		if endpointLabel := lookupClusterEndpointLabel(instance, nodeName); endpointLabel != "" {
			return endpointLabel
		}

		if baseName != "" && baseName != "unknown-node" {
			return baseName
		}

		if friendly != "" {
			return friendly
		}

		return baseName
	}

	if friendly != "" {
		return friendly
	}

	if baseName != "" && baseName != "unknown-node" {
		return baseName
	}

	if label := normalizeEndpointHost(instance.Host); label != "" && !isLikelyIPAddress(label) {
		return label
	}

	return baseName
}

func mergeNVMeTempsIntoDisks(disks []models.PhysicalDisk, nodes []models.Node) []models.PhysicalDisk {
	if len(disks) == 0 || len(nodes) == 0 {
		return disks
	}

	nvmeTempsByNode := make(map[string][]models.NVMeTemp)
	for _, node := range nodes {
		if node.Temperature == nil || !node.Temperature.Available || len(node.Temperature.NVMe) == 0 {
			continue
		}

		temps := make([]models.NVMeTemp, len(node.Temperature.NVMe))
		copy(temps, node.Temperature.NVMe)
		sort.Slice(temps, func(i, j int) bool {
			return temps[i].Device < temps[j].Device
		})

		nvmeTempsByNode[node.Name] = temps
	}

	if len(nvmeTempsByNode) == 0 {
		return disks
	}

	updated := make([]models.PhysicalDisk, len(disks))
	copy(updated, disks)

	disksByNode := make(map[string][]int)
	for i := range updated {
		if strings.EqualFold(updated[i].Type, "nvme") {
			disksByNode[updated[i].Node] = append(disksByNode[updated[i].Node], i)
		}
	}

	for nodeName, diskIndexes := range disksByNode {
		temps, ok := nvmeTempsByNode[nodeName]
		if !ok || len(temps) == 0 {
			for _, idx := range diskIndexes {
				updated[idx].Temperature = 0
			}
			continue
		}

		sort.Slice(diskIndexes, func(i, j int) bool {
			return updated[diskIndexes[i]].DevPath < updated[diskIndexes[j]].DevPath
		})

		for _, idx := range diskIndexes {
			updated[idx].Temperature = 0
		}

		for idx, diskIdx := range diskIndexes {
			if idx >= len(temps) {
				break
			}

			tempVal := temps[idx].Temp
			if tempVal <= 0 || math.IsNaN(tempVal) {
				continue
			}

			updated[diskIdx].Temperature = int(math.Round(tempVal))
		}
	}

	return updated
}

func lookupClusterEndpointLabel(instance *config.PVEInstance, nodeName string) string {
	if instance == nil {
		return ""
	}

	for _, endpoint := range instance.ClusterEndpoints {
		if !strings.EqualFold(endpoint.NodeName, nodeName) {
			continue
		}

		if host := strings.TrimSpace(endpoint.Host); host != "" {
			if label := normalizeEndpointHost(host); label != "" && !isLikelyIPAddress(label) {
				return label
			}
		}

		if nodeNameLabel := strings.TrimSpace(endpoint.NodeName); nodeNameLabel != "" {
			return nodeNameLabel
		}

		if ip := strings.TrimSpace(endpoint.IP); ip != "" {
			return ip
		}
	}

	return ""
}

func normalizeEndpointHost(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}

	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		host := parsed.Hostname()
		if host != "" {
			return host
		}
		return parsed.Host
	}

	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if idx := strings.Index(value, "/"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}

	if idx := strings.Index(value, ":"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}

	return value
}

func isLikelyIPAddress(value string) bool {
	if value == "" {
		return false
	}

	if ip := net.ParseIP(value); ip != nil {
		return true
	}

	// Handle IPv6 with zone identifier (fe80::1%eth0)
	if i := strings.Index(value, "%"); i > 0 {
		if ip := net.ParseIP(value[:i]); ip != nil {
			return true
		}
	}

	return false
}

func ensureClusterEndpointURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}

	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return value
	}

	if _, _, err := net.SplitHostPort(value); err == nil {
		return "https://" + value
	}

	return "https://" + net.JoinHostPort(value, "8006")
}

func clusterEndpointEffectiveURL(endpoint config.ClusterEndpoint) string {
	if endpoint.Host != "" {
		return ensureClusterEndpointURL(endpoint.Host)
	}
	if endpoint.IP != "" {
		return ensureClusterEndpointURL(endpoint.IP)
	}
	return ""
}

// PollExecutor defines the contract for executing polling tasks.
type PollExecutor interface {
	Execute(ctx context.Context, task PollTask)
}

type realExecutor struct {
	monitor *Monitor
}

func newRealExecutor(m *Monitor) PollExecutor {
	return &realExecutor{monitor: m}
}

func (r *realExecutor) Execute(ctx context.Context, task PollTask) {
	if r == nil || r.monitor == nil {
		return
	}

	switch strings.ToLower(task.InstanceType) {
	case "pve":
		if task.PVEClient == nil {
			log.Warn().
				Str("instance", task.InstanceName).
				Msg("PollExecutor received nil PVE client")
			return
		}
		r.monitor.pollPVEInstance(ctx, task.InstanceName, task.PVEClient)
	case "pbs":
		if task.PBSClient == nil {
			log.Warn().
				Str("instance", task.InstanceName).
				Msg("PollExecutor received nil PBS client")
			return
		}
		r.monitor.pollPBSInstance(ctx, task.InstanceName, task.PBSClient)
	case "pmg":
		if task.PMGClient == nil {
			log.Warn().
				Str("instance", task.InstanceName).
				Msg("PollExecutor received nil PMG client")
			return
		}
		r.monitor.pollPMGInstance(ctx, task.InstanceName, task.PMGClient)
	default:
		if logging.IsLevelEnabled(zerolog.DebugLevel) {
			log.Debug().
				Str("instance", task.InstanceName).
				Str("type", task.InstanceType).
				Msg("PollExecutor received unsupported task type")
		}
	}
}

type instanceInfo struct {
	Key         string
	Type        InstanceType
	DisplayName string
	Connection  string
	Metadata    map[string]string
}

type pollStatus struct {
	LastSuccess         time.Time
	LastErrorAt         time.Time
	LastErrorMessage    string
	LastErrorCategory   string
	ConsecutiveFailures int
	FirstFailureAt      time.Time
}

type dlqInsight struct {
	Reason       string
	FirstAttempt time.Time
	LastAttempt  time.Time
	RetryCount   int
	NextRetry    time.Time
}

type ErrorDetail struct {
	At       time.Time `json:"at"`
	Message  string    `json:"message"`
	Category string    `json:"category"`
}

type InstancePollStatus struct {
	LastSuccess         *time.Time   `json:"lastSuccess,omitempty"`
	LastError           *ErrorDetail `json:"lastError,omitempty"`
	ConsecutiveFailures int          `json:"consecutiveFailures"`
	FirstFailureAt      *time.Time   `json:"firstFailureAt,omitempty"`
}

type InstanceBreaker struct {
	State          string     `json:"state"`
	Since          *time.Time `json:"since,omitempty"`
	LastTransition *time.Time `json:"lastTransition,omitempty"`
	RetryAt        *time.Time `json:"retryAt,omitempty"`
	FailureCount   int        `json:"failureCount"`
}

type InstanceDLQ struct {
	Present      bool       `json:"present"`
	Reason       string     `json:"reason,omitempty"`
	FirstAttempt *time.Time `json:"firstAttempt,omitempty"`
	LastAttempt  *time.Time `json:"lastAttempt,omitempty"`
	RetryCount   int        `json:"retryCount,omitempty"`
	NextRetry    *time.Time `json:"nextRetry,omitempty"`
}

type InstanceHealth struct {
	Key         string             `json:"key"`
	Type        string             `json:"type"`
	DisplayName string             `json:"displayName"`
	Instance    string             `json:"instance"`
	Connection  string             `json:"connection"`
	PollStatus  InstancePollStatus `json:"pollStatus"`
	Breaker     InstanceBreaker    `json:"breaker"`
	DeadLetter  InstanceDLQ        `json:"deadLetter"`
}

func schedulerKey(instanceType InstanceType, name string) string {
	return string(instanceType) + "::" + name
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	copy := t
	return &copy
}

// Monitor handles all monitoring operations
type Monitor struct {
	config                *config.Config
	state                 *models.State
	pveClients            map[string]PVEClientInterface
	pbsClients            map[string]*pbs.Client
	pmgClients            map[string]*pmg.Client
	pollMetrics           *PollMetrics
	scheduler             *AdaptiveScheduler
	stalenessTracker      *StalenessTracker
	taskQueue             *TaskQueue
	circuitBreakers       map[string]*circuitBreaker
	deadLetterQueue       *TaskQueue
	failureCounts         map[string]int
	lastOutcome           map[string]taskOutcome
	backoffCfg            backoffConfig
	rng                   *rand.Rand
	maxRetryAttempts      int
	tempCollector         *TemperatureCollector // SSH-based temperature collector
	mu                    sync.RWMutex
	startTime             time.Time
	rateTracker           *RateTracker
	metricsHistory        *MetricsHistory
	alertManager          *alerts.Manager
	notificationMgr       *notifications.NotificationManager
	configPersist         *config.ConfigPersistence
	discoveryService      *discovery.Service        // Background discovery service
	activePollCount       int32                     // Number of active polling operations
	pollCounter           int64                     // Counter for polling cycles
	authFailures          map[string]int            // Track consecutive auth failures per node
	lastAuthAttempt       map[string]time.Time      // Track last auth attempt time
	lastClusterCheck      map[string]time.Time      // Track last cluster check for standalone nodes
	lastPhysicalDiskPoll  map[string]time.Time      // Track last physical disk poll time per instance
	lastPVEBackupPoll     map[string]time.Time      // Track last PVE backup poll per instance
	lastPBSBackupPoll     map[string]time.Time      // Track last PBS backup poll per instance
	persistence           *config.ConfigPersistence // Add persistence for saving updated configs
	pbsBackupPollers      map[string]bool           // Track PBS backup polling goroutines per instance
	runtimeCtx            context.Context           // Context used while monitor is running
	wsHub                 *websocket.Hub            // Hub used for broadcasting state
	diagMu                sync.RWMutex              // Protects diagnostic snapshot maps
	nodeSnapshots         map[string]NodeMemorySnapshot
	guestSnapshots        map[string]GuestMemorySnapshot
	rrdCacheMu            sync.RWMutex // Protects RRD memavailable cache
	nodeRRDMemCache       map[string]rrdMemCacheEntry
	removedDockerHosts    map[string]time.Time // Track deliberately removed Docker hosts (ID -> removal time)
	dockerCommands        map[string]*dockerHostCommand
	dockerCommandIndex    map[string]string
	guestMetadataMu       sync.RWMutex
	guestMetadataCache    map[string]guestMetadataCacheEntry
	executor              PollExecutor
	breakerBaseRetry      time.Duration
	breakerMaxDelay       time.Duration
	breakerHalfOpenWindow time.Duration
	instanceInfoCache     map[string]*instanceInfo
	pollStatusMap         map[string]*pollStatus
	dlqInsightMap         map[string]*dlqInsight
}

type rrdMemCacheEntry struct {
	available uint64
	used      uint64
	total     uint64
	fetchedAt time.Time
}

// safePercentage calculates percentage safely, returning 0 if divisor is 0
func safePercentage(used, total float64) float64 {
	if total == 0 {
		return 0
	}
	result := used / total * 100
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0
	}
	return result
}

// maxInt64 returns the maximum of two int64 values
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// safeFloat ensures a float value is not NaN or Inf
func safeFloat(val float64) float64 {
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val
}

func cloneStringFloatMap(src map[string]float64) map[string]float64 {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]float64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// shouldRunBackupPoll determines whether a backup polling cycle should execute.
// Returns whether polling should run, a human-readable skip reason, and the timestamp to record.
func (m *Monitor) shouldRunBackupPoll(last time.Time, now time.Time) (bool, string, time.Time) {
	if m == nil || m.config == nil {
		return false, "configuration unavailable", last
	}

	if !m.config.EnableBackupPolling {
		return false, "backup polling globally disabled", last
	}

	interval := m.config.BackupPollingInterval
	if interval > 0 {
		if !last.IsZero() && now.Sub(last) < interval {
			next := last.Add(interval)
			return false, fmt.Sprintf("next run scheduled for %s", next.Format(time.RFC3339)), last
		}
		return true, "", now
	}

	backupCycles := m.config.BackupPollingCycles
	if backupCycles <= 0 {
		backupCycles = 10
	}

	if m.pollCounter%int64(backupCycles) == 0 || m.pollCounter == 1 {
		return true, "", now
	}

	remaining := int64(backupCycles) - (m.pollCounter % int64(backupCycles))
	if remaining <= 0 {
		remaining = int64(backupCycles)
	}
	return false, fmt.Sprintf("next run in %d polling cycles", remaining), last
}

const (
	dockerConnectionPrefix       = "docker-"
	hostConnectionPrefix         = "host-"
	dockerOfflineGraceMultiplier = 4
	dockerMinimumHealthWindow    = 30 * time.Second
	dockerMaximumHealthWindow    = 10 * time.Minute
	nodeRRDCacheTTL              = 30 * time.Second
	nodeRRDRequestTimeout        = 2 * time.Second
	guestMetadataCacheTTL        = 5 * time.Minute
)

type guestMetadataCacheEntry struct {
	ipAddresses       []string
	networkInterfaces []models.GuestNetworkInterface
	osName            string
	osVersion         string
	agentVersion      string
	fetchedAt         time.Time
}

type taskOutcome struct {
	success    bool
	transient  bool
	err        error
	recordedAt time.Time
}

func (m *Monitor) getNodeRRDMetrics(ctx context.Context, client PVEClientInterface, nodeName string) (rrdMemCacheEntry, error) {
	if client == nil || nodeName == "" {
		return rrdMemCacheEntry{}, fmt.Errorf("invalid arguments for RRD lookup")
	}

	now := time.Now()

	m.rrdCacheMu.RLock()
	if entry, ok := m.nodeRRDMemCache[nodeName]; ok && now.Sub(entry.fetchedAt) < nodeRRDCacheTTL {
		m.rrdCacheMu.RUnlock()
		return entry, nil
	}
	m.rrdCacheMu.RUnlock()

	requestCtx, cancel := context.WithTimeout(ctx, nodeRRDRequestTimeout)
	defer cancel()

	points, err := client.GetNodeRRDData(requestCtx, nodeName, "hour", "AVERAGE", []string{"memavailable", "memused", "memtotal"})
	if err != nil {
		return rrdMemCacheEntry{}, err
	}

	var memAvailable uint64
	var memUsed uint64
	var memTotal uint64

	for i := len(points) - 1; i >= 0; i-- {
		point := points[i]

		if memTotal == 0 && point.MemTotal != nil && !math.IsNaN(*point.MemTotal) && *point.MemTotal > 0 {
			memTotal = uint64(math.Round(*point.MemTotal))
		}

		if memAvailable == 0 && point.MemAvailable != nil && !math.IsNaN(*point.MemAvailable) && *point.MemAvailable > 0 {
			memAvailable = uint64(math.Round(*point.MemAvailable))
		}

		if memUsed == 0 && point.MemUsed != nil && !math.IsNaN(*point.MemUsed) && *point.MemUsed > 0 {
			memUsed = uint64(math.Round(*point.MemUsed))
		}

		if memTotal > 0 && (memAvailable > 0 || memUsed > 0) {
			break
		}
	}

	if memTotal > 0 {
		if memAvailable > memTotal {
			memAvailable = memTotal
		}
		if memUsed > memTotal {
			memUsed = memTotal
		}
	}

	if memAvailable == 0 && memUsed == 0 {
		return rrdMemCacheEntry{}, fmt.Errorf("rrd mem metrics not present")
	}

	entry := rrdMemCacheEntry{
		available: memAvailable,
		used:      memUsed,
		total:     memTotal,
		fetchedAt: now,
	}

	m.rrdCacheMu.Lock()
	m.nodeRRDMemCache[nodeName] = entry
	m.rrdCacheMu.Unlock()

	return entry, nil
}

// RemoveDockerHost removes a docker host from the shared state and clears related alerts.
func (m *Monitor) RemoveDockerHost(hostID string) (models.DockerHost, error) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return models.DockerHost{}, fmt.Errorf("docker host id is required")
	}

	host, removed := m.state.RemoveDockerHost(hostID)
	if !removed {
		if logging.IsLevelEnabled(zerolog.DebugLevel) {
			log.Debug().Str("dockerHostID", hostID).Msg("Docker host not present in state during removal; proceeding to clear alerts")
		}
		host = models.DockerHost{
			ID:          hostID,
			Hostname:    hostID,
			DisplayName: hostID,
		}
	}

	// Track removal to prevent resurrection from cached reports
	m.mu.Lock()
	m.removedDockerHosts[hostID] = time.Now()
	if cmd, ok := m.dockerCommands[hostID]; ok {
		delete(m.dockerCommandIndex, cmd.status.ID)
	}
	delete(m.dockerCommands, hostID)
	m.mu.Unlock()

	m.state.RemoveConnectionHealth(dockerConnectionPrefix + hostID)
	if m.alertManager != nil {
		m.alertManager.HandleDockerHostRemoved(host)
		m.SyncAlertState()
	}

	log.Info().
		Str("dockerHost", host.Hostname).
		Str("dockerHostID", hostID).
		Bool("removed", removed).
		Msg("Docker host removed and alerts cleared")

	return host, nil
}

// HideDockerHost marks a docker host as hidden without removing it from state.
// Hidden hosts will not be shown in the frontend but will continue to accept updates.
func (m *Monitor) HideDockerHost(hostID string) (models.DockerHost, error) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return models.DockerHost{}, fmt.Errorf("docker host id is required")
	}

	host, ok := m.state.SetDockerHostHidden(hostID, true)
	if !ok {
		return models.DockerHost{}, fmt.Errorf("docker host %q not found", hostID)
	}

	log.Info().
		Str("dockerHost", host.Hostname).
		Str("dockerHostID", hostID).
		Msg("Docker host hidden from view")

	return host, nil
}

// UnhideDockerHost marks a docker host as visible again.
func (m *Monitor) UnhideDockerHost(hostID string) (models.DockerHost, error) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return models.DockerHost{}, fmt.Errorf("docker host id is required")
	}

	host, ok := m.state.SetDockerHostHidden(hostID, false)
	if !ok {
		return models.DockerHost{}, fmt.Errorf("docker host %q not found", hostID)
	}

	// Clear removal tracking if it was marked as removed
	m.mu.Lock()
	delete(m.removedDockerHosts, hostID)
	m.mu.Unlock()

	log.Info().
		Str("dockerHost", host.Hostname).
		Str("dockerHostID", hostID).
		Msg("Docker host unhidden")

	return host, nil
}

// MarkDockerHostPendingUninstall marks a docker host as pending uninstall.
// This is used when the user has run the uninstall command and is waiting for the host to go offline.
func (m *Monitor) MarkDockerHostPendingUninstall(hostID string) (models.DockerHost, error) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return models.DockerHost{}, fmt.Errorf("docker host id is required")
	}

	host, ok := m.state.SetDockerHostPendingUninstall(hostID, true)
	if !ok {
		return models.DockerHost{}, fmt.Errorf("docker host %q not found", hostID)
	}

	log.Info().
		Str("dockerHost", host.Hostname).
		Str("dockerHostID", hostID).
		Msg("Docker host marked as pending uninstall")

	return host, nil
}

// AllowDockerHostReenroll removes a host ID from the removal blocklist so it can report again.
func (m *Monitor) AllowDockerHostReenroll(hostID string) error {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return fmt.Errorf("docker host id is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.removedDockerHosts[hostID]; !exists {
		log.Debug().
			Str("dockerHostID", hostID).
			Msg("Allow re-enroll requested for docker host that was not blocked")
		return nil
	}

	delete(m.removedDockerHosts, hostID)
	if cmd, exists := m.dockerCommands[hostID]; exists {
		delete(m.dockerCommandIndex, cmd.status.ID)
		delete(m.dockerCommands, hostID)
	}
	m.state.SetDockerHostCommand(hostID, nil)

	log.Info().
		Str("dockerHostID", hostID).
		Msg("Docker host removal block cleared; host may report again")

	return nil
}

// GetDockerHost retrieves a docker host by identifier if present in state.
func (m *Monitor) GetDockerHost(hostID string) (models.DockerHost, bool) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return models.DockerHost{}, false
	}

	hosts := m.state.GetDockerHosts()
	for _, host := range hosts {
		if host.ID == hostID {
			return host, true
		}
	}
	return models.DockerHost{}, false
}

// GetDockerHosts returns a point-in-time snapshot of all Docker hosts Pulse knows about.
func (m *Monitor) GetDockerHosts() []models.DockerHost {
	if m == nil || m.state == nil {
		return nil
	}
	return m.state.GetDockerHosts()
}

// QueueDockerHostStop queues a stop command for the specified docker host.
func (m *Monitor) QueueDockerHostStop(hostID string) (models.DockerHostCommandStatus, error) {
	return m.queueDockerStopCommand(hostID)
}

// FetchDockerCommandForHost retrieves the next command payload (if any) for the host.
func (m *Monitor) FetchDockerCommandForHost(hostID string) (map[string]any, *models.DockerHostCommandStatus) {
	return m.getDockerCommandPayload(hostID)
}

// AcknowledgeDockerHostCommand updates the lifecycle status for a docker host command.
func (m *Monitor) AcknowledgeDockerHostCommand(commandID, hostID, status, message string) (models.DockerHostCommandStatus, string, bool, error) {
	return m.acknowledgeDockerCommand(commandID, hostID, status, message)
}

func tokenHintFromRecord(record *config.APITokenRecord) string {
	if record == nil {
		return ""
	}
	switch {
	case record.Prefix != "" && record.Suffix != "":
		return fmt.Sprintf("%s…%s", record.Prefix, record.Suffix)
	case record.Prefix != "":
		return record.Prefix + "…"
	case record.Suffix != "":
		return "…" + record.Suffix
	default:
		return ""
	}
}

func resolveDockerHostIdentifier(report agentsdocker.Report, tokenRecord *config.APITokenRecord, hosts []models.DockerHost) (string, []string, models.DockerHost, bool) {
	base := strings.TrimSpace(report.AgentKey())
	fallbacks := uniqueNonEmptyStrings(
		base,
		strings.TrimSpace(report.Agent.ID),
		strings.TrimSpace(report.Host.MachineID),
		strings.TrimSpace(report.Host.Hostname),
	)

	if existing, ok := findMatchingDockerHost(hosts, report, tokenRecord); ok {
		return existing.ID, fallbacks, existing, true
	}

	identifier := base
	if identifier == "" {
		identifier = strings.TrimSpace(report.Host.MachineID)
	}
	if identifier == "" {
		identifier = strings.TrimSpace(report.Host.Hostname)
	}
	if identifier == "" {
		identifier = strings.TrimSpace(report.Agent.ID)
	}
	if identifier == "" {
		identifier = fallbackDockerHostID(report, tokenRecord)
	}
	if identifier == "" {
		identifier = "docker-host"
	}

	if dockerHostIDExists(identifier, hosts) {
		identifier = generateDockerHostIdentifier(identifier, report, tokenRecord, hosts)
	}

	return identifier, fallbacks, models.DockerHost{}, false
}

func findMatchingDockerHost(hosts []models.DockerHost, report agentsdocker.Report, tokenRecord *config.APITokenRecord) (models.DockerHost, bool) {
	agentID := strings.TrimSpace(report.Agent.ID)
	tokenID := ""
	if tokenRecord != nil {
		tokenID = strings.TrimSpace(tokenRecord.ID)
	}
	machineID := strings.TrimSpace(report.Host.MachineID)
	hostname := strings.TrimSpace(report.Host.Hostname)

	if agentID != "" {
		for _, host := range hosts {
			if strings.TrimSpace(host.AgentID) == agentID {
				return host, true
			}
		}
	}

	if tokenID != "" {
		for _, host := range hosts {
			if strings.TrimSpace(host.TokenID) == tokenID {
				return host, true
			}
		}
	}

	if machineID != "" && hostname != "" {
		for _, host := range hosts {
			if strings.TrimSpace(host.MachineID) == machineID && strings.TrimSpace(host.Hostname) == hostname {
				if tokenID == "" || strings.TrimSpace(host.TokenID) == tokenID {
					return host, true
				}
			}
		}
	}

	if machineID != "" && tokenID == "" {
		for _, host := range hosts {
			if strings.TrimSpace(host.MachineID) == machineID && strings.TrimSpace(host.TokenID) == "" {
				return host, true
			}
		}
	}

	if hostname != "" && tokenID == "" {
		for _, host := range hosts {
			if strings.TrimSpace(host.Hostname) == hostname && strings.TrimSpace(host.TokenID) == "" {
				return host, true
			}
		}
	}

	return models.DockerHost{}, false
}

func dockerHostIDExists(id string, hosts []models.DockerHost) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	for _, host := range hosts {
		if host.ID == id {
			return true
		}
	}
	return false
}

func generateDockerHostIdentifier(base string, report agentsdocker.Report, tokenRecord *config.APITokenRecord, hosts []models.DockerHost) string {
	if strings.TrimSpace(base) == "" {
		base = fallbackDockerHostID(report, tokenRecord)
	}
	if strings.TrimSpace(base) == "" {
		base = "docker-host"
	}

	used := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		used[host.ID] = struct{}{}
	}

	suffixes := dockerHostSuffixCandidates(report, tokenRecord)
	for _, suffix := range suffixes {
		candidate := fmt.Sprintf("%s::%s", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}

	seed := strings.Join(suffixes, "|")
	if strings.TrimSpace(seed) == "" {
		seed = base
	}
	sum := sha1.Sum([]byte(seed))
	hashSuffix := fmt.Sprintf("hash-%s", hex.EncodeToString(sum[:6]))
	candidate := fmt.Sprintf("%s::%s", base, hashSuffix)
	if _, exists := used[candidate]; !exists {
		return candidate
	}

	for idx := 2; ; idx++ {
		candidate = fmt.Sprintf("%s::%d", base, idx)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func dockerHostSuffixCandidates(report agentsdocker.Report, tokenRecord *config.APITokenRecord) []string {
	candidates := make([]string, 0, 5)

	if tokenRecord != nil {
		if sanitized := sanitizeDockerHostSuffix(tokenRecord.ID); sanitized != "" {
			candidates = append(candidates, "token-"+sanitized)
		}
	}

	if agentID := sanitizeDockerHostSuffix(report.Agent.ID); agentID != "" {
		candidates = append(candidates, "agent-"+agentID)
	}

	if machineID := sanitizeDockerHostSuffix(report.Host.MachineID); machineID != "" {
		candidates = append(candidates, "machine-"+machineID)
	}

	hostNameSanitized := sanitizeDockerHostSuffix(report.Host.Hostname)
	if hostNameSanitized != "" {
		candidates = append(candidates, "host-"+hostNameSanitized)
	}

	hostDisplay := sanitizeDockerHostSuffix(report.Host.Name)
	if hostDisplay != "" && hostDisplay != hostNameSanitized {
		candidates = append(candidates, "name-"+hostDisplay)
	}

	return uniqueNonEmptyStrings(candidates...)
}

func sanitizeDockerHostSuffix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(value))
	lastHyphen := false
	runeCount := 0

	for _, r := range value {
		if runeCount >= 48 {
			break
		}

		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastHyphen = false
			runeCount++
		default:
			if !lastHyphen {
				builder.WriteRune('-')
				lastHyphen = true
				runeCount++
			}
		}
	}

	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return ""
	}
	return result
}

func fallbackDockerHostID(report agentsdocker.Report, tokenRecord *config.APITokenRecord) string {
	seedParts := dockerHostSuffixCandidates(report, tokenRecord)
	if len(seedParts) == 0 {
		seedParts = uniqueNonEmptyStrings(
			report.Host.Hostname,
			report.Host.MachineID,
			report.Agent.ID,
		)
	}
	if len(seedParts) == 0 {
		return ""
	}
	seed := strings.Join(seedParts, "|")
	sum := sha1.Sum([]byte(seed))
	return fmt.Sprintf("docker-host-%s", hex.EncodeToString(sum[:6]))
}

func uniqueNonEmptyStrings(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// ApplyDockerReport ingests a docker agent report into the shared state.
func (m *Monitor) ApplyDockerReport(report agentsdocker.Report, tokenRecord *config.APITokenRecord) (models.DockerHost, error) {
	hostsSnapshot := m.state.GetDockerHosts()
	identifier, legacyIDs, previous, hasPrevious := resolveDockerHostIdentifier(report, tokenRecord, hostsSnapshot)
	if strings.TrimSpace(identifier) == "" {
		return models.DockerHost{}, fmt.Errorf("docker report missing agent identifier")
	}

	// Check if this host was deliberately removed - reject report to prevent resurrection
	m.mu.RLock()
	removedAt, wasRemoved := m.removedDockerHosts[identifier]
	if !wasRemoved {
		for _, legacyID := range legacyIDs {
			if legacyID == "" || legacyID == identifier {
				continue
			}
			if ts, ok := m.removedDockerHosts[legacyID]; ok {
				removedAt = ts
				wasRemoved = true
				break
			}
		}
	}
	m.mu.RUnlock()

	if wasRemoved {
		log.Info().
			Str("dockerHostID", identifier).
			Time("removedAt", removedAt).
			Msg("Rejecting report from deliberately removed Docker host")
		return models.DockerHost{}, fmt.Errorf("docker host %q was removed at %v and cannot report again", identifier, removedAt.Format(time.RFC3339))
	}

	hostname := strings.TrimSpace(report.Host.Hostname)
	if hostname == "" {
		return models.DockerHost{}, fmt.Errorf("docker report missing hostname")
	}

	timestamp := report.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	agentID := strings.TrimSpace(report.Agent.ID)
	if agentID == "" {
		agentID = identifier
	}

	displayName := strings.TrimSpace(report.Host.Name)
	if displayName == "" {
		displayName = hostname
	}

	containers := make([]models.DockerContainer, 0, len(report.Containers))
	for _, payload := range report.Containers {
		container := models.DockerContainer{
			ID:            payload.ID,
			Name:          payload.Name,
			Image:         payload.Image,
			State:         payload.State,
			Status:        payload.Status,
			Health:        payload.Health,
			CPUPercent:    safeFloat(payload.CPUPercent),
			MemoryUsage:   payload.MemoryUsageBytes,
			MemoryLimit:   payload.MemoryLimitBytes,
			MemoryPercent: safeFloat(payload.MemoryPercent),
			UptimeSeconds: payload.UptimeSeconds,
			RestartCount:  payload.RestartCount,
			ExitCode:      payload.ExitCode,
			CreatedAt:     payload.CreatedAt,
			StartedAt:     payload.StartedAt,
			FinishedAt:    payload.FinishedAt,
		}

		if len(payload.Ports) > 0 {
			ports := make([]models.DockerContainerPort, len(payload.Ports))
			for i, port := range payload.Ports {
				ports[i] = models.DockerContainerPort{
					PrivatePort: port.PrivatePort,
					PublicPort:  port.PublicPort,
					Protocol:    port.Protocol,
					IP:          port.IP,
				}
			}
			container.Ports = ports
		}

		if len(payload.Labels) > 0 {
			labels := make(map[string]string, len(payload.Labels))
			for k, v := range payload.Labels {
				labels[k] = v
			}
			container.Labels = labels
		}

		if len(payload.Networks) > 0 {
			networks := make([]models.DockerContainerNetworkLink, len(payload.Networks))
			for i, net := range payload.Networks {
				networks[i] = models.DockerContainerNetworkLink{
					Name: net.Name,
					IPv4: net.IPv4,
					IPv6: net.IPv6,
				}
			}
			container.Networks = networks
		}

		containers = append(containers, container)
	}

	host := models.DockerHost{
		ID:               identifier,
		AgentID:          agentID,
		Hostname:         hostname,
		DisplayName:      displayName,
		MachineID:        strings.TrimSpace(report.Host.MachineID),
		OS:               report.Host.OS,
		KernelVersion:    report.Host.KernelVersion,
		Architecture:     report.Host.Architecture,
		DockerVersion:    report.Host.DockerVersion,
		CPUs:             report.Host.TotalCPU,
		TotalMemoryBytes: report.Host.TotalMemoryBytes,
		UptimeSeconds:    report.Host.UptimeSeconds,
		Status:           "online",
		LastSeen:         timestamp,
		IntervalSeconds:  report.Agent.IntervalSeconds,
		AgentVersion:     report.Agent.Version,
		Containers:       containers,
	}

	if tokenRecord != nil {
		host.TokenID = tokenRecord.ID
		host.TokenName = tokenRecord.Name
		host.TokenHint = tokenHintFromRecord(tokenRecord)
		if tokenRecord.LastUsedAt != nil {
			t := tokenRecord.LastUsedAt.UTC()
			host.TokenLastUsedAt = &t
		} else {
			t := time.Now().UTC()
			host.TokenLastUsedAt = &t
		}
	} else if hasPrevious {
		host.TokenID = previous.TokenID
		host.TokenName = previous.TokenName
		host.TokenHint = previous.TokenHint
		host.TokenLastUsedAt = previous.TokenLastUsedAt
	}

	m.state.UpsertDockerHost(host)
	m.state.SetConnectionHealth(dockerConnectionPrefix+host.ID, true)

	// Check if the host was previously hidden and is now visible again
	if hasPrevious && previous.Hidden && !host.Hidden {
		log.Info().
			Str("dockerHost", host.Hostname).
			Str("dockerHostID", host.ID).
			Msg("Docker host auto-unhidden after receiving report")
	}

	// Check if the host was pending uninstall - if so, log a warning that uninstall failed and clear the flag
	if hasPrevious && previous.PendingUninstall {
		log.Warn().
			Str("dockerHost", host.Hostname).
			Str("dockerHostID", host.ID).
			Msg("Docker host reporting again after pending uninstall - uninstall may have failed")

		// Clear the pending uninstall flag since the host is clearly still active
		m.state.SetDockerHostPendingUninstall(host.ID, false)
	}

	if m.alertManager != nil {
		m.alertManager.CheckDockerHost(host)
	}

	log.Debug().
		Str("dockerHost", host.Hostname).
		Int("containers", len(containers)).
		Msg("Docker host report processed")

	return host, nil
}

// ApplyHostReport ingests a host agent report into the shared state.
func (m *Monitor) ApplyHostReport(report agentshost.Report, tokenRecord *config.APITokenRecord) (models.Host, error) {
	hostname := strings.TrimSpace(report.Host.Hostname)
	if hostname == "" {
		return models.Host{}, fmt.Errorf("host report missing hostname")
	}

	identifier := strings.TrimSpace(report.Host.ID)
	if identifier != "" {
		identifier = sanitizeDockerHostSuffix(identifier)
	}
	if identifier == "" {
		if machine := sanitizeDockerHostSuffix(report.Host.MachineID); machine != "" {
			identifier = machine
		}
	}
	if identifier == "" {
		if agentID := sanitizeDockerHostSuffix(report.Agent.ID); agentID != "" {
			identifier = agentID
		}
	}
	if identifier == "" {
		if hostName := sanitizeDockerHostSuffix(hostname); hostName != "" {
			identifier = hostName
		}
	}
	if identifier == "" {
		seedParts := uniqueNonEmptyStrings(
			report.Host.MachineID,
			report.Agent.ID,
			report.Host.Hostname,
		)
		if len(seedParts) == 0 {
			seedParts = []string{hostname}
		}
		seed := strings.Join(seedParts, "|")
		sum := sha1.Sum([]byte(seed))
		identifier = fmt.Sprintf("host-%s", hex.EncodeToString(sum[:6]))
	}

	existingHosts := m.state.GetHosts()
	var previous models.Host
	var hasPrevious bool
	for _, candidate := range existingHosts {
		if candidate.ID == identifier {
			previous = candidate
			hasPrevious = true
			break
		}
	}

	displayName := strings.TrimSpace(report.Host.DisplayName)
	if displayName == "" {
		displayName = hostname
	}

	timestamp := report.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	memory := models.Memory{
		Total:     report.Metrics.Memory.TotalBytes,
		Used:      report.Metrics.Memory.UsedBytes,
		Free:      report.Metrics.Memory.FreeBytes,
		Usage:     safeFloat(report.Metrics.Memory.Usage),
		SwapTotal: report.Metrics.Memory.SwapTotal,
		SwapUsed:  report.Metrics.Memory.SwapUsed,
	}
	if memory.Usage <= 0 && memory.Total > 0 {
		memory.Usage = safePercentage(float64(memory.Used), float64(memory.Total))
	}

	disks := make([]models.Disk, 0, len(report.Disks))
	for _, disk := range report.Disks {
		usage := safeFloat(disk.Usage)
		if usage <= 0 && disk.TotalBytes > 0 {
			usage = safePercentage(float64(disk.UsedBytes), float64(disk.TotalBytes))
		}
		disks = append(disks, models.Disk{
			Total:      disk.TotalBytes,
			Used:       disk.UsedBytes,
			Free:       disk.FreeBytes,
			Usage:      usage,
			Mountpoint: disk.Mountpoint,
			Type:       disk.Type,
			Device:     disk.Device,
		})
	}

	network := make([]models.HostNetworkInterface, 0, len(report.Network))
	for _, nic := range report.Network {
		network = append(network, models.HostNetworkInterface{
			Name:      nic.Name,
			MAC:       nic.MAC,
			Addresses: append([]string(nil), nic.Addresses...),
			RXBytes:   nic.RXBytes,
			TXBytes:   nic.TXBytes,
			SpeedMbps: nic.SpeedMbps,
		})
	}

	host := models.Host{
		ID:                identifier,
		Hostname:          hostname,
		DisplayName:       displayName,
		Platform:          strings.TrimSpace(strings.ToLower(report.Host.Platform)),
		OSName:            strings.TrimSpace(report.Host.OSName),
		OSVersion:         strings.TrimSpace(report.Host.OSVersion),
		KernelVersion:     strings.TrimSpace(report.Host.KernelVersion),
		Architecture:      strings.TrimSpace(report.Host.Architecture),
		CPUCount:          report.Host.CPUCount,
		CPUUsage:          safeFloat(report.Metrics.CPUUsagePercent),
		LoadAverage:       append([]float64(nil), report.Host.LoadAverage...),
		Memory:            memory,
		Disks:             disks,
		NetworkInterfaces: network,
		Sensors: models.HostSensorSummary{
			TemperatureCelsius: cloneStringFloatMap(report.Sensors.TemperatureCelsius),
			FanRPM:             cloneStringFloatMap(report.Sensors.FanRPM),
			Additional:         cloneStringFloatMap(report.Sensors.Additional),
		},
		Status:          "online",
		UptimeSeconds:   report.Host.UptimeSeconds,
		IntervalSeconds: report.Agent.IntervalSeconds,
		LastSeen:        timestamp,
		AgentVersion:    strings.TrimSpace(report.Agent.Version),
		Tags:            append([]string(nil), report.Tags...),
	}

	if len(host.LoadAverage) == 0 {
		host.LoadAverage = nil
	}
	if len(host.Disks) == 0 {
		host.Disks = nil
	}
	if len(host.NetworkInterfaces) == 0 {
		host.NetworkInterfaces = nil
	}

	if tokenRecord != nil {
		host.TokenID = tokenRecord.ID
		host.TokenName = tokenRecord.Name
		host.TokenHint = tokenHintFromRecord(tokenRecord)
		if tokenRecord.LastUsedAt != nil {
			t := tokenRecord.LastUsedAt.UTC()
			host.TokenLastUsedAt = &t
		} else {
			now := time.Now().UTC()
			host.TokenLastUsedAt = &now
		}
	} else if hasPrevious {
		host.TokenID = previous.TokenID
		host.TokenName = previous.TokenName
		host.TokenHint = previous.TokenHint
		host.TokenLastUsedAt = previous.TokenLastUsedAt
	}

	m.state.UpsertHost(host)
	m.state.SetConnectionHealth(hostConnectionPrefix+host.ID, true)

	return host, nil
}

const (
	removedDockerHostsTTL = 24 * time.Hour // Clean up removed hosts tracking after 24 hours
)

// cleanupRemovedDockerHosts removes entries from the removed hosts map that are older than 24 hours.
func (m *Monitor) cleanupRemovedDockerHosts(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for hostID, removedAt := range m.removedDockerHosts {
		if now.Sub(removedAt) > removedDockerHostsTTL {
			delete(m.removedDockerHosts, hostID)
			log.Debug().
				Str("dockerHostID", hostID).
				Time("removedAt", removedAt).
				Msg("Cleaned up old removed Docker host entry")
		}
	}
}

// evaluateDockerAgents updates health for Docker hosts based on last report time.
func (m *Monitor) evaluateDockerAgents(now time.Time) {
	hosts := m.state.GetDockerHosts()
	for _, host := range hosts {
		interval := host.IntervalSeconds
		if interval <= 0 {
			interval = int(dockerMinimumHealthWindow / time.Second)
		}

		window := time.Duration(interval) * time.Second * dockerOfflineGraceMultiplier
		if window < dockerMinimumHealthWindow {
			window = dockerMinimumHealthWindow
		} else if window > dockerMaximumHealthWindow {
			window = dockerMaximumHealthWindow
		}

		healthy := !host.LastSeen.IsZero() && now.Sub(host.LastSeen) <= window
		key := dockerConnectionPrefix + host.ID
		m.state.SetConnectionHealth(key, healthy)
		hostCopy := host
		if healthy {
			hostCopy.Status = "online"
			m.state.SetDockerHostStatus(host.ID, "online")
			if m.alertManager != nil {
				m.alertManager.HandleDockerHostOnline(hostCopy)
			}
		} else {
			hostCopy.Status = "offline"
			m.state.SetDockerHostStatus(host.ID, "offline")
			if m.alertManager != nil {
				m.alertManager.HandleDockerHostOffline(hostCopy)
			}
		}
	}
}

// sortContent sorts comma-separated content values for consistent display
func sortContent(content string) string {
	if content == "" {
		return ""
	}
	parts := strings.Split(content, ",")
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (m *Monitor) fetchGuestAgentMetadata(ctx context.Context, client PVEClientInterface, instanceName, nodeName, vmName string, vmid int, vmStatus *proxmox.VMStatus) ([]string, []models.GuestNetworkInterface, string, string, string) {
	if vmStatus == nil || client == nil {
		m.clearGuestMetadataCache(instanceName, nodeName, vmid)
		return nil, nil, "", "", ""
	}

	if vmStatus.Agent <= 0 {
		m.clearGuestMetadataCache(instanceName, nodeName, vmid)
		return nil, nil, "", "", ""
	}

	key := guestMetadataCacheKey(instanceName, nodeName, vmid)
	now := time.Now()

	m.guestMetadataMu.RLock()
	cached, ok := m.guestMetadataCache[key]
	m.guestMetadataMu.RUnlock()

	if ok && now.Sub(cached.fetchedAt) < guestMetadataCacheTTL {
		return cloneStringSlice(cached.ipAddresses), cloneGuestNetworkInterfaces(cached.networkInterfaces), cached.osName, cached.osVersion, cached.agentVersion
	}

	// Start with cached values as fallback in case new calls fail
	ipAddresses := cloneStringSlice(cached.ipAddresses)
	networkIfaces := cloneGuestNetworkInterfaces(cached.networkInterfaces)
	osName := cached.osName
	osVersion := cached.osVersion
	agentVersion := cached.agentVersion

	ifaceCtx, cancelIface := context.WithTimeout(ctx, 5*time.Second)
	interfaces, err := client.GetVMNetworkInterfaces(ifaceCtx, nodeName, vmid)
	cancelIface()
	if err != nil {
		log.Debug().
			Str("instance", instanceName).
			Str("vm", vmName).
			Int("vmid", vmid).
			Err(err).
			Msg("Guest agent network interfaces unavailable")
	} else if len(interfaces) > 0 {
		ipAddresses, networkIfaces = processGuestNetworkInterfaces(interfaces)
	} else {
		ipAddresses = nil
		networkIfaces = nil
	}

	osCtx, cancelOS := context.WithTimeout(ctx, 3*time.Second)
	agentInfo, err := client.GetVMAgentInfo(osCtx, nodeName, vmid)
	cancelOS()
	if err != nil {
		log.Debug().
			Str("instance", instanceName).
			Str("vm", vmName).
			Int("vmid", vmid).
			Err(err).
			Msg("Guest agent OS info unavailable")
	} else if len(agentInfo) > 0 {
		osName, osVersion = extractGuestOSInfo(agentInfo)
	} else {
		osName = ""
		osVersion = ""
	}

	versionCtx, cancelVersion := context.WithTimeout(ctx, 3*time.Second)
	version, err := client.GetVMAgentVersion(versionCtx, nodeName, vmid)
	cancelVersion()
	if err != nil {
		log.Debug().
			Str("instance", instanceName).
			Str("vm", vmName).
			Int("vmid", vmid).
			Err(err).
			Msg("Guest agent version unavailable")
	} else if version != "" {
		agentVersion = version
	} else {
		agentVersion = ""
	}

	entry := guestMetadataCacheEntry{
		ipAddresses:       cloneStringSlice(ipAddresses),
		networkInterfaces: cloneGuestNetworkInterfaces(networkIfaces),
		osName:            osName,
		osVersion:         osVersion,
		agentVersion:      agentVersion,
		fetchedAt:         time.Now(),
	}

	m.guestMetadataMu.Lock()
	if m.guestMetadataCache == nil {
		m.guestMetadataCache = make(map[string]guestMetadataCacheEntry)
	}
	m.guestMetadataCache[key] = entry
	m.guestMetadataMu.Unlock()

	return ipAddresses, networkIfaces, osName, osVersion, agentVersion
}

func guestMetadataCacheKey(instanceName, nodeName string, vmid int) string {
	return fmt.Sprintf("%s|%s|%d", instanceName, nodeName, vmid)
}

func (m *Monitor) clearGuestMetadataCache(instanceName, nodeName string, vmid int) {
	if m == nil {
		return
	}

	key := guestMetadataCacheKey(instanceName, nodeName, vmid)
	m.guestMetadataMu.Lock()
	if m.guestMetadataCache != nil {
		delete(m.guestMetadataCache, key)
	}
	m.guestMetadataMu.Unlock()
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

func cloneGuestNetworkInterfaces(src []models.GuestNetworkInterface) []models.GuestNetworkInterface {
	if len(src) == 0 {
		return nil
	}
	dst := make([]models.GuestNetworkInterface, len(src))
	for i, iface := range src {
		dst[i] = iface
		if len(iface.Addresses) > 0 {
			dst[i].Addresses = cloneStringSlice(iface.Addresses)
		}
	}
	return dst
}

func processGuestNetworkInterfaces(raw []proxmox.VMNetworkInterface) ([]string, []models.GuestNetworkInterface) {
	ipSet := make(map[string]struct{})
	ipAddresses := make([]string, 0)
	guestIfaces := make([]models.GuestNetworkInterface, 0, len(raw))

	for _, iface := range raw {
		ifaceName := strings.TrimSpace(iface.Name)
		mac := strings.TrimSpace(iface.HardwareAddr)

		addrSet := make(map[string]struct{})
		addresses := make([]string, 0, len(iface.IPAddresses))

		for _, addr := range iface.IPAddresses {
			ip := strings.TrimSpace(addr.Address)
			if ip == "" {
				continue
			}
			lower := strings.ToLower(ip)
			if strings.HasPrefix(ip, "127.") || strings.HasPrefix(lower, "fe80") || ip == "::1" {
				continue
			}

			if _, exists := addrSet[ip]; !exists {
				addrSet[ip] = struct{}{}
				addresses = append(addresses, ip)
			}

			if _, exists := ipSet[ip]; !exists {
				ipSet[ip] = struct{}{}
				ipAddresses = append(ipAddresses, ip)
			}
		}

		if len(addresses) > 1 {
			sort.Strings(addresses)
		}

		rxBytes := parseInterfaceStat(iface.Statistics, "rx-bytes")
		txBytes := parseInterfaceStat(iface.Statistics, "tx-bytes")

		if len(addresses) == 0 && rxBytes == 0 && txBytes == 0 {
			continue
		}

		guestIfaces = append(guestIfaces, models.GuestNetworkInterface{
			Name:      ifaceName,
			MAC:       mac,
			Addresses: addresses,
			RXBytes:   rxBytes,
			TXBytes:   txBytes,
		})
	}

	if len(ipAddresses) > 1 {
		sort.Strings(ipAddresses)
	}

	if len(guestIfaces) > 1 {
		sort.SliceStable(guestIfaces, func(i, j int) bool {
			return guestIfaces[i].Name < guestIfaces[j].Name
		})
	}

	return ipAddresses, guestIfaces
}

func parseInterfaceStat(stats interface{}, key string) int64 {
	if stats == nil {
		return 0
	}
	statsMap, ok := stats.(map[string]interface{})
	if !ok {
		return 0
	}
	val, ok := statsMap[key]
	if !ok {
		return 0
	}
	return anyToInt64(val)
}

func extractGuestOSInfo(data map[string]interface{}) (string, string) {
	if data == nil {
		return "", ""
	}

	if result, ok := data["result"]; ok {
		if resultMap, ok := result.(map[string]interface{}); ok {
			data = resultMap
		}
	}

	name := stringValue(data["name"])
	prettyName := stringValue(data["pretty-name"])
	version := stringValue(data["version"])
	versionID := stringValue(data["version-id"])

	osName := name
	if osName == "" {
		osName = prettyName
	}
	if osName == "" {
		osName = stringValue(data["id"])
	}

	osVersion := version
	if osVersion == "" && versionID != "" {
		osVersion = versionID
	}
	if osVersion == "" && prettyName != "" && prettyName != osName {
		osVersion = prettyName
	}
	if osVersion == "" {
		osVersion = stringValue(data["kernel-release"])
	}
	if osVersion == osName {
		osVersion = ""
	}

	return osName, osVersion
}

func stringValue(val interface{}) string {
	switch v := val.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(v, 'f', -1, 64))
	case float32:
		return strings.TrimSpace(strconv.FormatFloat(float64(v), 'f', -1, 32))
	case int:
		return strconv.Itoa(v)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	default:
		return ""
	}
}

func anyToInt64(val interface{}) int64 {
	switch v := val.(type) {
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case uint32:
		return int64(v)
	case uint64:
		if v > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(v)
	case float32:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		if v == "" {
			return 0
		}
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			return parsed
		}
		if parsedFloat, err := strconv.ParseFloat(v, 64); err == nil {
			return int64(parsedFloat)
		}
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return parsed
		}
		if parsedFloat, err := v.Float64(); err == nil {
			return int64(parsedFloat)
		}
	}
	return 0
}

func (m *Monitor) enrichContainerMetadata(ctx context.Context, client PVEClientInterface, instanceName, nodeName string, container *models.Container) {
	if container == nil {
		return
	}

	ensureContainerRootDiskEntry(container)

	if client == nil || container.Status != "running" {
		return
	}

	statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	status, err := client.GetContainerStatus(statusCtx, nodeName, container.VMID)
	cancel()
	if err != nil {
		log.Debug().
			Err(err).
			Str("instance", instanceName).
			Str("node", nodeName).
			Str("container", container.Name).
			Int("vmid", container.VMID).
			Msg("Container status metadata unavailable")
		return
	}
	if status == nil {
		return
	}

	rootDeviceHint := ""
	addressSet := make(map[string]struct{})
	addressOrder := make([]string, 0, 4)

	addAddress := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		if _, exists := addressSet[addr]; exists {
			return
		}
		addressSet[addr] = struct{}{}
		addressOrder = append(addressOrder, addr)
	}

	for _, addr := range sanitizeGuestAddressStrings(status.IP) {
		addAddress(addr)
	}
	for _, addr := range sanitizeGuestAddressStrings(status.IP6) {
		addAddress(addr)
	}
	for _, addr := range parseContainerRawIPs(status.IPv4) {
		addAddress(addr)
	}
	for _, addr := range parseContainerRawIPs(status.IPv6) {
		addAddress(addr)
	}

	networkIfaces := make([]models.GuestNetworkInterface, 0, len(status.Network))
	for rawName, cfg := range status.Network {
		if cfg == (proxmox.ContainerNetworkConfig{}) {
			continue
		}

		iface := models.GuestNetworkInterface{}
		name := strings.TrimSpace(cfg.Name)
		if name == "" {
			name = strings.TrimSpace(rawName)
		}
		if name != "" {
			iface.Name = name
		}
		if mac := strings.TrimSpace(cfg.HWAddr); mac != "" {
			iface.MAC = mac
		}

		addrCandidates := make([]string, 0, 4)
		addrCandidates = append(addrCandidates, collectIPsFromInterface(cfg.IP)...)
		addrCandidates = append(addrCandidates, collectIPsFromInterface(cfg.IP6)...)
		addrCandidates = append(addrCandidates, collectIPsFromInterface(cfg.IPv4)...)
		addrCandidates = append(addrCandidates, collectIPsFromInterface(cfg.IPv6)...)

		if len(addrCandidates) > 0 {
			deduped := dedupeStringsPreserveOrder(addrCandidates)
			if len(deduped) > 0 {
				iface.Addresses = deduped
				for _, addr := range deduped {
					addAddress(addr)
				}
			}
		}

		if iface.Name != "" || iface.MAC != "" || len(iface.Addresses) > 0 {
			networkIfaces = append(networkIfaces, iface)
		}
	}

	configCtx, cancelConfig := context.WithTimeout(ctx, 5*time.Second)
	configData, configErr := client.GetContainerConfig(configCtx, nodeName, container.VMID)
	cancelConfig()
	if configErr != nil {
		log.Debug().
			Err(configErr).
			Str("instance", instanceName).
			Str("node", nodeName).
			Str("container", container.Name).
			Int("vmid", container.VMID).
			Msg("Container config metadata unavailable")
	} else if len(configData) > 0 {
		if hint := extractContainerRootDeviceFromConfig(configData); hint != "" {
			rootDeviceHint = hint
		}
		for _, detail := range parseContainerConfigNetworks(configData) {
			if len(detail.Addresses) > 0 {
				for _, addr := range detail.Addresses {
					addAddress(addr)
				}
			}
			mergeContainerNetworkInterface(&networkIfaces, detail)
		}
	}

	if len(addressOrder) == 0 {
		interfacesCtx, cancelInterfaces := context.WithTimeout(ctx, 5*time.Second)
		ifaceDetails, ifaceErr := client.GetContainerInterfaces(interfacesCtx, nodeName, container.VMID)
		cancelInterfaces()
		if ifaceErr != nil {
			log.Debug().
				Err(ifaceErr).
				Str("instance", instanceName).
				Str("node", nodeName).
				Str("container", container.Name).
				Int("vmid", container.VMID).
				Msg("Container interface metadata unavailable")
		} else if len(ifaceDetails) > 0 {
			for _, detail := range ifaceDetails {
				parsed := containerNetworkDetails{}
				parsed.Name = strings.TrimSpace(detail.Name)
				parsed.MAC = strings.ToUpper(strings.TrimSpace(detail.HWAddr))

				for _, addr := range detail.IPAddresses {
					stripped := strings.TrimSpace(addr.Address)
					if stripped == "" {
						continue
					}
					if slash := strings.Index(stripped, "/"); slash > 0 {
						stripped = stripped[:slash]
					}
					parsed.Addresses = append(parsed.Addresses, sanitizeGuestAddressStrings(stripped)...)
				}

				if len(parsed.Addresses) == 0 && strings.TrimSpace(detail.Inet) != "" {
					parts := strings.Fields(detail.Inet)
					for _, part := range parts {
						stripped := strings.TrimSpace(part)
						if stripped == "" {
							continue
						}
						if slash := strings.Index(stripped, "/"); slash > 0 {
							stripped = stripped[:slash]
						}
						parsed.Addresses = append(parsed.Addresses, sanitizeGuestAddressStrings(stripped)...)
					}
				}

				parsed.Addresses = dedupeStringsPreserveOrder(parsed.Addresses)

				if len(parsed.Addresses) > 0 {
					for _, addr := range parsed.Addresses {
						addAddress(addr)
					}
				}

				if parsed.Name != "" || parsed.MAC != "" || len(parsed.Addresses) > 0 {
					mergeContainerNetworkInterface(&networkIfaces, parsed)
				}
			}
		}
	}

	if len(networkIfaces) > 1 {
		sort.SliceStable(networkIfaces, func(i, j int) bool {
			left := strings.TrimSpace(networkIfaces[i].Name)
			right := strings.TrimSpace(networkIfaces[j].Name)
			return left < right
		})
	}

	if len(addressOrder) > 1 {
		sort.Strings(addressOrder)
	}

	if len(addressOrder) > 0 {
		container.IPAddresses = addressOrder
	}

	if len(networkIfaces) > 0 {
		container.NetworkInterfaces = networkIfaces
	}

	if disks := convertContainerDiskInfo(status); len(disks) > 0 {
		container.Disks = disks
	}

	ensureContainerRootDiskEntry(container)

	if rootDeviceHint != "" && len(container.Disks) > 0 {
		for i := range container.Disks {
			if container.Disks[i].Mountpoint == "/" && container.Disks[i].Device == "" {
				container.Disks[i].Device = rootDeviceHint
			}
		}
	}
}

func ensureContainerRootDiskEntry(container *models.Container) {
	if container == nil || len(container.Disks) > 0 {
		return
	}

	total := container.Disk.Total
	used := container.Disk.Used
	if total > 0 && used > total {
		used = total
	}

	free := total - used
	if free < 0 {
		free = 0
	}

	usage := container.Disk.Usage
	if total > 0 && usage <= 0 {
		usage = safePercentage(float64(used), float64(total))
	}

	container.Disks = []models.Disk{
		{
			Total:      total,
			Used:       used,
			Free:       free,
			Usage:      usage,
			Mountpoint: "/",
			Type:       "rootfs",
		},
	}
}

func convertContainerDiskInfo(status *proxmox.Container) []models.Disk {
	if status == nil || len(status.DiskInfo) == 0 {
		return nil
	}

	disks := make([]models.Disk, 0, len(status.DiskInfo))
	for name, info := range status.DiskInfo {
		total := clampToInt64(info.Total)
		used := clampToInt64(info.Used)
		if total > 0 && used > total {
			used = total
		}
		free := total - used
		if free < 0 {
			free = 0
		}

		disk := models.Disk{
			Total: total,
			Used:  used,
			Free:  free,
		}

		if total > 0 {
			disk.Usage = safePercentage(float64(used), float64(total))
		}

		label := strings.TrimSpace(name)
		if strings.EqualFold(label, "rootfs") || label == "" {
			disk.Mountpoint = "/"
			disk.Type = "rootfs"
			if device := sanitizeRootFSDevice(status.RootFS); device != "" {
				disk.Device = device
			}
		} else {
			disk.Mountpoint = label
			disk.Type = strings.ToLower(label)
		}

		disks = append(disks, disk)
	}

	if len(disks) > 1 {
		sort.SliceStable(disks, func(i, j int) bool {
			return disks[i].Mountpoint < disks[j].Mountpoint
		})
	}

	return disks
}

func sanitizeRootFSDevice(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	if idx := strings.Index(root, ","); idx != -1 {
		root = root[:idx]
	}
	return root
}

func parseContainerRawIPs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var data interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil
	}
	return collectIPsFromInterface(data)
}

func collectIPsFromInterface(value interface{}) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return sanitizeGuestAddressStrings(v)
	case []interface{}:
		results := make([]string, 0, len(v))
		for _, item := range v {
			results = append(results, collectIPsFromInterface(item)...)
		}
		return results
	case []string:
		results := make([]string, 0, len(v))
		for _, item := range v {
			results = append(results, sanitizeGuestAddressStrings(item)...)
		}
		return results
	case map[string]interface{}:
		results := make([]string, 0)
		for _, key := range []string{"ip", "ip6", "ipv4", "ipv6", "address", "value"} {
			if val, ok := v[key]; ok {
				results = append(results, collectIPsFromInterface(val)...)
			}
		}
		return results
	case json.Number:
		return sanitizeGuestAddressStrings(v.String())
	default:
		return nil
	}
}

func sanitizeGuestAddressStrings(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	lower := strings.ToLower(value)
	switch lower {
	case "dhcp", "manual", "static", "auto", "none", "n/a", "unknown", "0.0.0.0", "::", "::1":
		return nil
	}

	parts := strings.FieldsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	})

	if len(parts) > 1 {
		results := make([]string, 0, len(parts))
		for _, part := range parts {
			results = append(results, sanitizeGuestAddressStrings(part)...)
		}
		return results
	}

	if idx := strings.Index(value, "/"); idx > 0 {
		value = strings.TrimSpace(value[:idx])
	}

	lower = strings.ToLower(value)

	if idx := strings.Index(value, "%"); idx > 0 {
		value = strings.TrimSpace(value[:idx])
		lower = strings.ToLower(value)
	}

	if strings.HasPrefix(value, "127.") || strings.HasPrefix(lower, "0.0.0.0") {
		return nil
	}

	if strings.HasPrefix(lower, "fe80") {
		return nil
	}

	if strings.HasPrefix(lower, "::1") {
		return nil
	}

	return []string{value}
}

func dedupeStringsPreserveOrder(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

type containerNetworkDetails struct {
	Name      string
	MAC       string
	Addresses []string
}

func parseContainerConfigNetworks(config map[string]interface{}) []containerNetworkDetails {
	if len(config) == 0 {
		return nil
	}

	keys := make([]string, 0, len(config))
	for key := range config {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "net") {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)

	results := make([]containerNetworkDetails, 0, len(keys))
	for _, key := range keys {
		raw := fmt.Sprint(config[key])
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		detail := containerNetworkDetails{}
		parts := strings.Split(raw, ",")
		for _, part := range parts {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.ToLower(strings.TrimSpace(kv[0]))
			value := strings.TrimSpace(kv[1])
			switch k {
			case "name":
				detail.Name = value
			case "hwaddr", "mac", "macaddr":
				detail.MAC = strings.ToUpper(value)
			case "ip", "ip6", "ips", "ip6addr", "ip6prefix":
				detail.Addresses = append(detail.Addresses, sanitizeGuestAddressStrings(value)...)
			}
		}

		if detail.Name == "" {
			detail.Name = strings.TrimSpace(key)
		}
		if len(detail.Addresses) > 0 {
			detail.Addresses = dedupeStringsPreserveOrder(detail.Addresses)
		}

		if detail.Name != "" || detail.MAC != "" || len(detail.Addresses) > 0 {
			results = append(results, detail)
		}
	}

	if len(results) == 0 {
		return nil
	}

	return results
}

func mergeContainerNetworkInterface(target *[]models.GuestNetworkInterface, detail containerNetworkDetails) {
	if target == nil {
		return
	}
	if len(detail.Addresses) > 0 {
		detail.Addresses = dedupeStringsPreserveOrder(detail.Addresses)
	}

	findMatch := func() int {
		for i := range *target {
			if detail.Name != "" && (*target)[i].Name != "" && strings.EqualFold((*target)[i].Name, detail.Name) {
				return i
			}
			if detail.MAC != "" && (*target)[i].MAC != "" && strings.EqualFold((*target)[i].MAC, detail.MAC) {
				return i
			}
		}
		return -1
	}

	if idx := findMatch(); idx >= 0 {
		if detail.Name != "" && (*target)[idx].Name == "" {
			(*target)[idx].Name = detail.Name
		}
		if detail.MAC != "" && (*target)[idx].MAC == "" {
			(*target)[idx].MAC = detail.MAC
		}
		if len(detail.Addresses) > 0 {
			combined := append((*target)[idx].Addresses, detail.Addresses...)
			(*target)[idx].Addresses = dedupeStringsPreserveOrder(combined)
		}
		return
	}

	newIface := models.GuestNetworkInterface{
		Name: detail.Name,
		MAC:  detail.MAC,
	}
	if len(detail.Addresses) > 0 {
		newIface.Addresses = dedupeStringsPreserveOrder(detail.Addresses)
	}
	*target = append(*target, newIface)
}

func extractContainerRootDeviceFromConfig(config map[string]interface{}) string {
	if len(config) == 0 {
		return ""
	}
	raw, ok := config["rootfs"]
	if !ok {
		return ""
	}

	value := strings.TrimSpace(fmt.Sprint(raw))
	if value == "" {
		return ""
	}

	parts := strings.Split(value, ",")
	device := strings.TrimSpace(parts[0])
	return device
}

// GetConnectionStatuses returns the current connection status for all nodes
func (m *Monitor) GetConnectionStatuses() map[string]bool {
	if mock.IsMockEnabled() {
		statuses := make(map[string]bool)
		state := mock.GetMockState()
		for _, node := range state.Nodes {
			key := "pve-" + node.Name
			statuses[key] = strings.ToLower(node.Status) == "online"
			if node.Host != "" {
				statuses[node.Host] = strings.ToLower(node.Status) == "online"
			}
		}
		for _, pbsInst := range state.PBSInstances {
			key := "pbs-" + pbsInst.Name
			statuses[key] = strings.ToLower(pbsInst.Status) != "offline"
			if pbsInst.Host != "" {
				statuses[pbsInst.Host] = strings.ToLower(pbsInst.Status) != "offline"
			}
		}

		for _, dockerHost := range state.DockerHosts {
			key := dockerConnectionPrefix + dockerHost.ID
			statuses[key] = strings.ToLower(dockerHost.Status) == "online"
		}
		return statuses
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]bool)

	// Check all configured PVE nodes (not just ones with clients)
	for _, pve := range m.config.PVEInstances {
		key := "pve-" + pve.Name
		// Check if we have a client for this node
		if client, exists := m.pveClients[pve.Name]; exists && client != nil {
			// We have a client, check actual connection health from state
			if m.state != nil && m.state.ConnectionHealth != nil {
				statuses[key] = m.state.ConnectionHealth[pve.Name]
			} else {
				statuses[key] = true // Assume connected if we have a client
			}
		} else {
			// No client means disconnected
			statuses[key] = false
		}
	}

	// Check all configured PBS nodes (not just ones with clients)
	for _, pbs := range m.config.PBSInstances {
		key := "pbs-" + pbs.Name
		// Check if we have a client for this node
		if client, exists := m.pbsClients[pbs.Name]; exists && client != nil {
			// We have a client, check actual connection health from state
			if m.state != nil && m.state.ConnectionHealth != nil {
				statuses[key] = m.state.ConnectionHealth["pbs-"+pbs.Name]
			} else {
				statuses[key] = true // Assume connected if we have a client
			}
		} else {
			// No client means disconnected
			statuses[key] = false
		}
	}

	return statuses
}

// checkContainerizedTempMonitoring logs a security warning if Pulse is running
// in a container with SSH-based temperature monitoring enabled
func checkContainerizedTempMonitoring() {
	// Check if running in container
	isContainer := os.Getenv("PULSE_DOCKER") == "true" || isRunningInContainer()
	if !isContainer {
		return
	}

	// Check if SSH keys exist (indicates temperature monitoring is configured)
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/home/pulse"
	}
	sshKeyPath := homeDir + "/.ssh/id_ed25519"
	if _, err := os.Stat(sshKeyPath); err != nil {
		// No SSH key found, temperature monitoring not configured
		return
	}

	// Log warning
	log.Warn().
		Msg("🔐 SECURITY NOTICE: Pulse is running in a container with SSH-based temperature monitoring enabled. " +
			"SSH private keys are stored inside the container, which could be a security risk if the container is compromised. " +
			"Future versions will use agent-based architecture for better security. " +
			"See documentation for hardening recommendations.")
}

// isRunningInContainer detects if running inside a container
func isRunningInContainer() bool {
	// Check for /.dockerenv
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Check cgroup for container indicators
	data, err := os.ReadFile("/proc/1/cgroup")
	if err == nil {
		content := string(data)
		if strings.Contains(content, "docker") ||
			strings.Contains(content, "lxc") ||
			strings.Contains(content, "containerd") {
			return true
		}
	}

	return false
}

// New creates a new Monitor instance
func New(cfg *config.Config) (*Monitor, error) {
	// Initialize temperature collector with sensors SSH key
	// Will use root user for now - can be made configurable later
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/home/pulse"
	}
	sshKeyPath := filepath.Join(homeDir, ".ssh/id_ed25519_sensors")
	tempCollector := NewTemperatureCollector("root", sshKeyPath)

	// Security warning if running in container with SSH temperature monitoring
	checkContainerizedTempMonitoring()

	stalenessTracker := NewStalenessTracker(getPollMetrics())
	stalenessTracker.SetBounds(cfg.AdaptivePollingBaseInterval, cfg.AdaptivePollingMaxInterval)
	taskQueue := NewTaskQueue()
	deadLetterQueue := NewTaskQueue()
	breakers := make(map[string]*circuitBreaker)
	failureCounts := make(map[string]int)
	lastOutcome := make(map[string]taskOutcome)
	backoff := backoffConfig{
		Initial:    5 * time.Second,
		Multiplier: 2,
		Jitter:     0.2,
		Max:        5 * time.Minute,
	}

	if cfg.AdaptivePollingEnabled && cfg.AdaptivePollingMaxInterval > 0 && cfg.AdaptivePollingMaxInterval <= 15*time.Second {
		backoff.Initial = 750 * time.Millisecond
		backoff.Max = 6 * time.Second
	}

	var scheduler *AdaptiveScheduler
	if cfg.AdaptivePollingEnabled {
		scheduler = NewAdaptiveScheduler(SchedulerConfig{
			BaseInterval: cfg.AdaptivePollingBaseInterval,
			MinInterval:  cfg.AdaptivePollingMinInterval,
			MaxInterval:  cfg.AdaptivePollingMaxInterval,
		}, stalenessTracker, nil, nil)
	}

	m := &Monitor{
		config:               cfg,
		state:                models.NewState(),
		pveClients:           make(map[string]PVEClientInterface),
		pbsClients:           make(map[string]*pbs.Client),
		pmgClients:           make(map[string]*pmg.Client),
		pollMetrics:          getPollMetrics(),
		scheduler:            scheduler,
		stalenessTracker:     stalenessTracker,
		taskQueue:            taskQueue,
		deadLetterQueue:      deadLetterQueue,
		circuitBreakers:      breakers,
		failureCounts:        failureCounts,
		lastOutcome:          lastOutcome,
		backoffCfg:           backoff,
		rng:                  rand.New(rand.NewSource(time.Now().UnixNano())),
		maxRetryAttempts:     5,
		tempCollector:        tempCollector,
		startTime:            time.Now(),
		rateTracker:          NewRateTracker(),
		metricsHistory:       NewMetricsHistory(1000, 24*time.Hour), // Keep up to 1000 points or 24 hours
		alertManager:         alerts.NewManager(),
		notificationMgr:      notifications.NewNotificationManager(cfg.PublicURL),
		configPersist:        config.NewConfigPersistence(cfg.DataPath),
		discoveryService:     nil, // Will be initialized in Start()
		authFailures:         make(map[string]int),
		lastAuthAttempt:      make(map[string]time.Time),
		lastClusterCheck:     make(map[string]time.Time),
		lastPhysicalDiskPoll: make(map[string]time.Time),
		lastPVEBackupPoll:    make(map[string]time.Time),
		lastPBSBackupPoll:    make(map[string]time.Time),
		persistence:          config.NewConfigPersistence(cfg.DataPath),
		pbsBackupPollers:     make(map[string]bool),
		nodeSnapshots:        make(map[string]NodeMemorySnapshot),
		guestSnapshots:       make(map[string]GuestMemorySnapshot),
		nodeRRDMemCache:      make(map[string]rrdMemCacheEntry),
		removedDockerHosts:   make(map[string]time.Time),
		dockerCommands:       make(map[string]*dockerHostCommand),
		dockerCommandIndex:   make(map[string]string),
		guestMetadataCache:   make(map[string]guestMetadataCacheEntry),
		instanceInfoCache:    make(map[string]*instanceInfo),
		pollStatusMap:        make(map[string]*pollStatus),
		dlqInsightMap:        make(map[string]*dlqInsight),
	}

	m.breakerBaseRetry = 5 * time.Second
	m.breakerMaxDelay = 5 * time.Minute
	m.breakerHalfOpenWindow = 30 * time.Second

	if cfg.AdaptivePollingEnabled && cfg.AdaptivePollingMaxInterval > 0 && cfg.AdaptivePollingMaxInterval <= 15*time.Second {
		m.breakerBaseRetry = 2 * time.Second
		m.breakerMaxDelay = 10 * time.Second
		m.breakerHalfOpenWindow = 2 * time.Second
	}

	m.executor = newRealExecutor(m)
	m.buildInstanceInfoCache(cfg)

	if m.pollMetrics != nil {
		m.pollMetrics.ResetQueueDepth(0)
	}

	// Load saved configurations
	if alertConfig, err := m.configPersist.LoadAlertConfig(); err == nil {
		m.alertManager.UpdateConfig(*alertConfig)
		// Apply schedule settings to notification manager
		if alertConfig.Schedule.Cooldown > 0 {
			m.notificationMgr.SetCooldown(alertConfig.Schedule.Cooldown)
		}
		if alertConfig.Schedule.GroupingWindow > 0 {
			m.notificationMgr.SetGroupingWindow(alertConfig.Schedule.GroupingWindow)
		} else if alertConfig.Schedule.Grouping.Window > 0 {
			m.notificationMgr.SetGroupingWindow(alertConfig.Schedule.Grouping.Window)
		}
		m.notificationMgr.SetGroupingOptions(
			alertConfig.Schedule.Grouping.ByNode,
			alertConfig.Schedule.Grouping.ByGuest,
		)
	} else {
		log.Warn().Err(err).Msg("Failed to load alert configuration")
	}

	if emailConfig, err := m.configPersist.LoadEmailConfig(); err == nil {
		m.notificationMgr.SetEmailConfig(*emailConfig)
	} else {
		log.Warn().Err(err).Msg("Failed to load email configuration")
	}

	if appriseConfig, err := m.configPersist.LoadAppriseConfig(); err == nil {
		m.notificationMgr.SetAppriseConfig(*appriseConfig)
	} else {
		log.Warn().Err(err).Msg("Failed to load Apprise configuration")
	}

	// Migrate webhooks if needed (from unencrypted to encrypted)
	if err := m.configPersist.MigrateWebhooksIfNeeded(); err != nil {
		log.Warn().Err(err).Msg("Failed to migrate webhooks")
	}

	if webhooks, err := m.configPersist.LoadWebhooks(); err == nil {
		for _, webhook := range webhooks {
			m.notificationMgr.AddWebhook(webhook)
		}
	} else {
		log.Warn().Err(err).Msg("Failed to load webhook configuration")
	}

	// Check if mock mode is enabled before initializing clients
	mockEnabled := mock.IsMockEnabled()

	if mockEnabled {
		log.Info().Msg("Mock mode enabled - skipping PVE/PBS client initialization")
	} else {
		// Initialize PVE clients
		log.Info().Int("count", len(cfg.PVEInstances)).Msg("Initializing PVE clients")
		for _, pve := range cfg.PVEInstances {
			log.Info().
				Str("name", pve.Name).
				Str("host", pve.Host).
				Str("user", pve.User).
				Bool("hasToken", pve.TokenName != "").
				Msg("Configuring PVE instance")

				// Check if this is a cluster
			if pve.IsCluster && len(pve.ClusterEndpoints) > 0 {
				// For clusters, check if endpoints have IPs/resolvable hosts
				// If not, use the main host for all connections (Proxmox will route cluster API calls)
				hasValidEndpoints := false
				endpoints := make([]string, 0, len(pve.ClusterEndpoints))

				for _, ep := range pve.ClusterEndpoints {
					effectiveURL := clusterEndpointEffectiveURL(ep)
					if effectiveURL == "" {
						log.Warn().
							Str("node", ep.NodeName).
							Msg("Skipping cluster endpoint with no host/IP")
						continue
					}

					if parsed, err := url.Parse(effectiveURL); err == nil {
						hostname := parsed.Hostname()
						if hostname != "" && (strings.Contains(hostname, ".") || net.ParseIP(hostname) != nil) {
							hasValidEndpoints = true
						}
					} else {
						hostname := normalizeEndpointHost(effectiveURL)
						if hostname != "" && (strings.Contains(hostname, ".") || net.ParseIP(hostname) != nil) {
							hasValidEndpoints = true
						}
					}

					endpoints = append(endpoints, effectiveURL)
				}

				// If endpoints are just node names (not FQDNs or IPs), use main host only
				// This is common when cluster nodes are discovered but not directly reachable
				if !hasValidEndpoints || len(endpoints) == 0 {
					log.Info().
						Str("instance", pve.Name).
						Str("mainHost", pve.Host).
						Msg("Cluster endpoints are not resolvable, using main host for all cluster operations")
					fallback := ensureClusterEndpointURL(pve.Host)
					if fallback == "" {
						fallback = ensureClusterEndpointURL(pve.Host)
					}
					endpoints = []string{fallback}
				}

				log.Info().
					Str("cluster", pve.ClusterName).
					Strs("endpoints", endpoints).
					Msg("Creating cluster-aware client")

				clientConfig := config.CreateProxmoxConfig(&pve)
				clientConfig.Timeout = cfg.ConnectionTimeout
				clusterClient := proxmox.NewClusterClient(
					pve.Name,
					clientConfig,
					endpoints,
				)
				m.pveClients[pve.Name] = clusterClient
				log.Info().
					Str("instance", pve.Name).
					Str("cluster", pve.ClusterName).
					Int("endpoints", len(endpoints)).
					Msg("Cluster client created successfully")
				// Set initial connection health to true for cluster
				m.state.SetConnectionHealth(pve.Name, true)
			} else {
				// Create regular client
				clientConfig := config.CreateProxmoxConfig(&pve)
				clientConfig.Timeout = cfg.ConnectionTimeout
				client, err := proxmox.NewClient(clientConfig)
				if err != nil {
					monErr := errors.WrapConnectionError("create_pve_client", pve.Name, err)
					log.Error().
						Err(monErr).
						Str("instance", pve.Name).
						Str("host", pve.Host).
						Str("user", pve.User).
						Bool("hasPassword", pve.Password != "").
						Bool("hasToken", pve.TokenValue != "").
						Msg("Failed to create PVE client - node will show as disconnected")
					// Set initial connection health to false for this node
					m.state.SetConnectionHealth(pve.Name, false)
					continue
				}
				m.pveClients[pve.Name] = client
				log.Info().Str("instance", pve.Name).Msg("PVE client created successfully")
				// Set initial connection health to true
				m.state.SetConnectionHealth(pve.Name, true)
			}
		}

		// Initialize PBS clients
		log.Info().Int("count", len(cfg.PBSInstances)).Msg("Initializing PBS clients")
		for _, pbsInst := range cfg.PBSInstances {
			log.Info().
				Str("name", pbsInst.Name).
				Str("host", pbsInst.Host).
				Str("user", pbsInst.User).
				Bool("hasToken", pbsInst.TokenName != "").
				Msg("Configuring PBS instance")

			clientConfig := config.CreatePBSConfig(&pbsInst)
			clientConfig.Timeout = 60 * time.Second // Very generous timeout for slow PBS servers
			client, err := pbs.NewClient(clientConfig)
			if err != nil {
				monErr := errors.WrapConnectionError("create_pbs_client", pbsInst.Name, err)
				log.Error().
					Err(monErr).
					Str("instance", pbsInst.Name).
					Str("host", pbsInst.Host).
					Str("user", pbsInst.User).
					Bool("hasPassword", pbsInst.Password != "").
					Bool("hasToken", pbsInst.TokenValue != "").
					Msg("Failed to create PBS client - node will show as disconnected")
				// Set initial connection health to false for this node
				m.state.SetConnectionHealth("pbs-"+pbsInst.Name, false)
				continue
			}
			m.pbsClients[pbsInst.Name] = client
			log.Info().Str("instance", pbsInst.Name).Msg("PBS client created successfully")
			// Set initial connection health to true
			m.state.SetConnectionHealth("pbs-"+pbsInst.Name, true)
		}

		// Initialize PMG clients
		log.Info().Int("count", len(cfg.PMGInstances)).Msg("Initializing PMG clients")
		for _, pmgInst := range cfg.PMGInstances {
			log.Info().
				Str("name", pmgInst.Name).
				Str("host", pmgInst.Host).
				Str("user", pmgInst.User).
				Bool("hasToken", pmgInst.TokenName != "").
				Msg("Configuring PMG instance")

			clientConfig := config.CreatePMGConfig(&pmgInst)
			if clientConfig.Timeout <= 0 {
				clientConfig.Timeout = 45 * time.Second
			}

			client, err := pmg.NewClient(clientConfig)
			if err != nil {
				monErr := errors.WrapConnectionError("create_pmg_client", pmgInst.Name, err)
				log.Error().
					Err(monErr).
					Str("instance", pmgInst.Name).
					Str("host", pmgInst.Host).
					Str("user", pmgInst.User).
					Bool("hasPassword", pmgInst.Password != "").
					Bool("hasToken", pmgInst.TokenValue != "").
					Msg("Failed to create PMG client - gateway will show as disconnected")
				m.state.SetConnectionHealth("pmg-"+pmgInst.Name, false)
				continue
			}

			m.pmgClients[pmgInst.Name] = client
			log.Info().Str("instance", pmgInst.Name).Msg("PMG client created successfully")
			m.state.SetConnectionHealth("pmg-"+pmgInst.Name, true)
		}
	} // End of else block for mock mode check

	// Initialize state stats
	m.state.Stats = models.Stats{
		StartTime: m.startTime,
		Version:   "2.0.0-go",
	}

	return m, nil
}

// SetExecutor allows tests to override the poll executor; passing nil restores the default executor.
func (m *Monitor) SetExecutor(exec PollExecutor) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if exec == nil {
		m.executor = newRealExecutor(m)
		return
	}

	m.executor = exec
}

func (m *Monitor) buildInstanceInfoCache(cfg *config.Config) {
	if m == nil || cfg == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.instanceInfoCache == nil {
		m.instanceInfoCache = make(map[string]*instanceInfo)
	}

	add := func(instType InstanceType, name string, displayName string, connection string, metadata map[string]string) {
		key := schedulerKey(instType, name)
		m.instanceInfoCache[key] = &instanceInfo{
			Key:         key,
			Type:        instType,
			DisplayName: displayName,
			Connection:  connection,
			Metadata:    metadata,
		}
	}

	// PVE instances
	for _, inst := range cfg.PVEInstances {
		name := strings.TrimSpace(inst.Name)
		if name == "" {
			name = strings.TrimSpace(inst.Host)
		}
		if name == "" {
			name = "pve-instance"
		}
		display := name
		if display == "" {
			display = strings.TrimSpace(inst.Host)
		}
		connection := strings.TrimSpace(inst.Host)
		add(InstanceTypePVE, name, display, connection, nil)
	}

	// PBS instances
	for _, inst := range cfg.PBSInstances {
		name := strings.TrimSpace(inst.Name)
		if name == "" {
			name = strings.TrimSpace(inst.Host)
		}
		if name == "" {
			name = "pbs-instance"
		}
		display := name
		if display == "" {
			display = strings.TrimSpace(inst.Host)
		}
		connection := strings.TrimSpace(inst.Host)
		add(InstanceTypePBS, name, display, connection, nil)
	}

	// PMG instances
	for _, inst := range cfg.PMGInstances {
		name := strings.TrimSpace(inst.Name)
		if name == "" {
			name = strings.TrimSpace(inst.Host)
		}
		if name == "" {
			name = "pmg-instance"
		}
		display := name
		if display == "" {
			display = strings.TrimSpace(inst.Host)
		}
		connection := strings.TrimSpace(inst.Host)
		add(InstanceTypePMG, name, display, connection, nil)
	}
}

func (m *Monitor) getExecutor() PollExecutor {
	m.mu.RLock()
	exec := m.executor
	m.mu.RUnlock()
	return exec
}

// Start begins the monitoring loop
func (m *Monitor) Start(ctx context.Context, wsHub *websocket.Hub) {
	log.Info().
		Dur("pollingInterval", 10*time.Second).
		Msg("Starting monitoring loop")

	m.mu.Lock()
	m.runtimeCtx = ctx
	m.wsHub = wsHub
	m.mu.Unlock()

	// Initialize and start discovery service if enabled
	if mock.IsMockEnabled() {
		log.Info().Msg("Mock mode enabled - skipping discovery service")
		m.discoveryService = nil
	} else if m.config.DiscoveryEnabled {
		discoverySubnet := m.config.DiscoverySubnet
		if discoverySubnet == "" {
			discoverySubnet = "auto"
		}
		cfgProvider := func() config.DiscoveryConfig {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.config == nil {
				return config.DefaultDiscoveryConfig()
			}
			return config.CloneDiscoveryConfig(m.config.Discovery)
		}
		m.discoveryService = discovery.NewService(wsHub, 5*time.Minute, discoverySubnet, cfgProvider)
		if m.discoveryService != nil {
			m.discoveryService.Start(ctx)
			log.Info().Msg("Discovery service initialized and started")
		} else {
			log.Error().Msg("Failed to initialize discovery service")
		}
	} else {
		log.Info().Msg("Discovery service disabled by configuration")
		m.discoveryService = nil
	}

	// Set up alert callbacks
	m.alertManager.SetAlertCallback(func(alert *alerts.Alert) {
		wsHub.BroadcastAlert(alert)
		// Send notifications
		log.Debug().
			Str("alertID", alert.ID).
			Str("level", string(alert.Level)).
			Msg("Alert raised, sending to notification manager")
		go m.notificationMgr.SendAlert(alert)
	})
	m.alertManager.SetResolvedCallback(func(alertID string) {
		wsHub.BroadcastAlertResolved(alertID)
		m.notificationMgr.CancelAlert(alertID)
		// Don't broadcast full state here - it causes a cascade with many guests
		// The frontend will get the updated alerts through the regular broadcast ticker
		// state := m.GetState()
		// wsHub.BroadcastState(state)
	})
	m.alertManager.SetEscalateCallback(func(alert *alerts.Alert, level int) {
		log.Info().
			Str("alertID", alert.ID).
			Int("level", level).
			Msg("Alert escalated - sending notifications")

		// Get escalation config
		config := m.alertManager.GetConfig()
		if level <= 0 || level > len(config.Schedule.Escalation.Levels) {
			return
		}

		escalationLevel := config.Schedule.Escalation.Levels[level-1]

		// Send notifications based on escalation level
		switch escalationLevel.Notify {
		case "email":
			// Only send email
			if emailConfig := m.notificationMgr.GetEmailConfig(); emailConfig.Enabled {
				m.notificationMgr.SendAlert(alert)
			}
		case "webhook":
			// Only send webhooks
			for _, webhook := range m.notificationMgr.GetWebhooks() {
				if webhook.Enabled {
					m.notificationMgr.SendAlert(alert)
					break
				}
			}
		case "all":
			// Send all notifications
			m.notificationMgr.SendAlert(alert)
		}

		// Update WebSocket with escalation
		wsHub.BroadcastAlert(alert)
	})

	// Create separate tickers for polling and broadcasting
	// Hardcoded to 10 seconds since Proxmox updates cluster/resources every 10 seconds
	const pollingInterval = 10 * time.Second

	workerCount := len(m.pveClients) + len(m.pbsClients) + len(m.pmgClients)
	m.startTaskWorkers(ctx, workerCount)

	pollTicker := time.NewTicker(pollingInterval)
	defer pollTicker.Stop()

	broadcastTicker := time.NewTicker(pollingInterval)
	defer broadcastTicker.Stop()

	// Start connection retry mechanism for failed clients
	// This handles cases where network/Proxmox isn't ready on initial startup
	if !mock.IsMockEnabled() {
		go m.retryFailedConnections(ctx)
	}

	// Do an immediate poll on start (only if not in mock mode)
	if mock.IsMockEnabled() {
		log.Info().Msg("Mock mode enabled - skipping real node polling")
		go m.checkMockAlerts()
	} else {
		go m.poll(ctx, wsHub)
	}

	for {
		select {
		case <-pollTicker.C:
			now := time.Now()
			m.evaluateDockerAgents(now)
			m.cleanupRemovedDockerHosts(now)
			if mock.IsMockEnabled() {
				// In mock mode, keep synthetic alerts fresh
				go m.checkMockAlerts()
			} else {
				// Poll real infrastructure
				go m.poll(ctx, wsHub)
			}

		case <-broadcastTicker.C:
			// Broadcast current state regardless of polling status
			// Use GetState() instead of m.state.GetSnapshot() to respect mock mode
			state := m.GetState()
			log.Info().
				Int("nodes", len(state.Nodes)).
				Int("vms", len(state.VMs)).
				Int("containers", len(state.Containers)).
				Int("pbs", len(state.PBSInstances)).
				Int("pbsBackups", len(state.Backups.PBS)).
				Int("physicalDisks", len(state.PhysicalDisks)).
				Msg("Broadcasting state update (ticker)")
			// Convert to frontend format before broadcasting (converts time.Time to int64, etc.)
			wsHub.BroadcastState(state.ToFrontend())

		case <-ctx.Done():
			log.Info().Msg("Monitoring loop stopped")
			return
		}
	}
}

// retryFailedConnections attempts to recreate clients that failed during initialization
// This handles cases where Proxmox/network isn't ready when Pulse starts
func (m *Monitor) retryFailedConnections(ctx context.Context) {
	// Retry schedule: 5s, 10s, 20s, 40s, 60s, then every 60s for up to 5 minutes total
	retryDelays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
	}

	maxRetryDuration := 5 * time.Minute
	startTime := time.Now()
	retryIndex := 0

	for {
		// Stop retrying after max duration or if context is cancelled
		select {
		case <-ctx.Done():
			return
		default:
		}

		if time.Since(startTime) > maxRetryDuration {
			log.Info().Msg("Connection retry period expired")
			return
		}

		// Calculate next retry delay
		var delay time.Duration
		if retryIndex < len(retryDelays) {
			delay = retryDelays[retryIndex]
			retryIndex++
		} else {
			delay = 60 * time.Second // Continue retrying every 60s
		}

		// Wait before retry
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}

		// Check for missing clients and try to recreate them
		m.mu.Lock()
		missingPVE := []config.PVEInstance{}
		missingPBS := []config.PBSInstance{}

		// Find PVE instances without clients
		for _, pve := range m.config.PVEInstances {
			if _, exists := m.pveClients[pve.Name]; !exists {
				missingPVE = append(missingPVE, pve)
			}
		}

		// Find PBS instances without clients
		for _, pbs := range m.config.PBSInstances {
			if _, exists := m.pbsClients[pbs.Name]; !exists {
				missingPBS = append(missingPBS, pbs)
			}
		}
		m.mu.Unlock()

		// If no missing clients, we're done
		if len(missingPVE) == 0 && len(missingPBS) == 0 {
			log.Info().Msg("All client connections established successfully")
			return
		}

		log.Info().
			Int("missingPVE", len(missingPVE)).
			Int("missingPBS", len(missingPBS)).
			Dur("nextRetry", delay).
			Msg("Attempting to reconnect failed clients")

		// Try to recreate PVE clients
		for _, pve := range missingPVE {
			if pve.IsCluster && len(pve.ClusterEndpoints) > 0 {
				// Create cluster client
				hasValidEndpoints := false
				endpoints := make([]string, 0, len(pve.ClusterEndpoints))

				for _, ep := range pve.ClusterEndpoints {
					host := ep.IP
					if host == "" {
						host = ep.Host
					}
					if host == "" {
						continue
					}
					if strings.Contains(host, ".") || net.ParseIP(host) != nil {
						hasValidEndpoints = true
					}
					if !strings.HasPrefix(host, "http") {
						host = fmt.Sprintf("https://%s:8006", host)
					}
					endpoints = append(endpoints, host)
				}

				if !hasValidEndpoints || len(endpoints) == 0 {
					endpoints = []string{pve.Host}
					if !strings.HasPrefix(endpoints[0], "http") {
						endpoints[0] = fmt.Sprintf("https://%s:8006", endpoints[0])
					}
				}

				clientConfig := config.CreateProxmoxConfig(&pve)
				clientConfig.Timeout = m.config.ConnectionTimeout
				clusterClient := proxmox.NewClusterClient(pve.Name, clientConfig, endpoints)

				m.mu.Lock()
				m.pveClients[pve.Name] = clusterClient
				m.state.SetConnectionHealth(pve.Name, true)
				m.mu.Unlock()

				log.Info().
					Str("instance", pve.Name).
					Str("cluster", pve.ClusterName).
					Msg("Successfully reconnected cluster client")
			} else {
				// Create regular client
				clientConfig := config.CreateProxmoxConfig(&pve)
				clientConfig.Timeout = m.config.ConnectionTimeout
				client, err := proxmox.NewClient(clientConfig)
				if err != nil {
					log.Warn().
						Err(err).
						Str("instance", pve.Name).
						Msg("Failed to reconnect PVE client, will retry")
					continue
				}

				m.mu.Lock()
				m.pveClients[pve.Name] = client
				m.state.SetConnectionHealth(pve.Name, true)
				m.mu.Unlock()

				log.Info().
					Str("instance", pve.Name).
					Msg("Successfully reconnected PVE client")
			}
		}

		// Try to recreate PBS clients
		for _, pbsInst := range missingPBS {
			clientConfig := config.CreatePBSConfig(&pbsInst)
			clientConfig.Timeout = 60 * time.Second
			client, err := pbs.NewClient(clientConfig)
			if err != nil {
				log.Warn().
					Err(err).
					Str("instance", pbsInst.Name).
					Msg("Failed to reconnect PBS client, will retry")
				continue
			}

			m.mu.Lock()
			m.pbsClients[pbsInst.Name] = client
			m.state.SetConnectionHealth("pbs-"+pbsInst.Name, true)
			m.mu.Unlock()

			log.Info().
				Str("instance", pbsInst.Name).
				Msg("Successfully reconnected PBS client")
		}
	}
}

// poll fetches data from all configured instances
func (m *Monitor) poll(ctx context.Context, wsHub *websocket.Hub) {
	// Limit concurrent polls to 2 to prevent resource exhaustion
	currentCount := atomic.AddInt32(&m.activePollCount, 1)
	if currentCount > 2 {
		atomic.AddInt32(&m.activePollCount, -1)
		if logging.IsLevelEnabled(zerolog.DebugLevel) {
			log.Debug().Int32("activePolls", currentCount-1).Msg("Too many concurrent polls, skipping")
		}
		return
	}
	defer atomic.AddInt32(&m.activePollCount, -1)

	if logging.IsLevelEnabled(zerolog.DebugLevel) {
		log.Debug().Msg("Starting polling cycle")
	}
	startTime := time.Now()
	now := startTime

	plannedTasks := m.buildScheduledTasks(now)
	for _, task := range plannedTasks {
		m.taskQueue.Upsert(task)
	}
	m.updateQueueDepthMetric()

	// Update performance metrics
	m.state.Performance.LastPollDuration = time.Since(startTime).Seconds()
	m.state.Stats.PollingCycles++
	m.state.Stats.Uptime = int64(time.Since(m.startTime).Seconds())
	m.state.Stats.WebSocketClients = wsHub.GetClientCount()

	// Sync alert state so broadcasts include the latest acknowledgement data
	m.syncAlertsToState()

	// Increment poll counter
	m.mu.Lock()
	m.pollCounter++
	m.mu.Unlock()

	if logging.IsLevelEnabled(zerolog.DebugLevel) {
		log.Debug().Dur("duration", time.Since(startTime)).Msg("Polling cycle completed")
	}

	// Broadcasting is now handled by the timer in Start()
}

// syncAlertsToState copies the latest alert manager data into the shared state snapshot.
// This keeps WebSocket broadcasts aligned with in-memory acknowledgement updates.
func (m *Monitor) syncAlertsToState() {
	if m.pruneStaleDockerAlerts() {
		if logging.IsLevelEnabled(zerolog.DebugLevel) {
			log.Debug().Msg("Pruned stale docker alerts during sync")
		}
	}

	activeAlerts := m.alertManager.GetActiveAlerts()
	modelAlerts := make([]models.Alert, 0, len(activeAlerts))
	for _, alert := range activeAlerts {
		modelAlerts = append(modelAlerts, models.Alert{
			ID:           alert.ID,
			Type:         alert.Type,
			Level:        string(alert.Level),
			ResourceID:   alert.ResourceID,
			ResourceName: alert.ResourceName,
			Node:         alert.Node,
			Instance:     alert.Instance,
			Message:      alert.Message,
			Value:        alert.Value,
			Threshold:    alert.Threshold,
			StartTime:    alert.StartTime,
			Acknowledged: alert.Acknowledged,
			AckTime:      alert.AckTime,
			AckUser:      alert.AckUser,
		})
		if alert.Acknowledged && logging.IsLevelEnabled(zerolog.DebugLevel) {
			log.Debug().Str("alertID", alert.ID).Interface("ackTime", alert.AckTime).Msg("Syncing acknowledged alert")
		}
	}
	m.state.UpdateActiveAlerts(modelAlerts)

	recentlyResolved := m.alertManager.GetRecentlyResolved()
	if len(recentlyResolved) > 0 {
		log.Info().Int("count", len(recentlyResolved)).Msg("Syncing recently resolved alerts")
	}
	m.state.UpdateRecentlyResolved(recentlyResolved)
}

// SyncAlertState is the exported wrapper used by APIs that mutate alerts outside the poll loop.
func (m *Monitor) SyncAlertState() {
	m.syncAlertsToState()
}

// pruneStaleDockerAlerts removes docker alerts that reference hosts no longer present in state.
func (m *Monitor) pruneStaleDockerAlerts() bool {
	if m.alertManager == nil {
		return false
	}

	hosts := m.state.GetDockerHosts()
	knownHosts := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		id := strings.TrimSpace(host.ID)
		if id != "" {
			knownHosts[id] = struct{}{}
		}
	}

	if len(knownHosts) == 0 {
		// Still allow stale entries to be cleared if no hosts remain.
	}

	active := m.alertManager.GetActiveAlerts()
	processed := make(map[string]struct{})
	cleared := false

	for _, alert := range active {
		var hostID string

		switch {
		case alert.Type == "docker-host-offline":
			hostID = strings.TrimPrefix(alert.ID, "docker-host-offline-")
		case strings.HasPrefix(alert.ResourceID, "docker:"):
			resource := strings.TrimPrefix(alert.ResourceID, "docker:")
			if idx := strings.Index(resource, "/"); idx >= 0 {
				hostID = resource[:idx]
			} else {
				hostID = resource
			}
		default:
			continue
		}

		hostID = strings.TrimSpace(hostID)
		if hostID == "" {
			continue
		}

		if _, known := knownHosts[hostID]; known {
			continue
		}
		if _, alreadyCleared := processed[hostID]; alreadyCleared {
			continue
		}

		host := models.DockerHost{
			ID:          hostID,
			DisplayName: alert.ResourceName,
			Hostname:    alert.Node,
		}
		if host.DisplayName == "" {
			host.DisplayName = hostID
		}
		if host.Hostname == "" {
			host.Hostname = hostID
		}

		m.alertManager.HandleDockerHostRemoved(host)
		processed[hostID] = struct{}{}
		cleared = true
	}

	return cleared
}

func (m *Monitor) startTaskWorkers(ctx context.Context, workers int) {
	if m.taskQueue == nil {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > 10 {
		workers = 10
	}
	for i := 0; i < workers; i++ {
		go m.taskWorker(ctx, i)
	}
}

func (m *Monitor) taskWorker(ctx context.Context, id int) {
	if logging.IsLevelEnabled(zerolog.DebugLevel) {
		log.Debug().Int("worker", id).Msg("Task worker started")
	}
	for {
		task, ok := m.taskQueue.WaitNext(ctx)
		if !ok {
			if logging.IsLevelEnabled(zerolog.DebugLevel) {
				log.Debug().Int("worker", id).Msg("Task worker stopping")
			}
			return
		}

		m.executeScheduledTask(ctx, task)

		m.rescheduleTask(task)
		m.updateQueueDepthMetric()
	}
}

func (m *Monitor) executeScheduledTask(ctx context.Context, task ScheduledTask) {
	if !m.allowExecution(task) {
		if logging.IsLevelEnabled(zerolog.DebugLevel) {
			log.Debug().
				Str("instance", task.InstanceName).
				Str("type", string(task.InstanceType)).
				Msg("Task blocked by circuit breaker")
		}
		return
	}

	if m.pollMetrics != nil {
		wait := time.Duration(0)
		if !task.NextRun.IsZero() {
			wait = time.Since(task.NextRun)
			if wait < 0 {
				wait = 0
			}
		}
		instanceType := string(task.InstanceType)
		if strings.TrimSpace(instanceType) == "" {
			instanceType = "unknown"
		}
		m.pollMetrics.RecordQueueWait(instanceType, wait)
	}

	executor := m.getExecutor()
	if executor == nil {
		log.Error().
			Str("instance", task.InstanceName).
			Str("type", string(task.InstanceType)).
			Msg("No poll executor configured; skipping task")
		return
	}

	pollTask := PollTask{
		InstanceName: task.InstanceName,
		InstanceType: string(task.InstanceType),
	}

	switch task.InstanceType {
	case InstanceTypePVE:
		client, ok := m.pveClients[task.InstanceName]
		if !ok || client == nil {
			log.Warn().Str("instance", task.InstanceName).Msg("PVE client missing for scheduled task")
			return
		}
		pollTask.PVEClient = client
	case InstanceTypePBS:
		client, ok := m.pbsClients[task.InstanceName]
		if !ok || client == nil {
			log.Warn().Str("instance", task.InstanceName).Msg("PBS client missing for scheduled task")
			return
		}
		pollTask.PBSClient = client
	case InstanceTypePMG:
		client, ok := m.pmgClients[task.InstanceName]
		if !ok || client == nil {
			log.Warn().Str("instance", task.InstanceName).Msg("PMG client missing for scheduled task")
			return
		}
		pollTask.PMGClient = client
	default:
		log.Debug().
			Str("instance", task.InstanceName).
			Str("type", string(task.InstanceType)).
			Msg("Skipping unsupported task type")
		return
	}

	executor.Execute(ctx, pollTask)
}

func (m *Monitor) rescheduleTask(task ScheduledTask) {
	if m.taskQueue == nil {
		return
	}

	key := schedulerKey(task.InstanceType, task.InstanceName)
	m.mu.Lock()
	outcome, hasOutcome := m.lastOutcome[key]
	failureCount := m.failureCounts[key]
	m.mu.Unlock()

	if hasOutcome && !outcome.success {
		if !outcome.transient || failureCount >= m.maxRetryAttempts {
			m.sendToDeadLetter(task, outcome.err)
			return
		}
		delay := m.backoffCfg.nextDelay(failureCount-1, m.randomFloat())
		if delay <= 0 {
			delay = 5 * time.Second
		}
		if m.config != nil && m.config.AdaptivePollingEnabled && m.config.AdaptivePollingMaxInterval > 0 && m.config.AdaptivePollingMaxInterval <= 15*time.Second {
			maxDelay := 4 * time.Second
			if delay > maxDelay {
				delay = maxDelay
			}
		}
		next := task
		next.Interval = delay
		next.NextRun = time.Now().Add(delay)
		m.taskQueue.Upsert(next)
		return
	}

	if m.scheduler == nil {
		nextInterval := task.Interval
		if nextInterval <= 0 && m.config != nil {
			nextInterval = m.config.AdaptivePollingBaseInterval
		}
		if nextInterval <= 0 {
			nextInterval = DefaultSchedulerConfig().BaseInterval
		}
		next := task
		next.NextRun = time.Now().Add(nextInterval)
		next.Interval = nextInterval
		m.taskQueue.Upsert(next)
		return
	}

	desc := InstanceDescriptor{
		Name:          task.InstanceName,
		Type:          task.InstanceType,
		LastInterval:  task.Interval,
		LastScheduled: task.NextRun,
	}
	if m.stalenessTracker != nil {
		if snap, ok := m.stalenessTracker.snapshot(task.InstanceType, task.InstanceName); ok {
			desc.LastSuccess = snap.LastSuccess
			desc.LastFailure = snap.LastError
			if snap.ChangeHash != "" {
				desc.Metadata = map[string]any{"changeHash": snap.ChangeHash}
			}
		}
	}

	tasks := m.scheduler.BuildPlan(time.Now(), []InstanceDescriptor{desc}, m.taskQueue.Size())
	if len(tasks) == 0 {
		next := task
		nextInterval := task.Interval
		if nextInterval <= 0 && m.config != nil {
			nextInterval = m.config.AdaptivePollingBaseInterval
		}
		if nextInterval <= 0 {
			nextInterval = DefaultSchedulerConfig().BaseInterval
		}
		next.Interval = nextInterval
		next.NextRun = time.Now().Add(nextInterval)
		m.taskQueue.Upsert(next)
		return
	}
	for _, next := range tasks {
		m.taskQueue.Upsert(next)
	}
}

func (m *Monitor) sendToDeadLetter(task ScheduledTask, err error) {
	if m.deadLetterQueue == nil {
		log.Error().
			Str("instance", task.InstanceName).
			Str("type", string(task.InstanceType)).
			Err(err).
			Msg("Dead-letter queue unavailable; dropping task")
		return
	}

	log.Error().
		Str("instance", task.InstanceName).
		Str("type", string(task.InstanceType)).
		Err(err).
		Msg("Routing task to dead-letter queue after repeated failures")

	next := task
	next.Interval = 30 * time.Minute
	next.NextRun = time.Now().Add(next.Interval)
	m.deadLetterQueue.Upsert(next)
	m.updateDeadLetterMetrics()

	key := schedulerKey(task.InstanceType, task.InstanceName)
	now := time.Now()

	m.mu.Lock()
	if m.dlqInsightMap == nil {
		m.dlqInsightMap = make(map[string]*dlqInsight)
	}
	info, ok := m.dlqInsightMap[key]
	if !ok {
		info = &dlqInsight{}
		m.dlqInsightMap[key] = info
	}
	if info.FirstAttempt.IsZero() {
		info.FirstAttempt = now
	}
	info.LastAttempt = now
	info.RetryCount++
	info.NextRetry = next.NextRun
	if err != nil {
		info.Reason = classifyDLQReason(err)
	}
	m.mu.Unlock()
}

func classifyDLQReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.IsRetryableError(err) {
		return "max_retry_attempts"
	}
	return "permanent_failure"
}

func (m *Monitor) updateDeadLetterMetrics() {
	if m.pollMetrics == nil || m.deadLetterQueue == nil {
		return
	}

	size := m.deadLetterQueue.Size()
	if size <= 0 {
		m.pollMetrics.UpdateDeadLetterCounts(nil)
		return
	}

	tasks := m.deadLetterQueue.PeekAll(size)
	m.pollMetrics.UpdateDeadLetterCounts(tasks)
}

func (m *Monitor) updateBreakerMetric(instanceType InstanceType, instance string, breaker *circuitBreaker) {
	if m.pollMetrics == nil || breaker == nil {
		return
	}

	state, failures, retryAt, _, _ := breaker.stateDetails()
	m.pollMetrics.SetBreakerState(string(instanceType), instance, state, failures, retryAt)
}

func (m *Monitor) randomFloat() float64 {
	if m.rng == nil {
		m.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return m.rng.Float64()
}

func (m *Monitor) updateQueueDepthMetric() {
	if m.pollMetrics == nil || m.taskQueue == nil {
		return
	}
	snapshot := m.taskQueue.Snapshot()
	m.pollMetrics.SetQueueDepth(snapshot.Depth)
	m.pollMetrics.UpdateQueueSnapshot(snapshot)
}

func (m *Monitor) allowExecution(task ScheduledTask) bool {
	if m.circuitBreakers == nil {
		return true
	}
	key := schedulerKey(task.InstanceType, task.InstanceName)
	breaker := m.ensureBreaker(key)
	allowed := breaker.allow(time.Now())
	m.updateBreakerMetric(task.InstanceType, task.InstanceName, breaker)
	return allowed
}

func (m *Monitor) ensureBreaker(key string) *circuitBreaker {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.circuitBreakers == nil {
		m.circuitBreakers = make(map[string]*circuitBreaker)
	}
	if breaker, ok := m.circuitBreakers[key]; ok {
		return breaker
	}
	baseRetry := m.breakerBaseRetry
	if baseRetry <= 0 {
		baseRetry = 5 * time.Second
	}
	maxDelay := m.breakerMaxDelay
	if maxDelay <= 0 {
		maxDelay = 5 * time.Minute
	}
	halfOpen := m.breakerHalfOpenWindow
	if halfOpen <= 0 {
		halfOpen = 30 * time.Second
	}
	breaker := newCircuitBreaker(3, baseRetry, maxDelay, halfOpen)
	m.circuitBreakers[key] = breaker
	return breaker
}

func (m *Monitor) recordTaskResult(instanceType InstanceType, instance string, pollErr error) {
	if m == nil {
		return
	}

	key := schedulerKey(instanceType, instance)
	now := time.Now()

	breaker := m.ensureBreaker(key)

	m.mu.Lock()
	status, ok := m.pollStatusMap[key]
	if !ok {
		status = &pollStatus{}
		m.pollStatusMap[key] = status
	}

	if pollErr == nil {
		if m.failureCounts != nil {
			m.failureCounts[key] = 0
		}
		if m.lastOutcome != nil {
			m.lastOutcome[key] = taskOutcome{
				success:    true,
				transient:  true,
				err:        nil,
				recordedAt: now,
			}
		}
		status.LastSuccess = now
		status.ConsecutiveFailures = 0
		status.FirstFailureAt = time.Time{}
		m.mu.Unlock()
		if breaker != nil {
			breaker.recordSuccess()
			m.updateBreakerMetric(instanceType, instance, breaker)
		}
		return
	}

	transient := isTransientError(pollErr)
	category := "permanent"
	if transient {
		category = "transient"
	}
	if m.failureCounts != nil {
		m.failureCounts[key] = m.failureCounts[key] + 1
	}
	if m.lastOutcome != nil {
		m.lastOutcome[key] = taskOutcome{
			success:    false,
			transient:  transient,
			err:        pollErr,
			recordedAt: now,
		}
	}
	status.LastErrorAt = now
	status.LastErrorMessage = pollErr.Error()
	status.LastErrorCategory = category
	status.ConsecutiveFailures++
	if status.ConsecutiveFailures == 1 {
		status.FirstFailureAt = now
	}
	m.mu.Unlock()
	if breaker != nil {
		breaker.recordFailure(now)
		m.updateBreakerMetric(instanceType, instance, breaker)
	}
}

// SchedulerHealthResponse contains complete scheduler health data for API exposure.
type SchedulerHealthResponse struct {
	UpdatedAt  time.Time           `json:"updatedAt"`
	Enabled    bool                `json:"enabled"`
	Queue      QueueSnapshot       `json:"queue"`
	DeadLetter DeadLetterSnapshot  `json:"deadLetter"`
	Breakers   []BreakerSnapshot   `json:"breakers,omitempty"`
	Staleness  []StalenessSnapshot `json:"staleness,omitempty"`
	Instances  []InstanceHealth    `json:"instances"`
}

// DeadLetterSnapshot contains dead-letter queue data.
type DeadLetterSnapshot struct {
	Count int              `json:"count"`
	Tasks []DeadLetterTask `json:"tasks"`
}

// SchedulerHealth returns a complete snapshot of scheduler health for API exposure.
func (m *Monitor) SchedulerHealth() SchedulerHealthResponse {
	response := SchedulerHealthResponse{
		UpdatedAt: time.Now(),
		Enabled:   m.config != nil && m.config.AdaptivePollingEnabled,
	}

	// Queue snapshot
	if m.taskQueue != nil {
		response.Queue = m.taskQueue.Snapshot()
		if m.pollMetrics != nil {
			m.pollMetrics.UpdateQueueSnapshot(response.Queue)
		}
	}

	// Dead-letter queue snapshot
	if m.deadLetterQueue != nil {
		deadLetterTasks := m.deadLetterQueue.PeekAll(25) // limit to top 25
		m.mu.RLock()
		for i := range deadLetterTasks {
			key := schedulerKey(InstanceType(deadLetterTasks[i].Type), deadLetterTasks[i].Instance)
			if outcome, ok := m.lastOutcome[key]; ok && outcome.err != nil {
				deadLetterTasks[i].LastError = outcome.err.Error()
			}
			if count, ok := m.failureCounts[key]; ok {
				deadLetterTasks[i].Failures = count
			}
		}
		m.mu.RUnlock()
		response.DeadLetter = DeadLetterSnapshot{
			Count: m.deadLetterQueue.Size(),
			Tasks: deadLetterTasks,
		}
		m.updateDeadLetterMetrics()
	}

	// Circuit breaker snapshots
	m.mu.RLock()
	breakerSnapshots := make([]BreakerSnapshot, 0, len(m.circuitBreakers))
	for key, breaker := range m.circuitBreakers {
		state, failures, retryAt := breaker.State()
		// Only include breakers that are not in default closed state with 0 failures
		if state != "closed" || failures > 0 {
			// Parse instance type and name from key
			parts := strings.SplitN(key, "::", 2)
			instanceType, instanceName := "unknown", key
			if len(parts) == 2 {
				instanceType, instanceName = parts[0], parts[1]
			}
			breakerSnapshots = append(breakerSnapshots, BreakerSnapshot{
				Instance: instanceName,
				Type:     instanceType,
				State:    state,
				Failures: failures,
				RetryAt:  retryAt,
			})
		}
	}
	m.mu.RUnlock()
	response.Breakers = breakerSnapshots

	// Staleness snapshots
	if m.stalenessTracker != nil {
		response.Staleness = m.stalenessTracker.Snapshot()
	}

	instanceInfos := make(map[string]*instanceInfo)
	pollStatuses := make(map[string]pollStatus)
	dlqInsights := make(map[string]dlqInsight)
	breakerRefs := make(map[string]*circuitBreaker)

	m.mu.RLock()
	for k, v := range m.instanceInfoCache {
		if v == nil {
			continue
		}
		copyVal := *v
		instanceInfos[k] = &copyVal
	}
	for k, v := range m.pollStatusMap {
		if v == nil {
			continue
		}
		pollStatuses[k] = *v
	}
	for k, v := range m.dlqInsightMap {
		if v == nil {
			continue
		}
		dlqInsights[k] = *v
	}
	for k, v := range m.circuitBreakers {
		if v != nil {
			breakerRefs[k] = v
		}
	}
	m.mu.RUnlock()
	for key, breaker := range breakerRefs {
		instanceType := InstanceType("unknown")
		instanceName := key
		if parts := strings.SplitN(key, "::", 2); len(parts) == 2 {
			if parts[0] != "" {
				instanceType = InstanceType(parts[0])
			}
			if parts[1] != "" {
				instanceName = parts[1]
			}
		}
		m.updateBreakerMetric(instanceType, instanceName, breaker)
	}

	keySet := make(map[string]struct{})
	for k := range instanceInfos {
		if k != "" {
			keySet[k] = struct{}{}
		}
	}
	for k := range pollStatuses {
		if k != "" {
			keySet[k] = struct{}{}
		}
	}
	for k := range dlqInsights {
		if k != "" {
			keySet[k] = struct{}{}
		}
	}
	for k := range breakerRefs {
		if k != "" {
			keySet[k] = struct{}{}
		}
	}
	for _, task := range response.DeadLetter.Tasks {
		if task.Instance == "" {
			continue
		}
		keySet[schedulerKey(InstanceType(task.Type), task.Instance)] = struct{}{}
	}
	for _, snap := range response.Staleness {
		if snap.Instance == "" {
			continue
		}
		keySet[schedulerKey(InstanceType(snap.Type), snap.Instance)] = struct{}{}
	}

	if len(keySet) > 0 {
		keys := make([]string, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		instances := make([]InstanceHealth, 0, len(keys))
		for _, key := range keys {
			instType := "unknown"
			instName := key
			if parts := strings.SplitN(key, "::", 2); len(parts) == 2 {
				if parts[0] != "" {
					instType = parts[0]
				}
				if parts[1] != "" {
					instName = parts[1]
				}
			}
			instType = strings.TrimSpace(instType)
			instName = strings.TrimSpace(instName)

			info := instanceInfos[key]
			display := instName
			connection := ""
			if info != nil {
				if instType == "unknown" || instType == "" {
					if info.Type != "" {
						instType = string(info.Type)
					}
				}
				if strings.Contains(info.Key, "::") {
					if parts := strings.SplitN(info.Key, "::", 2); len(parts) == 2 {
						if instName == key {
							instName = parts[1]
						}
						if (instType == "" || instType == "unknown") && parts[0] != "" {
							instType = parts[0]
						}
					}
				}
				if info.DisplayName != "" {
					display = info.DisplayName
				}
				if info.Connection != "" {
					connection = info.Connection
				}
			}
			display = strings.TrimSpace(display)
			connection = strings.TrimSpace(connection)
			if display == "" {
				display = instName
			}
			if display == "" {
				display = connection
			}
			if instType == "" {
				instType = "unknown"
			}
			if instName == "" {
				instName = key
			}

			status, hasStatus := pollStatuses[key]
			instanceStatus := InstancePollStatus{}
			if hasStatus {
				instanceStatus.ConsecutiveFailures = status.ConsecutiveFailures
				instanceStatus.LastSuccess = timePtr(status.LastSuccess)
				if !status.FirstFailureAt.IsZero() {
					instanceStatus.FirstFailureAt = timePtr(status.FirstFailureAt)
				}
				if !status.LastErrorAt.IsZero() && status.LastErrorMessage != "" {
					instanceStatus.LastError = &ErrorDetail{
						At:       status.LastErrorAt,
						Message:  status.LastErrorMessage,
						Category: status.LastErrorCategory,
					}
				}
			}

			breakerInfo := InstanceBreaker{
				State:        "closed",
				FailureCount: 0,
			}
			if br, ok := breakerRefs[key]; ok && br != nil {
				state, failures, retryAt, since, lastTransition := br.stateDetails()
				if state != "" {
					breakerInfo.State = state
				}
				breakerInfo.FailureCount = failures
				breakerInfo.RetryAt = timePtr(retryAt)
				breakerInfo.Since = timePtr(since)
				breakerInfo.LastTransition = timePtr(lastTransition)
			}

			dlqInfo := InstanceDLQ{Present: false}
			if dlq, ok := dlqInsights[key]; ok {
				dlqInfo.Present = true
				dlqInfo.Reason = dlq.Reason
				dlqInfo.FirstAttempt = timePtr(dlq.FirstAttempt)
				dlqInfo.LastAttempt = timePtr(dlq.LastAttempt)
				dlqInfo.RetryCount = dlq.RetryCount
				dlqInfo.NextRetry = timePtr(dlq.NextRetry)
			}

			instances = append(instances, InstanceHealth{
				Key:         key,
				Type:        instType,
				DisplayName: display,
				Instance:    instName,
				Connection:  connection,
				PollStatus:  instanceStatus,
				Breaker:     breakerInfo,
				DeadLetter:  dlqInfo,
			})
		}

		response.Instances = instances
	} else {
		response.Instances = []InstanceHealth{}
	}

	return response
}

func isTransientError(err error) bool {
	if err == nil {
		return true
	}
	if errors.IsRetryableError(err) {
		return true
	}
	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

// pollPVEInstance polls a single PVE instance
func (m *Monitor) pollPVEInstance(ctx context.Context, instanceName string, client PVEClientInterface) {
	start := time.Now()
	debugEnabled := logging.IsLevelEnabled(zerolog.DebugLevel)
	var pollErr error
	if m.pollMetrics != nil {
		m.pollMetrics.IncInFlight("pve")
		defer m.pollMetrics.DecInFlight("pve")
		defer func() {
			m.pollMetrics.RecordResult(PollResult{
				InstanceName: instanceName,
				InstanceType: "pve",
				Success:      pollErr == nil,
				Error:        pollErr,
				StartTime:    start,
				EndTime:      time.Now(),
			})
		}()
	}
	if m.stalenessTracker != nil {
		defer func() {
			if pollErr == nil {
				m.stalenessTracker.UpdateSuccess(InstanceTypePVE, instanceName, nil)
			} else {
				m.stalenessTracker.UpdateError(InstanceTypePVE, instanceName)
			}
		}()
	}
	defer m.recordTaskResult(InstanceTypePVE, instanceName, pollErr)

	// Check if context is cancelled
	select {
	case <-ctx.Done():
		pollErr = ctx.Err()
		if debugEnabled {
			log.Debug().Str("instance", instanceName).Msg("Polling cancelled")
		}
		return
	default:
	}

	if debugEnabled {
		log.Debug().Str("instance", instanceName).Msg("Polling PVE instance")
	}

	// Get instance config
	var instanceCfg *config.PVEInstance
	for _, cfg := range m.config.PVEInstances {
		if cfg.Name == instanceName {
			instanceCfg = &cfg
			break
		}
	}
	if instanceCfg == nil {
		pollErr = fmt.Errorf("pve instance config not found for %s", instanceName)
		return
	}

	// Poll nodes
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("poll_nodes", instanceName, err)
		pollErr = monErr
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes")
		m.state.SetConnectionHealth(instanceName, false)

		// Track auth failure if it's an authentication error
		if errors.IsAuthError(err) {
			m.recordAuthFailure(instanceName, "pve")
		}
		return
	}

	// Reset auth failures on successful connection
	m.resetAuthFailures(instanceName, "pve")

	// Check if client is a ClusterClient to determine health status
	connectionHealthStr := "healthy"
	if clusterClient, ok := client.(*proxmox.ClusterClient); ok {
		// For cluster clients, check if all endpoints are healthy
		healthStatus := clusterClient.GetHealthStatus()
		healthyCount := 0
		totalCount := len(healthStatus)

		for _, isHealthy := range healthStatus {
			if isHealthy {
				healthyCount++
			}
		}

		if healthyCount == 0 {
			// All endpoints are down
			connectionHealthStr = "error"
			m.state.SetConnectionHealth(instanceName, false)
		} else if healthyCount < totalCount {
			// Some endpoints are down - degraded state
			connectionHealthStr = "degraded"
			m.state.SetConnectionHealth(instanceName, true) // Still functional but degraded
			log.Warn().
				Str("instance", instanceName).
				Int("healthy", healthyCount).
				Int("total", totalCount).
				Msg("Cluster is in degraded state - some nodes are unreachable")
		} else {
			// All endpoints are healthy
			connectionHealthStr = "healthy"
			m.state.SetConnectionHealth(instanceName, true)
		}
	} else {
		// Regular client - simple healthy/unhealthy
		m.state.SetConnectionHealth(instanceName, true)
	}

	// Capture previous memory metrics so we can preserve them if detailed status fails
	prevState := m.GetState()
	prevNodeMemory := make(map[string]models.Memory)
	prevInstanceNodes := make([]models.Node, 0)
	for _, existingNode := range prevState.Nodes {
		if existingNode.Instance != instanceName {
			continue
		}
		prevNodeMemory[existingNode.ID] = existingNode.Memory
		prevInstanceNodes = append(prevInstanceNodes, existingNode)
	}

	// Convert to models
	var modelNodes []models.Node
	for _, node := range nodes {
		nodeStart := time.Now()
		displayName := getNodeDisplayName(instanceCfg, node.Node)
		connectionHost := instanceCfg.Host
		if instanceCfg.IsCluster && len(instanceCfg.ClusterEndpoints) > 0 {
			for _, ep := range instanceCfg.ClusterEndpoints {
				if strings.EqualFold(ep.NodeName, node.Node) {
					if effective := clusterEndpointEffectiveURL(ep); effective != "" {
						connectionHost = effective
					}
					break
				}
			}
		}

		modelNode := models.Node{
			ID:          instanceName + "-" + node.Node,
			Name:        node.Node,
			DisplayName: displayName,
			Instance:    instanceName,
			Host:        connectionHost,
			Status:      node.Status,
			Type:        "node",
			CPU:         safeFloat(node.CPU), // Already in percentage
			Memory: models.Memory{
				Total: int64(node.MaxMem),
				Used:  int64(node.Mem),
				Free:  int64(node.MaxMem - node.Mem),
				Usage: safePercentage(float64(node.Mem), float64(node.MaxMem)),
			},
			Disk: models.Disk{
				Total: int64(node.MaxDisk),
				Used:  int64(node.Disk),
				Free:  int64(node.MaxDisk - node.Disk),
				Usage: safePercentage(float64(node.Disk), float64(node.MaxDisk)),
			},
			Uptime:           int64(node.Uptime),
			LoadAverage:      []float64{},
			LastSeen:         time.Now(),
			ConnectionHealth: connectionHealthStr, // Use the determined health status
			IsClusterMember:  instanceCfg.IsCluster,
			ClusterName:      instanceCfg.ClusterName,
		}

		nodeSnapshotRaw := NodeMemoryRaw{
			Total:               node.MaxMem,
			Used:                node.Mem,
			Free:                node.MaxMem - node.Mem,
			FallbackTotal:       node.MaxMem,
			FallbackUsed:        node.Mem,
			FallbackFree:        node.MaxMem - node.Mem,
			FallbackCalculated:  true,
			ProxmoxMemorySource: "nodes-endpoint",
		}
		nodeMemorySource := "nodes-endpoint"
		var nodeFallbackReason string

		// Debug logging for disk metrics - note that these values can fluctuate
		// due to thin provisioning and dynamic allocation
		if node.Disk > 0 && node.MaxDisk > 0 {
			log.Debug().
				Str("node", node.Node).
				Uint64("disk", node.Disk).
				Uint64("maxDisk", node.MaxDisk).
				Float64("diskUsage", safePercentage(float64(node.Disk), float64(node.MaxDisk))).
				Msg("Node disk metrics from /nodes endpoint")
		}

		// Track whether we successfully replaced memory metrics with detailed status data
		memoryUpdated := false

		// Get detailed node info if available (skip for offline nodes)
		if node.Status == "online" {
			nodeInfo, nodeErr := client.GetNodeStatus(ctx, node.Node)
			if nodeErr != nil {
				nodeFallbackReason = "node-status-unavailable"
				// If we can't get node status, log but continue with data from /nodes endpoint
				if node.Disk > 0 && node.MaxDisk > 0 {
					log.Warn().
						Str("instance", instanceName).
						Str("node", node.Node).
						Err(nodeErr).
						Uint64("usingDisk", node.Disk).
						Uint64("usingMaxDisk", node.MaxDisk).
						Msg("Could not get node status - using fallback metrics (memory will include cache/buffers)")
				} else {
					log.Warn().
						Str("instance", instanceName).
						Str("node", node.Node).
						Err(nodeErr).
						Uint64("disk", node.Disk).
						Uint64("maxDisk", node.MaxDisk).
						Msg("Could not get node status - no fallback metrics available (memory will include cache/buffers)")
				}
			} else if nodeInfo != nil {
				if nodeInfo.Memory != nil {
					nodeSnapshotRaw.Total = nodeInfo.Memory.Total
					nodeSnapshotRaw.Used = nodeInfo.Memory.Used
					nodeSnapshotRaw.Free = nodeInfo.Memory.Free
					nodeSnapshotRaw.Available = nodeInfo.Memory.Available
					nodeSnapshotRaw.Avail = nodeInfo.Memory.Avail
					nodeSnapshotRaw.Buffers = nodeInfo.Memory.Buffers
					nodeSnapshotRaw.Cached = nodeInfo.Memory.Cached
					nodeSnapshotRaw.Shared = nodeInfo.Memory.Shared
					nodeSnapshotRaw.EffectiveAvailable = nodeInfo.Memory.EffectiveAvailable()
					nodeSnapshotRaw.ProxmoxMemorySource = "node-status"
					nodeSnapshotRaw.FallbackCalculated = false
				}

				// Convert LoadAvg from interface{} to float64
				loadAvg := make([]float64, 0, len(nodeInfo.LoadAvg))
				for _, val := range nodeInfo.LoadAvg {
					switch v := val.(type) {
					case float64:
						loadAvg = append(loadAvg, v)
					case string:
						if f, err := strconv.ParseFloat(v, 64); err == nil {
							loadAvg = append(loadAvg, f)
						}
					}
				}
				modelNode.LoadAverage = loadAvg
				modelNode.KernelVersion = nodeInfo.KernelVersion
				modelNode.PVEVersion = nodeInfo.PVEVersion

				// Prefer rootfs data for more accurate disk metrics, but ensure we have valid fallback
				if nodeInfo.RootFS != nil && nodeInfo.RootFS.Total > 0 {
					modelNode.Disk = models.Disk{
						Total: int64(nodeInfo.RootFS.Total),
						Used:  int64(nodeInfo.RootFS.Used),
						Free:  int64(nodeInfo.RootFS.Free),
						Usage: safePercentage(float64(nodeInfo.RootFS.Used), float64(nodeInfo.RootFS.Total)),
					}
					log.Debug().
						Str("node", node.Node).
						Uint64("rootfsUsed", nodeInfo.RootFS.Used).
						Uint64("rootfsTotal", nodeInfo.RootFS.Total).
						Float64("rootfsUsage", modelNode.Disk.Usage).
						Msg("Using rootfs for disk metrics")
				} else if node.Disk > 0 && node.MaxDisk > 0 {
					// RootFS unavailable but we have valid disk data from /nodes endpoint
					// Keep the values we already set from the nodes list
					log.Debug().
						Str("node", node.Node).
						Bool("rootfsNil", nodeInfo.RootFS == nil).
						Uint64("fallbackDisk", node.Disk).
						Uint64("fallbackMaxDisk", node.MaxDisk).
						Msg("RootFS data unavailable - using /nodes endpoint disk metrics")
				} else {
					// Neither rootfs nor valid node disk data available
					log.Warn().
						Str("node", node.Node).
						Bool("rootfsNil", nodeInfo.RootFS == nil).
						Uint64("nodeDisk", node.Disk).
						Uint64("nodeMaxDisk", node.MaxDisk).
						Msg("No valid disk metrics available for node")
				}

				// Update memory metrics to use Available field for more accurate usage
				if nodeInfo.Memory != nil && nodeInfo.Memory.Total > 0 {
					var actualUsed uint64
					effectiveAvailable := nodeInfo.Memory.EffectiveAvailable()
					componentAvailable := nodeInfo.Memory.Free
					if nodeInfo.Memory.Buffers > 0 {
						if math.MaxUint64-componentAvailable < nodeInfo.Memory.Buffers {
							componentAvailable = math.MaxUint64
						} else {
							componentAvailable += nodeInfo.Memory.Buffers
						}
					}
					if nodeInfo.Memory.Cached > 0 {
						if math.MaxUint64-componentAvailable < nodeInfo.Memory.Cached {
							componentAvailable = math.MaxUint64
						} else {
							componentAvailable += nodeInfo.Memory.Cached
						}
					}
					if nodeInfo.Memory.Total > 0 && componentAvailable > nodeInfo.Memory.Total {
						componentAvailable = nodeInfo.Memory.Total
					}

					availableFromUsed := uint64(0)
					if nodeInfo.Memory.Total > 0 && nodeInfo.Memory.Used > 0 && nodeInfo.Memory.Total >= nodeInfo.Memory.Used {
						availableFromUsed = nodeInfo.Memory.Total - nodeInfo.Memory.Used
					}
					nodeSnapshotRaw.TotalMinusUsed = availableFromUsed

					missingCacheMetrics := nodeInfo.Memory.Available == 0 &&
						nodeInfo.Memory.Avail == 0 &&
						nodeInfo.Memory.Buffers == 0 &&
						nodeInfo.Memory.Cached == 0

					var rrdMetrics rrdMemCacheEntry
					haveRRDMetrics := false
					usedRRDAvailableFallback := false
					rrdMemUsedFallback := false

					if effectiveAvailable == 0 && missingCacheMetrics {
						if metrics, err := m.getNodeRRDMetrics(ctx, client, node.Node); err == nil {
							haveRRDMetrics = true
							rrdMetrics = metrics
							if metrics.available > 0 {
								effectiveAvailable = metrics.available
								usedRRDAvailableFallback = true
							}
							if metrics.used > 0 {
								rrdMemUsedFallback = true
							}
						} else if err != nil {
							log.Debug().
								Err(err).
								Str("instance", instanceName).
								Str("node", node.Node).
								Msg("RRD memavailable fallback unavailable")
						}
					}

					const totalMinusUsedGapTolerance uint64 = 16 * 1024 * 1024
					gapGreaterThanComponents := false
					if availableFromUsed > componentAvailable {
						gap := availableFromUsed - componentAvailable
						if componentAvailable == 0 || gap >= totalMinusUsedGapTolerance {
							gapGreaterThanComponents = true
						}
					}

					derivedFromTotalMinusUsed := !usedRRDAvailableFallback &&
						missingCacheMetrics &&
						availableFromUsed > 0 &&
						gapGreaterThanComponents &&
						effectiveAvailable == availableFromUsed

					switch {
					case effectiveAvailable > 0 && effectiveAvailable <= nodeInfo.Memory.Total:
						// Prefer available/avail fields or derived buffers+cache values when present.
						actualUsed = nodeInfo.Memory.Total - effectiveAvailable
						if actualUsed > nodeInfo.Memory.Total {
							actualUsed = nodeInfo.Memory.Total
						}

						logCtx := log.Debug().
							Str("node", node.Node).
							Uint64("total", nodeInfo.Memory.Total).
							Uint64("effectiveAvailable", effectiveAvailable).
							Uint64("actualUsed", actualUsed).
							Float64("usage", safePercentage(float64(actualUsed), float64(nodeInfo.Memory.Total)))
						if usedRRDAvailableFallback {
							if haveRRDMetrics && rrdMetrics.available > 0 {
								logCtx = logCtx.Uint64("rrdAvailable", rrdMetrics.available)
							}
							logCtx.Msg("Node memory: using RRD memavailable fallback (excludes reclaimable cache)")
							nodeMemorySource = "rrd-memavailable"
							nodeFallbackReason = "rrd-memavailable"
							nodeSnapshotRaw.FallbackCalculated = true
							nodeSnapshotRaw.ProxmoxMemorySource = "rrd-memavailable"
						} else if nodeInfo.Memory.Available > 0 {
							logCtx.Msg("Node memory: using available field (excludes reclaimable cache)")
							nodeMemorySource = "available-field"
						} else if nodeInfo.Memory.Avail > 0 {
							logCtx.Msg("Node memory: using avail field (excludes reclaimable cache)")
							nodeMemorySource = "avail-field"
						} else if derivedFromTotalMinusUsed {
							logCtx.
								Uint64("availableFromUsed", availableFromUsed).
								Uint64("reportedFree", nodeInfo.Memory.Free).
								Msg("Node memory: derived available from total-used gap (cache fields missing)")
							nodeMemorySource = "derived-total-minus-used"
							if nodeFallbackReason == "" {
								nodeFallbackReason = "node-status-total-minus-used"
							}
							nodeSnapshotRaw.FallbackCalculated = true
							nodeSnapshotRaw.ProxmoxMemorySource = "node-status-total-minus-used"
						} else {
							logCtx.
								Uint64("free", nodeInfo.Memory.Free).
								Uint64("buffers", nodeInfo.Memory.Buffers).
								Uint64("cached", nodeInfo.Memory.Cached).
								Msg("Node memory: derived available from free+buffers+cached (excludes reclaimable cache)")
							nodeMemorySource = "derived-free-buffers-cached"
						}
					default:
						switch {
						case rrdMemUsedFallback && haveRRDMetrics && rrdMetrics.used > 0:
							actualUsed = rrdMetrics.used
							if actualUsed > nodeInfo.Memory.Total {
								actualUsed = nodeInfo.Memory.Total
							}
							log.Debug().
								Str("node", node.Node).
								Uint64("total", nodeInfo.Memory.Total).
								Uint64("rrdUsed", rrdMetrics.used).
								Msg("Node memory: using RRD memused fallback (excludes reclaimable cache)")
							nodeMemorySource = "rrd-memused"
							if nodeFallbackReason == "" {
								nodeFallbackReason = "rrd-memused"
							}
							nodeSnapshotRaw.FallbackCalculated = true
							nodeSnapshotRaw.ProxmoxMemorySource = "rrd-memused"
						default:
							// Fallback to traditional used memory if no cache-aware data is exposed
							actualUsed = nodeInfo.Memory.Used
							if actualUsed > nodeInfo.Memory.Total {
								actualUsed = nodeInfo.Memory.Total
							}
							log.Debug().
								Str("node", node.Node).
								Uint64("total", nodeInfo.Memory.Total).
								Uint64("used", actualUsed).
								Msg("Node memory: no cache-aware metrics - using traditional calculation (includes cache)")
							nodeMemorySource = "node-status-used"
						}
					}

					nodeSnapshotRaw.EffectiveAvailable = effectiveAvailable
					if haveRRDMetrics {
						nodeSnapshotRaw.RRDAvailable = rrdMetrics.available
						nodeSnapshotRaw.RRDUsed = rrdMetrics.used
						nodeSnapshotRaw.RRDTotal = rrdMetrics.total
					}

					free := int64(nodeInfo.Memory.Total - actualUsed)
					if free < 0 {
						free = 0
					}

					modelNode.Memory = models.Memory{
						Total: int64(nodeInfo.Memory.Total),
						Used:  int64(actualUsed),
						Free:  free,
						Usage: safePercentage(float64(actualUsed), float64(nodeInfo.Memory.Total)),
					}
					memoryUpdated = true
				}

				if nodeInfo.CPUInfo != nil {
					// Use MaxCPU from node data for logical CPU count (includes hyperthreading)
					// If MaxCPU is not available or 0, fall back to physical cores
					logicalCores := node.MaxCPU
					if logicalCores == 0 {
						logicalCores = nodeInfo.CPUInfo.Cores
					}

					mhzStr := nodeInfo.CPUInfo.GetMHzString()
					log.Debug().
						Str("node", node.Node).
						Str("model", nodeInfo.CPUInfo.Model).
						Int("cores", nodeInfo.CPUInfo.Cores).
						Int("logicalCores", logicalCores).
						Int("sockets", nodeInfo.CPUInfo.Sockets).
						Str("mhz", mhzStr).
						Msg("Node CPU info from Proxmox")
					modelNode.CPUInfo = models.CPUInfo{
						Model:   nodeInfo.CPUInfo.Model,
						Cores:   logicalCores, // Use logical cores for display
						Sockets: nodeInfo.CPUInfo.Sockets,
						MHz:     mhzStr,
					}
				}
			}
		}

		// If we couldn't update memory metrics using detailed status, preserve previous accurate values if available
		if !memoryUpdated && node.Status == "online" {
			if prevMem, exists := prevNodeMemory[modelNode.ID]; exists && prevMem.Total > 0 {
				total := int64(node.MaxMem)
				if total == 0 {
					total = prevMem.Total
				}
				used := prevMem.Used
				if total > 0 && used > total {
					used = total
				}
				free := total - used
				if free < 0 {
					free = 0
				}

				preserved := prevMem
				preserved.Total = total
				preserved.Used = used
				preserved.Free = free
				preserved.Usage = safePercentage(float64(used), float64(total))

				modelNode.Memory = preserved
				log.Debug().
					Str("instance", instanceName).
					Str("node", node.Node).
					Msg("Preserving previous memory metrics - node status unavailable this cycle")

				if nodeFallbackReason == "" {
					nodeFallbackReason = "preserved-previous-snapshot"
				}
				nodeMemorySource = "previous-snapshot"
				if nodeSnapshotRaw.ProxmoxMemorySource == "node-status" && nodeSnapshotRaw.Total == 0 {
					nodeSnapshotRaw.ProxmoxMemorySource = "previous-snapshot"
				}
			}
		}

		m.recordNodeSnapshot(instanceName, node.Node, NodeMemorySnapshot{
			RetrievedAt:    time.Now(),
			MemorySource:   nodeMemorySource,
			FallbackReason: nodeFallbackReason,
			Memory:         modelNode.Memory,
			Raw:            nodeSnapshotRaw,
		})

		// Collect temperature data via SSH (non-blocking, best effort)
		// Only attempt for online nodes
		if node.Status == "online" && m.tempCollector != nil {
			tempCtx, tempCancel := context.WithTimeout(ctx, 30*time.Second) // Increased to accommodate SSH operations via proxy

			// Determine SSH hostname to use (most robust approach):
			// Prefer the resolved host for this node, with cluster overrides when available.
			sshHost := modelNode.Host

			if modelNode.IsClusterMember && instanceCfg.IsCluster {
				for _, ep := range instanceCfg.ClusterEndpoints {
					if strings.EqualFold(ep.NodeName, node.Node) {
						if effective := clusterEndpointEffectiveURL(ep); effective != "" {
							sshHost = effective
						}
						break
					}
				}
			}

			if strings.TrimSpace(sshHost) == "" {
				sshHost = node.Node
			}

			temp, err := m.tempCollector.CollectTemperature(tempCtx, sshHost, node.Node)
			tempCancel()

			if err == nil && temp != nil && temp.Available {
				// Get the current CPU temperature (prefer package, fall back to max)
				currentTemp := temp.CPUPackage
				if currentTemp == 0 && temp.CPUMax > 0 {
					currentTemp = temp.CPUMax
				}

				// Find previous temperature data for this node to preserve min/max
				var prevTemp *models.Temperature
				for _, prevNode := range prevInstanceNodes {
					if prevNode.ID == modelNode.ID && prevNode.Temperature != nil {
						prevTemp = prevNode.Temperature
						break
					}
				}

				// Initialize or update min/max tracking
				if prevTemp != nil && prevTemp.CPUMin > 0 {
					// Preserve existing min/max and update if necessary
					temp.CPUMin = prevTemp.CPUMin
					temp.CPUMaxRecord = prevTemp.CPUMaxRecord
					temp.MinRecorded = prevTemp.MinRecorded
					temp.MaxRecorded = prevTemp.MaxRecorded

					// Update min if current is lower
					if currentTemp > 0 && currentTemp < temp.CPUMin {
						temp.CPUMin = currentTemp
						temp.MinRecorded = time.Now()
					}

					// Update max if current is higher
					if currentTemp > temp.CPUMaxRecord {
						temp.CPUMaxRecord = currentTemp
						temp.MaxRecorded = time.Now()
					}
				} else if currentTemp > 0 {
					// First reading - initialize min/max to current value
					temp.CPUMin = currentTemp
					temp.CPUMaxRecord = currentTemp
					temp.MinRecorded = time.Now()
					temp.MaxRecorded = time.Now()
				}

				modelNode.Temperature = temp
				log.Debug().
					Str("node", node.Node).
					Str("sshHost", sshHost).
					Float64("cpuPackage", temp.CPUPackage).
					Float64("cpuMax", temp.CPUMax).
					Float64("cpuMin", temp.CPUMin).
					Float64("cpuMaxRecord", temp.CPUMaxRecord).
					Int("nvmeCount", len(temp.NVMe)).
					Msg("Collected temperature data")
			} else if err != nil {
				log.Debug().
					Str("node", node.Node).
					Str("sshHost", sshHost).
					Bool("isCluster", modelNode.IsClusterMember).
					Int("endpointCount", len(instanceCfg.ClusterEndpoints)).
					Msg("Temperature collection failed - check SSH access")
			} else if temp != nil {
				log.Debug().
					Str("node", node.Node).
					Str("sshHost", sshHost).
					Bool("available", temp.Available).
					Msg("Temperature data unavailable after collection")
			}
		}

		if m.pollMetrics != nil {
			nodeNameLabel := strings.TrimSpace(node.Node)
			if nodeNameLabel == "" {
				nodeNameLabel = strings.TrimSpace(modelNode.DisplayName)
			}
			if nodeNameLabel == "" {
				nodeNameLabel = "unknown-node"
			}

			success := true
			nodeErrReason := ""
			health := strings.ToLower(strings.TrimSpace(modelNode.ConnectionHealth))
			if health != "" && health != "healthy" {
				success = false
				nodeErrReason = fmt.Sprintf("connection health %s", health)
			}

			status := strings.ToLower(strings.TrimSpace(modelNode.Status))
			if success && status != "" && status != "online" {
				success = false
				nodeErrReason = fmt.Sprintf("status %s", status)
			}

			var nodeErr error
			if !success {
				if nodeErrReason == "" {
					nodeErrReason = "unknown node error"
				}
				nodeErr = stderrors.New(nodeErrReason)
			}

			m.pollMetrics.RecordNodeResult(NodePollResult{
				InstanceName: instanceName,
				InstanceType: "pve",
				NodeName:     nodeNameLabel,
				Success:      success,
				Error:        nodeErr,
				StartTime:    nodeStart,
				EndTime:      time.Now(),
			})
		}

		modelNodes = append(modelNodes, modelNode)
	}

	if len(modelNodes) == 0 && len(prevInstanceNodes) > 0 {
		log.Warn().
			Str("instance", instanceName).
			Int("previousCount", len(prevInstanceNodes)).
			Msg("No Proxmox nodes returned this cycle - preserving previous state")

		// Mark connection health as degraded to reflect polling failure
		m.state.SetConnectionHealth(instanceName, false)

		preserved := make([]models.Node, 0, len(prevInstanceNodes))
		for _, prevNode := range prevInstanceNodes {
			nodeCopy := prevNode
			nodeCopy.Status = "offline"
			nodeCopy.ConnectionHealth = "error"
			nodeCopy.Uptime = 0
			nodeCopy.CPU = 0
			preserved = append(preserved, nodeCopy)
		}
		modelNodes = preserved
	}

	// Update state first so we have nodes available
	m.state.UpdateNodesForInstance(instanceName, modelNodes)

	// Now get storage data to use as fallback for disk metrics if needed
	storageByNode := make(map[string]models.Disk)
	if instanceCfg.MonitorStorage {
		_, err := client.GetAllStorage(ctx)
		if err == nil {
			for _, node := range nodes {
				// Skip offline nodes to avoid 595 errors
				if node.Status != "online" {
					continue
				}

				nodeStorages, err := client.GetStorage(ctx, node.Node)
				if err == nil {
					// Look for local or local-lvm storage as most stable disk metric
					for _, storage := range nodeStorages {
						if reason, skip := readOnlyFilesystemReason(storage.Type, storage.Total, storage.Used); skip {
							log.Debug().
								Str("node", node.Node).
								Str("storage", storage.Storage).
								Str("type", storage.Type).
								Str("skipReason", reason).
								Uint64("total", storage.Total).
								Uint64("used", storage.Used).
								Msg("Skipping read-only storage while building disk fallback")
							continue
						}
						if storage.Storage == "local" || storage.Storage == "local-lvm" {
							disk := models.Disk{
								Total: int64(storage.Total),
								Used:  int64(storage.Used),
								Free:  int64(storage.Available),
								Usage: safePercentage(float64(storage.Used), float64(storage.Total)),
							}
							// Prefer "local" over "local-lvm"
							if _, exists := storageByNode[node.Node]; !exists || storage.Storage == "local" {
								storageByNode[node.Node] = disk
								log.Debug().
									Str("node", node.Node).
									Str("storage", storage.Storage).
									Float64("usage", disk.Usage).
									Msg("Using storage for disk metrics fallback")
							}
						}
					}
				}
			}
		}
	}

	// Poll physical disks for health monitoring (enabled by default unless explicitly disabled)
	// Skip if MonitorPhysicalDisks is explicitly set to false
	if instanceCfg.MonitorPhysicalDisks != nil && !*instanceCfg.MonitorPhysicalDisks {
		log.Debug().Str("instance", instanceName).Msg("Physical disk monitoring explicitly disabled")
		// Keep any existing disk data visible (don't clear it)
	} else {
		// Enabled by default (when nil or true)
		// Determine polling interval (default 5 minutes to avoid spinning up HDDs too frequently)
		pollingInterval := 5 * time.Minute
		if instanceCfg.PhysicalDiskPollingMinutes > 0 {
			pollingInterval = time.Duration(instanceCfg.PhysicalDiskPollingMinutes) * time.Minute
		}

		// Check if enough time has elapsed since last poll
		m.mu.Lock()
		lastPoll, exists := m.lastPhysicalDiskPoll[instanceName]
		shouldPoll := !exists || time.Since(lastPoll) >= pollingInterval
		if shouldPoll {
			m.lastPhysicalDiskPoll[instanceName] = time.Now()
		}
		m.mu.Unlock()

		if !shouldPoll {
			log.Debug().
				Str("instance", instanceName).
				Dur("sinceLastPoll", time.Since(lastPoll)).
				Dur("interval", pollingInterval).
				Msg("Skipping physical disk poll - interval not elapsed")
			// Refresh NVMe temperatures using the latest sensor data even when we skip the disk poll
			currentState := m.state.GetSnapshot()
			existing := make([]models.PhysicalDisk, 0)
			for _, disk := range currentState.PhysicalDisks {
				if disk.Instance == instanceName {
					existing = append(existing, disk)
				}
			}
			if len(existing) > 0 {
				updated := mergeNVMeTempsIntoDisks(existing, modelNodes)
				m.state.UpdatePhysicalDisks(instanceName, updated)
			}
		} else {
			log.Debug().
				Int("nodeCount", len(nodes)).
				Dur("interval", pollingInterval).
				Msg("Starting disk health polling")

			// Get existing disks from state to preserve data for offline nodes
			currentState := m.state.GetSnapshot()
			existingDisksMap := make(map[string]models.PhysicalDisk)
			for _, disk := range currentState.PhysicalDisks {
				if disk.Instance == instanceName {
					existingDisksMap[disk.ID] = disk
				}
			}

			var allDisks []models.PhysicalDisk
			polledNodes := make(map[string]bool) // Track which nodes we successfully polled

			for _, node := range nodes {
				// Skip offline nodes but preserve their existing disk data
				if node.Status != "online" {
					log.Debug().Str("node", node.Node).Msg("Skipping disk poll for offline node - preserving existing data")
					continue
				}

				// Get disk list for this node
				log.Debug().Str("node", node.Node).Msg("Getting disk list for node")
				disks, err := client.GetDisks(ctx, node.Node)
				if err != nil {
					// Check if it's a permission error or if the endpoint doesn't exist
					if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
						log.Warn().
							Str("node", node.Node).
							Err(err).
							Msg("Insufficient permissions to access disk information - check API token permissions")
					} else if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "501") {
						log.Info().
							Str("node", node.Node).
							Msg("Disk monitoring not available on this node (may be using non-standard storage)")
					} else {
						log.Warn().
							Str("node", node.Node).
							Err(err).
							Msg("Failed to get disk list")
					}
					continue
				}

				log.Debug().
					Str("node", node.Node).
					Int("diskCount", len(disks)).
					Msg("Got disk list for node")

				// Mark this node as successfully polled
				polledNodes[node.Node] = true

				// Check each disk for health issues and add to state
				for _, disk := range disks {
					// Create PhysicalDisk model
					diskID := fmt.Sprintf("%s-%s-%s", instanceName, node.Node, strings.ReplaceAll(disk.DevPath, "/", "-"))
					physicalDisk := models.PhysicalDisk{
						ID:          diskID,
						Node:        node.Node,
						Instance:    instanceName,
						DevPath:     disk.DevPath,
						Model:       disk.Model,
						Serial:      disk.Serial,
						Type:        disk.Type,
						Size:        disk.Size,
						Health:      disk.Health,
						Wearout:     disk.Wearout,
						RPM:         disk.RPM,
						Used:        disk.Used,
						LastChecked: time.Now(),
					}

					allDisks = append(allDisks, physicalDisk)

					log.Debug().
						Str("node", node.Node).
						Str("disk", disk.DevPath).
						Str("model", disk.Model).
						Str("health", disk.Health).
						Int("wearout", disk.Wearout).
						Msg("Checking disk health")

					normalizedHealth := strings.ToUpper(strings.TrimSpace(disk.Health))
					if normalizedHealth != "" && normalizedHealth != "UNKNOWN" && normalizedHealth != "PASSED" && normalizedHealth != "OK" {
						// Disk has failed or is failing - alert manager will handle this
						log.Warn().
							Str("node", node.Node).
							Str("disk", disk.DevPath).
							Str("model", disk.Model).
							Str("health", disk.Health).
							Int("wearout", disk.Wearout).
							Msg("Disk health issue detected")

						// Pass disk info to alert manager
						m.alertManager.CheckDiskHealth(instanceName, node.Node, disk)
					} else if disk.Wearout > 0 && disk.Wearout < 10 {
						// Low wearout warning (less than 10% life remaining)
						log.Warn().
							Str("node", node.Node).
							Str("disk", disk.DevPath).
							Str("model", disk.Model).
							Int("wearout", disk.Wearout).
							Msg("SSD wearout critical - less than 10% life remaining")

						// Pass to alert manager for wearout alert
						m.alertManager.CheckDiskHealth(instanceName, node.Node, disk)
					}
				}
			}

			// Preserve existing disk data for nodes that weren't polled (offline or error)
			for _, existingDisk := range existingDisksMap {
				// Only preserve if we didn't poll this node
				if !polledNodes[existingDisk.Node] {
					// Keep the existing disk data but update the LastChecked to indicate it's stale
					allDisks = append(allDisks, existingDisk)
					log.Debug().
						Str("node", existingDisk.Node).
						Str("disk", existingDisk.DevPath).
						Msg("Preserving existing disk data for unpolled node")
				}
			}

			allDisks = mergeNVMeTempsIntoDisks(allDisks, modelNodes)

			// Update physical disks in state
			log.Debug().
				Str("instance", instanceName).
				Int("diskCount", len(allDisks)).
				Int("preservedCount", len(existingDisksMap)-len(polledNodes)).
				Msg("Updating physical disks in state")
			m.state.UpdatePhysicalDisks(instanceName, allDisks)
		}
	}
	// Note: Physical disk monitoring is now enabled by default with a 5-minute polling interval.
	// Users can explicitly disable it in node settings. Disk data is preserved between polls.

	// Update nodes with storage fallback if rootfs was not available
	for i := range modelNodes {
		if modelNodes[i].Disk.Total == 0 {
			if disk, exists := storageByNode[modelNodes[i].Name]; exists {
				modelNodes[i].Disk = disk
				log.Debug().
					Str("node", modelNodes[i].Name).
					Float64("usage", disk.Usage).
					Msg("Applied storage fallback for disk metrics")
			}
		}

		if modelNodes[i].Status == "online" {
			// Record node metrics history only for online nodes
			now := time.Now()
			m.metricsHistory.AddNodeMetric(modelNodes[i].ID, "cpu", modelNodes[i].CPU*100, now)
			m.metricsHistory.AddNodeMetric(modelNodes[i].ID, "memory", modelNodes[i].Memory.Usage, now)
			m.metricsHistory.AddNodeMetric(modelNodes[i].ID, "disk", modelNodes[i].Disk.Usage, now)
		}

		// Check thresholds for alerts
		m.alertManager.CheckNode(modelNodes[i])
	}

	// Update state again with corrected disk metrics
	m.state.UpdateNodesForInstance(instanceName, modelNodes)

	// Clean up alerts for nodes that no longer exist
	// Get all nodes from the global state (includes all instances)
	existingNodes := make(map[string]bool)
	allState := m.state.GetSnapshot()
	for _, node := range allState.Nodes {
		existingNodes[node.Name] = true
	}
	m.alertManager.CleanupAlertsForNodes(existingNodes)

	// Periodically re-check cluster status for nodes marked as standalone
	// This addresses issue #437 where clusters aren't detected on first attempt
	if !instanceCfg.IsCluster {
		// Check every 5 minutes if this is actually a cluster
		if time.Since(m.lastClusterCheck[instanceName]) > 5*time.Minute {
			m.lastClusterCheck[instanceName] = time.Now()

			// Try to detect if this is actually a cluster
			isActuallyCluster, checkErr := client.IsClusterMember(ctx)
			if checkErr == nil && isActuallyCluster {
				// This node is actually part of a cluster!
				log.Info().
					Str("instance", instanceName).
					Msg("Detected that standalone node is actually part of a cluster - updating configuration")

				// Update the configuration
				for i := range m.config.PVEInstances {
					if m.config.PVEInstances[i].Name == instanceName {
						m.config.PVEInstances[i].IsCluster = true
						// Note: We can't get the cluster name here without direct client access
						// It will be detected on the next configuration update
						log.Info().
							Str("instance", instanceName).
							Msg("Marked node as cluster member - cluster name will be detected on next update")

							// Save the updated configuration
						if m.persistence != nil {
							if err := m.persistence.SaveNodesConfig(m.config.PVEInstances, m.config.PBSInstances, m.config.PMGInstances); err != nil {
								log.Warn().Err(err).Msg("Failed to persist updated node configuration")
							}
						}
						break
					}
				}
			}
		}
	}

	// Update cluster endpoint online status if this is a cluster
	if instanceCfg.IsCluster && len(instanceCfg.ClusterEndpoints) > 0 {
		// Create a map of online nodes from our polling results
		onlineNodes := make(map[string]bool)
		for _, node := range modelNodes {
			// Node is online if we successfully got its data
			onlineNodes[node.Name] = node.Status == "online"
		}

		// Update the online status for each cluster endpoint
		for i := range instanceCfg.ClusterEndpoints {
			if online, exists := onlineNodes[instanceCfg.ClusterEndpoints[i].NodeName]; exists {
				instanceCfg.ClusterEndpoints[i].Online = online
				if online {
					instanceCfg.ClusterEndpoints[i].LastSeen = time.Now()
				}
			}
		}

		// Update the config with the new online status
		// This is needed so the UI can reflect the current status
		for idx, cfg := range m.config.PVEInstances {
			if cfg.Name == instanceName {
				m.config.PVEInstances[idx].ClusterEndpoints = instanceCfg.ClusterEndpoints
				break
			}
		}
	}

	// Poll VMs and containers together using cluster/resources for efficiency
	if instanceCfg.MonitorVMs || instanceCfg.MonitorContainers {
		select {
		case <-ctx.Done():
			pollErr = ctx.Err()
			return
		default:
			// Always try the efficient cluster/resources endpoint first
			// This endpoint works on both clustered and standalone nodes
			// Testing confirmed it works on standalone nodes like pimox
			useClusterEndpoint := m.pollVMsAndContainersEfficient(ctx, instanceName, client)

			if !useClusterEndpoint {
				// Fall back to traditional polling only if cluster/resources not available
				// This should be rare - only for very old Proxmox versions
				log.Debug().
					Str("instance", instanceName).
					Msg("cluster/resources endpoint not available, using traditional polling")

				// Check if configuration needs updating
				if instanceCfg.IsCluster {
					isActuallyCluster, checkErr := client.IsClusterMember(ctx)
					if checkErr == nil && !isActuallyCluster {
						log.Warn().
							Str("instance", instanceName).
							Msg("Instance marked as cluster but is actually standalone - consider updating configuration")
						instanceCfg.IsCluster = false
					}
				}

				// Use optimized parallel polling for better performance
				if instanceCfg.MonitorVMs {
					m.pollVMsWithNodes(ctx, instanceName, client, nodes)
				}
				if instanceCfg.MonitorContainers {
					m.pollContainersWithNodes(ctx, instanceName, client, nodes)
				}
			}
		}
	}

	// Poll storage if enabled
	if instanceCfg.MonitorStorage {
		select {
		case <-ctx.Done():
			pollErr = ctx.Err()
			return
		default:
			m.pollStorageWithNodes(ctx, instanceName, client, nodes)
		}
	}

	// Poll backups if enabled - respect configured interval or cycle gating
	if instanceCfg.MonitorBackups {
		if !m.config.EnableBackupPolling {
			log.Debug().
				Str("instance", instanceName).
				Msg("Skipping backup polling - globally disabled")
		} else {
			now := time.Now()

			m.mu.RLock()
			lastPoll := m.lastPVEBackupPoll[instanceName]
			m.mu.RUnlock()

			shouldPoll, reason, newLast := m.shouldRunBackupPoll(lastPoll, now)
			if !shouldPoll {
				if reason != "" {
					log.Debug().
						Str("instance", instanceName).
						Str("reason", reason).
						Msg("Skipping PVE backup polling this cycle")
				}
			} else {
				select {
				case <-ctx.Done():
					pollErr = ctx.Err()
					return
				default:
					m.mu.Lock()
					m.lastPVEBackupPoll[instanceName] = newLast
					m.mu.Unlock()

					// Run backup polling in a separate goroutine to avoid blocking real-time stats
					go func(startTime time.Time, inst string, pveClient PVEClientInterface) {
						timeout := m.calculateBackupOperationTimeout(inst)
						log.Info().
							Str("instance", inst).
							Dur("timeout", timeout).
							Msg("Starting background backup/snapshot polling")

						// Create a separate context with longer timeout for backup operations
						backupCtx, cancel := context.WithTimeout(context.Background(), timeout)
						defer cancel()

						// Poll backup tasks
						m.pollBackupTasks(backupCtx, inst, pveClient)

						// Poll storage backups - pass nodes to avoid duplicate API calls
						m.pollStorageBackupsWithNodes(backupCtx, inst, pveClient, nodes)

						// Poll guest snapshots
						m.pollGuestSnapshots(backupCtx, inst, pveClient)

						duration := time.Since(startTime)
						log.Info().
							Str("instance", inst).
							Dur("duration", duration).
							Msg("Completed background backup/snapshot polling")

						// Record actual completion time for interval scheduling
						m.mu.Lock()
						m.lastPVEBackupPoll[inst] = time.Now()
						m.mu.Unlock()
					}(now, instanceName, client)
				}
			}
		}
	}
}

// pollVMsAndContainersEfficient uses the cluster/resources endpoint to get all VMs and containers in one call
// This works on both clustered and standalone nodes for efficient polling
func (m *Monitor) pollVMsAndContainersEfficient(ctx context.Context, instanceName string, client PVEClientInterface) bool {
	log.Info().Str("instance", instanceName).Msg("Polling VMs and containers using efficient cluster/resources endpoint")

	// Get all resources in a single API call
	resources, err := client.GetClusterResources(ctx, "vm")
	if err != nil {
		log.Debug().Err(err).Str("instance", instanceName).Msg("cluster/resources not available, falling back to traditional polling")
		return false
	}

	var allVMs []models.VM
	var allContainers []models.Container

	for _, res := range resources {
		// Avoid duplicating node name in ID when instance name equals node name
		var guestID string
		if instanceName == res.Node {
			guestID = fmt.Sprintf("%s-%d", res.Node, res.VMID)
		} else {
			guestID = fmt.Sprintf("%s-%s-%d", instanceName, res.Node, res.VMID)
		}

		// Debug log the resource type
		log.Debug().
			Str("instance", instanceName).
			Str("name", res.Name).
			Int("vmid", res.VMID).
			Str("type", res.Type).
			Msg("Processing cluster resource")

		// Initialize I/O metrics from cluster resources (may be 0 for VMs)
		diskReadBytes := int64(res.DiskRead)
		diskWriteBytes := int64(res.DiskWrite)
		networkInBytes := int64(res.NetIn)
		networkOutBytes := int64(res.NetOut)
		var individualDisks []models.Disk // Store individual filesystems for multi-disk monitoring
		var ipAddresses []string
		var networkInterfaces []models.GuestNetworkInterface
		var osName, osVersion, agentVersion string

		if res.Type == "qemu" {
			// Skip templates if configured
			if res.Template == 1 {
				continue
			}

			memTotal := res.MaxMem
			memUsed := res.Mem
			memorySource := "cluster-resources"
			guestRaw := VMMemoryRaw{
				ListingMem:    res.Mem,
				ListingMaxMem: res.MaxMem,
			}
			var detailedStatus *proxmox.VMStatus

			// Try to get actual disk usage from guest agent if VM is running
			diskUsed := res.Disk
			diskTotal := res.MaxDisk
			diskFree := diskTotal - diskUsed
			diskUsage := safePercentage(float64(diskUsed), float64(diskTotal))

			// If VM shows 0 disk usage but has allocated disk, it's likely guest agent issue
			// Set to -1 to indicate "unknown" rather than showing misleading 0%
			if res.Type == "qemu" && diskUsed == 0 && diskTotal > 0 && res.Status == "running" {
				diskUsage = -1
			}

			// For running VMs, always try to get filesystem info from guest agent
			// The cluster/resources endpoint often returns 0 or incorrect values for disk usage
			// We should prefer guest agent data when available for accurate metrics
			if res.Status == "running" && res.Type == "qemu" {
				// First check if agent is enabled by getting VM status
				status, err := client.GetVMStatus(ctx, res.Node, res.VMID)
				if err != nil {
					log.Debug().
						Err(err).
						Str("instance", instanceName).
						Str("vm", res.Name).
						Int("vmid", res.VMID).
						Msg("Could not get VM status to check guest agent availability")
				} else if status != nil {
					detailedStatus = status
					guestRaw.StatusMaxMem = detailedStatus.MaxMem
					guestRaw.StatusMem = detailedStatus.Mem
					guestRaw.StatusFreeMem = detailedStatus.FreeMem
					guestRaw.Balloon = detailedStatus.Balloon
					guestRaw.BalloonMin = detailedStatus.BalloonMin
					guestRaw.Agent = detailedStatus.Agent
					memAvailable := uint64(0)
					if detailedStatus.MemInfo != nil {
						guestRaw.MemInfoUsed = detailedStatus.MemInfo.Used
						guestRaw.MemInfoFree = detailedStatus.MemInfo.Free
						guestRaw.MemInfoTotal = detailedStatus.MemInfo.Total
						guestRaw.MemInfoAvailable = detailedStatus.MemInfo.Available
						guestRaw.MemInfoBuffers = detailedStatus.MemInfo.Buffers
						guestRaw.MemInfoCached = detailedStatus.MemInfo.Cached
						guestRaw.MemInfoShared = detailedStatus.MemInfo.Shared

						switch {
						case detailedStatus.MemInfo.Available > 0:
							memAvailable = detailedStatus.MemInfo.Available
							memorySource = "meminfo-available"
						case detailedStatus.MemInfo.Free > 0 ||
							detailedStatus.MemInfo.Buffers > 0 ||
							detailedStatus.MemInfo.Cached > 0:
							memAvailable = detailedStatus.MemInfo.Free +
								detailedStatus.MemInfo.Buffers +
								detailedStatus.MemInfo.Cached
							memorySource = "meminfo-derived"
						}
					}

					// Use actual disk I/O values from detailed status
					diskReadBytes = int64(detailedStatus.DiskRead)
					diskWriteBytes = int64(detailedStatus.DiskWrite)
					networkInBytes = int64(detailedStatus.NetIn)
					networkOutBytes = int64(detailedStatus.NetOut)

					if detailedStatus.Balloon > 0 && detailedStatus.Balloon < detailedStatus.MaxMem {
						memTotal = detailedStatus.Balloon
						guestRaw.DerivedFromBall = true
					} else if detailedStatus.MaxMem > 0 {
						memTotal = detailedStatus.MaxMem
						guestRaw.DerivedFromBall = false
					}

					switch {
					case memAvailable > 0:
						if memAvailable > memTotal {
							memAvailable = memTotal
						}
						memUsed = memTotal - memAvailable
					case detailedStatus.FreeMem > 0 && memTotal >= detailedStatus.FreeMem:
						memUsed = memTotal - detailedStatus.FreeMem
						memorySource = "status-freemem"
					case detailedStatus.Mem > 0:
						memUsed = detailedStatus.Mem
						memorySource = "status-mem"
					}
					if memUsed > memTotal {
						memUsed = memTotal
					}

					// Gather guest metadata from the agent when available
					guestIPs, guestIfaces, guestOSName, guestOSVersion, guestAgentVersion := m.fetchGuestAgentMetadata(ctx, client, instanceName, res.Node, res.Name, res.VMID, detailedStatus)
					if len(guestIPs) > 0 {
						ipAddresses = guestIPs
					}
					if len(guestIfaces) > 0 {
						networkInterfaces = guestIfaces
					}
					if guestOSName != "" {
						osName = guestOSName
					}
					if guestOSVersion != "" {
						osVersion = guestOSVersion
					}
					if guestAgentVersion != "" {
						agentVersion = guestAgentVersion
					}

					// Always try to get filesystem info if agent is enabled
					// Prefer guest agent data over cluster/resources data for accuracy
					if detailedStatus.Agent > 0 {
						log.Debug().
							Str("instance", instanceName).
							Str("vm", res.Name).
							Int("vmid", res.VMID).
							Int("agent", detailedStatus.Agent).
							Uint64("current_disk", diskUsed).
							Uint64("current_maxdisk", diskTotal).
							Msg("Guest agent enabled, querying filesystem info for accurate disk usage")

						fsInfo, err := client.GetVMFSInfo(ctx, res.Node, res.VMID)
						if err != nil {
							// Log more helpful error messages based on the error type
							errMsg := err.Error()
							if strings.Contains(errMsg, "500") || strings.Contains(errMsg, "QEMU guest agent is not running") {
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("vmid", res.VMID).
									Msg("Guest agent enabled in VM config but not running inside guest OS. Install and start qemu-guest-agent in the VM")
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Msg("To verify: ssh into VM and run 'systemctl status qemu-guest-agent' or 'ps aux | grep qemu-ga'")
							} else if strings.Contains(errMsg, "timeout") {
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("vmid", res.VMID).
									Msg("Guest agent timeout - agent may be installed but not responding")
							} else if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "authentication error") {
								// Permission error - user/token lacks required permissions
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("vmid", res.VMID).
									Msg("VM disk monitoring permission denied. Check permissions:")
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Msg("• Proxmox 9: Ensure token/user has VM.GuestAgent.Audit privilege (Pulse setup adds this via PulseMonitor role)")
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Msg("• Proxmox 8: Ensure token/user has VM.Monitor privilege (Pulse setup adds this via PulseMonitor role)")
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Msg("• All versions: Sys.Audit is recommended for Ceph metrics and applied when available")
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Msg("• Re-run Pulse setup script if node was added before v4.7")
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Msg("• Verify guest agent is installed and running inside the VM")
							} else {
								log.Debug().
									Err(err).
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("vmid", res.VMID).
									Msg("Failed to get filesystem info from guest agent")
							}
						} else if len(fsInfo) == 0 {
							log.Info().
								Str("instance", instanceName).
								Str("vm", res.Name).
								Int("vmid", res.VMID).
								Msg("Guest agent returned no filesystem info - agent may need restart or VM may have no mounted filesystems")
						} else {
							log.Debug().
								Str("instance", instanceName).
								Str("vm", res.Name).
								Int("filesystems", len(fsInfo)).
								Msg("Got filesystem info from guest agent")

							// Aggregate disk usage from all filesystems AND preserve individual disk data
							var totalBytes, usedBytes uint64
							var skippedFS []string
							var includedFS []string

							// Log all filesystems received for debugging
							log.Debug().
								Str("instance", instanceName).
								Str("vm", res.Name).
								Int("vmid", res.VMID).
								Int("filesystem_count", len(fsInfo)).
								Msg("Processing filesystems from guest agent")

							for _, fs := range fsInfo {
								// Skip special filesystems and mounts
								skipReasons := []string{}
								reasonReadOnly := ""
								shouldSkip := false

								// Check filesystem type
								fsTypeLower := strings.ToLower(fs.Type)
								if reason, skip := readOnlyFilesystemReason(fs.Type, fs.TotalBytes, fs.UsedBytes); skip {
									skipReasons = append(skipReasons, fmt.Sprintf("read-only-%s", reason))
									reasonReadOnly = reason
									shouldSkip = true
								}
								if fs.Type == "tmpfs" || fs.Type == "devtmpfs" ||
									fs.Type == "cgroup" || fs.Type == "cgroup2" ||
									fs.Type == "sysfs" || fs.Type == "proc" ||
									fs.Type == "devpts" || fs.Type == "securityfs" ||
									fs.Type == "debugfs" || fs.Type == "tracefs" ||
									fs.Type == "fusectl" || fs.Type == "configfs" ||
									fs.Type == "pstore" || fs.Type == "hugetlbfs" ||
									fs.Type == "mqueue" || fs.Type == "bpf" ||
									strings.Contains(fsTypeLower, "fuse") || // Skip FUSE mounts (often network/special)
									strings.Contains(fsTypeLower, "9p") || // Skip 9p mounts (VM shared folders)
									strings.Contains(fsTypeLower, "nfs") || // Skip NFS mounts
									strings.Contains(fsTypeLower, "cifs") || // Skip CIFS/SMB mounts
									strings.Contains(fsTypeLower, "smb") { // Skip SMB mounts
									skipReasons = append(skipReasons, "special-fs-type")
									shouldSkip = true
								}

								// Check mountpoint patterns
								if strings.HasPrefix(fs.Mountpoint, "/dev") ||
									strings.HasPrefix(fs.Mountpoint, "/proc") ||
									strings.HasPrefix(fs.Mountpoint, "/sys") ||
									strings.HasPrefix(fs.Mountpoint, "/run") ||
									strings.HasPrefix(fs.Mountpoint, "/var/lib/docker") || // Skip Docker volumes
									strings.HasPrefix(fs.Mountpoint, "/snap") || // Skip snap mounts
									fs.Mountpoint == "/boot/efi" ||
									fs.Mountpoint == "System Reserved" || // Windows System Reserved partition
									strings.Contains(fs.Mountpoint, "System Reserved") { // Various Windows reserved formats
									skipReasons = append(skipReasons, "special-mountpoint")
									shouldSkip = true
								}

								if shouldSkip {
									if reasonReadOnly != "" {
										log.Debug().
											Str("instance", instanceName).
											Str("vm", res.Name).
											Int("vmid", res.VMID).
											Str("mountpoint", fs.Mountpoint).
											Str("type", fs.Type).
											Float64("total_gb", float64(fs.TotalBytes)/1073741824).
											Float64("used_gb", float64(fs.UsedBytes)/1073741824).
											Msg("Skipping read-only filesystem from disk aggregation")
									}
									skippedFS = append(skippedFS, fmt.Sprintf("%s(%s,%s)",
										fs.Mountpoint, fs.Type, strings.Join(skipReasons, ",")))
									continue
								}

								// Only count real filesystems with valid data
								// Some filesystems report 0 bytes (like unformatted or system partitions)
								if fs.TotalBytes > 0 {
									totalBytes += fs.TotalBytes
									usedBytes += fs.UsedBytes
									includedFS = append(includedFS, fmt.Sprintf("%s(%s,%.1fGB)",
										fs.Mountpoint, fs.Type, float64(fs.TotalBytes)/1073741824))

									// Add to individual disks array
									individualDisks = append(individualDisks, models.Disk{
										Total:      int64(fs.TotalBytes),
										Used:       int64(fs.UsedBytes),
										Free:       int64(fs.TotalBytes - fs.UsedBytes),
										Usage:      safePercentage(float64(fs.UsedBytes), float64(fs.TotalBytes)),
										Mountpoint: fs.Mountpoint,
										Type:       fs.Type,
										Device:     fs.Disk,
									})

									log.Debug().
										Str("instance", instanceName).
										Str("vm", res.Name).
										Int("vmid", res.VMID).
										Str("mountpoint", fs.Mountpoint).
										Str("type", fs.Type).
										Uint64("total", fs.TotalBytes).
										Uint64("used", fs.UsedBytes).
										Float64("total_gb", float64(fs.TotalBytes)/1073741824).
										Float64("used_gb", float64(fs.UsedBytes)/1073741824).
										Msg("Including filesystem in disk usage calculation")
								} else if fs.TotalBytes == 0 && len(fs.Mountpoint) > 0 {
									skippedFS = append(skippedFS, fmt.Sprintf("%s(%s,0GB)", fs.Mountpoint, fs.Type))
									log.Debug().
										Str("instance", instanceName).
										Str("vm", res.Name).
										Int("vmid", res.VMID).
										Str("mountpoint", fs.Mountpoint).
										Str("type", fs.Type).
										Msg("Skipping filesystem with zero total bytes")
								}
							}

							if len(skippedFS) > 0 {
								log.Debug().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Strs("skipped", skippedFS).
									Msg("Skipped special filesystems")
							}

							if len(includedFS) > 0 {
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("vmid", res.VMID).
									Strs("included", includedFS).
									Msg("Filesystems included in disk calculation")
							}

							// If we got valid data from guest agent, use it
							if totalBytes > 0 {
								// Sanity check: if the reported disk is way larger than allocated disk,
								// we might be getting host disk info somehow
								allocatedDiskGB := float64(res.MaxDisk) / 1073741824
								reportedDiskGB := float64(totalBytes) / 1073741824

								// If reported disk is more than 2x the allocated disk, log a warning
								// This could indicate we're getting host disk or network shares
								if allocatedDiskGB > 0 && reportedDiskGB > allocatedDiskGB*2 {
									log.Warn().
										Str("instance", instanceName).
										Str("vm", res.Name).
										Int("vmid", res.VMID).
										Float64("allocated_gb", allocatedDiskGB).
										Float64("reported_gb", reportedDiskGB).
										Float64("ratio", reportedDiskGB/allocatedDiskGB).
										Strs("filesystems", includedFS).
										Msg("VM reports disk usage significantly larger than allocated disk - possible issue with filesystem detection")
								}

								diskTotal = totalBytes
								diskUsed = usedBytes
								diskFree = totalBytes - usedBytes
								diskUsage = safePercentage(float64(usedBytes), float64(totalBytes))

								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("vmid", res.VMID).
									Uint64("totalBytes", totalBytes).
									Uint64("usedBytes", usedBytes).
									Float64("total_gb", float64(totalBytes)/1073741824).
									Float64("used_gb", float64(usedBytes)/1073741824).
									Float64("allocated_gb", allocatedDiskGB).
									Float64("usage", diskUsage).
									Uint64("old_disk", res.Disk).
									Uint64("old_maxdisk", res.MaxDisk).
									Msg("Using guest agent data for accurate disk usage (replacing cluster/resources data)")
							} else {
								// Only special filesystems found - show allocated disk size instead
								if diskTotal > 0 {
									diskUsage = -1 // Show as allocated size
								}
								log.Info().
									Str("instance", instanceName).
									Str("vm", res.Name).
									Int("filesystems_found", len(fsInfo)).
									Msg("Guest agent provided filesystem info but no usable filesystems found (all were special mounts)")
							}
						}
					} else {
						// Agent disabled - show allocated disk size
						if diskTotal > 0 {
							diskUsage = -1 // Show as allocated size
						}
						log.Debug().
							Str("instance", instanceName).
							Str("vm", res.Name).
							Int("vmid", res.VMID).
							Int("agent", detailedStatus.Agent).
							Msg("VM does not have guest agent enabled in config")
					}
				} else {
					// No vmStatus available - keep cluster/resources data
					log.Debug().
						Str("instance", instanceName).
						Str("vm", res.Name).
						Int("vmid", res.VMID).
						Msg("Could not get VM status, using cluster/resources disk data")
				}
			}

			if res.Status != "running" {
				memorySource = "powered-off"
				memUsed = 0
			}

			memFree := uint64(0)
			if memTotal >= memUsed {
				memFree = memTotal - memUsed
			}

			sampleTime := time.Now()
			currentMetrics := IOMetrics{
				DiskRead:   diskReadBytes,
				DiskWrite:  diskWriteBytes,
				NetworkIn:  networkInBytes,
				NetworkOut: networkOutBytes,
				Timestamp:  sampleTime,
			}
			diskReadRate, diskWriteRate, netInRate, netOutRate := m.rateTracker.CalculateRates(guestID, currentMetrics)

			memoryUsage := safePercentage(float64(memUsed), float64(memTotal))
			memory := models.Memory{
				Total: int64(memTotal),
				Used:  int64(memUsed),
				Free:  int64(memFree),
				Usage: memoryUsage,
			}
			if memory.Free < 0 {
				memory.Free = 0
			}
			if memory.Used > memory.Total {
				memory.Used = memory.Total
			}
			if detailedStatus != nil && detailedStatus.Balloon > 0 {
				memory.Balloon = int64(detailedStatus.Balloon)
			}

			vm := models.VM{
				ID:       guestID,
				VMID:     res.VMID,
				Name:     res.Name,
				Node:     res.Node,
				Instance: instanceName,
				Status:   res.Status,
				Type:     "qemu",
				CPU:      safeFloat(res.CPU),
				CPUs:     res.MaxCPU,
				Memory:   memory,
				Disk: models.Disk{
					Total: int64(diskTotal),
					Used:  int64(diskUsed),
					Free:  int64(diskFree),
					Usage: diskUsage,
				},
				Disks:             individualDisks, // Individual filesystem data
				IPAddresses:       ipAddresses,
				OSName:            osName,
				OSVersion:         osVersion,
				AgentVersion:      agentVersion,
				NetworkInterfaces: networkInterfaces,
				NetworkIn:         maxInt64(0, int64(netInRate)),
				NetworkOut:        maxInt64(0, int64(netOutRate)),
				DiskRead:          maxInt64(0, int64(diskReadRate)),
				DiskWrite:         maxInt64(0, int64(diskWriteRate)),
				Uptime:            int64(res.Uptime),
				Template:          res.Template == 1,
				LastSeen:          sampleTime,
			}

			// Parse tags
			if res.Tags != "" {
				vm.Tags = strings.Split(res.Tags, ";")

				// Log if Pulse-specific tags are detected
				for _, tag := range vm.Tags {
					switch tag {
					case "pulse-no-alerts", "pulse-monitor-only", "pulse-relaxed":
						log.Info().
							Str("vm", vm.Name).
							Str("node", vm.Node).
							Str("tag", tag).
							Msg("Pulse control tag detected on VM")
					}
				}
			}

			allVMs = append(allVMs, vm)

			m.recordGuestSnapshot(instanceName, vm.Type, res.Node, res.VMID, GuestMemorySnapshot{
				Name:         vm.Name,
				Status:       vm.Status,
				RetrievedAt:  sampleTime,
				MemorySource: memorySource,
				Memory:       vm.Memory,
				Raw:          guestRaw,
			})

			// For non-running VMs, zero out resource usage metrics to prevent false alerts
			// Proxmox may report stale or residual metrics for stopped VMs
			if vm.Status != "running" {
				log.Debug().
					Str("vm", vm.Name).
					Str("status", vm.Status).
					Float64("originalCpu", vm.CPU).
					Float64("originalMemUsage", vm.Memory.Usage).
					Msg("Non-running VM detected - zeroing metrics")

				// Zero out all usage metrics for stopped/paused/suspended VMs
				vm.CPU = 0
				vm.Memory.Usage = 0
				vm.Disk.Usage = 0
				vm.NetworkIn = 0
				vm.NetworkOut = 0
				vm.DiskRead = 0
				vm.DiskWrite = 0
			}

			// Check thresholds for alerts
			m.alertManager.CheckGuest(vm, instanceName)

		} else if res.Type == "lxc" {
			// Skip templates if configured
			if res.Template == 1 {
				continue
			}

			// Calculate I/O rates for container
			currentMetrics := IOMetrics{
				DiskRead:   int64(res.DiskRead),
				DiskWrite:  int64(res.DiskWrite),
				NetworkIn:  int64(res.NetIn),
				NetworkOut: int64(res.NetOut),
				Timestamp:  time.Now(),
			}
			diskReadRate, diskWriteRate, netInRate, netOutRate := m.rateTracker.CalculateRates(guestID, currentMetrics)

			container := models.Container{
				ID:       guestID,
				VMID:     res.VMID,
				Name:     res.Name,
				Node:     res.Node,
				Instance: instanceName,
				Status:   res.Status,
				Type:     "lxc",
				CPU:      safeFloat(res.CPU),
				CPUs:     int(res.MaxCPU),
				Memory: models.Memory{
					Total: int64(res.MaxMem),
					Used:  int64(res.Mem),
					Free:  int64(res.MaxMem - res.Mem),
					Usage: safePercentage(float64(res.Mem), float64(res.MaxMem)),
				},
				Disk: models.Disk{
					Total: int64(res.MaxDisk),
					Used:  int64(res.Disk),
					Free:  int64(res.MaxDisk - res.Disk),
					Usage: safePercentage(float64(res.Disk), float64(res.MaxDisk)),
				},
				NetworkIn:  maxInt64(0, int64(netInRate)),
				NetworkOut: maxInt64(0, int64(netOutRate)),
				DiskRead:   maxInt64(0, int64(diskReadRate)),
				DiskWrite:  maxInt64(0, int64(diskWriteRate)),
				Uptime:     int64(res.Uptime),
				Template:   res.Template == 1,
				LastSeen:   time.Now(),
			}

			// Parse tags
			if res.Tags != "" {
				container.Tags = strings.Split(res.Tags, ";")

				// Log if Pulse-specific tags are detected
				for _, tag := range container.Tags {
					switch tag {
					case "pulse-no-alerts", "pulse-monitor-only", "pulse-relaxed":
						log.Info().
							Str("container", container.Name).
							Str("node", container.Node).
							Str("tag", tag).
							Msg("Pulse control tag detected on container")
					}
				}
			}

			m.enrichContainerMetadata(ctx, client, instanceName, res.Node, &container)

			allContainers = append(allContainers, container)

			// For non-running containers, zero out resource usage metrics to prevent false alerts
			// Proxmox may report stale or residual metrics for stopped containers
			if container.Status != "running" {
				log.Debug().
					Str("container", container.Name).
					Str("status", container.Status).
					Float64("originalCpu", container.CPU).
					Float64("originalMemUsage", container.Memory.Usage).
					Msg("Non-running container detected - zeroing metrics")

				// Zero out all usage metrics for stopped/paused containers
				container.CPU = 0
				container.Memory.Usage = 0
				container.Disk.Usage = 0
				container.NetworkIn = 0
				container.NetworkOut = 0
				container.DiskRead = 0
				container.DiskWrite = 0
			}

			// Check thresholds for alerts
			m.alertManager.CheckGuest(container, instanceName)
		}
	}

	// Update state
	if len(allVMs) > 0 {
		m.state.UpdateVMsForInstance(instanceName, allVMs)
	}
	if len(allContainers) > 0 {
		m.state.UpdateContainersForInstance(instanceName, allContainers)
	}

	m.pollReplicationStatus(ctx, instanceName, client, allVMs)

	log.Info().
		Str("instance", instanceName).
		Int("vms", len(allVMs)).
		Int("containers", len(allContainers)).
		Msg("VMs and containers polled efficiently with cluster/resources")

	return true
}

// pollBackupTasks polls backup tasks from a PVE instance
func (m *Monitor) pollBackupTasks(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Debug().Str("instance", instanceName).Msg("Polling backup tasks")

	tasks, err := client.GetBackupTasks(ctx)
	if err != nil {
		monErr := errors.WrapAPIError("get_backup_tasks", instanceName, err, 0)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get backup tasks")
		return
	}

	var backupTasks []models.BackupTask
	for _, task := range tasks {
		// Extract VMID from task ID (format: "UPID:node:pid:starttime:type:vmid:user@realm:")
		vmid := 0
		if task.ID != "" {
			if vmidInt, err := strconv.Atoi(task.ID); err == nil {
				vmid = vmidInt
			}
		}

		taskID := fmt.Sprintf("%s-%s", instanceName, task.UPID)

		backupTask := models.BackupTask{
			ID:        taskID,
			Node:      task.Node,
			Type:      task.Type,
			VMID:      vmid,
			Status:    task.Status,
			StartTime: time.Unix(task.StartTime, 0),
		}

		if task.EndTime > 0 {
			backupTask.EndTime = time.Unix(task.EndTime, 0)
		}

		backupTasks = append(backupTasks, backupTask)
	}

	// Update state with new backup tasks for this instance
	m.state.UpdateBackupTasksForInstance(instanceName, backupTasks)
}

// pollReplicationStatus polls storage replication jobs for a PVE instance.
func (m *Monitor) pollReplicationStatus(ctx context.Context, instanceName string, client PVEClientInterface, vms []models.VM) {
	log.Debug().Str("instance", instanceName).Msg("Polling replication status")

	jobs, err := client.GetReplicationStatus(ctx)
	if err != nil {
		errMsg := err.Error()
		lowerMsg := strings.ToLower(errMsg)
		if strings.Contains(errMsg, "501") || strings.Contains(errMsg, "404") || strings.Contains(lowerMsg, "not implemented") || strings.Contains(lowerMsg, "not supported") {
			log.Debug().
				Str("instance", instanceName).
				Msg("Replication API not available on this Proxmox instance")
			m.state.UpdateReplicationJobsForInstance(instanceName, []models.ReplicationJob{})
			return
		}

		monErr := errors.WrapAPIError("get_replication_status", instanceName, err, 0)
		log.Warn().
			Err(monErr).
			Str("instance", instanceName).
			Msg("Failed to get replication status")
		return
	}

	if len(jobs) == 0 {
		m.state.UpdateReplicationJobsForInstance(instanceName, []models.ReplicationJob{})
		return
	}

	vmByID := make(map[int]models.VM, len(vms))
	for _, vm := range vms {
		vmByID[vm.VMID] = vm
	}

	converted := make([]models.ReplicationJob, 0, len(jobs))
	now := time.Now()

	for idx, job := range jobs {
		guestID := job.GuestID
		if guestID == 0 {
			if parsed, err := strconv.Atoi(strings.TrimSpace(job.Guest)); err == nil {
				guestID = parsed
			}
		}

		guestName := ""
		guestType := ""
		guestNode := ""
		if guestID > 0 {
			if vm, ok := vmByID[guestID]; ok {
				guestName = vm.Name
				guestType = vm.Type
				guestNode = vm.Node
			}
		}
		if guestNode == "" {
			guestNode = strings.TrimSpace(job.Source)
		}

		sourceNode := strings.TrimSpace(job.Source)
		if sourceNode == "" {
			sourceNode = guestNode
		}

		targetNode := strings.TrimSpace(job.Target)

		var lastSyncTime *time.Time
		if job.LastSyncTime != nil && !job.LastSyncTime.IsZero() {
			t := job.LastSyncTime.UTC()
			lastSyncTime = &t
		}

		var nextSyncTime *time.Time
		if job.NextSyncTime != nil && !job.NextSyncTime.IsZero() {
			t := job.NextSyncTime.UTC()
			nextSyncTime = &t
		}

		lastSyncDurationHuman := job.LastSyncDurationHuman
		if lastSyncDurationHuman == "" && job.LastSyncDurationSeconds > 0 {
			lastSyncDurationHuman = formatSeconds(job.LastSyncDurationSeconds)
		}
		durationHuman := job.DurationHuman
		if durationHuman == "" && job.DurationSeconds > 0 {
			durationHuman = formatSeconds(job.DurationSeconds)
		}

		rateLimit := copyFloatPointer(job.RateLimitMbps)

		status := job.Status
		if status == "" {
			status = job.State
		}

		jobID := strings.TrimSpace(job.ID)
		if jobID == "" {
			if job.JobNumber > 0 && guestID > 0 {
				jobID = fmt.Sprintf("%d-%d", guestID, job.JobNumber)
			} else {
				jobID = fmt.Sprintf("job-%s-%d", instanceName, idx)
			}
		}

		uniqueID := fmt.Sprintf("%s-%s", instanceName, jobID)

		converted = append(converted, models.ReplicationJob{
			ID:                      uniqueID,
			Instance:                instanceName,
			JobID:                   jobID,
			JobNumber:               job.JobNumber,
			Guest:                   job.Guest,
			GuestID:                 guestID,
			GuestName:               guestName,
			GuestType:               guestType,
			GuestNode:               guestNode,
			SourceNode:              sourceNode,
			SourceStorage:           job.SourceStorage,
			TargetNode:              targetNode,
			TargetStorage:           job.TargetStorage,
			Schedule:                job.Schedule,
			Type:                    job.Type,
			Enabled:                 job.Enabled,
			State:                   job.State,
			Status:                  status,
			LastSyncStatus:          job.LastSyncStatus,
			LastSyncTime:            lastSyncTime,
			LastSyncUnix:            job.LastSyncUnix,
			LastSyncDurationSeconds: job.LastSyncDurationSeconds,
			LastSyncDurationHuman:   lastSyncDurationHuman,
			NextSyncTime:            nextSyncTime,
			NextSyncUnix:            job.NextSyncUnix,
			DurationSeconds:         job.DurationSeconds,
			DurationHuman:           durationHuman,
			FailCount:               job.FailCount,
			Error:                   job.Error,
			Comment:                 job.Comment,
			RemoveJob:               job.RemoveJob,
			RateLimitMbps:           rateLimit,
			LastPolled:              now,
		})
	}

	m.state.UpdateReplicationJobsForInstance(instanceName, converted)
}

func formatSeconds(total int) string {
	if total <= 0 {
		return ""
	}
	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}

func copyFloatPointer(src *float64) *float64 {
	if src == nil {
		return nil
	}
	val := *src
	return &val
}

// pollPBSInstance polls a single PBS instance
func (m *Monitor) pollPBSInstance(ctx context.Context, instanceName string, client *pbs.Client) {
	start := time.Now()
	debugEnabled := logging.IsLevelEnabled(zerolog.DebugLevel)
	var pollErr error
	if m.pollMetrics != nil {
		m.pollMetrics.IncInFlight("pbs")
		defer m.pollMetrics.DecInFlight("pbs")
		defer func() {
			m.pollMetrics.RecordResult(PollResult{
				InstanceName: instanceName,
				InstanceType: "pbs",
				Success:      pollErr == nil,
				Error:        pollErr,
				StartTime:    start,
				EndTime:      time.Now(),
			})
		}()
	}
	if m.stalenessTracker != nil {
		defer func() {
			if pollErr == nil {
				m.stalenessTracker.UpdateSuccess(InstanceTypePBS, instanceName, nil)
			} else {
				m.stalenessTracker.UpdateError(InstanceTypePBS, instanceName)
			}
		}()
	}
	defer m.recordTaskResult(InstanceTypePBS, instanceName, pollErr)

	// Check if context is cancelled
	select {
	case <-ctx.Done():
		pollErr = ctx.Err()
		if debugEnabled {
			log.Debug().Str("instance", instanceName).Msg("Polling cancelled")
		}
		return
	default:
	}

	if debugEnabled {
		log.Debug().Str("instance", instanceName).Msg("Polling PBS instance")
	}

	// Get instance config
	var instanceCfg *config.PBSInstance
	for _, cfg := range m.config.PBSInstances {
		if cfg.Name == instanceName {
			instanceCfg = &cfg
			if debugEnabled {
				log.Debug().
					Str("instance", instanceName).
					Bool("monitorDatastores", cfg.MonitorDatastores).
					Msg("Found PBS instance config")
			}
			break
		}
	}
	if instanceCfg == nil {
		log.Error().Str("instance", instanceName).Msg("PBS instance config not found")
		return
	}

	// Initialize PBS instance with default values
	pbsInst := models.PBSInstance{
		ID:               "pbs-" + instanceName,
		Name:             instanceName,
		Host:             instanceCfg.Host,
		Status:           "offline",
		Version:          "unknown",
		ConnectionHealth: "unhealthy",
		LastSeen:         time.Now(),
	}

	// Try to get version first
	version, versionErr := client.GetVersion(ctx)
	if versionErr == nil {
		pbsInst.Status = "online"
		pbsInst.Version = version.Version
		pbsInst.ConnectionHealth = "healthy"
		m.resetAuthFailures(instanceName, "pbs")
		m.state.SetConnectionHealth("pbs-"+instanceName, true)

		if debugEnabled {
			log.Debug().
				Str("instance", instanceName).
				Str("version", version.Version).
				Bool("monitorDatastores", instanceCfg.MonitorDatastores).
				Msg("PBS version retrieved successfully")
		}
	} else {
		if debugEnabled {
			log.Debug().Err(versionErr).Str("instance", instanceName).Msg("Failed to get PBS version, trying fallback")
		}

		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		_, datastoreErr := client.GetDatastores(ctx2)
		if datastoreErr == nil {
			pbsInst.Status = "online"
			pbsInst.Version = "connected"
			pbsInst.ConnectionHealth = "healthy"
			m.resetAuthFailures(instanceName, "pbs")
			m.state.SetConnectionHealth("pbs-"+instanceName, true)

			log.Info().
				Str("instance", instanceName).
				Msg("PBS connected (version unavailable but datastores accessible)")
		} else {
			pbsInst.Status = "offline"
			pbsInst.ConnectionHealth = "error"
			monErr := errors.WrapConnectionError("get_pbs_version", instanceName, versionErr)
			log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to connect to PBS")
			m.state.SetConnectionHealth("pbs-"+instanceName, false)

			if errors.IsAuthError(versionErr) || errors.IsAuthError(datastoreErr) {
				m.recordAuthFailure(instanceName, "pbs")
				return
			}
		}
	}

	// Get node status (CPU, memory, etc.)
	nodeStatus, err := client.GetNodeStatus(ctx)
	if err != nil {
		if debugEnabled {
			log.Debug().Err(err).Str("instance", instanceName).Msg("Could not get PBS node status (may need Sys.Audit permission)")
		}
	} else if nodeStatus != nil {
		pbsInst.CPU = nodeStatus.CPU
		if nodeStatus.Memory.Total > 0 {
			pbsInst.Memory = float64(nodeStatus.Memory.Used) / float64(nodeStatus.Memory.Total) * 100
			pbsInst.MemoryUsed = nodeStatus.Memory.Used
			pbsInst.MemoryTotal = nodeStatus.Memory.Total
		}
		pbsInst.Uptime = nodeStatus.Uptime

		log.Debug().
			Str("instance", instanceName).
			Float64("cpu", pbsInst.CPU).
			Float64("memory", pbsInst.Memory).
			Int64("uptime", pbsInst.Uptime).
			Msg("PBS node status retrieved")
	}

	// Poll datastores if enabled
	if instanceCfg.MonitorDatastores {
		datastores, err := client.GetDatastores(ctx)
		if err != nil {
			monErr := errors.WrapAPIError("get_datastores", instanceName, err, 0)
			log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get datastores")
		} else {
			log.Info().
				Str("instance", instanceName).
				Int("count", len(datastores)).
				Msg("Got PBS datastores")

			for _, ds := range datastores {
				total := ds.Total
				if total == 0 && ds.TotalSpace > 0 {
					total = ds.TotalSpace
				}
				used := ds.Used
				if used == 0 && ds.UsedSpace > 0 {
					used = ds.UsedSpace
				}
				avail := ds.Avail
				if avail == 0 && ds.AvailSpace > 0 {
					avail = ds.AvailSpace
				}
				if total == 0 && used > 0 && avail > 0 {
					total = used + avail
				}

				log.Debug().
					Str("store", ds.Store).
					Int64("total", total).
					Int64("used", used).
					Int64("avail", avail).
					Int64("orig_total", ds.Total).
					Int64("orig_total_space", ds.TotalSpace).
					Msg("PBS datastore details")

				modelDS := models.PBSDatastore{
					Name:                ds.Store,
					Total:               total,
					Used:                used,
					Free:                avail,
					Usage:               safePercentage(float64(used), float64(total)),
					Status:              "available",
					DeduplicationFactor: ds.DeduplicationFactor,
				}

				namespaces, err := client.ListNamespaces(ctx, ds.Store, "", 0)
				if err != nil {
					log.Warn().Err(err).
						Str("instance", instanceName).
						Str("datastore", ds.Store).
						Msg("Failed to list namespaces")
				} else {
					for _, ns := range namespaces {
						nsPath := ns.NS
						if nsPath == "" {
							nsPath = ns.Path
						}
						if nsPath == "" {
							nsPath = ns.Name
						}

						modelNS := models.PBSNamespace{
							Path:   nsPath,
							Parent: ns.Parent,
							Depth:  strings.Count(nsPath, "/"),
						}
						modelDS.Namespaces = append(modelDS.Namespaces, modelNS)
					}

					hasRoot := false
					for _, ns := range modelDS.Namespaces {
						if ns.Path == "" {
							hasRoot = true
							break
						}
					}
					if !hasRoot {
						modelDS.Namespaces = append([]models.PBSNamespace{{Path: "", Depth: 0}}, modelDS.Namespaces...)
					}
				}

				pbsInst.Datastores = append(pbsInst.Datastores, modelDS)
			}
		}
	}

	// Update state and run alerts
	m.state.UpdatePBSInstance(pbsInst)
	log.Info().
		Str("instance", instanceName).
		Str("id", pbsInst.ID).
		Int("datastores", len(pbsInst.Datastores)).
		Msg("PBS instance updated in state")

	if m.alertManager != nil {
		m.alertManager.CheckPBS(pbsInst)
	}

	// Poll backups if enabled
	if instanceCfg.MonitorBackups {
		if len(pbsInst.Datastores) == 0 {
			log.Debug().
				Str("instance", instanceName).
				Msg("No PBS datastores available for backup polling")
		} else if !m.config.EnableBackupPolling {
			log.Debug().
				Str("instance", instanceName).
				Msg("Skipping PBS backup polling - globally disabled")
		} else {
			now := time.Now()

			m.mu.RLock()
			lastPoll := m.lastPBSBackupPoll[instanceName]
			inProgress := m.pbsBackupPollers[instanceName]
			m.mu.RUnlock()

			shouldPoll, reason, newLast := m.shouldRunBackupPoll(lastPoll, now)
			if !shouldPoll {
				if reason != "" {
					log.Debug().
						Str("instance", instanceName).
						Str("reason", reason).
						Msg("Skipping PBS backup polling this cycle")
				}
			} else if inProgress {
				log.Debug().
					Str("instance", instanceName).
					Msg("PBS backup polling already in progress")
			} else {
				datastoreSnapshot := make([]models.PBSDatastore, len(pbsInst.Datastores))
				copy(datastoreSnapshot, pbsInst.Datastores)

				m.mu.Lock()
				if m.pbsBackupPollers == nil {
					m.pbsBackupPollers = make(map[string]bool)
				}
				if m.pbsBackupPollers[instanceName] {
					m.mu.Unlock()
				} else {
					m.pbsBackupPollers[instanceName] = true
					m.lastPBSBackupPoll[instanceName] = newLast
					m.mu.Unlock()

					go func(ds []models.PBSDatastore, inst string, start time.Time, pbsClient *pbs.Client) {
						defer func() {
							m.mu.Lock()
							delete(m.pbsBackupPollers, inst)
							m.lastPBSBackupPoll[inst] = time.Now()
							m.mu.Unlock()
						}()

						log.Info().
							Str("instance", inst).
							Int("datastores", len(ds)).
							Msg("Starting background PBS backup polling")

						backupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancel()

						m.pollPBSBackups(backupCtx, inst, pbsClient, ds)

						log.Info().
							Str("instance", inst).
							Dur("duration", time.Since(start)).
							Msg("Completed background PBS backup polling")
					}(datastoreSnapshot, instanceName, now, client)
				}
			}
		}
	} else {
		log.Debug().
			Str("instance", instanceName).
			Msg("PBS backup monitoring disabled")
	}
}

// pollPMGInstance polls a single Proxmox Mail Gateway instance
func (m *Monitor) pollPMGInstance(ctx context.Context, instanceName string, client *pmg.Client) {
	start := time.Now()
	debugEnabled := logging.IsLevelEnabled(zerolog.DebugLevel)
	var pollErr error
	if m.pollMetrics != nil {
		m.pollMetrics.IncInFlight("pmg")
		defer m.pollMetrics.DecInFlight("pmg")
		defer func() {
			m.pollMetrics.RecordResult(PollResult{
				InstanceName: instanceName,
				InstanceType: "pmg",
				Success:      pollErr == nil,
				Error:        pollErr,
				StartTime:    start,
				EndTime:      time.Now(),
			})
		}()
	}
	if m.stalenessTracker != nil {
		defer func() {
			if pollErr == nil {
				m.stalenessTracker.UpdateSuccess(InstanceTypePMG, instanceName, nil)
			} else {
				m.stalenessTracker.UpdateError(InstanceTypePMG, instanceName)
			}
		}()
	}
	defer m.recordTaskResult(InstanceTypePMG, instanceName, pollErr)

	select {
	case <-ctx.Done():
		pollErr = ctx.Err()
		if debugEnabled {
			log.Debug().Str("instance", instanceName).Msg("PMG polling cancelled by context")
		}
		return
	default:
	}

	if debugEnabled {
		log.Debug().Str("instance", instanceName).Msg("Polling PMG instance")
	}

	var instanceCfg *config.PMGInstance
	for idx := range m.config.PMGInstances {
		if m.config.PMGInstances[idx].Name == instanceName {
			instanceCfg = &m.config.PMGInstances[idx]
			break
		}
	}

	if instanceCfg == nil {
		log.Error().Str("instance", instanceName).Msg("PMG instance config not found")
		pollErr = fmt.Errorf("pmg instance config not found for %s", instanceName)
		return
	}

	now := time.Now()
	pmgInst := models.PMGInstance{
		ID:               "pmg-" + instanceName,
		Name:             instanceName,
		Host:             instanceCfg.Host,
		Status:           "offline",
		ConnectionHealth: "unhealthy",
		LastSeen:         now,
		LastUpdated:      now,
	}

	version, err := client.GetVersion(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("pmg_get_version", instanceName, err)
		pollErr = monErr
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to connect to PMG instance")
		m.state.SetConnectionHealth("pmg-"+instanceName, false)
		m.state.UpdatePMGInstance(pmgInst)

		// Check PMG offline status against alert thresholds
		if m.alertManager != nil {
			m.alertManager.CheckPMG(pmgInst)
		}

		if errors.IsAuthError(err) {
			m.recordAuthFailure(instanceName, "pmg")
		}
		return
	}

	pmgInst.Status = "online"
	pmgInst.ConnectionHealth = "healthy"
	if version != nil {
		pmgInst.Version = strings.TrimSpace(version.Version)
	}
	m.state.SetConnectionHealth("pmg-"+instanceName, true)
	m.resetAuthFailures(instanceName, "pmg")

	cluster, err := client.GetClusterStatus(ctx, true)
	if err != nil {
		if debugEnabled {
			log.Debug().Err(err).Str("instance", instanceName).Msg("Failed to retrieve PMG cluster status")
		}
	}

	backupNodes := make(map[string]struct{})

	if len(cluster) > 0 {
		nodes := make([]models.PMGNodeStatus, 0, len(cluster))
		for _, entry := range cluster {
			status := strings.ToLower(strings.TrimSpace(entry.Type))
			if status == "" {
				status = "online"
			}
			node := models.PMGNodeStatus{
				Name:   entry.Name,
				Status: status,
				Role:   entry.Type,
			}

			backupNodes[entry.Name] = struct{}{}

			// Fetch queue status for this node
			if queueData, qErr := client.GetQueueStatus(ctx, entry.Name); qErr != nil {
				if debugEnabled {
					log.Debug().Err(qErr).
						Str("instance", instanceName).
						Str("node", entry.Name).
						Msg("Failed to fetch PMG queue status")
				}
			} else if queueData != nil {
				total := queueData.Active.Int64() + queueData.Deferred.Int64() + queueData.Hold.Int64() + queueData.Incoming.Int64()
				node.QueueStatus = &models.PMGQueueStatus{
					Active:    queueData.Active.Int(),
					Deferred:  queueData.Deferred.Int(),
					Hold:      queueData.Hold.Int(),
					Incoming:  queueData.Incoming.Int(),
					Total:     int(total),
					OldestAge: queueData.OldestAge.Int64(),
					UpdatedAt: time.Now(),
				}
			}

			nodes = append(nodes, node)
		}
		pmgInst.Nodes = nodes
	}

	if len(backupNodes) == 0 {
		trimmed := strings.TrimSpace(instanceName)
		if trimmed != "" {
			backupNodes[trimmed] = struct{}{}
		}
	}

	pmgBackups := make([]models.PMGBackup, 0)
	seenBackupIDs := make(map[string]struct{})

	for nodeName := range backupNodes {
		if ctx.Err() != nil {
			break
		}

		backups, backupErr := client.ListBackups(ctx, nodeName)
		if backupErr != nil {
			if debugEnabled {
				log.Debug().Err(backupErr).
					Str("instance", instanceName).
					Str("node", nodeName).
					Msg("Failed to list PMG configuration backups")
			}
			continue
		}

		for _, b := range backups {
			timestamp := b.Timestamp.Int64()
			backupTime := time.Unix(timestamp, 0)
			id := fmt.Sprintf("pmg-%s-%s-%d", instanceName, nodeName, timestamp)
			if _, exists := seenBackupIDs[id]; exists {
				continue
			}
			seenBackupIDs[id] = struct{}{}
			pmgBackups = append(pmgBackups, models.PMGBackup{
				ID:         id,
				Instance:   instanceName,
				Node:       nodeName,
				Filename:   b.Filename,
				BackupTime: backupTime,
				Size:       b.Size.Int64(),
			})
		}
	}

	if debugEnabled {
		log.Debug().
			Str("instance", instanceName).
			Int("backupCount", len(pmgBackups)).
			Msg("PMG backups polled")
	}

	if stats, err := client.GetMailStatistics(ctx, "day"); err != nil {
		log.Warn().Err(err).Str("instance", instanceName).Msg("Failed to fetch PMG mail statistics")
	} else if stats != nil {
		pmgInst.MailStats = &models.PMGMailStats{
			Timeframe:            "day",
			CountTotal:           stats.Count.Float64(),
			CountIn:              stats.CountIn.Float64(),
			CountOut:             stats.CountOut.Float64(),
			SpamIn:               stats.SpamIn.Float64(),
			SpamOut:              stats.SpamOut.Float64(),
			VirusIn:              stats.VirusIn.Float64(),
			VirusOut:             stats.VirusOut.Float64(),
			BouncesIn:            stats.BouncesIn.Float64(),
			BouncesOut:           stats.BouncesOut.Float64(),
			BytesIn:              stats.BytesIn.Float64(),
			BytesOut:             stats.BytesOut.Float64(),
			GreylistCount:        stats.GreylistCount.Float64(),
			JunkIn:               stats.JunkIn.Float64(),
			AverageProcessTimeMs: stats.AvgProcessSec.Float64() * 1000,
			RBLRejects:           stats.RBLRejects.Float64(),
			PregreetRejects:      stats.Pregreet.Float64(),
			UpdatedAt:            time.Now(),
		}
	}

	if counts, err := client.GetMailCount(ctx, 24); err != nil {
		if debugEnabled {
			log.Debug().Err(err).Str("instance", instanceName).Msg("Failed to fetch PMG mail count data")
		}
	} else if len(counts) > 0 {
		points := make([]models.PMGMailCountPoint, 0, len(counts))
		for _, entry := range counts {
			ts := time.Unix(entry.Time.Int64(), 0)
			points = append(points, models.PMGMailCountPoint{
				Timestamp:   ts,
				Count:       entry.Count.Float64(),
				CountIn:     entry.CountIn.Float64(),
				CountOut:    entry.CountOut.Float64(),
				SpamIn:      entry.SpamIn.Float64(),
				SpamOut:     entry.SpamOut.Float64(),
				VirusIn:     entry.VirusIn.Float64(),
				VirusOut:    entry.VirusOut.Float64(),
				RBLRejects:  entry.RBLRejects.Float64(),
				Pregreet:    entry.PregreetReject.Float64(),
				BouncesIn:   entry.BouncesIn.Float64(),
				BouncesOut:  entry.BouncesOut.Float64(),
				Greylist:    entry.GreylistCount.Float64(),
				Index:       entry.Index.Int(),
				Timeframe:   "hour",
				WindowStart: ts,
			})
		}
		pmgInst.MailCount = points
	}

	if scores, err := client.GetSpamScores(ctx); err != nil {
		if debugEnabled {
			log.Debug().Err(err).Str("instance", instanceName).Msg("Failed to fetch PMG spam score distribution")
		}
	} else if len(scores) > 0 {
		buckets := make([]models.PMGSpamBucket, 0, len(scores))
		for _, bucket := range scores {
			buckets = append(buckets, models.PMGSpamBucket{
				Score: bucket.Level,
				Count: float64(bucket.Count.Int()),
			})
		}
		pmgInst.SpamDistribution = buckets
	}

	quarantine := models.PMGQuarantineTotals{}
	if spamStatus, err := client.GetQuarantineStatus(ctx, "spam"); err == nil && spamStatus != nil {
		quarantine.Spam = int(spamStatus.Count.Int64())
	}
	if virusStatus, err := client.GetQuarantineStatus(ctx, "virus"); err == nil && virusStatus != nil {
		quarantine.Virus = int(virusStatus.Count.Int64())
	}
	pmgInst.Quarantine = &quarantine

	m.state.UpdatePMGBackups(instanceName, pmgBackups)
	m.state.UpdatePMGInstance(pmgInst)
	log.Info().
		Str("instance", instanceName).
		Str("status", pmgInst.Status).
		Int("nodes", len(pmgInst.Nodes)).
		Msg("PMG instance updated in state")

	// Check PMG metrics against alert thresholds
	if m.alertManager != nil {
		m.alertManager.CheckPMG(pmgInst)
	}
}

// GetState returns the current state
func (m *Monitor) GetState() models.StateSnapshot {
	// Check if mock mode is enabled
	if mock.IsMockEnabled() {
		state := mock.GetMockState()
		if state.ActiveAlerts == nil {
			// Populate snapshot lazily if the cache hasn't been filled yet.
			mock.UpdateAlertSnapshots(m.alertManager.GetActiveAlerts(), m.alertManager.GetRecentlyResolved())
			state = mock.GetMockState()
		}
		return state
	}
	return m.state.GetSnapshot()
}

// SetMockMode switches between mock data and real infrastructure data at runtime.
func (m *Monitor) SetMockMode(enable bool) {
	current := mock.IsMockEnabled()
	if current == enable {
		log.Info().Bool("mockMode", enable).Msg("Mock mode already in desired state")
		return
	}

	if enable {
		mock.SetEnabled(true)
		m.alertManager.ClearActiveAlerts()
		m.mu.Lock()
		m.resetStateLocked()
		m.mu.Unlock()
		m.StopDiscoveryService()
		log.Info().Msg("Switched monitor to mock mode")
	} else {
		mock.SetEnabled(false)
		m.alertManager.ClearActiveAlerts()
		m.mu.Lock()
		m.resetStateLocked()
		m.mu.Unlock()
		log.Info().Msg("Switched monitor to real data mode")
	}

	m.mu.RLock()
	ctx := m.runtimeCtx
	hub := m.wsHub
	m.mu.RUnlock()

	if hub != nil {
		hub.BroadcastState(m.GetState().ToFrontend())
	}

	if !enable && ctx != nil && hub != nil {
		// Kick off an immediate poll to repopulate state with live data
		go m.poll(ctx, hub)
		if m.config.DiscoveryEnabled {
			go m.StartDiscoveryService(ctx, hub, m.config.DiscoverySubnet)
		}
	}
}

func (m *Monitor) resetStateLocked() {
	m.state = models.NewState()
	m.state.Stats = models.Stats{
		StartTime: m.startTime,
		Version:   "2.0.0-go",
	}
}

// GetStartTime returns the monitor start time
func (m *Monitor) GetStartTime() time.Time {
	return m.startTime
}

// GetDiscoveryService returns the discovery service
func (m *Monitor) GetDiscoveryService() *discovery.Service {
	return m.discoveryService
}

// StartDiscoveryService starts the discovery service if not already running
func (m *Monitor) StartDiscoveryService(ctx context.Context, wsHub *websocket.Hub, subnet string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.discoveryService != nil {
		log.Debug().Msg("Discovery service already running")
		return
	}

	if subnet == "" {
		subnet = "auto"
	}

	cfgProvider := func() config.DiscoveryConfig {
		m.mu.RLock()
		defer m.mu.RUnlock()
		if m.config == nil {
			return config.DefaultDiscoveryConfig()
		}
		return config.CloneDiscoveryConfig(m.config.Discovery)
	}

	m.discoveryService = discovery.NewService(wsHub, 5*time.Minute, subnet, cfgProvider)
	if m.discoveryService != nil {
		m.discoveryService.Start(ctx)
		log.Info().Str("subnet", subnet).Msg("Discovery service started")
	} else {
		log.Error().Msg("Failed to create discovery service")
	}
}

// StopDiscoveryService stops the discovery service if running
func (m *Monitor) StopDiscoveryService() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.discoveryService != nil {
		m.discoveryService.Stop()
		m.discoveryService = nil
		log.Info().Msg("Discovery service stopped")
	}
}

// GetGuestMetrics returns historical metrics for a guest
func (m *Monitor) GetGuestMetrics(guestID string, duration time.Duration) map[string][]MetricPoint {
	return m.metricsHistory.GetAllGuestMetrics(guestID, duration)
}

// GetNodeMetrics returns historical metrics for a node
func (m *Monitor) GetNodeMetrics(nodeID string, metricType string, duration time.Duration) []MetricPoint {
	return m.metricsHistory.GetNodeMetrics(nodeID, metricType, duration)
}

// GetStorageMetrics returns historical metrics for storage
func (m *Monitor) GetStorageMetrics(storageID string, duration time.Duration) map[string][]MetricPoint {
	return m.metricsHistory.GetAllStorageMetrics(storageID, duration)
}

// GetAlertManager returns the alert manager
func (m *Monitor) GetAlertManager() *alerts.Manager {
	return m.alertManager
}

// GetNotificationManager returns the notification manager
func (m *Monitor) GetNotificationManager() *notifications.NotificationManager {
	return m.notificationMgr
}

// GetConfigPersistence returns the config persistence manager
func (m *Monitor) GetConfigPersistence() *config.ConfigPersistence {
	return m.configPersist
}

// pollStorageBackups polls backup files from storage
// Deprecated: This function should not be called directly as it causes duplicate GetNodes calls.
// Use pollStorageBackupsWithNodes instead.
func (m *Monitor) pollStorageBackups(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Warn().Str("instance", instanceName).Msg("pollStorageBackups called directly - this causes duplicate GetNodes calls and syslog spam on non-clustered nodes")

	// Get all nodes
	nodes, err := client.GetNodes(ctx)
	if err != nil {
		monErr := errors.WrapConnectionError("get_nodes_for_backups", instanceName, err)
		log.Error().Err(monErr).Str("instance", instanceName).Msg("Failed to get nodes for backup polling")
		return
	}

	m.pollStorageBackupsWithNodes(ctx, instanceName, client, nodes)
}

// pollStorageBackupsWithNodes polls backups using a provided nodes list to avoid duplicate GetNodes calls
func (m *Monitor) pollStorageBackupsWithNodes(ctx context.Context, instanceName string, client PVEClientInterface, nodes []proxmox.Node) {

	var allBackups []models.StorageBackup
	seenVolids := make(map[string]bool) // Track seen volume IDs to avoid duplicates
	hadSuccessfulNode := false          // Track if at least one node responded successfully
	storagesWithBackup := 0             // Number of storages that should contain backups
	contentSuccess := 0                 // Number of successful storage content fetches
	contentFailures := 0                // Number of failed storage content fetches
	storageQueryErrors := 0             // Number of nodes where storage list could not be queried

	// For each node, get storage and check content
	for _, node := range nodes {
		if node.Status != "online" {
			continue
		}

		// Get storage for this node - retry once on timeout
		var storages []proxmox.Storage
		var err error

		for attempt := 1; attempt <= 2; attempt++ {
			storages, err = client.GetStorage(ctx, node.Node)
			if err == nil {
				break // Success
			}

			// Check if it's a timeout error
			if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
				if attempt == 1 {
					log.Warn().
						Str("node", node.Node).
						Str("instance", instanceName).
						Msg("Storage query timed out, retrying with extended timeout...")
					// Give it a bit more time on retry
					time.Sleep(2 * time.Second)
					continue
				}
			}
			// Non-timeout error or second attempt failed
			break
		}

		if err != nil {
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_storage_for_backups", instanceName, err).WithNode(node.Node)
			log.Warn().Err(monErr).Str("node", node.Node).Msg("Failed to get storage for backups - skipping node")
			storageQueryErrors++
			continue
		}

		hadSuccessfulNode = true

		// For each storage that can contain backups or templates
		for _, storage := range storages {
			// Check if storage supports backup content
			if !strings.Contains(storage.Content, "backup") {
				continue
			}

			storagesWithBackup++

			// Get storage content
			contents, err := client.GetStorageContent(ctx, node.Node, storage.Storage)
			if err != nil {
				monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_storage_content", instanceName, err).WithNode(node.Node)
				log.Debug().Err(monErr).
					Str("node", node.Node).
					Str("storage", storage.Storage).
					Msg("Failed to get storage content")
				contentFailures++
				continue
			}

			contentSuccess++

			// Convert to models
			for _, content := range contents {
				// Skip if we've already seen this item (shared storage duplicate)
				if seenVolids[content.Volid] {
					continue
				}
				seenVolids[content.Volid] = true

				// Skip templates and ISOs - they're not backups
				if content.Content == "vztmpl" || content.Content == "iso" {
					continue
				}

				// Determine type from content type and VMID
				backupType := "unknown"
				if content.VMID == 0 {
					backupType = "host"
				} else if strings.Contains(content.Volid, "/vm/") || strings.Contains(content.Volid, "qemu") {
					backupType = "qemu"
				} else if strings.Contains(content.Volid, "/ct/") || strings.Contains(content.Volid, "lxc") {
					backupType = "lxc"
				} else if strings.Contains(content.Format, "pbs-ct") {
					// PBS format check as fallback
					backupType = "lxc"
				} else if strings.Contains(content.Format, "pbs-vm") {
					// PBS format check as fallback
					backupType = "qemu"
				}

				// Always use the actual node name
				backupNode := node.Node
				isPBSStorage := strings.HasPrefix(storage.Storage, "pbs-") || storage.Type == "pbs"

				// Check verification status for PBS backups
				verified := false
				verificationInfo := ""
				if isPBSStorage {
					// Check if verified flag is set
					if content.Verified > 0 {
						verified = true
					}
					// Also check verification map if available
					if content.Verification != nil {
						if state, ok := content.Verification["state"].(string); ok {
							verified = (state == "ok")
							verificationInfo = state
						}
					}
				}

				backup := models.StorageBackup{
					ID:           fmt.Sprintf("%s-%s", instanceName, content.Volid),
					Storage:      storage.Storage,
					Node:         backupNode,
					Instance:     instanceName,
					Type:         backupType,
					VMID:         content.VMID,
					Time:         time.Unix(content.CTime, 0),
					CTime:        content.CTime,
					Size:         int64(content.Size),
					Format:       content.Format,
					Notes:        content.Notes,
					Protected:    content.Protected > 0,
					Volid:        content.Volid,
					IsPBS:        isPBSStorage,
					Verified:     verified,
					Verification: verificationInfo,
				}

				allBackups = append(allBackups, backup)
			}
		}
	}

	// Decide whether to keep existing backups when every query failed
	if shouldPreserveBackups(len(nodes), hadSuccessfulNode, storagesWithBackup, contentSuccess) {
		if len(nodes) > 0 && !hadSuccessfulNode {
			log.Warn().
				Str("instance", instanceName).
				Int("nodes", len(nodes)).
				Int("errors", storageQueryErrors).
				Msg("Failed to query storage on all nodes; keeping previous backup list")
		} else if storagesWithBackup > 0 && contentSuccess == 0 {
			log.Warn().
				Str("instance", instanceName).
				Int("storages", storagesWithBackup).
				Int("failures", contentFailures).
				Msg("All storage content queries failed; keeping previous backup list")
		}
		return
	}

	// Update state with storage backups for this instance
	m.state.UpdateStorageBackupsForInstance(instanceName, allBackups)

	if m.alertManager != nil {
		snapshot := m.state.GetSnapshot()
		guestsByKey, guestsByVMID := buildGuestLookups(snapshot)
		pveStorage := snapshot.Backups.PVE.StorageBackups
		if len(pveStorage) == 0 && len(snapshot.PVEBackups.StorageBackups) > 0 {
			pveStorage = snapshot.PVEBackups.StorageBackups
		}
		pbsBackups := snapshot.Backups.PBS
		if len(pbsBackups) == 0 && len(snapshot.PBSBackups) > 0 {
			pbsBackups = snapshot.PBSBackups
		}
		pmgBackups := snapshot.Backups.PMG
		if len(pmgBackups) == 0 && len(snapshot.PMGBackups) > 0 {
			pmgBackups = snapshot.PMGBackups
		}
		m.alertManager.CheckBackups(pveStorage, pbsBackups, pmgBackups, guestsByKey, guestsByVMID)
	}

	log.Debug().
		Str("instance", instanceName).
		Int("count", len(allBackups)).
		Msg("Storage backups polled")
}

func shouldPreserveBackups(nodeCount int, hadSuccessfulNode bool, storagesWithBackup, contentSuccess int) bool {
	if nodeCount > 0 && !hadSuccessfulNode {
		return true
	}
	if storagesWithBackup > 0 && contentSuccess == 0 {
		return true
	}
	return false
}

func buildGuestLookups(snapshot models.StateSnapshot) (map[string]alerts.GuestLookup, map[string]alerts.GuestLookup) {
	byKey := make(map[string]alerts.GuestLookup)
	byVMID := make(map[string]alerts.GuestLookup)

	for _, vm := range snapshot.VMs {
		info := alerts.GuestLookup{
			Name:     vm.Name,
			Instance: vm.Instance,
			Node:     vm.Node,
			Type:     vm.Type,
			VMID:     vm.VMID,
		}
		key := alerts.BuildGuestKey(vm.Instance, vm.Node, vm.VMID)
		byKey[key] = info

		vmidKey := fmt.Sprintf("%d", vm.VMID)
		if _, exists := byVMID[vmidKey]; !exists {
			byVMID[vmidKey] = info
		}
	}

	for _, ct := range snapshot.Containers {
		info := alerts.GuestLookup{
			Name:     ct.Name,
			Instance: ct.Instance,
			Node:     ct.Node,
			Type:     ct.Type,
			VMID:     int(ct.VMID),
		}
		key := alerts.BuildGuestKey(ct.Instance, ct.Node, int(ct.VMID))
		if _, exists := byKey[key]; !exists {
			byKey[key] = info
		}

		vmidKey := fmt.Sprintf("%d", ct.VMID)
		if _, exists := byVMID[vmidKey]; !exists {
			byVMID[vmidKey] = info
		}
	}

	return byKey, byVMID
}

func (m *Monitor) calculateBackupOperationTimeout(instanceName string) time.Duration {
	const (
		minTimeout      = 2 * time.Minute
		maxTimeout      = 5 * time.Minute
		timeoutPerGuest = 2 * time.Second
	)

	timeout := minTimeout
	snapshot := m.state.GetSnapshot()

	guestCount := 0
	for _, vm := range snapshot.VMs {
		if vm.Instance == instanceName && !vm.Template {
			guestCount++
		}
	}
	for _, ct := range snapshot.Containers {
		if ct.Instance == instanceName && !ct.Template {
			guestCount++
		}
	}

	if guestCount > 0 {
		dynamic := time.Duration(guestCount) * timeoutPerGuest
		if dynamic > timeout {
			timeout = dynamic
		}
	}

	if timeout > maxTimeout {
		return maxTimeout
	}

	return timeout
}

// pollGuestSnapshots polls snapshots for all VMs and containers
func (m *Monitor) pollGuestSnapshots(ctx context.Context, instanceName string, client PVEClientInterface) {
	log.Debug().Str("instance", instanceName).Msg("Polling guest snapshots")

	// Get current VMs and containers from state for this instance
	m.mu.RLock()
	var vms []models.VM
	for _, vm := range m.state.VMs {
		if vm.Instance == instanceName {
			vms = append(vms, vm)
		}
	}
	var containers []models.Container
	for _, ct := range m.state.Containers {
		if ct.Instance == instanceName {
			containers = append(containers, ct)
		}
	}
	m.mu.RUnlock()

	guestKey := func(instance, node string, vmid int) string {
		if instance == node {
			return fmt.Sprintf("%s-%d", node, vmid)
		}
		return fmt.Sprintf("%s-%s-%d", instance, node, vmid)
	}

	guestNames := make(map[string]string, len(vms)+len(containers))
	for _, vm := range vms {
		guestNames[guestKey(instanceName, vm.Node, vm.VMID)] = vm.Name
	}
	for _, ct := range containers {
		guestNames[guestKey(instanceName, ct.Node, ct.VMID)] = ct.Name
	}

	activeGuests := 0
	for _, vm := range vms {
		if !vm.Template {
			activeGuests++
		}
	}
	for _, ct := range containers {
		if !ct.Template {
			activeGuests++
		}
	}

	const (
		minSnapshotTimeout      = 60 * time.Second
		maxSnapshotTimeout      = 4 * time.Minute
		snapshotTimeoutPerGuest = 2 * time.Second
	)

	timeout := minSnapshotTimeout
	if activeGuests > 0 {
		dynamic := time.Duration(activeGuests) * snapshotTimeoutPerGuest
		if dynamic > timeout {
			timeout = dynamic
		}
	}
	if timeout > maxSnapshotTimeout {
		timeout = maxSnapshotTimeout
	}

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			log.Warn().
				Str("instance", instanceName).
				Msg("Skipping guest snapshot polling; backup context deadline exceeded")
			return
		}
		if timeout > remaining {
			timeout = remaining
		}
	}

	snapshotCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Debug().
		Str("instance", instanceName).
		Int("guestCount", activeGuests).
		Dur("timeout", timeout).
		Msg("Guest snapshot polling budget established")

	var allSnapshots []models.GuestSnapshot
	deadlineExceeded := false

	// Poll VM snapshots
	for _, vm := range vms {
		// Skip templates
		if vm.Template {
			continue
		}

		snapshots, err := client.GetVMSnapshots(snapshotCtx, vm.Node, vm.VMID)
		if err != nil {
			if snapshotCtx.Err() != nil {
				log.Warn().
					Str("instance", instanceName).
					Str("node", vm.Node).
					Int("vmid", vm.VMID).
					Err(snapshotCtx.Err()).
					Msg("Aborting guest snapshot polling due to context cancellation while fetching VM snapshots")
				deadlineExceeded = true
				break
			}
			// This is common for VMs without snapshots, so use debug level
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_vm_snapshots", instanceName, err).WithNode(vm.Node)
			log.Debug().
				Err(monErr).
				Str("node", vm.Node).
				Int("vmid", vm.VMID).
				Msg("Failed to get VM snapshots")
			continue
		}

		for _, snap := range snapshots {
			snapshot := models.GuestSnapshot{
				ID:          fmt.Sprintf("%s-%s-%d-%s", instanceName, vm.Node, vm.VMID, snap.Name),
				Name:        snap.Name,
				Node:        vm.Node,
				Instance:    instanceName,
				Type:        "qemu",
				VMID:        vm.VMID,
				Time:        time.Unix(snap.SnapTime, 0),
				Description: snap.Description,
				Parent:      snap.Parent,
				VMState:     true, // VM state support enabled
			}

			allSnapshots = append(allSnapshots, snapshot)
		}
	}

	if deadlineExceeded {
		log.Warn().
			Str("instance", instanceName).
			Msg("Guest snapshot polling timed out before completing VM collection; retaining previous snapshots")
		return
	}

	// Poll container snapshots
	for _, ct := range containers {
		// Skip templates
		if ct.Template {
			continue
		}

		snapshots, err := client.GetContainerSnapshots(snapshotCtx, ct.Node, ct.VMID)
		if err != nil {
			if snapshotCtx.Err() != nil {
				log.Warn().
					Str("instance", instanceName).
					Str("node", ct.Node).
					Int("vmid", ct.VMID).
					Err(snapshotCtx.Err()).
					Msg("Aborting guest snapshot polling due to context cancellation while fetching container snapshots")
				deadlineExceeded = true
				break
			}
			// API error 596 means snapshots not supported/available - this is expected for many containers
			errStr := err.Error()
			if strings.Contains(errStr, "596") || strings.Contains(errStr, "not available") {
				// Silently skip containers without snapshot support
				continue
			}
			// Log other errors at debug level
			monErr := errors.NewMonitorError(errors.ErrorTypeAPI, "get_container_snapshots", instanceName, err).WithNode(ct.Node)
			log.Debug().
				Err(monErr).
				Str("node", ct.Node).
				Int("vmid", ct.VMID).
				Msg("Failed to get container snapshots")
			continue
		}

		for _, snap := range snapshots {
			snapshot := models.GuestSnapshot{
				ID:          fmt.Sprintf("%s-%s-%d-%s", instanceName, ct.Node, ct.VMID, snap.Name),
				Name:        snap.Name,
				Node:        ct.Node,
				Instance:    instanceName,
				Type:        "lxc",
				VMID:        ct.VMID,
				Time:        time.Unix(snap.SnapTime, 0),
				Description: snap.Description,
				Parent:      snap.Parent,
				VMState:     false,
			}

			allSnapshots = append(allSnapshots, snapshot)
		}
	}

	if deadlineExceeded || snapshotCtx.Err() != nil {
		log.Warn().
			Str("instance", instanceName).
			Msg("Guest snapshot polling timed out before completion; retaining previous snapshots")
		return
	}

	if len(allSnapshots) > 0 {
		sizeMap := m.collectSnapshotSizes(snapshotCtx, instanceName, client, allSnapshots)
		if len(sizeMap) > 0 {
			for i := range allSnapshots {
				if size, ok := sizeMap[allSnapshots[i].ID]; ok && size > 0 {
					allSnapshots[i].SizeBytes = size
				}
			}
		}
	}

	// Update state with guest snapshots for this instance
	m.state.UpdateGuestSnapshotsForInstance(instanceName, allSnapshots)

	if m.alertManager != nil {
		m.alertManager.CheckSnapshotsForInstance(instanceName, allSnapshots, guestNames)
	}

	log.Debug().
		Str("instance", instanceName).
		Int("count", len(allSnapshots)).
		Msg("Guest snapshots polled")
}

func (m *Monitor) collectSnapshotSizes(ctx context.Context, instanceName string, client PVEClientInterface, snapshots []models.GuestSnapshot) map[string]int64 {
	sizes := make(map[string]int64, len(snapshots))
	if len(snapshots) == 0 {
		return sizes
	}

	validSnapshots := make(map[string]struct{}, len(snapshots))
	nodes := make(map[string]struct{})

	for _, snap := range snapshots {
		validSnapshots[snap.ID] = struct{}{}
		if snap.Node != "" {
			nodes[snap.Node] = struct{}{}
		}
	}

	if len(nodes) == 0 {
		return sizes
	}

	seenVolids := make(map[string]struct{})

	for nodeName := range nodes {
		if ctx.Err() != nil {
			break
		}

		storages, err := client.GetStorage(ctx, nodeName)
		if err != nil {
			log.Debug().
				Err(err).
				Str("node", nodeName).
				Str("instance", instanceName).
				Msg("Failed to get storage list for snapshot sizing")
			continue
		}

		for _, storage := range storages {
			if ctx.Err() != nil {
				break
			}

			contentTypes := strings.ToLower(storage.Content)
			if !strings.Contains(contentTypes, "images") && !strings.Contains(contentTypes, "rootdir") {
				continue
			}

			contents, err := client.GetStorageContent(ctx, nodeName, storage.Storage)
			if err != nil {
				log.Debug().
					Err(err).
					Str("node", nodeName).
					Str("storage", storage.Storage).
					Str("instance", instanceName).
					Msg("Failed to get storage content for snapshot sizing")
				continue
			}

			for _, item := range contents {
				if item.VMID <= 0 {
					continue
				}

				if _, seen := seenVolids[item.Volid]; seen {
					continue
				}

				snapName := extractSnapshotName(item.Volid)
				if snapName == "" {
					continue
				}

				key := fmt.Sprintf("%s-%s-%d-%s", instanceName, nodeName, item.VMID, snapName)
				if _, ok := validSnapshots[key]; !ok {
					continue
				}

				seenVolids[item.Volid] = struct{}{}

				size := int64(item.Size)
				if size < 0 {
					size = 0
				}

				sizes[key] += size
			}
		}
	}

	return sizes
}

func extractSnapshotName(volid string) string {
	if volid == "" {
		return ""
	}

	parts := strings.SplitN(volid, ":", 2)
	remainder := volid
	if len(parts) == 2 {
		remainder = parts[1]
	}

	if idx := strings.Index(remainder, "@"); idx >= 0 && idx+1 < len(remainder) {
		return strings.TrimSpace(remainder[idx+1:])
	}

	return ""
}

// Stop gracefully stops the monitor
func (m *Monitor) Stop() {
	log.Info().Msg("Stopping monitor")

	// Stop the alert manager to save history
	if m.alertManager != nil {
		m.alertManager.Stop()
	}

	// Stop notification manager
	if m.notificationMgr != nil {
		m.notificationMgr.Stop()
	}

	log.Info().Msg("Monitor stopped")
}

// recordAuthFailure records an authentication failure for a node
func (m *Monitor) recordAuthFailure(instanceName string, nodeType string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nodeID := instanceName
	if nodeType != "" {
		nodeID = nodeType + "-" + instanceName
	}

	// Increment failure count
	m.authFailures[nodeID]++
	m.lastAuthAttempt[nodeID] = time.Now()

	log.Warn().
		Str("node", nodeID).
		Int("failures", m.authFailures[nodeID]).
		Msg("Authentication failure recorded")

	// If we've exceeded the threshold, remove the node
	const maxAuthFailures = 5
	if m.authFailures[nodeID] >= maxAuthFailures {
		log.Error().
			Str("node", nodeID).
			Int("failures", m.authFailures[nodeID]).
			Msg("Maximum authentication failures reached, removing node from state")

		// Remove from state based on type
		if nodeType == "pve" {
			m.removeFailedPVENode(instanceName)
		} else if nodeType == "pbs" {
			m.removeFailedPBSNode(instanceName)
		} else if nodeType == "pmg" {
			m.removeFailedPMGInstance(instanceName)
		}

		// Reset the counter since we've removed the node
		delete(m.authFailures, nodeID)
		delete(m.lastAuthAttempt, nodeID)
	}
}

// resetAuthFailures resets the failure count for a node after successful auth
func (m *Monitor) resetAuthFailures(instanceName string, nodeType string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nodeID := instanceName
	if nodeType != "" {
		nodeID = nodeType + "-" + instanceName
	}

	if count, exists := m.authFailures[nodeID]; exists && count > 0 {
		log.Info().
			Str("node", nodeID).
			Int("previousFailures", count).
			Msg("Authentication succeeded, resetting failure count")

		delete(m.authFailures, nodeID)
		delete(m.lastAuthAttempt, nodeID)
	}
}

// removeFailedPVENode updates a PVE node to show failed authentication status
func (m *Monitor) removeFailedPVENode(instanceName string) {
	// Get instance config to get host URL
	var hostURL string
	for _, cfg := range m.config.PVEInstances {
		if cfg.Name == instanceName {
			hostURL = cfg.Host
			break
		}
	}

	// Create a failed node entry to show in UI with error status
	failedNode := models.Node{
		ID:               instanceName + "-failed",
		Name:             instanceName,
		DisplayName:      instanceName,
		Instance:         instanceName,
		Host:             hostURL, // Include host URL even for failed nodes
		Status:           "offline",
		Type:             "node",
		ConnectionHealth: "error",
		LastSeen:         time.Now(),
		// Set other fields to zero values to indicate no data
		CPU:    0,
		Memory: models.Memory{},
		Disk:   models.Disk{},
	}

	// Update with just the failed node
	m.state.UpdateNodesForInstance(instanceName, []models.Node{failedNode})

	// Remove all other resources associated with this instance
	m.state.UpdateVMsForInstance(instanceName, []models.VM{})
	m.state.UpdateContainersForInstance(instanceName, []models.Container{})
	m.state.UpdateStorageForInstance(instanceName, []models.Storage{})
	m.state.UpdateCephClustersForInstance(instanceName, []models.CephCluster{})
	m.state.UpdateBackupTasksForInstance(instanceName, []models.BackupTask{})
	m.state.UpdateStorageBackupsForInstance(instanceName, []models.StorageBackup{})
	m.state.UpdateGuestSnapshotsForInstance(instanceName, []models.GuestSnapshot{})

	// Set connection health to false
	m.state.SetConnectionHealth(instanceName, false)
}

// removeFailedPBSNode removes a PBS node and all its resources from state
func (m *Monitor) removeFailedPBSNode(instanceName string) {
	// Remove PBS instance by passing empty array
	currentInstances := m.state.PBSInstances
	var updatedInstances []models.PBSInstance
	for _, inst := range currentInstances {
		if inst.Name != instanceName {
			updatedInstances = append(updatedInstances, inst)
		}
	}
	m.state.UpdatePBSInstances(updatedInstances)

	// Remove PBS backups
	m.state.UpdatePBSBackups(instanceName, []models.PBSBackup{})

	// Set connection health to false
	m.state.SetConnectionHealth("pbs-"+instanceName, false)
}

// removeFailedPMGInstance removes PMG data from state when authentication fails repeatedly
func (m *Monitor) removeFailedPMGInstance(instanceName string) {
	currentInstances := m.state.PMGInstances
	updated := make([]models.PMGInstance, 0, len(currentInstances))
	for _, inst := range currentInstances {
		if inst.Name != instanceName {
			updated = append(updated, inst)
		}
	}

	m.state.UpdatePMGInstances(updated)
	m.state.UpdatePMGBackups(instanceName, nil)
	m.state.SetConnectionHealth("pmg-"+instanceName, false)
}

// pollPBSBackups fetches all backups from PBS datastores
func (m *Monitor) pollPBSBackups(ctx context.Context, instanceName string, client *pbs.Client, datastores []models.PBSDatastore) {
	log.Debug().Str("instance", instanceName).Msg("Polling PBS backups")

	var allBackups []models.PBSBackup

	// Process each datastore
	for _, ds := range datastores {
		// Get namespace paths
		namespacePaths := make([]string, 0, len(ds.Namespaces))
		for _, ns := range ds.Namespaces {
			namespacePaths = append(namespacePaths, ns.Path)
		}

		log.Info().
			Str("instance", instanceName).
			Str("datastore", ds.Name).
			Int("namespaces", len(namespacePaths)).
			Strs("namespace_paths", namespacePaths).
			Msg("Processing datastore namespaces")

		// Fetch backups from all namespaces concurrently
		backupsMap, err := client.ListAllBackups(ctx, ds.Name, namespacePaths)
		if err != nil {
			log.Error().Err(err).
				Str("instance", instanceName).
				Str("datastore", ds.Name).
				Msg("Failed to fetch PBS backups")
			continue
		}

		// Convert PBS backups to model backups
		for namespace, snapshots := range backupsMap {
			for _, snapshot := range snapshots {
				backupTime := time.Unix(snapshot.BackupTime, 0)

				// Generate unique ID
				id := fmt.Sprintf("pbs-%s-%s-%s-%s-%s-%d",
					instanceName, ds.Name, namespace,
					snapshot.BackupType, snapshot.BackupID,
					snapshot.BackupTime)

				// Extract file names from files (which can be strings or objects)
				var fileNames []string
				for _, file := range snapshot.Files {
					switch f := file.(type) {
					case string:
						fileNames = append(fileNames, f)
					case map[string]interface{}:
						if filename, ok := f["filename"].(string); ok {
							fileNames = append(fileNames, filename)
						}
					}
				}

				// Extract verification status
				verified := false
				if snapshot.Verification != nil {
					switch v := snapshot.Verification.(type) {
					case string:
						verified = v == "ok"
					case map[string]interface{}:
						if state, ok := v["state"].(string); ok {
							verified = state == "ok"
						}
					}

					// Debug log verification data
					log.Debug().
						Str("vmid", snapshot.BackupID).
						Int64("time", snapshot.BackupTime).
						Interface("verification", snapshot.Verification).
						Bool("verified", verified).
						Msg("PBS backup verification status")
				}

				backup := models.PBSBackup{
					ID:         id,
					Instance:   instanceName,
					Datastore:  ds.Name,
					Namespace:  namespace,
					BackupType: snapshot.BackupType,
					VMID:       snapshot.BackupID,
					BackupTime: backupTime,
					Size:       snapshot.Size,
					Protected:  snapshot.Protected,
					Verified:   verified,
					Comment:    snapshot.Comment,
					Files:      fileNames,
					Owner:      snapshot.Owner,
				}

				allBackups = append(allBackups, backup)
			}
		}
	}

	log.Info().
		Str("instance", instanceName).
		Int("count", len(allBackups)).
		Msg("PBS backups fetched")

	// Update state
	m.state.UpdatePBSBackups(instanceName, allBackups)

	if m.alertManager != nil {
		snapshot := m.state.GetSnapshot()
		guestsByKey, guestsByVMID := buildGuestLookups(snapshot)
		pveStorage := snapshot.Backups.PVE.StorageBackups
		if len(pveStorage) == 0 && len(snapshot.PVEBackups.StorageBackups) > 0 {
			pveStorage = snapshot.PVEBackups.StorageBackups
		}
		pbsBackups := snapshot.Backups.PBS
		if len(pbsBackups) == 0 && len(snapshot.PBSBackups) > 0 {
			pbsBackups = snapshot.PBSBackups
		}
		pmgBackups := snapshot.Backups.PMG
		if len(pmgBackups) == 0 && len(snapshot.PMGBackups) > 0 {
			pmgBackups = snapshot.PMGBackups
		}
		m.alertManager.CheckBackups(pveStorage, pbsBackups, pmgBackups, guestsByKey, guestsByVMID)
	}
}

// checkMockAlerts checks alerts for mock data
func (m *Monitor) checkMockAlerts() {
	log.Info().Bool("mockEnabled", mock.IsMockEnabled()).Msg("checkMockAlerts called")
	if !mock.IsMockEnabled() {
		log.Info().Msg("Mock mode not enabled, skipping mock alert check")
		return
	}

	// Get mock state
	state := mock.GetMockState()

	log.Info().
		Int("vms", len(state.VMs)).
		Int("containers", len(state.Containers)).
		Int("nodes", len(state.Nodes)).
		Msg("Checking alerts for mock data")

	// Clean up alerts for nodes that no longer exist
	existingNodes := make(map[string]bool)
	for _, node := range state.Nodes {
		existingNodes[node.Name] = true
		if node.Host != "" {
			existingNodes[node.Host] = true
		}
	}
	for _, pbsInst := range state.PBSInstances {
		existingNodes[pbsInst.Name] = true
		existingNodes["pbs-"+pbsInst.Name] = true
		if pbsInst.Host != "" {
			existingNodes[pbsInst.Host] = true
		}
	}
	log.Info().
		Int("trackedNodes", len(existingNodes)).
		Msg("Collecting resources for alert cleanup in mock mode")
	m.alertManager.CleanupAlertsForNodes(existingNodes)

	guestsByKey, guestsByVMID := buildGuestLookups(state)
	pveStorage := state.Backups.PVE.StorageBackups
	if len(pveStorage) == 0 && len(state.PVEBackups.StorageBackups) > 0 {
		pveStorage = state.PVEBackups.StorageBackups
	}
	pbsBackups := state.Backups.PBS
	if len(pbsBackups) == 0 && len(state.PBSBackups) > 0 {
		pbsBackups = state.PBSBackups
	}
	pmgBackups := state.Backups.PMG
	if len(pmgBackups) == 0 && len(state.PMGBackups) > 0 {
		pmgBackups = state.PMGBackups
	}
	m.alertManager.CheckBackups(pveStorage, pbsBackups, pmgBackups, guestsByKey, guestsByVMID)

	// Limit how many guests we check per cycle to prevent blocking with large datasets
	const maxGuestsPerCycle = 50
	guestsChecked := 0

	// Check alerts for VMs (up to limit)
	for _, vm := range state.VMs {
		if guestsChecked >= maxGuestsPerCycle {
			log.Debug().
				Int("checked", guestsChecked).
				Int("total", len(state.VMs)+len(state.Containers)).
				Msg("Reached guest check limit for this cycle")
			break
		}
		m.alertManager.CheckGuest(vm, "mock")
		guestsChecked++
	}

	// Check alerts for containers (if we haven't hit the limit)
	for _, container := range state.Containers {
		if guestsChecked >= maxGuestsPerCycle {
			break
		}
		m.alertManager.CheckGuest(container, "mock")
		guestsChecked++
	}

	// Check alerts for each node
	for _, node := range state.Nodes {
		m.alertManager.CheckNode(node)
	}

	// Check alerts for storage
	log.Info().Int("storageCount", len(state.Storage)).Msg("Checking storage alerts")
	for _, storage := range state.Storage {
		log.Debug().
			Str("name", storage.Name).
			Float64("usage", storage.Usage).
			Msg("Checking storage for alerts")
		m.alertManager.CheckStorage(storage)
	}

	// Check alerts for PBS instances
	log.Info().Int("pbsCount", len(state.PBSInstances)).Msg("Checking PBS alerts")
	for _, pbsInst := range state.PBSInstances {
		m.alertManager.CheckPBS(pbsInst)
	}

	// Check alerts for PMG instances
	log.Info().Int("pmgCount", len(state.PMGInstances)).Msg("Checking PMG alerts")
	for _, pmgInst := range state.PMGInstances {
		m.alertManager.CheckPMG(pmgInst)
	}

	// Cache the latest alert snapshots directly in the mock data so the API can serve
	// mock state without needing to grab the alert manager lock again.
	mock.UpdateAlertSnapshots(m.alertManager.GetActiveAlerts(), m.alertManager.GetRecentlyResolved())
}
