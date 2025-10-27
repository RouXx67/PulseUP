package alerts

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RouXx67/PulseUP/internal/models"
	"github.com/RouXx67/PulseUP/internal/utils"
	"github.com/RouXx67/PulseUP/pkg/proxmox"
	"github.com/rs/zerolog/log"
)

// AlertLevel represents the severity of an alert
type AlertLevel string

const (
	AlertLevelWarning  AlertLevel = "warning"
	AlertLevelCritical AlertLevel = "critical"
)

// ActivationState represents the alert notification activation state
type ActivationState string

const (
	ActivationPending ActivationState = "pending_review"
	ActivationActive  ActivationState = "active"
	ActivationSnoozed ActivationState = "snoozed"
)

func normalizePoweredOffSeverity(level AlertLevel) AlertLevel {
	switch strings.ToLower(string(level)) {
	case string(AlertLevelCritical):
		return AlertLevelCritical
	default:
		return AlertLevelWarning
	}
}

// Alert represents an active alert
type Alert struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"` // cpu, memory, disk, etc.
	Level        AlertLevel             `json:"level"`
	ResourceID   string                 `json:"resourceId"` // guest or node ID
	ResourceName string                 `json:"resourceName"`
	Node         string                 `json:"node"`
	Instance     string                 `json:"instance"`
	Message      string                 `json:"message"`
	Value        float64                `json:"value"`
	Threshold    float64                `json:"threshold"`
	StartTime    time.Time              `json:"startTime"`
	LastSeen     time.Time              `json:"lastSeen"`
	Acknowledged bool                   `json:"acknowledged"`
	AckTime      *time.Time             `json:"ackTime,omitempty"`
	AckUser      string                 `json:"ackUser,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	// Notification tracking
	LastNotified *time.Time `json:"lastNotified,omitempty"` // Last time notification was sent
	// Escalation tracking
	LastEscalation  int         `json:"lastEscalation,omitempty"`  // Last escalation level notified
	EscalationTimes []time.Time `json:"escalationTimes,omitempty"` // Times when escalations were sent
}

// Clone returns a deep copy of the alert so it can be safely shared across goroutines.
func (a *Alert) Clone() *Alert {
	if a == nil {
		return nil
	}

	clone := *a

	if a.AckTime != nil {
		t := *a.AckTime
		clone.AckTime = &t
	}

	if a.LastNotified != nil {
		t := *a.LastNotified
		clone.LastNotified = &t
	}

	if len(a.EscalationTimes) > 0 {
		clone.EscalationTimes = append([]time.Time(nil), a.EscalationTimes...)
	}

	if a.Metadata != nil {
		clone.Metadata = cloneMetadata(a.Metadata)
	}

	return &clone
}

func cloneMetadata(src map[string]interface{}) map[string]interface{} {
	if src == nil {
		return nil
	}

	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = cloneMetadataValue(v)
	}
	return dst
}

func cloneMetadataValue(val interface{}) interface{} {
	switch v := val.(type) {
	case map[string]interface{}:
		return cloneMetadata(v)
	case map[string]string:
		m := make(map[string]interface{}, len(v))
		for key, value := range v {
			m[key] = value
		}
		return m
	case []interface{}:
		arr := make([]interface{}, len(v))
		for i, elem := range v {
			arr[i] = cloneMetadataValue(elem)
		}
		return arr
	case []string:
		arr := make([]string, len(v))
		copy(arr, v)
		return arr
	case []int:
		arr := make([]int, len(v))
		copy(arr, v)
		return arr
	case []float64:
		arr := make([]float64, len(v))
		copy(arr, v)
		return arr
	default:
		return v
	}
}

// ResolvedAlert represents a recently resolved alert
type ResolvedAlert struct {
	*Alert
	ResolvedTime time.Time `json:"resolvedTime"`
}

// HysteresisThreshold represents a threshold with hysteresis
type HysteresisThreshold struct {
	Trigger float64 `json:"trigger"` // Threshold to trigger alert
	Clear   float64 `json:"clear"`   // Threshold to clear alert
}

// ThresholdConfig represents threshold configuration
type ThresholdConfig struct {
	Disabled            bool                 `json:"disabled,omitempty"`            // Completely disable alerts for this guest
	DisableConnectivity bool                 `json:"disableConnectivity,omitempty"` // Disable node offline/connectivity/powered-off alerts
	PoweredOffSeverity  AlertLevel           `json:"poweredOffSeverity,omitempty"`  // Severity for powered-off alerts
	CPU                 *HysteresisThreshold `json:"cpu,omitempty"`
	Memory              *HysteresisThreshold `json:"memory,omitempty"`
	Disk                *HysteresisThreshold `json:"disk,omitempty"`
	DiskRead            *HysteresisThreshold `json:"diskRead,omitempty"`
	DiskWrite           *HysteresisThreshold `json:"diskWrite,omitempty"`
	NetworkIn           *HysteresisThreshold `json:"networkIn,omitempty"`
	NetworkOut          *HysteresisThreshold `json:"networkOut,omitempty"`
	Usage               *HysteresisThreshold `json:"usage,omitempty"`       // For storage devices
	Temperature         *HysteresisThreshold `json:"temperature,omitempty"` // For node CPU temperature
	// Legacy fields for backward compatibility
	CPULegacy        *float64 `json:"cpuLegacy,omitempty"`
	MemoryLegacy     *float64 `json:"memoryLegacy,omitempty"`
	DiskLegacy       *float64 `json:"diskLegacy,omitempty"`
	DiskReadLegacy   *float64 `json:"diskReadLegacy,omitempty"`
	DiskWriteLegacy  *float64 `json:"diskWriteLegacy,omitempty"`
	NetworkInLegacy  *float64 `json:"networkInLegacy,omitempty"`
	NetworkOutLegacy *float64 `json:"networkOutLegacy,omitempty"`
}

// QuietHours represents quiet hours configuration
type QuietHours struct {
	Enabled  bool                  `json:"enabled"`
	Start    string                `json:"start"` // 24-hour format "HH:MM"
	End      string                `json:"end"`   // 24-hour format "HH:MM"
	Timezone string                `json:"timezone"`
	Days     map[string]bool       `json:"days"` // monday, tuesday, etc.
	Suppress QuietHoursSuppression `json:"suppress"`
}

// QuietHoursSuppression controls which alert categories are silenced during quiet hours.
type QuietHoursSuppression struct {
	Performance bool `json:"performance"`
	Storage     bool `json:"storage"`
	Offline     bool `json:"offline"`
}

// EscalationLevel represents an escalation rule
type EscalationLevel struct {
	After  int    `json:"after"`  // minutes after initial alert
	Notify string `json:"notify"` // "email", "webhook", or "all"
}

// EscalationConfig represents alert escalation configuration
type EscalationConfig struct {
	Enabled bool              `json:"enabled"`
	Levels  []EscalationLevel `json:"levels"`
}

// GroupingConfig represents alert grouping configuration
type GroupingConfig struct {
	Enabled bool `json:"enabled"`
	Window  int  `json:"window"`  // seconds
	ByNode  bool `json:"byNode"`  // Group alerts by node
	ByGuest bool `json:"byGuest"` // Group alerts by guest type
}

// ScheduleConfig represents alerting schedule configuration
type ScheduleConfig struct {
	QuietHours     QuietHours       `json:"quietHours"`
	Cooldown       int              `json:"cooldown"`       // minutes
	GroupingWindow int              `json:"groupingWindow"` // seconds (deprecated, use Grouping.Window)
	MaxAlertsHour  int              `json:"maxAlertsHour"`  // max alerts per hour per resource
	Escalation     EscalationConfig `json:"escalation"`
	Grouping       GroupingConfig   `json:"grouping"`
}

// FilterCondition represents a single filter condition
type FilterCondition struct {
	Type     string      `json:"type"` // "metric", "text", or "raw"
	Field    string      `json:"field,omitempty"`
	Operator string      `json:"operator,omitempty"`
	Value    interface{} `json:"value,omitempty"`
	RawText  string      `json:"rawText,omitempty"`
}

// FilterStack represents a collection of filters with logical operator
type FilterStack struct {
	Filters         []FilterCondition `json:"filters"`
	LogicalOperator string            `json:"logicalOperator"` // "AND" or "OR"
}

// CustomAlertRule represents a custom alert rule with filter conditions
type CustomAlertRule struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Description      string          `json:"description,omitempty"`
	FilterConditions FilterStack     `json:"filterConditions"`
	Thresholds       ThresholdConfig `json:"thresholds"`
	Priority         int             `json:"priority"`
	Enabled          bool            `json:"enabled"`
	Notifications    struct {
		Email *struct {
			Enabled    bool     `json:"enabled"`
			Recipients []string `json:"recipients"`
		} `json:"email,omitempty"`
		Webhook *struct {
			Enabled bool   `json:"enabled"`
			URL     string `json:"url"`
		} `json:"webhook,omitempty"`
	} `json:"notifications"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DockerThresholdConfig represents Docker-specific alert thresholds
type DockerThresholdConfig struct {
	CPU               HysteresisThreshold `json:"cpu"`               // CPU usage % threshold (default: 80%)
	Memory            HysteresisThreshold `json:"memory"`            // Memory usage % threshold (default: 85%)
	RestartCount      int                 `json:"restartCount"`      // Number of restarts to trigger alert (default: 3)
	RestartWindow     int                 `json:"restartWindow"`     // Time window in seconds for restart loop detection (default: 300 = 5min)
	MemoryWarnPct     int                 `json:"memoryWarnPct"`     // Memory limit % to trigger warning (default: 90)
	MemoryCriticalPct int                 `json:"memoryCriticalPct"` // Memory limit % to trigger critical (default: 95)
}

// PMGThresholdConfig represents Proxmox Mail Gateway-specific alert thresholds
type PMGThresholdConfig struct {
	QueueTotalWarning       int `json:"queueTotalWarning"`       // Total queue depth warning threshold (default: 500)
	QueueTotalCritical      int `json:"queueTotalCritical"`      // Total queue depth critical threshold (default: 1000)
	OldestMessageWarnMins   int `json:"oldestMessageWarnMins"`   // Oldest queued message age warning in minutes (default: 30)
	OldestMessageCritMins   int `json:"oldestMessageCritMins"`   // Oldest queued message age critical in minutes (default: 60)
	DeferredQueueWarn       int `json:"deferredQueueWarn"`       // Deferred queue depth warning (default: 200)
	DeferredQueueCritical   int `json:"deferredQueueCritical"`   // Deferred queue depth critical (default: 500)
	HoldQueueWarn           int `json:"holdQueueWarn"`           // Hold queue depth warning (default: 100)
	HoldQueueCritical       int `json:"holdQueueCritical"`       // Hold queue depth critical (default: 300)
	QuarantineSpamWarn      int `json:"quarantineSpamWarn"`      // Spam quarantine absolute warning (default: 2000)
	QuarantineSpamCritical  int `json:"quarantineSpamCritical"`  // Spam quarantine absolute critical (default: 5000)
	QuarantineVirusWarn     int `json:"quarantineVirusWarn"`     // Virus quarantine absolute warning (default: 2000)
	QuarantineVirusCritical int `json:"quarantineVirusCritical"` // Virus quarantine absolute critical (default: 5000)
	QuarantineGrowthWarnPct int `json:"quarantineGrowthWarnPct"` // Growth % to trigger warning (default: 25)
	QuarantineGrowthWarnMin int `json:"quarantineGrowthWarnMin"` // Minimum message growth for warning (default: 250)
	QuarantineGrowthCritPct int `json:"quarantineGrowthCritPct"` // Growth % to trigger critical (default: 50)
	QuarantineGrowthCritMin int `json:"quarantineGrowthCritMin"` // Minimum message growth for critical (default: 500)
}

// SnapshotAlertConfig represents snapshot age alert configuration
type SnapshotAlertConfig struct {
	Enabled         bool    `json:"enabled"`
	WarningDays     int     `json:"warningDays"`
	CriticalDays    int     `json:"criticalDays"`
	WarningSizeGiB  float64 `json:"warningSizeGiB,omitempty"`
	CriticalSizeGiB float64 `json:"criticalSizeGiB,omitempty"`
}

// BackupAlertConfig represents backup age alert configuration
type BackupAlertConfig struct {
	Enabled      bool `json:"enabled"`
	WarningDays  int  `json:"warningDays"`
	CriticalDays int  `json:"criticalDays"`
}

// GuestLookup describes a guest identity used for snapshot/backup evaluations.
type GuestLookup struct {
	Name     string
	Instance string
	Node     string
	Type     string
	VMID     int
}

// AlertConfig represents the complete alert configuration
type AlertConfig struct {
	Enabled                        bool                       `json:"enabled"`
	ActivationState                ActivationState            `json:"activationState,omitempty"`
	ObservationWindowHours         int                        `json:"observationWindowHours,omitempty"`
	ActivationTime                 *time.Time                 `json:"activationTime,omitempty"`
	GuestDefaults                  ThresholdConfig            `json:"guestDefaults"`
	NodeDefaults                   ThresholdConfig            `json:"nodeDefaults"`
	StorageDefault                 HysteresisThreshold        `json:"storageDefault"`
	DockerDefaults                 DockerThresholdConfig      `json:"dockerDefaults"`
	DockerIgnoredContainerPrefixes []string                   `json:"dockerIgnoredContainerPrefixes,omitempty"`
	PMGDefaults                    PMGThresholdConfig         `json:"pmgDefaults"`
	SnapshotDefaults               SnapshotAlertConfig        `json:"snapshotDefaults"`
	BackupDefaults                 BackupAlertConfig          `json:"backupDefaults"`
	Overrides                      map[string]ThresholdConfig `json:"overrides"` // keyed by resource ID
	CustomRules                    []CustomAlertRule          `json:"customRules,omitempty"`
	Schedule                       ScheduleConfig             `json:"schedule"`
	// Global disable flags per resource type
	DisableAllNodes              bool `json:"disableAllNodes"`              // Disable all alerts for Proxmox nodes
	DisableAllGuests             bool `json:"disableAllGuests"`             // Disable all alerts for VMs/containers
	DisableAllStorage            bool `json:"disableAllStorage"`            // Disable all alerts for storage
	DisableAllPBS                bool `json:"disableAllPBS"`                // Disable all alerts for PBS servers
	DisableAllPMG                bool `json:"disableAllPMG"`                // Disable all alerts for PMG instances
	DisableAllDockerHosts        bool `json:"disableAllDockerHosts"`        // Disable all alerts for Docker hosts
	DisableAllDockerContainers   bool `json:"disableAllDockerContainers"`   // Disable all alerts for Docker containers
	DisableAllNodesOffline       bool `json:"disableAllNodesOffline"`       // Disable node offline/connectivity alerts globally
	DisableAllGuestsOffline      bool `json:"disableAllGuestsOffline"`      // Disable guest powered-off alerts globally
	DisableAllPBSOffline         bool `json:"disableAllPBSOffline"`         // Disable PBS offline alerts globally
	DisableAllPMGOffline         bool `json:"disableAllPMGOffline"`         // Disable PMG offline alerts globally
	DisableAllDockerHostsOffline bool `json:"disableAllDockerHostsOffline"` // Disable Docker host offline alerts globally
	// New configuration options
	MinimumDelta         float64                   `json:"minimumDelta"`         // Minimum % change to trigger new alert
	SuppressionWindow    int                       `json:"suppressionWindow"`    // Minutes to suppress duplicate alerts
	HysteresisMargin     float64                   `json:"hysteresisMargin"`     // Default margin for legacy thresholds
	TimeThreshold        int                       `json:"timeThreshold"`        // Legacy: Seconds that threshold must be exceeded before triggering
	TimeThresholds       map[string]int            `json:"timeThresholds"`       // Per-type delays: guest, node, storage, pbs
	MetricTimeThresholds map[string]map[string]int `json:"metricTimeThresholds"` // Optional per-metric delays keyed by resource type
}

// pmgQuarantineSnapshot stores quarantine counts at a point in time for growth detection
type pmgQuarantineSnapshot struct {
	Spam      int
	Virus     int
	Timestamp time.Time
}

// pmgMailMetricSample stores a single hourly mail count sample
type pmgMailMetricSample struct {
	SpamIn    float64
	SpamOut   float64
	VirusIn   float64
	VirusOut  float64
	Timestamp time.Time
}

// pmgBaselineCache stores calculated baseline values for a metric
type pmgBaselineCache struct {
	TrimmedMean float64
	Median      float64
	LastUpdated time.Time
}

// pmgAnomalyTracker tracks history and baselines for anomaly detection
type pmgAnomalyTracker struct {
	Samples        []pmgMailMetricSample       // Ring buffer (max 48 samples)
	Baselines      map[string]pmgBaselineCache // Cached baselines per metric (spamIn, spamOut, virusIn, virusOut)
	LastSampleTime time.Time                   // Timestamp of most recent sample
	SampleCount    int                         // Total samples collected (for warmup check)
}

// Manager handles alert monitoring and state
//
// Lock Ordering Documentation:
// The Manager uses two mutexes to prevent deadlocks:
//  1. m.mu (primary lock) - protects most manager state
//  2. m.resolvedMutex - protects only recentlyResolved map
//
// Lock Ordering Rules:
//   - NEVER hold m.mu when acquiring resolvedMutex
//   - ALWAYS release m.mu before acquiring resolvedMutex
//   - resolvedMutex can be held independently without m.mu
//   - When both locks are needed, acquire m.mu first, then release it before acquiring resolvedMutex
//
// This ordering prevents deadlock scenarios where different goroutines acquire locks in different orders.
type Manager struct {
	mu             sync.RWMutex
	config         AlertConfig
	activeAlerts   map[string]*Alert
	historyManager *HistoryManager
	onAlert        func(alert *Alert)
	onResolved     func(alertID string)
	onEscalate     func(alert *Alert, level int)
	escalationStop chan struct{}
	alertRateLimit map[string][]time.Time // Track alert times for rate limiting
	// New fields for deduplication and suppression
	recentAlerts    map[string]*Alert    // Track recent alerts for deduplication
	suppressedUntil map[string]time.Time // Track suppression windows
	// Recently resolved alerts (kept for 5 minutes)
	recentlyResolved map[string]*ResolvedAlert
	resolvedMutex    sync.RWMutex // Secondary lock - see Lock Ordering Documentation above
	// Time threshold tracking
	pendingAlerts map[string]time.Time // Track when thresholds were first exceeded
	// Offline confirmation tracking
	nodeOfflineCount      map[string]int                  // Track consecutive offline counts for nodes (legacy)
	offlineConfirmations  map[string]int                  // Track consecutive offline counts for all resources
	dockerOfflineCount    map[string]int                  // Track consecutive offline counts for Docker hosts
	dockerStateConfirm    map[string]int                  // Track consecutive state confirmations for Docker containers
	dockerRestartTracking map[string]*dockerRestartRecord // Track restart counts and times for restart loop detection
	dockerLastExitCode    map[string]int                  // Track last exit code for OOM detection
	// PMG quarantine growth tracking
	pmgQuarantineHistory map[string][]pmgQuarantineSnapshot // Track quarantine snapshots for growth detection
	// PMG anomaly detection tracking
	pmgAnomalyTrackers map[string]*pmgAnomalyTracker // Track mail metrics for anomaly detection per PMG instance
	// Persistent acknowledgement state so quick alert rebuilds keep user acknowledgements
	ackState map[string]ackRecord
}

type ackRecord struct {
	acknowledged bool
	user         string
	time         time.Time
}

type dockerRestartRecord struct {
	count       int
	lastCount   int
	times       []time.Time // Track restart times for loop detection
	lastChecked time.Time
}

// NewManager creates a new alert manager
func NewManager() *Manager {
	alertsDir := filepath.Join(utils.GetDataDir(), "alerts")
	m := &Manager{
		activeAlerts:          make(map[string]*Alert),
		historyManager:        NewHistoryManager(alertsDir),
		escalationStop:        make(chan struct{}),
		alertRateLimit:        make(map[string][]time.Time),
		recentAlerts:          make(map[string]*Alert),
		suppressedUntil:       make(map[string]time.Time),
		recentlyResolved:      make(map[string]*ResolvedAlert),
		pendingAlerts:         make(map[string]time.Time),
		nodeOfflineCount:      make(map[string]int),
		offlineConfirmations:  make(map[string]int),
		dockerOfflineCount:    make(map[string]int),
		dockerStateConfirm:    make(map[string]int),
		dockerRestartTracking: make(map[string]*dockerRestartRecord),
		dockerLastExitCode:    make(map[string]int),
		pmgQuarantineHistory:  make(map[string][]pmgQuarantineSnapshot),
		pmgAnomalyTrackers:    make(map[string]*pmgAnomalyTracker),
		ackState:              make(map[string]ackRecord),
		config: AlertConfig{
			Enabled:                true,
			ActivationState:        ActivationPending,
			ObservationWindowHours: 24,
			GuestDefaults: ThresholdConfig{
				PoweredOffSeverity: AlertLevelWarning,
				CPU:                &HysteresisThreshold{Trigger: 80, Clear: 75},
				Memory:             &HysteresisThreshold{Trigger: 85, Clear: 80},
				Disk:               &HysteresisThreshold{Trigger: 90, Clear: 85},
				DiskRead:           &HysteresisThreshold{Trigger: 0, Clear: 0}, // Off by default
				DiskWrite:          &HysteresisThreshold{Trigger: 0, Clear: 0}, // Off by default
				NetworkIn:          &HysteresisThreshold{Trigger: 0, Clear: 0}, // Off by default
				NetworkOut:         &HysteresisThreshold{Trigger: 0, Clear: 0}, // Off by default
			},
			NodeDefaults: ThresholdConfig{
				CPU:         &HysteresisThreshold{Trigger: 80, Clear: 75},
				Memory:      &HysteresisThreshold{Trigger: 85, Clear: 80},
				Disk:        &HysteresisThreshold{Trigger: 90, Clear: 85},
				Temperature: &HysteresisThreshold{Trigger: 80, Clear: 75}, // Warning at 80°C, clear at 75°C
			},
			DockerDefaults: DockerThresholdConfig{
				CPU:               HysteresisThreshold{Trigger: 80, Clear: 75},
				Memory:            HysteresisThreshold{Trigger: 85, Clear: 80},
				RestartCount:      3,
				RestartWindow:     300, // 5 minutes
				MemoryWarnPct:     90,
				MemoryCriticalPct: 95,
			},
			PMGDefaults: PMGThresholdConfig{
				QueueTotalWarning:       500,  // Warning at 500 total queued messages
				QueueTotalCritical:      1000, // Critical at 1000 total queued messages
				OldestMessageWarnMins:   30,   // Warning if oldest message is 30+ minutes old
				OldestMessageCritMins:   60,   // Critical if oldest message is 60+ minutes old
				DeferredQueueWarn:       200,  // Warning at 200 deferred messages
				DeferredQueueCritical:   500,  // Critical at 500 deferred messages
				HoldQueueWarn:           100,  // Warning at 100 held messages
				HoldQueueCritical:       300,  // Critical at 300 held messages
				QuarantineSpamWarn:      2000, // Warning at 2000 spam quarantined
				QuarantineSpamCritical:  5000, // Critical at 5000 spam quarantined
				QuarantineVirusWarn:     2000, // Warning at 2000 virus quarantined
				QuarantineVirusCritical: 5000, // Critical at 5000 virus quarantined
				QuarantineGrowthWarnPct: 25,   // Warning if growth ≥25%
				QuarantineGrowthWarnMin: 250,  // AND ≥250 messages
				QuarantineGrowthCritPct: 50,   // Critical if growth ≥50%
				QuarantineGrowthCritMin: 500,  // AND ≥500 messages
			},
			SnapshotDefaults: SnapshotAlertConfig{
				Enabled:         false,
				WarningDays:     30,
				CriticalDays:    45,
				WarningSizeGiB:  0,
				CriticalSizeGiB: 0,
			},
			BackupDefaults: BackupAlertConfig{
				Enabled:      false,
				WarningDays:  7,
				CriticalDays: 14,
			},
			StorageDefault:    HysteresisThreshold{Trigger: 85, Clear: 80},
			MinimumDelta:      2.0, // 2% minimum change
			SuppressionWindow: 5,   // 5 minutes
			HysteresisMargin:  5.0, // 5% default margin
			TimeThreshold:     5,
			TimeThresholds: map[string]int{
				"guest":   5,
				"node":    5,
				"storage": 5,
				"pbs":     5,
			},
			Overrides: make(map[string]ThresholdConfig),
			Schedule: ScheduleConfig{
				QuietHours: QuietHours{
					Enabled:  false, // OFF - users should opt-in to quiet hours
					Start:    "22:00",
					End:      "08:00",
					Timezone: "America/New_York",
					Days: map[string]bool{
						"monday":    true,
						"tuesday":   true,
						"wednesday": true,
						"thursday":  true,
						"friday":    true,
						"saturday":  false,
						"sunday":    false,
					},
					Suppress: QuietHoursSuppression{},
				},
				Cooldown:       5,  // ON - 5 minutes prevents spam
				GroupingWindow: 30, // ON - 30 seconds groups related alerts
				MaxAlertsHour:  10, // ON - 10 alerts/hour prevents flooding
				Escalation: EscalationConfig{
					Enabled: false, // OFF - requires user configuration
					Levels: []EscalationLevel{
						{After: 15, Notify: "email"},
						{After: 30, Notify: "webhook"},
						{After: 60, Notify: "all"},
					},
				},
				Grouping: GroupingConfig{
					Enabled: true,  // ON - reduces notification noise
					Window:  30,    // 30 second window for grouping
					ByNode:  true,  // Group by node for mass node issues
					ByGuest: false, // Don't group by guest by default
				},
			},
		},
	}

	// Load saved active alerts
	if err := m.LoadActiveAlerts(); err != nil {
		log.Error().Err(err).Msg("Failed to load active alerts")
	}

	// Start escalation checker
	go m.escalationChecker()

	// Start periodic save of active alerts
	go m.periodicSaveAlerts()

	return m
}

// addRecentlyResolvedUnlocked records a resolved alert assuming the caller does not hold m.mu.
func (m *Manager) addRecentlyResolvedUnlocked(alertID string, resolved *ResolvedAlert) {
	m.resolvedMutex.Lock()
	m.recentlyResolved[alertID] = resolved
	m.resolvedMutex.Unlock()
}

// addRecentlyResolvedWithPrimaryLock records a resolved alert while preserving the caller's
// ownership of m.mu. Callers must hold m.mu before invoking this helper.
func (m *Manager) addRecentlyResolvedWithPrimaryLock(alertID string, resolved *ResolvedAlert) {
	m.mu.Unlock()
	m.addRecentlyResolvedUnlocked(alertID, resolved)
	m.mu.Lock()
}

// SetAlertCallback sets the callback for new alerts
func (m *Manager) SetAlertCallback(cb func(alert *Alert)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAlert = cb
}

// SetResolvedCallback sets the callback for resolved alerts
func (m *Manager) SetResolvedCallback(cb func(alertID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onResolved = cb
}

// SetEscalateCallback sets the callback for escalated alerts
func (m *Manager) SetEscalateCallback(cb func(alert *Alert, level int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEscalate = cb
}

// safeCallResolvedCallback invokes onResolved with panic recovery
func (m *Manager) safeCallResolvedCallback(alertID string, async bool) {
	if m.onResolved == nil {
		return
	}

	callbackFunc := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().
					Interface("panic", r).
					Str("alertID", alertID).
					Msg("Panic in onResolved callback")
			}
		}()
		m.onResolved(alertID)
	}

	if async {
		go callbackFunc()
	} else {
		callbackFunc()
	}
}

// safeCallEscalateCallback invokes onEscalate with panic recovery and alert cloning
func (m *Manager) safeCallEscalateCallback(alert *Alert, level int) {
	if m.onEscalate == nil {
		return
	}

	// Clone alert to prevent concurrent modification
	alertCopy := alert.Clone()
	go func(a *Alert, lvl int) {
		defer func() {
			if r := recover(); r != nil {
				log.Error().
					Interface("panic", r).
					Str("alertID", a.ID).
					Int("level", lvl).
					Msg("Panic in onEscalate callback")
			}
		}()
		m.onEscalate(a, lvl)
	}(alertCopy, level)
}

// dispatchAlert delivers an alert to the configured callback, cloning it first to
// prevent concurrent mutations from racing with consumers.
func (m *Manager) dispatchAlert(alert *Alert, async bool) bool {
	if m.onAlert == nil || alert == nil {
		return false
	}

	// Check activation state - only dispatch notifications if active
	if m.config.ActivationState != ActivationActive {
		log.Debug().
			Str("alertID", alert.ID).
			Str("activationState", string(m.config.ActivationState)).
			Msg("Alert notification suppressed - not activated")
		return false
	}

	if suppressed, reason := m.shouldSuppressNotification(alert); suppressed {
		log.Debug().
			Str("alertID", alert.ID).
			Str("type", alert.Type).
			Str("level", string(alert.Level)).
			Str("quietHoursRule", reason).
			Msg("Alert notification suppressed during quiet hours")
		return false
	}

	alertCopy := alert.Clone()
	if async {
		go func(a *Alert) {
			defer func() {
				if r := recover(); r != nil {
					log.Error().
						Interface("panic", r).
						Str("alertID", a.ID).
						Str("type", a.Type).
						Msg("Panic in onAlert callback")
				}
			}()
			m.onAlert(a)
		}(alertCopy)
	} else {
		// Synchronous calls also need panic recovery to prevent service crash
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().
						Interface("panic", r).
						Str("alertID", alertCopy.ID).
						Str("type", alertCopy.Type).
						Msg("Panic in onAlert callback (synchronous)")
				}
			}()
			m.onAlert(alertCopy)
		}()
	}
	return true
}

// ensureValidHysteresis ensures clear < trigger for hysteresis thresholds
func ensureValidHysteresis(threshold *HysteresisThreshold, metricName string) {
	if threshold == nil {
		return
	}
	if threshold.Clear >= threshold.Trigger {
		log.Warn().
			Str("metric", metricName).
			Float64("trigger", threshold.Trigger).
			Float64("clear", threshold.Clear).
			Msg("Invalid hysteresis: clear >= trigger, auto-fixing")
		// Auto-fix: set clear to 5% below trigger
		threshold.Clear = threshold.Trigger - 5
		if threshold.Clear < 0 {
			threshold.Clear = 0
		}
	}
}

// UpdateConfig updates the alert configuration
func (m *Manager) UpdateConfig(config AlertConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Preserve defaults for zero values
	if config.StorageDefault.Trigger <= 0 {
		config.StorageDefault.Trigger = 85
		config.StorageDefault.Clear = 80
	}

	// Initialize Docker defaults if missing/zero
	if config.DockerDefaults.CPU.Trigger <= 0 {
		config.DockerDefaults.CPU = HysteresisThreshold{Trigger: 80, Clear: 75}
	}
	if config.DockerDefaults.Memory.Trigger <= 0 {
		config.DockerDefaults.Memory = HysteresisThreshold{Trigger: 85, Clear: 80}
	}
	if config.DockerDefaults.RestartCount <= 0 {
		config.DockerDefaults.RestartCount = 3
	}
	if config.DockerDefaults.RestartWindow <= 0 {
		config.DockerDefaults.RestartWindow = 300 // 5 minutes
	}
	if config.DockerDefaults.MemoryWarnPct <= 0 {
		config.DockerDefaults.MemoryWarnPct = 90
	}
	if config.DockerDefaults.MemoryCriticalPct <= 0 {
		config.DockerDefaults.MemoryCriticalPct = 95
	}

	// Initialize PMG defaults if missing/zero
	if config.PMGDefaults.QueueTotalWarning <= 0 {
		config.PMGDefaults.QueueTotalWarning = 500
	}
	if config.PMGDefaults.QueueTotalCritical <= 0 {
		config.PMGDefaults.QueueTotalCritical = 1000
	}
	if config.PMGDefaults.OldestMessageWarnMins <= 0 {
		config.PMGDefaults.OldestMessageWarnMins = 30
	}
	if config.PMGDefaults.OldestMessageCritMins <= 0 {
		config.PMGDefaults.OldestMessageCritMins = 60
	}
	if config.PMGDefaults.DeferredQueueWarn <= 0 {
		config.PMGDefaults.DeferredQueueWarn = 200
	}
	if config.PMGDefaults.DeferredQueueCritical <= 0 {
		config.PMGDefaults.DeferredQueueCritical = 500
	}
	if config.PMGDefaults.HoldQueueWarn <= 0 {
		config.PMGDefaults.HoldQueueWarn = 100
	}
	if config.PMGDefaults.HoldQueueCritical <= 0 {
		config.PMGDefaults.HoldQueueCritical = 300
	}
	if config.PMGDefaults.QuarantineSpamWarn <= 0 {
		config.PMGDefaults.QuarantineSpamWarn = 2000
	}
	if config.PMGDefaults.QuarantineSpamCritical <= 0 {
		config.PMGDefaults.QuarantineSpamCritical = 5000
	}
	if config.PMGDefaults.QuarantineVirusWarn <= 0 {
		config.PMGDefaults.QuarantineVirusWarn = 2000
	}
	if config.PMGDefaults.QuarantineVirusCritical <= 0 {
		config.PMGDefaults.QuarantineVirusCritical = 5000
	}
	if config.PMGDefaults.QuarantineGrowthWarnPct <= 0 {
		config.PMGDefaults.QuarantineGrowthWarnPct = 25
	}
	if config.PMGDefaults.QuarantineGrowthWarnMin <= 0 {
		config.PMGDefaults.QuarantineGrowthWarnMin = 250
	}
	if config.PMGDefaults.QuarantineGrowthCritPct <= 0 {
		config.PMGDefaults.QuarantineGrowthCritPct = 50
	}
	if config.PMGDefaults.QuarantineGrowthCritMin <= 0 {
		config.PMGDefaults.QuarantineGrowthCritMin = 500
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

	// Ensure minimums for other important fields
	if config.MinimumDelta <= 0 {
		config.MinimumDelta = 2.0
	}
	if config.SuppressionWindow <= 0 {
		config.SuppressionWindow = 5
	}
	if config.HysteresisMargin <= 0 {
		config.HysteresisMargin = 5.0
	}

	// Ensure temperature defaults exist for nodes so high temps alert out of the box
	if config.NodeDefaults.Temperature == nil || config.NodeDefaults.Temperature.Trigger <= 0 {
		config.NodeDefaults.Temperature = &HysteresisThreshold{Trigger: 80, Clear: 75}
	} else if config.NodeDefaults.Temperature.Clear <= 0 {
		config.NodeDefaults.Temperature.Clear = config.NodeDefaults.Temperature.Trigger - 5
		if config.NodeDefaults.Temperature.Clear <= 0 {
			config.NodeDefaults.Temperature.Clear = 75
		}
	}

	// Normalize any metric-level delay overrides
	config.MetricTimeThresholds = normalizeMetricTimeThresholds(config.MetricTimeThresholds)

	const defaultDelaySeconds = 5
	if config.TimeThreshold <= 0 {
		config.TimeThreshold = defaultDelaySeconds
	}
	if config.TimeThresholds == nil {
		config.TimeThresholds = make(map[string]int)
	}
	ensureDelay := func(key string) {
		delay, ok := config.TimeThresholds[key]
		if !ok || delay < 0 {
			config.TimeThresholds[key] = defaultDelaySeconds
		}
	}
	ensureDelay("guest")
	ensureDelay("node")
	ensureDelay("storage")
	ensureDelay("pbs")
	if delay, ok := config.TimeThresholds["all"]; ok && delay < 0 {
		config.TimeThresholds["all"] = defaultDelaySeconds
	}
	config.DockerIgnoredContainerPrefixes = NormalizeDockerIgnoredPrefixes(config.DockerIgnoredContainerPrefixes)

	config.GuestDefaults.PoweredOffSeverity = normalizePoweredOffSeverity(config.GuestDefaults.PoweredOffSeverity)
	config.NodeDefaults.PoweredOffSeverity = normalizePoweredOffSeverity(config.NodeDefaults.PoweredOffSeverity)

	// Migration logic for activation state (backward compatibility)
	if config.ObservationWindowHours <= 0 {
		config.ObservationWindowHours = 24
	}
	if config.ActivationState == "" {
		// Determine if this is an existing installation or new
		// Existing installations have active alerts already
		isExistingInstall := len(m.activeAlerts) > 0 || len(config.Overrides) > 0
		if isExistingInstall {
			// Existing install: auto-activate to preserve behavior
			config.ActivationState = ActivationActive
			now := time.Now()
			config.ActivationTime = &now
			log.Info().Msg("Migrating existing installation to active alert state")
		} else {
			// New install: start in pending review
			config.ActivationState = ActivationPending
			log.Info().Msg("New installation: alerts pending activation")
		}
	}

	// Validate hysteresis thresholds to prevent stuck alerts
	ensureValidHysteresis(config.GuestDefaults.CPU, "guest.cpu")
	ensureValidHysteresis(config.GuestDefaults.Memory, "guest.memory")
	ensureValidHysteresis(config.GuestDefaults.Disk, "guest.disk")
	ensureValidHysteresis(config.NodeDefaults.CPU, "node.cpu")
	ensureValidHysteresis(config.NodeDefaults.Memory, "node.memory")
	ensureValidHysteresis(config.NodeDefaults.Temperature, "node.temperature")
	ensureValidHysteresis(&config.StorageDefault, "storage")

	// Validate timezone if quiet hours are enabled
	if config.Schedule.QuietHours.Enabled {
		if config.Schedule.QuietHours.Timezone != "" {
			_, err := time.LoadLocation(config.Schedule.QuietHours.Timezone)
			if err != nil {
				log.Error().
					Err(err).
					Str("timezone", config.Schedule.QuietHours.Timezone).
					Msg("Invalid timezone in quiet hours config, disabling quiet hours")
				// Disable quiet hours rather than silently using wrong timezone
				config.Schedule.QuietHours.Enabled = false
			}
		}
	}

	m.config = config
	for id, override := range m.config.Overrides {
		override.PoweredOffSeverity = normalizePoweredOffSeverity(override.PoweredOffSeverity)
		if override.Usage != nil {
			override.Usage = ensureHysteresisThreshold(override.Usage)
			m.config.Overrides[id] = override
		}
		m.config.Overrides[id] = override
	}

	if !m.config.SnapshotDefaults.Enabled {
		m.clearSnapshotAlertsForInstanceLocked("")
	}
	if !m.config.BackupDefaults.Enabled {
		m.clearBackupAlertsLocked()
	}

	m.applyGlobalOfflineSettingsLocked()

	log.Info().
		Bool("enabled", config.Enabled).
		Interface("guestDefaults", config.GuestDefaults).
		Msg("Alert configuration updated")

	// Re-evaluate active alerts against new thresholds
	m.reevaluateActiveAlertsLocked()
}

// normalizeMetricTimeThresholds cleans resource/metric keys and drops invalid delay overrides.
func normalizeMetricTimeThresholds(input map[string]map[string]int) map[string]map[string]int {
	if len(input) == 0 {
		return nil
	}

	normalized := make(map[string]map[string]int)
	for rawType, metrics := range input {
		typeKey := strings.ToLower(strings.TrimSpace(rawType))
		if typeKey == "" || len(metrics) == 0 {
			continue
		}
		for rawMetric, delay := range metrics {
			metricKey := strings.ToLower(strings.TrimSpace(rawMetric))
			if metricKey == "" || delay < 0 {
				continue
			}
			if _, exists := normalized[typeKey]; !exists {
				normalized[typeKey] = make(map[string]int)
			}
			normalized[typeKey][metricKey] = delay
		}
	}

	if len(normalized) == 0 {
		return nil
	}

	return normalized
}

// NormalizeMetricTimeThresholds exposes normalization for other packages (e.g., config persistence).
func NormalizeMetricTimeThresholds(input map[string]map[string]int) map[string]map[string]int {
	return normalizeMetricTimeThresholds(input)
}

// NormalizeDockerIgnoredPrefixes trims, deduplicates, and lowercases comparison keys for ignored Docker containers.
// Returned values retain the user's original casing for display but guarantee uniqueness when compared case-insensitively.
func NormalizeDockerIgnoredPrefixes(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(prefixes))
	normalized := make([]string, 0, len(prefixes))

	for _, prefix := range prefixes {
		trimmed := strings.TrimSpace(prefix)
		if trimmed == "" {
			continue
		}

		key := strings.ToLower(trimmed)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, trimmed)
	}

	if len(normalized) == 0 {
		return nil
	}

	return normalized
}

// applyGlobalOfflineSettingsLocked clears tracking and active alerts for globally disabled offline detectors.
// Caller must hold m.mu.
func (m *Manager) applyGlobalOfflineSettingsLocked() {
	if m.config.DisableAllNodesOffline {
		var nodeAlerts []string
		for alertID := range m.activeAlerts {
			if strings.HasPrefix(alertID, "node-offline-") {
				nodeAlerts = append(nodeAlerts, alertID)
			}
		}
		for _, alertID := range nodeAlerts {
			m.clearAlertNoLock(alertID)
		}
		m.nodeOfflineCount = make(map[string]int)
	}

	if m.config.DisableAllPBSOffline {
		var pbsAlerts []string
		for alertID, alert := range m.activeAlerts {
			if strings.HasPrefix(alertID, "pbs-offline-") {
				pbsAlerts = append(pbsAlerts, alertID)
				delete(m.offlineConfirmations, alert.ResourceID)
			}
		}
		for _, alertID := range pbsAlerts {
			m.clearAlertNoLock(alertID)
		}
	}

	if m.config.DisableAllGuestsOffline {
		var guestAlerts []string
		for alertID, alert := range m.activeAlerts {
			if strings.HasPrefix(alertID, "guest-powered-off-") {
				guestAlerts = append(guestAlerts, alertID)
				delete(m.offlineConfirmations, alert.ResourceID)
			}
		}
		for _, alertID := range guestAlerts {
			m.clearAlertNoLock(alertID)
		}
	}

	if m.config.DisableAllDockerHostsOffline {
		var hostAlerts []string
		for alertID := range m.activeAlerts {
			if strings.HasPrefix(alertID, "docker-host-offline-") {
				hostAlerts = append(hostAlerts, alertID)
			}
		}
		for _, alertID := range hostAlerts {
			m.clearAlertNoLock(alertID)
		}
		m.dockerOfflineCount = make(map[string]int)
	}

	if m.config.DisableAllDockerContainers {
		var containerAlerts []string
		for alertID := range m.activeAlerts {
			if strings.HasPrefix(alertID, "docker-container-") {
				containerAlerts = append(containerAlerts, alertID)
			}
		}
		for _, alertID := range containerAlerts {
			m.clearAlertNoLock(alertID)
		}
		m.dockerStateConfirm = make(map[string]int)
		m.dockerRestartTracking = make(map[string]*dockerRestartRecord)
		m.dockerLastExitCode = make(map[string]int)
	}
}

// reevaluateActiveAlertsLocked re-evaluates all active alerts against the current configuration
// This should only be called with m.mu already locked
func (m *Manager) reevaluateActiveAlertsLocked() {
	if len(m.activeAlerts) == 0 {
		return
	}

	// Track alerts that should be resolved
	alertsToResolve := make([]string, 0)

	for alertID, alert := range m.activeAlerts {
		// Parse the alert ID to extract resource ID and metric type
		// Alert ID format: {resourceID}-{metricType}
		parts := strings.Split(alertID, "-")
		if len(parts) < 2 {
			continue
		}

		metricType := parts[len(parts)-1]
		resourceID := strings.Join(parts[:len(parts)-1], "-")

		// Get the appropriate threshold based on resource type and ID
		var threshold *HysteresisThreshold

		resourceTypeMeta := ""
		if alert.Metadata != nil {
			if metaType, ok := alert.Metadata["resourceType"].(string); ok {
				resourceTypeMeta = strings.ToLower(metaType)
			}
		}

		if alert.Type == "docker-host-offline" ||
			strings.HasPrefix(alertID, "docker-container-health-") ||
			strings.HasPrefix(alertID, "docker-container-state-") ||
			strings.HasPrefix(alertID, "docker-container-restart-loop-") ||
			strings.HasPrefix(alertID, "docker-container-oom-") ||
			strings.HasPrefix(alertID, "docker-container-memory-limit-") {
			// Non-metric Docker alerts are not governed by thresholds
			continue
		}

		if resourceTypeMeta == "dockerhost" {
			// No threshold evaluation for Docker hosts (connectivity handled separately)
			continue
		}
		if resourceTypeMeta == "docker container" {
			thresholds := m.config.GuestDefaults
			if override, exists := m.config.Overrides[resourceID]; exists {
				thresholds = m.applyThresholdOverride(thresholds, override)
			}
			threshold = getThresholdForMetric(thresholds, metricType)
		}

		// Determine the resource type from the alert's metadata or instance
		// We need to check what kind of resource this is
		if threshold == nil && (alert.Instance == "Node" || alert.Instance == alert.Node) {
			// This is a node alert
			thresholds := m.config.NodeDefaults
			if override, exists := m.config.Overrides[resourceID]; exists {
				thresholds = m.applyThresholdOverride(thresholds, override)
			}
			threshold = getThresholdForMetric(thresholds, metricType)
		} else if threshold == nil && (alert.Instance == "Storage" || strings.Contains(alert.ResourceID, ":storage/")) {
			// This is a storage alert
			if override, exists := m.config.Overrides[resourceID]; exists && override.Usage != nil {
				threshold = override.Usage
			} else {
				threshold = &m.config.StorageDefault
			}
		} else if threshold == nil && alert.Instance == "PBS" {
			// This is a PBS alert
			thresholds := m.config.NodeDefaults
			if override, exists := m.config.Overrides[resourceID]; exists {
				if override.CPU != nil && metricType == "cpu" {
					threshold = ensureHysteresisThreshold(override.CPU)
				} else if override.Memory != nil && metricType == "memory" {
					threshold = ensureHysteresisThreshold(override.Memory)
				}
			}
			if threshold == nil {
				threshold = getThresholdForMetric(thresholds, metricType)
			}
		}

		if threshold == nil {
			// This is a guest (qemu/lxc) alert
			// We need to evaluate custom rules, but we don't have the guest object here.
			// For now, we'll mark these alerts for re-evaluation by the monitor.
			// The next poll cycle will properly evaluate them with custom rules.

			// Check if there's an override for this specific guest
			if override, exists := m.config.Overrides[resourceID]; exists {
				if override.Disabled {
					// Alert is now disabled for this resource, resolve it
					alertsToResolve = append(alertsToResolve, alertID)
					continue
				}
				threshold = getThresholdForMetricFromConfig(override, metricType)
			}

			// If no override or override doesn't have this metric, use defaults
			// Note: This doesn't consider custom rules - those will be evaluated
			// on the next poll cycle when we have the full guest object
			if threshold == nil {
				threshold = getThresholdForMetric(m.config.GuestDefaults, metricType)
			}
		}

		// If no threshold found or threshold is disabled (trigger <= 0), resolve the alert
		if threshold == nil || threshold.Trigger <= 0 {
			alertsToResolve = append(alertsToResolve, alertID)
			continue
		}

		// Check if current value is now below the clear threshold
		clearThreshold := threshold.Clear
		if clearThreshold <= 0 {
			clearThreshold = threshold.Trigger
		}

		if alert.Value <= clearThreshold {
			// Alert should be resolved due to new threshold
			alertsToResolve = append(alertsToResolve, alertID)
			log.Info().
				Str("alertID", alertID).
				Float64("value", alert.Value).
				Float64("oldThreshold", alert.Threshold).
				Float64("newClearThreshold", clearThreshold).
				Msg("Resolving alert due to threshold change")
		} else if alert.Value < threshold.Trigger {
			// Value is between clear and trigger thresholds after config change
			// Resolve it to prevent confusion
			alertsToResolve = append(alertsToResolve, alertID)
			log.Info().
				Str("alertID", alertID).
				Float64("value", alert.Value).
				Float64("newTrigger", threshold.Trigger).
				Float64("newClear", clearThreshold).
				Msg("Resolving alert - value now below trigger threshold after config change")
		}
	}

	// Resolve all alerts that should be cleared
	for _, alertID := range alertsToResolve {
		if alert, exists := m.activeAlerts[alertID]; exists {
			resolvedAlert := &ResolvedAlert{
				Alert:        alert,
				ResolvedTime: time.Now(),
			}

			// Remove any pending notification tracking for this alert since it's no longer valid.
			if _, isPending := m.pendingAlerts[alertID]; isPending {
				delete(m.pendingAlerts, alertID)
				log.Debug().
					Str("alertID", alertID).
					Msg("Cleared pending alert after configuration update")
			}

			// Remove from active alerts
			m.removeActiveAlertNoLock(alertID)

			// Add to recently resolved while respecting lock ordering
			m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

			log.Info().
				Str("alertID", alertID).
				Msg("Alert auto-resolved after configuration change")

			m.safeCallResolvedCallback(alertID, true)
		}
	}

	// Save updated active alerts if any were resolved
	if len(alertsToResolve) > 0 {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine (config update)")
				}
			}()
			if err := m.SaveActiveAlerts(); err != nil {
				log.Error().Err(err).Msg("Failed to save active alerts after config update")
			}
		}()
	}
}

// ReevaluateGuestAlert reevaluates a specific guest's alerts with full threshold resolution including custom rules
// This should be called by the monitor with the current guest state
func (m *Manager) ReevaluateGuestAlert(guest interface{}, guestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get the correct thresholds for this guest (includes custom rules evaluation)
	thresholds := m.getGuestThresholds(guest, guestID)

	// Check all metric types for this guest
	metricTypes := []string{"cpu", "memory", "disk", "diskRead", "diskWrite", "networkIn", "networkOut"}

	for _, metricType := range metricTypes {
		alertID := fmt.Sprintf("%s-%s", guestID, metricType)
		alert, exists := m.activeAlerts[alertID]
		if !exists {
			continue
		}

		// Get the threshold for this metric
		var threshold *HysteresisThreshold
		switch metricType {
		case "cpu":
			threshold = thresholds.CPU
		case "memory":
			threshold = thresholds.Memory
		case "disk":
			threshold = thresholds.Disk
		case "diskRead":
			threshold = thresholds.DiskRead
		case "diskWrite":
			threshold = thresholds.DiskWrite
		case "networkIn":
			threshold = thresholds.NetworkIn
		case "networkOut":
			threshold = thresholds.NetworkOut
		}

		// If threshold is disabled or doesn't exist, clear the alert
		if threshold == nil || threshold.Trigger <= 0 {
			m.clearAlertNoLock(alertID)
			// Also clear any pending alert for this metric
			if _, isPending := m.pendingAlerts[alertID]; isPending {
				delete(m.pendingAlerts, alertID)
				log.Debug().
					Str("alertID", alertID).
					Msg("Cleared pending alert - threshold disabled")
			}
			log.Info().
				Str("alertID", alertID).
				Str("metric", metricType).
				Msg("Cleared alert - threshold disabled")
			continue
		}

		// Check if alert should be cleared based on new threshold
		clearThreshold := threshold.Clear
		if clearThreshold <= 0 {
			clearThreshold = threshold.Trigger
		}

		if alert.Value <= clearThreshold || alert.Value < threshold.Trigger {
			m.clearAlertNoLock(alertID)
			log.Info().
				Str("alertID", alertID).
				Str("metric", metricType).
				Float64("value", alert.Value).
				Float64("trigger", threshold.Trigger).
				Float64("clear", clearThreshold).
				Msg("Cleared alert - value now below threshold after config change")
		}
	}
}

// getThresholdForMetric returns the threshold for a specific metric type from a ThresholdConfig
func getThresholdForMetric(config ThresholdConfig, metricType string) *HysteresisThreshold {
	switch metricType {
	case "cpu":
		return config.CPU
	case "memory":
		return config.Memory
	case "disk":
		return config.Disk
	case "diskRead":
		return config.DiskRead
	case "diskWrite":
		return config.DiskWrite
	case "networkIn":
		return config.NetworkIn
	case "networkOut":
		return config.NetworkOut
	case "temperature":
		return config.Temperature
	case "usage":
		return config.Usage
	default:
		return nil
	}
}

// getThresholdForMetricFromConfig returns the threshold for a specific metric type from a ThresholdConfig
// ensuring hysteresis is properly set
func getThresholdForMetricFromConfig(config ThresholdConfig, metricType string) *HysteresisThreshold {
	var threshold *HysteresisThreshold
	switch metricType {
	case "cpu":
		if config.CPU != nil {
			threshold = ensureHysteresisThreshold(config.CPU)
		}
	case "memory":
		if config.Memory != nil {
			threshold = ensureHysteresisThreshold(config.Memory)
		}
	case "disk":
		if config.Disk != nil {
			threshold = ensureHysteresisThreshold(config.Disk)
		}
	case "diskRead":
		if config.DiskRead != nil {
			threshold = ensureHysteresisThreshold(config.DiskRead)
		}
	case "diskWrite":
		if config.DiskWrite != nil {
			threshold = ensureHysteresisThreshold(config.DiskWrite)
		}
	case "networkIn":
		if config.NetworkIn != nil {
			threshold = ensureHysteresisThreshold(config.NetworkIn)
		}
	case "networkOut":
		if config.NetworkOut != nil {
			threshold = ensureHysteresisThreshold(config.NetworkOut)
		}
	case "temperature":
		if config.Temperature != nil {
			threshold = ensureHysteresisThreshold(config.Temperature)
		}
	case "usage":
		if config.Usage != nil {
			threshold = ensureHysteresisThreshold(config.Usage)
		}
	}
	return threshold
}

// isInQuietHours checks if the current time is within quiet hours
func (m *Manager) isInQuietHours() bool {
	if !m.config.Schedule.QuietHours.Enabled {
		return false
	}

	// Load timezone
	loc, err := time.LoadLocation(m.config.Schedule.QuietHours.Timezone)
	if err != nil {
		log.Warn().Err(err).Str("timezone", m.config.Schedule.QuietHours.Timezone).Msg("Failed to load timezone, using local time")
		loc = time.Local
	}

	now := time.Now().In(loc)
	dayName := strings.ToLower(now.Format("Monday"))

	// Check if today is enabled for quiet hours
	if enabled, ok := m.config.Schedule.QuietHours.Days[dayName]; !ok || !enabled {
		return false
	}

	// Parse start and end times
	startTime, err := time.ParseInLocation("15:04", m.config.Schedule.QuietHours.Start, loc)
	if err != nil {
		log.Warn().Err(err).Str("start", m.config.Schedule.QuietHours.Start).Msg("Failed to parse quiet hours start time")
		return false
	}

	endTime, err := time.ParseInLocation("15:04", m.config.Schedule.QuietHours.End, loc)
	if err != nil {
		log.Warn().Err(err).Str("end", m.config.Schedule.QuietHours.End).Msg("Failed to parse quiet hours end time")
		return false
	}

	// Set to today's date
	startTime = time.Date(now.Year(), now.Month(), now.Day(), startTime.Hour(), startTime.Minute(), 0, 0, loc)
	endTime = time.Date(now.Year(), now.Month(), now.Day(), endTime.Hour(), endTime.Minute(), 0, 0, loc)

	// Handle overnight quiet hours (e.g., 22:00 to 08:00)
	if endTime.Before(startTime) {
		// If we're past the start time or before the end time
		if now.After(startTime) || now.Before(endTime) {
			return true
		}
	} else {
		// Normal case (e.g., 08:00 to 17:00)
		if now.After(startTime) && now.Before(endTime) {
			return true
		}
	}

	return false
}

func quietHoursCategoryForAlert(alert *Alert) string {
	if alert == nil {
		return ""
	}

	switch alert.Type {
	case "cpu", "memory", "disk", "diskRead", "diskWrite", "networkIn", "networkOut", "temperature":
		return "performance"
	case "queue-depth", "queue-deferred", "queue-hold", "message-age",
		"docker-container-health", "docker-container-restart-loop",
		"docker-container-oom-kill", "docker-container-memory-limit":
		return "performance"
	case "usage", "disk-health", "disk-wearout", "zfs-pool-state", "zfs-pool-errors", "zfs-device":
		return "storage"
	case "connectivity", "offline", "powered-off", "docker-host-offline":
		return "offline"
	}

	if strings.HasPrefix(alert.Type, "docker-container-") {
		if alert.Type == "docker-container-state" {
			return "offline"
		}
		return "performance"
	}

	return ""
}

func (m *Manager) shouldSuppressNotification(alert *Alert) (bool, string) {
	if alert == nil {
		return false, ""
	}

	if !m.isInQuietHours() {
		return false, ""
	}

	if alert.Level != AlertLevelCritical {
		return true, "non-critical"
	}

	category := quietHoursCategoryForAlert(alert)
	switch category {
	case "performance":
		if m.config.Schedule.QuietHours.Suppress.Performance {
			return true, category
		}
	case "storage":
		if m.config.Schedule.QuietHours.Suppress.Storage {
			return true, category
		}
	case "offline":
		if m.config.Schedule.QuietHours.Suppress.Offline {
			return true, category
		}
	}

	return false, ""
}

// shouldNotifyAfterCooldown checks if enough time has passed since the last notification
// Returns true if notification should be sent, false if still in cooldown period
func (m *Manager) shouldNotifyAfterCooldown(alert *Alert) bool {
	// If cooldown is 0 or negative, always allow notifications
	if m.config.Schedule.Cooldown <= 0 {
		return true
	}

	// If this is the first notification, allow it
	if alert.LastNotified == nil {
		return true
	}

	// Check if enough time has passed
	cooldownDuration := time.Duration(m.config.Schedule.Cooldown) * time.Minute
	timeSinceLastNotification := time.Since(*alert.LastNotified)

	return timeSinceLastNotification >= cooldownDuration
}

// GetConfig returns the current alert configuration
func (m *Manager) GetConfig() AlertConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// CheckGuest checks a guest (VM or container) against thresholds
func (m *Manager) CheckGuest(guest interface{}, instanceName string) {
	m.mu.RLock()
	enabled := m.config.Enabled
	disableAllGuests := m.config.DisableAllGuests
	disableAllGuestsOffline := m.config.DisableAllGuestsOffline
	m.mu.RUnlock()

	if !enabled {
		log.Debug().Msg("CheckGuest: alerts disabled globally")
		return
	}
	if disableAllGuests {
		log.Debug().Msg("CheckGuest: all guest alerts disabled")
		return
	}

	var guestID, name, node, guestType, status string
	var cpu, memUsage, diskUsage float64
	var diskRead, diskWrite, netIn, netOut int64
	var disks []models.Disk

	// Extract data based on guest type
	switch g := guest.(type) {
	case models.VM:
		guestID = g.ID
		name = g.Name
		node = g.Node
		status = g.Status
		guestType = "VM"
		cpu = g.CPU * 100 // Convert to percentage
		memUsage = g.Memory.Usage
		diskUsage = g.Disk.Usage
		diskRead = g.DiskRead
		diskWrite = g.DiskWrite
		netIn = g.NetworkIn
		netOut = g.NetworkOut
		disks = g.Disks

		// Debug logging for high memory VMs
		if memUsage > 85 {
			log.Info().
				Str("vm", name).
				Float64("memUsage", memUsage).
				Str("status", status).
				Msg("VM with high memory detected in CheckGuest")
		}
	case models.Container:
		guestID = g.ID
		name = g.Name
		node = g.Node
		status = g.Status
		guestType = "Container"
		cpu = g.CPU * 100 // Convert to percentage
		memUsage = g.Memory.Usage
		diskUsage = g.Disk.Usage
		diskRead = g.DiskRead
		diskWrite = g.DiskWrite
		netIn = g.NetworkIn
		netOut = g.NetworkOut
		disks = g.Disks
	default:
		log.Debug().
			Str("type", fmt.Sprintf("%T", guest)).
			Msg("CheckGuest: unsupported guest type")
		return
	}

	// Handle non-running guests
	// Proxmox VM states: running, stopped, paused, suspended
	if status != "running" {
		// Check for powered-off state and generate alert if configured
		if status == "stopped" {
			if disableAllGuestsOffline {
				// Clear any pending powered-off tracking and alerts when globally disabled
				m.mu.Lock()
				delete(m.offlineConfirmations, guestID)
				m.mu.Unlock()
				m.clearAlert(fmt.Sprintf("guest-powered-off-%s", guestID))
			} else {
				m.checkGuestPoweredOff(guestID, name, node, instanceName, guestType)
			}
		} else {
			// For paused/suspended, clear powered-off alert
			m.clearGuestPoweredOffAlert(guestID, name)
		}

		// Clear all resource metric alerts (cpu, memory, disk, etc.) for non-running guests
		m.mu.Lock()
		alertsCleared := 0
		for alertID, alert := range m.activeAlerts {
			// Only clear resource metric alerts, not powered-off alerts
			if alert.ResourceID == guestID && alert.Type != "powered-off" {
				m.clearAlertNoLock(alertID)
				alertsCleared++
				log.Debug().
					Str("alertID", alertID).
					Str("guest", name).
					Str("status", status).
					Msg("Cleared metric alert for non-running guest")
			}
		}
		m.mu.Unlock()

		if alertsCleared > 0 {
			log.Debug().
				Str("guest", name).
				Str("status", status).
				Int("alertsCleared", alertsCleared).
				Msg("Cleared metric alerts for non-running guest")
		}
		return
	}

	// If guest is running, clear any powered-off alert
	m.clearGuestPoweredOffAlert(guestID, name)

	// Get thresholds (check custom rules, then overrides, then defaults)
	m.mu.RLock()
	thresholds := m.getGuestThresholds(guest, guestID)
	m.mu.RUnlock()

	// If alerts are disabled for this guest, clear any existing alerts and return
	if thresholds.Disabled {
		m.mu.Lock()
		for alertID, alert := range m.activeAlerts {
			if alert.ResourceID == guestID {
				m.clearAlertNoLock(alertID)
				log.Info().
					Str("alertID", alertID).
					Str("guest", name).
					Msg("Cleared alert - guest has alerts disabled")
			}
		}
		m.mu.Unlock()
		return
	}

	// Check each metric
	log.Info().
		Str("guest", name).
		Float64("cpu", cpu).
		Float64("memory", memUsage).
		Float64("disk", diskUsage).
		Interface("thresholds", thresholds).
		Msg("Checking guest thresholds")

	// Check thresholds (checkMetric will skip if threshold is nil or <= 0)
	m.checkMetric(guestID, name, node, instanceName, guestType, "cpu", cpu, thresholds.CPU, nil)
	m.checkMetric(guestID, name, node, instanceName, guestType, "memory", memUsage, thresholds.Memory, nil)
	m.checkMetric(guestID, name, node, instanceName, guestType, "disk", diskUsage, thresholds.Disk, nil)

	if thresholds.Disk != nil && thresholds.Disk.Trigger > 0 && len(disks) > 0 {
		seenDisks := make(map[string]struct{})
		for idx, disk := range disks {
			if disk.Total <= 0 {
				continue
			}
			if disk.Usage < 0 {
				continue
			}

			label := strings.TrimSpace(disk.Mountpoint)
			if label == "" {
				label = strings.TrimSpace(disk.Device)
			}
			if label == "" {
				label = fmt.Sprintf("Disk %d", idx+1)
			}

			keySource := label
			if disk.Device != "" && !strings.EqualFold(disk.Device, label) {
				keySource = fmt.Sprintf("%s-%s", label, disk.Device)
			}
			sanitizedKey := sanitizeAlertKey(keySource)
			if sanitizedKey == "" {
				sanitizedKey = fmt.Sprintf("disk-%d", idx+1)
			}

			// Avoid duplicate checks if two disks resolve to the same key
			if _, exists := seenDisks[sanitizedKey]; exists {
				continue
			}
			seenDisks[sanitizedKey] = struct{}{}

			perDiskResourceID := fmt.Sprintf("%s-disk-%s", guestID, sanitizedKey)
			message := fmt.Sprintf("%s disk (%s) at %.1f%%", guestType, label, disk.Usage)

			log.Debug().
				Str("guest", name).
				Str("node", node).
				Str("instance", instanceName).
				Str("diskLabel", label).
				Float64("usage", disk.Usage).
				Msg("Evaluating individual disk for alert thresholds")

			metadata := map[string]interface{}{
				"mountpoint": disk.Mountpoint,
				"device":     disk.Device,
				"diskType":   disk.Type,
				"totalBytes": disk.Total,
				"usedBytes":  disk.Used,
				"freeBytes":  disk.Free,
				"diskIndex":  idx,
				"label":      label,
			}

			m.checkMetric(perDiskResourceID, name, node, instanceName, guestType, "disk", disk.Usage, thresholds.Disk, &metricOptions{
				Metadata: metadata,
				Message:  message,
			})
		}
	}

	// Check I/O metrics (convert bytes/s to MB/s) - checkMetric will skip if threshold is nil or <= 0
	if thresholds.DiskRead != nil && thresholds.DiskRead.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "diskRead", float64(diskRead)/1024/1024, thresholds.DiskRead, nil)
	}
	if thresholds.DiskWrite != nil && thresholds.DiskWrite.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "diskWrite", float64(diskWrite)/1024/1024, thresholds.DiskWrite, nil)
	}
	if thresholds.NetworkIn != nil && thresholds.NetworkIn.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "networkIn", float64(netIn)/1024/1024, thresholds.NetworkIn, nil)
	}
	if thresholds.NetworkOut != nil && thresholds.NetworkOut.Trigger > 0 {
		m.checkMetric(guestID, name, node, instanceName, guestType, "networkOut", float64(netOut)/1024/1024, thresholds.NetworkOut, nil)
	}
}

// CheckNode checks a node against thresholds
func (m *Manager) CheckNode(node models.Node) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	if m.config.DisableAllNodes {
		m.mu.RUnlock()
		return
	}
	disableNodesOffline := m.config.DisableAllNodesOffline
	thresholds := m.config.NodeDefaults
	if override, exists := m.config.Overrides[node.ID]; exists {
		thresholds = m.applyThresholdOverride(thresholds, override)
	}
	m.mu.RUnlock()

	if disableNodesOffline {
		// Clear tracking and any existing offline alerts when globally disabled
		m.mu.Lock()
		delete(m.nodeOfflineCount, node.ID)
		m.mu.Unlock()
		m.clearAlert(fmt.Sprintf("node-offline-%s", node.ID))
	} else {
		// CRITICAL: Check if node is offline first
		if node.Status == "offline" || node.ConnectionHealth == "error" || node.ConnectionHealth == "failed" {
			m.checkNodeOffline(node)
		} else {
			// Clear any existing offline alert if node is back online
			m.clearNodeOfflineAlert(node)
		}
	}

	// Check each metric (only if node is online) - checkMetric will skip if threshold is nil or <= 0
	if node.Status != "offline" {
		m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "cpu", node.CPU*100, thresholds.CPU, nil)
		m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "memory", node.Memory.Usage, thresholds.Memory, nil)
		m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "disk", node.Disk.Usage, thresholds.Disk, nil)

		// Check temperature if available
		if node.Temperature != nil && node.Temperature.Available && thresholds.Temperature != nil {
			// Use CPU package temp if available, otherwise use max core temp
			temp := node.Temperature.CPUPackage
			if temp == 0 {
				temp = node.Temperature.CPUMax
			}
			m.checkMetric(node.ID, node.Name, node.Name, node.Instance, "Node", "temperature", temp, thresholds.Temperature, nil)
		}
	}
}

// CheckPBS checks PBS instance metrics against thresholds
func (m *Manager) CheckPBS(pbs models.PBSInstance) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	if m.config.DisableAllPBS {
		m.mu.RUnlock()
		return
	}

	// Check if there's an override for this PBS instance
	override, hasOverride := m.config.Overrides[pbs.ID]

	// Use node defaults for PBS (same as nodes: CPU, Memory)
	cpuThreshold := m.config.NodeDefaults.CPU
	memoryThreshold := m.config.NodeDefaults.Memory
	disablePBSOffline := m.config.DisableAllPBSOffline
	m.mu.RUnlock()

	// Check override disable BEFORE offline detection to prevent spurious notifications
	if hasOverride && override.Disabled {
		m.mu.Lock()
		// Reset offline confirmation tracking
		delete(m.offlineConfirmations, pbs.ID)
		// Clear CPU alert
		cpuAlertID := fmt.Sprintf("%s-cpu", pbs.ID)
		if _, exists := m.activeAlerts[cpuAlertID]; exists {
			m.clearAlertNoLock(cpuAlertID)
			log.Debug().
				Str("alertID", cpuAlertID).
				Str("pbs", pbs.Name).
				Msg("Cleared CPU alert - PBS has alerts disabled")
		}
		// Clear Memory alert
		memAlertID := fmt.Sprintf("%s-memory", pbs.ID)
		if _, exists := m.activeAlerts[memAlertID]; exists {
			m.clearAlertNoLock(memAlertID)
			log.Debug().
				Str("alertID", memAlertID).
				Str("pbs", pbs.Name).
				Msg("Cleared Memory alert - PBS has alerts disabled")
		}
		// Clear offline alert
		offlineAlertID := fmt.Sprintf("pbs-offline-%s", pbs.ID)
		if _, exists := m.activeAlerts[offlineAlertID]; exists {
			m.clearAlertNoLock(offlineAlertID)
			log.Debug().
				Str("alertID", offlineAlertID).
				Str("pbs", pbs.Name).
				Msg("Cleared offline alert - PBS has alerts disabled")
		}
		m.mu.Unlock()
		return
	}

	if disablePBSOffline {
		// Clear tracking and any existing offline alerts when globally disabled
		m.mu.Lock()
		delete(m.offlineConfirmations, pbs.ID)
		m.mu.Unlock()
		m.clearAlert(fmt.Sprintf("pbs-offline-%s", pbs.ID))
	} else {
		// Check if PBS is offline first (similar to nodes)
		if pbs.Status == "offline" || pbs.ConnectionHealth == "error" || pbs.ConnectionHealth == "unhealthy" {
			m.checkPBSOffline(pbs)
		} else {
			// Clear any existing offline alert if PBS is back online
			m.clearPBSOfflineAlert(pbs)
		}
	}

	// Check if there are custom thresholds for this PBS instance
	if hasOverride {
		if override.CPU != nil {
			cpuThreshold = override.CPU
		}
		if override.Memory != nil {
			memoryThreshold = override.Memory
		}
	}

	// Check metrics only if PBS is online - checkMetric will skip if threshold is nil or <= 0
	if pbs.Status != "offline" {
		// PBS CPU is already a percentage
		m.checkMetric(pbs.ID, pbs.Name, pbs.Host, pbs.Name, "PBS", "cpu", pbs.CPU, cpuThreshold, nil)
		// PBS Memory is already a percentage
		m.checkMetric(pbs.ID, pbs.Name, pbs.Host, pbs.Name, "PBS", "memory", pbs.Memory, memoryThreshold, nil)
	}
}

// CheckPMG checks a Proxmox Mail Gateway instance against thresholds
func (m *Manager) CheckPMG(pmg models.PMGInstance) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	if m.config.DisableAllPMG {
		m.mu.RUnlock()
		return
	}

	// Check if there's an override for this PMG instance
	override, hasOverride := m.config.Overrides[pmg.ID]
	disablePMGOffline := m.config.DisableAllPMGOffline
	pmgDefaults := m.config.PMGDefaults
	m.mu.RUnlock()

	// Check override disable BEFORE offline detection to prevent spurious notifications
	if hasOverride && override.Disabled {
		m.mu.Lock()
		// Reset offline confirmation tracking
		delete(m.offlineConfirmations, pmg.ID)
		// Clear all possible PMG alert types
		alertTypes := []string{"queue-total", "queue-deferred", "queue-hold", "oldest-message"}
		for _, alertType := range alertTypes {
			alertID := fmt.Sprintf("%s-%s", pmg.ID, alertType)
			if _, exists := m.activeAlerts[alertID]; exists {
				m.clearAlertNoLock(alertID)
				log.Debug().
					Str("alertID", alertID).
					Str("pmg", pmg.Name).
					Msg("Cleared PMG alert - PMG has alerts disabled")
			}
		}
		// Clear offline alert
		offlineAlertID := fmt.Sprintf("pmg-offline-%s", pmg.ID)
		if _, exists := m.activeAlerts[offlineAlertID]; exists {
			m.clearAlertNoLock(offlineAlertID)
			log.Debug().
				Str("alertID", offlineAlertID).
				Str("pmg", pmg.Name).
				Msg("Cleared offline alert - PMG has alerts disabled")
		}
		m.mu.Unlock()
		return
	}

	// Handle offline detection
	if disablePMGOffline {
		// Clear tracking and any existing offline alerts when globally disabled
		m.mu.Lock()
		delete(m.offlineConfirmations, pmg.ID)
		m.mu.Unlock()
		m.clearAlert(fmt.Sprintf("pmg-offline-%s", pmg.ID))
	} else {
		// Check if PMG is offline (similar to PBS/nodes)
		if pmg.Status == "offline" || pmg.ConnectionHealth == "error" || pmg.ConnectionHealth == "unhealthy" {
			m.checkPMGOffline(pmg)
		} else {
			// Clear any existing offline alert if PMG is back online
			m.clearPMGOfflineAlert(pmg)
		}
	}

	// Check metrics only if PMG is online
	if pmg.Status != "offline" {
		// Check queue depths across all nodes
		m.checkPMGQueueDepths(pmg, pmgDefaults)
		// Check oldest message age across all nodes
		m.checkPMGOldestMessage(pmg, pmgDefaults)
		// Check quarantine backlog and growth
		m.checkPMGQuarantineBacklog(pmg, pmgDefaults)
		// Check spam/virus rate anomalies
		m.checkPMGAnomalies(pmg, pmgDefaults)
		// Check per-node queue health
		m.checkPMGNodeQueues(pmg, pmgDefaults)
	}
}

// dockerInstanceName returns the logical instance name used for Docker alerts.
func dockerInstanceName(host models.DockerHost) string {
	name := strings.TrimSpace(host.DisplayName)
	if name == "" {
		name = strings.TrimSpace(host.Hostname)
	}
	if name == "" {
		return "Docker"
	}
	return fmt.Sprintf("Docker:%s", name)
}

// dockerContainerDisplayName normalizes the container name for alert readability.
func dockerContainerDisplayName(container models.DockerContainer) string {
	name := strings.TrimSpace(container.Name)
	if strings.HasPrefix(name, "/") {
		name = strings.TrimLeft(name, "/")
	}
	if name == "" {
		id := strings.TrimSpace(container.ID)
		if len(id) > 12 {
			id = id[:12]
		}
		return id
	}
	return name
}

// dockerResourceID builds a stable identifier for Docker container alerts.
func dockerResourceID(hostID, containerID string) string {
	hostID = strings.TrimSpace(hostID)
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		if hostID == "" {
			return "docker:unknown"
		}
		return fmt.Sprintf("docker:%s", hostID)
	}
	if hostID == "" {
		return fmt.Sprintf("docker:container/%s", containerID)
	}
	return fmt.Sprintf("docker:%s/%s", hostID, containerID)
}

func matchesDockerIgnoredPrefix(name, id string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return false
	}

	name = strings.ToLower(strings.TrimSpace(name))
	id = strings.ToLower(strings.TrimSpace(id))

	for _, raw := range prefixes {
		prefix := strings.ToLower(strings.TrimSpace(raw))
		if prefix == "" {
			continue
		}
		if name != "" && strings.HasPrefix(name, prefix) {
			return true
		}
		if id != "" && strings.HasPrefix(id, prefix) {
			return true
		}
	}

	return false
}

// CheckDockerHost evaluates Docker host telemetry and container metrics for alerts.
func (m *Manager) CheckDockerHost(host models.DockerHost) {
	if host.ID == "" {
		return
	}

	// Fresh telemetry marks the host as online and clears any offline alert.
	m.HandleDockerHostOnline(host)

	m.mu.RLock()
	alertsEnabled := m.config.Enabled
	disableAllHosts := m.config.DisableAllDockerHosts
	ignoredPrefixes := append([]string(nil), m.config.DockerIgnoredContainerPrefixes...)
	m.mu.RUnlock()
	if !alertsEnabled {
		return
	}
	if disableAllHosts {
		return
	}

	seen := make(map[string]struct{}, len(host.Containers))
	for _, container := range host.Containers {
		containerName := dockerContainerDisplayName(container)
		resourceID := dockerResourceID(host.ID, container.ID)

		if matchesDockerIgnoredPrefix(containerName, container.ID, ignoredPrefixes) {
			log.Debug().
				Str("container", containerName).
				Str("host", host.DisplayName).
				Msg("Skipping Docker container alert evaluation due to ignored prefix")
			m.clearDockerContainerStateAlert(resourceID)
			m.clearDockerContainerHealthAlert(resourceID)
			m.clearDockerContainerMetricAlerts(resourceID)
			m.clearAlert(fmt.Sprintf("docker-container-restart-loop-%s", resourceID))
			m.clearAlert(fmt.Sprintf("docker-container-oom-%s", resourceID))
			m.clearAlert(fmt.Sprintf("docker-container-memory-limit-%s", resourceID))
			m.mu.Lock()
			delete(m.dockerRestartTracking, resourceID)
			delete(m.dockerLastExitCode, resourceID)
			m.mu.Unlock()
			continue
		}

		seen[resourceID] = struct{}{}
		m.evaluateDockerContainer(host, container, resourceID)
	}

	m.cleanupDockerContainerAlerts(host, seen)
}

func (m *Manager) evaluateDockerContainer(host models.DockerHost, container models.DockerContainer, resourceID string) {
	m.mu.RLock()
	disableAllContainers := m.config.DisableAllDockerContainers
	m.mu.RUnlock()
	if disableAllContainers {
		return
	}

	containerName := dockerContainerDisplayName(container)
	nodeName := strings.TrimSpace(host.Hostname)
	instanceName := dockerInstanceName(host)
	resourceType := "Docker Container"

	m.mu.RLock()
	overrideConfig, hasOverride := m.config.Overrides[resourceID]
	m.mu.RUnlock()
	if hasOverride && overrideConfig.Disabled {
		// Alerts disabled via override; clear any existing alerts and skip evaluation.
		m.clearDockerContainerStateAlert(resourceID)
		m.clearDockerContainerHealthAlert(resourceID)
		m.clearDockerContainerMetricAlerts(resourceID)
		return
	}

	state := strings.ToLower(strings.TrimSpace(container.State))
	if state == "" {
		state = strings.ToLower(strings.TrimSpace(container.Status))
	}

	if state != "running" {
		m.checkDockerContainerState(host, container, resourceID, containerName, instanceName, nodeName)
		m.clearDockerContainerMetricAlerts(resourceID, "cpu", "memory")
	} else {
		m.clearDockerContainerStateAlert(resourceID)

		// Use Docker-specific defaults for containers
		thresholds := ThresholdConfig{
			CPU:    &m.config.DockerDefaults.CPU,
			Memory: &m.config.DockerDefaults.Memory,
		}
		if hasOverride {
			thresholds = m.applyThresholdOverride(thresholds, overrideConfig)
		}

		if thresholds.CPU != nil {
			cpuMetadata := map[string]interface{}{
				"resourceType":  resourceType,
				"hostId":        host.ID,
				"hostName":      host.DisplayName,
				"hostHostname":  host.Hostname,
				"containerId":   container.ID,
				"containerName": containerName,
				"image":         container.Image,
				"state":         container.State,
				"status":        container.Status,
				"restartCount":  container.RestartCount,
				"metric":        "cpu",
				"cpuPercent":    container.CPUPercent,
			}
			m.checkMetric(resourceID, containerName, nodeName, instanceName, resourceType, "cpu", container.CPUPercent, thresholds.CPU, &metricOptions{Metadata: cpuMetadata})
		}

		if thresholds.Memory != nil {
			memMetadata := map[string]interface{}{
				"resourceType":     resourceType,
				"hostId":           host.ID,
				"hostName":         host.DisplayName,
				"hostHostname":     host.Hostname,
				"containerId":      container.ID,
				"containerName":    containerName,
				"image":            container.Image,
				"state":            container.State,
				"status":           container.Status,
				"restartCount":     container.RestartCount,
				"metric":           "memory",
				"memoryPercent":    container.MemoryPercent,
				"memoryUsageBytes": container.MemoryUsage,
			}
			if container.MemoryLimit > 0 {
				memMetadata["memoryLimitBytes"] = container.MemoryLimit
			}
			m.checkMetric(resourceID, containerName, nodeName, instanceName, resourceType, "memory", container.MemoryPercent, thresholds.Memory, &metricOptions{Metadata: memMetadata})
		}
	}

	m.checkDockerContainerHealth(host, container, resourceID, containerName, instanceName, nodeName)

	// Docker-specific checks
	m.checkDockerContainerRestartLoop(host, container, resourceID, containerName, instanceName, nodeName)
	m.checkDockerContainerOOMKill(host, container, resourceID, containerName, instanceName, nodeName)
	m.checkDockerContainerMemoryLimit(host, container, resourceID, containerName, instanceName, nodeName)
}

// HandleDockerHostOnline clears offline tracking and alerts for a Docker host.
func (m *Manager) HandleDockerHostOnline(host models.DockerHost) {
	if host.ID == "" {
		return
	}

	alertID := fmt.Sprintf("docker-host-offline-%s", host.ID)

	m.mu.Lock()
	delete(m.dockerOfflineCount, host.ID)
	_, exists := m.activeAlerts[alertID]
	m.mu.Unlock()

	if exists {
		m.clearAlert(alertID)
	}
}

// HandleDockerHostRemoved clears all alerts and tracking when a Docker host is deleted.
func (m *Manager) HandleDockerHostRemoved(host models.DockerHost) {
	if host.ID == "" {
		return
	}

	// Reuse the online handler to clear offline alerts and tracking.
	m.HandleDockerHostOnline(host)
	// Drop any container alerts and host-scoped tracking entries.
	m.clearDockerHostContainerAlerts(host.ID)
}

// HandleDockerHostOffline raises an alert when a Docker host stops reporting.
func (m *Manager) HandleDockerHostOffline(host models.DockerHost) {
	if host.ID == "" {
		return
	}

	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	disableDockerHostsOffline := m.config.DisableAllDockerHostsOffline
	m.mu.RUnlock()

	alertID := fmt.Sprintf("docker-host-offline-%s", host.ID)
	resourceID := fmt.Sprintf("docker:%s", strings.TrimSpace(host.ID))
	instanceName := dockerInstanceName(host)
	nodeName := strings.TrimSpace(host.Hostname)

	if disableDockerHostsOffline {
		m.mu.Lock()
		delete(m.dockerOfflineCount, host.ID)
		m.mu.Unlock()
		m.clearAlert(alertID)
		return
	}

	var disableConnectivity bool
	m.mu.RLock()
	if override, exists := m.config.Overrides[host.ID]; exists {
		disableConnectivity = override.DisableConnectivity
	}
	m.mu.RUnlock()

	if disableConnectivity {
		m.clearAlert(alertID)
		m.mu.Lock()
		delete(m.dockerOfflineCount, host.ID)
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	if alert, exists := m.activeAlerts[alertID]; exists && alert != nil {
		alert.LastSeen = time.Now()
		m.activeAlerts[alertID] = alert
		m.mu.Unlock()
		return
	}

	m.dockerOfflineCount[host.ID]++
	confirmations := m.dockerOfflineCount[host.ID]
	const requiredConfirmations = 3
	if confirmations < requiredConfirmations {
		m.mu.Unlock()
		log.Debug().
			Str("dockerHost", host.DisplayName).
			Str("hostID", host.ID).
			Int("confirmations", confirmations).
			Int("required", requiredConfirmations).
			Msg("Docker host appears offline, awaiting confirmation")
		return
	}

	alert := &Alert{
		ID:           alertID,
		Type:         "docker-host-offline",
		Level:        AlertLevelCritical,
		ResourceID:   resourceID,
		ResourceName: host.DisplayName,
		Node:         nodeName,
		Instance:     instanceName,
		Message:      fmt.Sprintf("Docker host '%s' is offline", host.DisplayName),
		Value:        0,
		Threshold:    0,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
		Metadata: map[string]interface{}{
			"resourceType": "DockerHost",
			"hostId":       host.ID,
			"hostname":     host.Hostname,
			"agentId":      host.AgentID,
			"displayName":  host.DisplayName,
		},
	}

	m.preserveAlertState(alertID, alert)
	m.activeAlerts[alertID] = alert
	m.recentAlerts[alertID] = alert
	m.historyManager.AddAlert(*alert)
	m.dispatchAlert(alert, false)
	m.mu.Unlock()

	log.Error().
		Str("dockerHost", host.DisplayName).
		Str("hostID", host.ID).
		Str("hostname", host.Hostname).
		Msg("CRITICAL: Docker host is offline")

	m.clearDockerHostContainerAlerts(host.ID)
}

func (m *Manager) checkDockerContainerState(host models.DockerHost, container models.DockerContainer, resourceID, containerName, instanceName, nodeName string) {
	alertID := fmt.Sprintf("docker-container-state-%s", resourceID)
	stateKey := resourceID

	m.mu.RLock()
	thresholds, exists := m.config.Overrides[resourceID]
	if !exists {
		thresholds = m.config.GuestDefaults
	}
	disableConnectivity := thresholds.DisableConnectivity
	severity := normalizePoweredOffSeverity(thresholds.PoweredOffSeverity)
	m.mu.RUnlock()

	if disableConnectivity {
		m.clearDockerContainerStateAlert(resourceID)
		return
	}

	m.mu.Lock()
	if alert, exists := m.activeAlerts[alertID]; exists && alert != nil {
		alert.LastSeen = time.Now()
		alert.Level = severity
		if alert.Metadata == nil {
			alert.Metadata = make(map[string]interface{})
		}
		alert.Metadata["state"] = container.State
		alert.Metadata["status"] = container.Status
		m.activeAlerts[alertID] = alert
		m.mu.Unlock()
		return
	}

	m.dockerStateConfirm[stateKey]++
	confirmations := m.dockerStateConfirm[stateKey]
	const requiredConfirmations = 2
	if confirmations < requiredConfirmations {
		m.mu.Unlock()
		log.Debug().
			Str("container", containerName).
			Str("host", host.DisplayName).
			Str("state", container.State).
			Int("confirmations", confirmations).
			Int("required", requiredConfirmations).
			Msg("Docker container state change detected, awaiting confirmation")
		return
	}

	message := fmt.Sprintf("Docker container '%s' is %s", containerName, strings.TrimSpace(container.Status))
	alert := &Alert{
		ID:           alertID,
		Type:         "docker-container-state",
		Level:        severity,
		ResourceID:   resourceID,
		ResourceName: containerName,
		Node:         nodeName,
		Instance:     instanceName,
		Message:      message,
		Value:        0,
		Threshold:    0,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
		Metadata: map[string]interface{}{
			"resourceType":  "Docker Container",
			"hostId":        host.ID,
			"hostName":      host.DisplayName,
			"hostHostname":  host.Hostname,
			"containerId":   container.ID,
			"containerName": containerName,
			"image":         container.Image,
			"state":         container.State,
			"status":        container.Status,
		},
	}

	m.preserveAlertState(alertID, alert)
	m.activeAlerts[alertID] = alert
	m.recentAlerts[alertID] = alert
	m.historyManager.AddAlert(*alert)
	m.dispatchAlert(alert, true)
	m.mu.Unlock()

	log.Warn().
		Str("container", containerName).
		Str("host", host.DisplayName).
		Str("state", container.State).
		Msg("Docker container state alert raised")
}

func (m *Manager) clearDockerContainerStateAlert(resourceID string) {
	alertID := fmt.Sprintf("docker-container-state-%s", resourceID)
	m.mu.Lock()
	delete(m.dockerStateConfirm, resourceID)
	m.mu.Unlock()
	m.clearAlert(alertID)
}

func (m *Manager) checkDockerContainerHealth(host models.DockerHost, container models.DockerContainer, resourceID, containerName, instanceName, nodeName string) {
	health := strings.ToLower(strings.TrimSpace(container.Health))
	if health == "" || health == "none" || health == "healthy" || health == "starting" {
		m.clearDockerContainerHealthAlert(resourceID)
		return
	}

	level := AlertLevelWarning
	if health == "unhealthy" {
		level = AlertLevelCritical
	}

	alertID := fmt.Sprintf("docker-container-health-%s", resourceID)
	alert := &Alert{
		ID:           alertID,
		Type:         "docker-container-health",
		Level:        level,
		ResourceID:   resourceID,
		ResourceName: containerName,
		Node:         nodeName,
		Instance:     instanceName,
		Message:      fmt.Sprintf("Docker container '%s' health is %s", containerName, container.Health),
		Value:        0,
		Threshold:    0,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
		Metadata: map[string]interface{}{
			"resourceType":  "Docker Container",
			"hostId":        host.ID,
			"hostName":      host.DisplayName,
			"hostHostname":  host.Hostname,
			"containerId":   container.ID,
			"containerName": containerName,
			"image":         container.Image,
			"state":         container.State,
			"status":        container.Status,
			"health":        container.Health,
		},
	}

	m.mu.Lock()
	if existing, exists := m.activeAlerts[alertID]; exists && existing != nil {
		alert.StartTime = existing.StartTime
	}
	m.preserveAlertState(alertID, alert)
	m.activeAlerts[alertID] = alert
	m.recentAlerts[alertID] = alert
	m.historyManager.AddAlert(*alert)
	m.dispatchAlert(alert, false)
	m.mu.Unlock()

	log.Warn().
		Str("container", containerName).
		Str("host", host.DisplayName).
		Str("health", container.Health).
		Msg("Docker container health alert raised")
}

func (m *Manager) clearDockerContainerHealthAlert(resourceID string) {
	alertID := fmt.Sprintf("docker-container-health-%s", resourceID)
	m.clearAlert(alertID)
}

// checkDockerContainerRestartLoop detects containers stuck in a restart loop
func (m *Manager) checkDockerContainerRestartLoop(host models.DockerHost, container models.DockerContainer, resourceID, containerName, instanceName, nodeName string) {
	alertID := fmt.Sprintf("docker-container-restart-loop-%s", resourceID)
	now := time.Now()

	// Get config values with defaults
	restartThreshold := m.config.DockerDefaults.RestartCount
	if restartThreshold == 0 {
		restartThreshold = 3 // Default: 3 restarts
	}
	timeWindow := m.config.DockerDefaults.RestartWindow
	if timeWindow == 0 {
		timeWindow = 300 // Default: 5 minutes (300 seconds)
	}

	m.mu.Lock()

	record, exists := m.dockerRestartTracking[resourceID]
	if !exists {
		record = &dockerRestartRecord{
			count:       container.RestartCount,
			lastCount:   container.RestartCount,
			times:       []time.Time{},
			lastChecked: now,
		}
		m.dockerRestartTracking[resourceID] = record
		m.mu.Unlock()
		return
	}

	// If restart count increased, track it
	if container.RestartCount > record.lastCount {
		newRestarts := container.RestartCount - record.lastCount
		for i := 0; i < newRestarts; i++ {
			record.times = append(record.times, now)
		}
		record.lastCount = container.RestartCount
	}

	// Clean up old restart times outside the window
	cutoff := now.Add(-time.Duration(timeWindow) * time.Second)
	var recentRestarts []time.Time
	for _, t := range record.times {
		if t.After(cutoff) {
			recentRestarts = append(recentRestarts, t)
		}
	}
	record.times = recentRestarts
	record.lastChecked = now

	recentCount := len(record.times)
	m.mu.Unlock()

	// Check if we have a restart loop
	if recentCount > restartThreshold {
		level := AlertLevelCritical

		alert := &Alert{
			ID:           alertID,
			Type:         "docker-container-restart-loop",
			Level:        level,
			ResourceID:   resourceID,
			ResourceName: containerName,
			Node:         nodeName,
			Instance:     instanceName,
			Message:      fmt.Sprintf("Docker container '%s' has restarted %d times in the last %d minutes (restart loop detected)", containerName, recentCount, timeWindow/60),
			StartTime:    now,
			LastSeen:     now,
			Metadata: map[string]interface{}{
				"hostId":         host.ID,
				"hostName":       host.DisplayName,
				"containerId":    container.ID,
				"containerName":  containerName,
				"image":          container.Image,
				"state":          container.State,
				"status":         container.Status,
				"restartCount":   container.RestartCount,
				"recentRestarts": recentCount,
			},
		}

		m.mu.Lock()
		if existing, exists := m.activeAlerts[alertID]; exists && existing != nil {
			alert.StartTime = existing.StartTime
		}
		m.preserveAlertState(alertID, alert)
		m.activeAlerts[alertID] = alert
		m.recentAlerts[alertID] = alert
		m.historyManager.AddAlert(*alert)
		m.dispatchAlert(alert, false)
		m.mu.Unlock()

		log.Warn().
			Str("container", containerName).
			Str("host", host.DisplayName).
			Int("restarts", recentCount).
			Msg("Docker container restart loop detected")
	} else {
		// Clear alert if restart loop has stopped
		m.clearAlert(alertID)
	}
}

// checkDockerContainerOOMKill detects when a container was killed due to out of memory
func (m *Manager) checkDockerContainerOOMKill(host models.DockerHost, container models.DockerContainer, resourceID, containerName, instanceName, nodeName string) {
	alertID := fmt.Sprintf("docker-container-oom-%s", resourceID)

	// Exit code 137 means the container was killed by SIGKILL, often due to OOM
	// Only alert if the container exited (not running) with exit code 137
	state := strings.ToLower(strings.TrimSpace(container.State))
	if (state == "exited" || state == "dead") && container.ExitCode == 137 {
		m.mu.Lock()
		lastExitCode, tracked := m.dockerLastExitCode[resourceID]

		// Only alert if this is a new OOM kill (exit code changed to 137)
		if !tracked || lastExitCode != 137 {
			m.dockerLastExitCode[resourceID] = 137
			m.mu.Unlock()

			level := AlertLevelCritical

			alert := &Alert{
				ID:           alertID,
				Type:         "docker-container-oom-kill",
				Level:        level,
				ResourceID:   resourceID,
				ResourceName: containerName,
				Node:         nodeName,
				Instance:     instanceName,
				Message:      fmt.Sprintf("Docker container '%s' was killed due to out of memory (OOM)", containerName),
				StartTime:    time.Now(),
				LastSeen:     time.Now(),
				Metadata: map[string]interface{}{
					"hostId":           host.ID,
					"hostName":         host.DisplayName,
					"containerId":      container.ID,
					"containerName":    containerName,
					"image":            container.Image,
					"state":            container.State,
					"status":           container.Status,
					"exitCode":         container.ExitCode,
					"memoryUsageBytes": container.MemoryUsage,
					"memoryLimitBytes": container.MemoryLimit,
				},
			}

			m.mu.Lock()
			if existing, exists := m.activeAlerts[alertID]; exists && existing != nil {
				alert.StartTime = existing.StartTime
			}
			m.preserveAlertState(alertID, alert)
			m.activeAlerts[alertID] = alert
			m.recentAlerts[alertID] = alert
			m.historyManager.AddAlert(*alert)
			m.dispatchAlert(alert, false)
			m.mu.Unlock()

			log.Error().
				Str("container", containerName).
				Str("host", host.DisplayName).
				Int64("memoryUsage", container.MemoryUsage).
				Int64("memoryLimit", container.MemoryLimit).
				Msg("Docker container OOM killed")
		} else {
			m.mu.Unlock()
		}
	} else {
		// Update last exit code if it changed
		if container.ExitCode != 0 {
			m.mu.Lock()
			m.dockerLastExitCode[resourceID] = container.ExitCode
			m.mu.Unlock()
		}
		// Clear OOM alert if container is running or exited with different code
		m.clearAlert(alertID)
	}
}

// checkDockerContainerMemoryLimit alerts when container approaches its memory limit
func (m *Manager) checkDockerContainerMemoryLimit(host models.DockerHost, container models.DockerContainer, resourceID, containerName, instanceName, nodeName string) {
	// Only check if container is running and has a memory limit
	state := strings.ToLower(strings.TrimSpace(container.State))
	if state != "running" || container.MemoryLimit <= 0 {
		return
	}

	alertID := fmt.Sprintf("docker-container-memory-limit-%s", resourceID)

	// Get config values with defaults
	warnThreshold := float64(m.config.DockerDefaults.MemoryWarnPct)
	if warnThreshold == 0 {
		warnThreshold = 90.0 // Default: 90%
	}
	criticalThreshold := float64(m.config.DockerDefaults.MemoryCriticalPct)
	if criticalThreshold == 0 {
		criticalThreshold = 95.0 // Default: 95%
	}

	// Calculate percentage of limit used
	limitPercent := (float64(container.MemoryUsage) / float64(container.MemoryLimit)) * 100

	if limitPercent >= warnThreshold {
		level := AlertLevelWarning
		if limitPercent >= criticalThreshold {
			level = AlertLevelCritical
		}

		alert := &Alert{
			ID:           alertID,
			Type:         "docker-container-memory-limit",
			Level:        level,
			ResourceID:   resourceID,
			ResourceName: containerName,
			Node:         nodeName,
			Instance:     instanceName,
			Message:      fmt.Sprintf("Docker container '%s' is using %.1f%% of its memory limit (%d MB / %d MB)", containerName, limitPercent, container.MemoryUsage/(1024*1024), container.MemoryLimit/(1024*1024)),
			StartTime:    time.Now(),
			LastSeen:     time.Now(),
			Metadata: map[string]interface{}{
				"hostId":           host.ID,
				"hostName":         host.DisplayName,
				"containerId":      container.ID,
				"containerName":    containerName,
				"image":            container.Image,
				"memoryUsageBytes": container.MemoryUsage,
				"memoryLimitBytes": container.MemoryLimit,
				"limitPercent":     limitPercent,
			},
		}

		m.mu.Lock()
		if existing, exists := m.activeAlerts[alertID]; exists && existing != nil {
			alert.StartTime = existing.StartTime
			existing.LastSeen = time.Now()
			existing.Level = level
			existing.Message = alert.Message
			existing.Metadata = alert.Metadata
			m.mu.Unlock()
			return
		}
		m.preserveAlertState(alertID, alert)
		m.activeAlerts[alertID] = alert
		m.recentAlerts[alertID] = alert
		m.historyManager.AddAlert(*alert)
		m.dispatchAlert(alert, false)
		m.mu.Unlock()

		log.Warn().
			Str("container", containerName).
			Str("host", host.DisplayName).
			Float64("limitPercent", limitPercent).
			Msg("Docker container approaching memory limit")
	} else {
		// Clear alert if below warning threshold minus 5% (hysteresis)
		clearThreshold := warnThreshold - 5
		if limitPercent < clearThreshold {
			m.clearAlert(alertID)
		}
	}
}

func (m *Manager) clearDockerContainerMetricAlerts(resourceID string, metrics ...string) {
	if len(metrics) == 0 {
		metrics = []string{"cpu", "memory"}
	}
	for _, metric := range metrics {
		alertID := fmt.Sprintf("%s-%s", resourceID, metric)
		m.clearAlert(alertID)
	}
}

func (m *Manager) cleanupDockerContainerAlerts(host models.DockerHost, seen map[string]struct{}) {
	prefix := fmt.Sprintf("docker:%s/", strings.TrimSpace(host.ID))

	m.mu.Lock()
	toClear := make([]string, 0)
	for alertID, alert := range m.activeAlerts {
		if !strings.HasPrefix(alert.ResourceID, prefix) {
			continue
		}
		if _, exists := seen[alert.ResourceID]; exists {
			continue
		}
		toClear = append(toClear, alertID)
	}
	for resourceID := range m.dockerStateConfirm {
		if strings.HasPrefix(resourceID, prefix) {
			if _, exists := seen[resourceID]; !exists {
				delete(m.dockerStateConfirm, resourceID)
			}
		}
	}
	m.mu.Unlock()

	for _, alertID := range toClear {
		m.clearAlert(alertID)
	}
}

func (m *Manager) clearDockerHostContainerAlerts(hostID string) {
	prefix := fmt.Sprintf("docker:%s/", strings.TrimSpace(hostID))

	m.mu.Lock()
	toClear := make([]string, 0)
	for alertID, alert := range m.activeAlerts {
		if strings.HasPrefix(alert.ResourceID, prefix) {
			toClear = append(toClear, alertID)
		}
	}
	for resourceID := range m.dockerStateConfirm {
		if strings.HasPrefix(resourceID, prefix) {
			delete(m.dockerStateConfirm, resourceID)
		}
	}
	for resourceID := range m.dockerRestartTracking {
		if strings.HasPrefix(resourceID, prefix) {
			delete(m.dockerRestartTracking, resourceID)
		}
	}
	for resourceID := range m.dockerLastExitCode {
		if strings.HasPrefix(resourceID, prefix) {
			delete(m.dockerLastExitCode, resourceID)
		}
	}
	m.mu.Unlock()

	for _, alertID := range toClear {
		m.clearAlert(alertID)
	}
}

// CheckStorage checks storage against thresholds
func (m *Manager) CheckStorage(storage models.Storage) {
	m.mu.RLock()
	if !m.config.Enabled {
		m.mu.RUnlock()
		return
	}
	if m.config.DisableAllStorage {
		m.mu.RUnlock()
		return
	}

	// Check if there's an override for this storage device
	override, hasOverride := m.config.Overrides[storage.ID]
	threshold := m.config.StorageDefault

	// Apply override if it exists for usage threshold
	if hasOverride && override.Usage != nil {
		threshold = *override.Usage
	}
	m.mu.RUnlock()

	// Check if storage is truly offline/unavailable (not just inactive from other nodes)
	// Note: In a cluster, local storage from other nodes shows as inactive which is normal
	if storage.Status == "offline" || storage.Status == "unavailable" {
		m.checkStorageOffline(storage)
	} else {
		// Clear any existing offline alert if storage is back online
		m.clearStorageOfflineAlert(storage)
	}

	// If alerts are disabled for this storage device, clear any existing alerts and return
	if hasOverride && override.Disabled {
		m.mu.Lock()
		// Clear usage alert
		usageAlertID := fmt.Sprintf("%s-usage", storage.ID)
		if _, exists := m.activeAlerts[usageAlertID]; exists {
			m.clearAlertNoLock(usageAlertID)
			log.Info().
				Str("alertID", usageAlertID).
				Str("storage", storage.Name).
				Msg("Cleared usage alert - storage has alerts disabled")
		}
		// Clear offline alert
		offlineAlertID := fmt.Sprintf("storage-offline-%s", storage.ID)
		if _, exists := m.activeAlerts[offlineAlertID]; exists {
			m.clearAlertNoLock(offlineAlertID)
			log.Info().
				Str("alertID", offlineAlertID).
				Str("storage", storage.Name).
				Msg("Cleared offline alert - storage has alerts disabled")
		}
		m.mu.Unlock()
		return
	}

	// Check usage if storage has valid data (even if not currently active on this node)
	// In clusters, storage may show as inactive on nodes where it's not currently mounted
	// but we still want to alert on high usage
	log.Info().
		Str("storage", storage.Name).
		Str("id", storage.ID).
		Float64("usage", storage.Usage).
		Str("status", storage.Status).
		Float64("trigger", threshold.Trigger).
		Float64("clear", threshold.Clear).
		Bool("hasOverride", hasOverride).
		Msg("Checking storage thresholds")

	// Check usage if storage is online - checkMetric will skip if threshold is nil or <= 0
	if storage.Status != "offline" && storage.Status != "unavailable" && storage.Usage > 0 {
		m.checkMetric(storage.ID, storage.Name, storage.Node, storage.Instance, "Storage", "usage", storage.Usage, &threshold, nil)
	}

	// Check ZFS pool status if this is ZFS storage
	if storage.ZFSPool != nil {
		m.checkZFSPoolHealth(storage)
	}
}

func BuildGuestKey(instance, node string, vmid int) string {
	instance = strings.TrimSpace(instance)
	node = strings.TrimSpace(node)
	if instance == "" {
		instance = node
	}
	if instance == node {
		return fmt.Sprintf("%s-%d", node, vmid)
	}
	return fmt.Sprintf("%s-%s-%d", instance, node, vmid)
}

// CheckSnapshotsForInstance evaluates guest snapshots for age-based alerts.
func (m *Manager) CheckSnapshotsForInstance(instanceName string, snapshots []models.GuestSnapshot, guestNames map[string]string) {
	m.mu.RLock()
	enabled := m.config.Enabled
	snapshotCfg := m.config.SnapshotDefaults
	m.mu.RUnlock()

	if !enabled {
		return
	}

	if !snapshotCfg.Enabled {
		m.clearSnapshotAlertsForInstance(instanceName)
		return
	}

	now := time.Now()
	validAlerts := make(map[string]struct{})

	for _, snapshot := range snapshots {
		if instanceName != "" && snapshot.Instance != "" && snapshot.Instance != instanceName {
			continue
		}
		if snapshot.Time.IsZero() {
			continue
		}

		ageHours := now.Sub(snapshot.Time).Hours()
		if ageHours < 0 {
			continue
		}
		ageDays := ageHours / 24

		const gib = 1024.0 * 1024 * 1024
		sizeGiB := 0.0
		if snapshot.SizeBytes > 0 {
			sizeGiB = float64(snapshot.SizeBytes) / gib
		}

		var (
			ageLevel       AlertLevel
			ageThreshold   int
			sizeLevel      AlertLevel
			sizeThreshold  float64
			triggeredStats []string
		)

		if snapshotCfg.CriticalDays > 0 && ageDays >= float64(snapshotCfg.CriticalDays) {
			ageLevel = AlertLevelCritical
			ageThreshold = snapshotCfg.CriticalDays
			triggeredStats = append(triggeredStats, "age")
		} else if snapshotCfg.WarningDays > 0 && ageDays >= float64(snapshotCfg.WarningDays) {
			ageLevel = AlertLevelWarning
			ageThreshold = snapshotCfg.WarningDays
			triggeredStats = append(triggeredStats, "age")
		}

		if snapshot.SizeBytes > 0 {
			if snapshotCfg.CriticalSizeGiB > 0 && sizeGiB >= snapshotCfg.CriticalSizeGiB {
				sizeLevel = AlertLevelCritical
				sizeThreshold = snapshotCfg.CriticalSizeGiB
				triggeredStats = append(triggeredStats, "size")
			} else if snapshotCfg.WarningSizeGiB > 0 && sizeGiB >= snapshotCfg.WarningSizeGiB {
				sizeLevel = AlertLevelWarning
				sizeThreshold = snapshotCfg.WarningSizeGiB
				triggeredStats = append(triggeredStats, "size")
			}
		}

		if ageLevel == "" && sizeLevel == "" {
			continue
		}

		var level AlertLevel
		switch {
		case ageLevel == AlertLevelCritical || sizeLevel == AlertLevelCritical:
			level = AlertLevelCritical
		case ageLevel == AlertLevelWarning || sizeLevel == AlertLevelWarning:
			level = AlertLevelWarning
		default:
			continue
		}

		useSizePrimary := false
		if sizeLevel == AlertLevelCritical && ageLevel != AlertLevelCritical {
			useSizePrimary = true
		} else if sizeLevel != "" && ageLevel == "" {
			useSizePrimary = true
		}

		alertID := fmt.Sprintf("snapshot-age-%s", snapshot.ID)
		validAlerts[alertID] = struct{}{}

		guestKey := BuildGuestKey(snapshot.Instance, snapshot.Node, snapshot.VMID)
		guestName := strings.TrimSpace(guestNames[guestKey])

		guestType := "VM"
		if strings.EqualFold(snapshot.Type, "lxc") {
			guestType = "Container"
		}

		if guestName == "" {
			switch guestType {
			case "Container":
				guestName = fmt.Sprintf("CT %d", snapshot.VMID)
			default:
				guestName = fmt.Sprintf("VM %d", snapshot.VMID)
			}
		}

		snapshotName := strings.TrimSpace(snapshot.Name)
		if snapshotName == "" {
			snapshotName = "(unnamed)"
		}

		ageDaysRounded := math.Round(ageDays*10) / 10
		sizeGiBRounded := math.Round(sizeGiB*10) / 10
		reasons := make([]string, 0, 2)
		if ageLevel != "" {
			reasons = append(reasons, fmt.Sprintf("%.1f days old (threshold %d days)", ageDaysRounded, ageThreshold))
		}
		if sizeLevel != "" {
			reasons = append(reasons, fmt.Sprintf("%.1f GiB (threshold %.1f GiB)", sizeGiBRounded, sizeThreshold))
		}
		reasonText := strings.Join(reasons, " and ")
		message := fmt.Sprintf(
			"%s snapshot '%s' for %s is %s on %s",
			guestType,
			snapshotName,
			guestName,
			reasonText,
			snapshot.Node,
		)

		alertValue := ageDays
		alertThreshold := float64(ageThreshold)
		thresholdTime := now
		if useSizePrimary {
			alertValue = sizeGiB
			alertThreshold = sizeThreshold
		} else if ageThreshold > 0 {
			thresholdTime = snapshot.Time.Add(time.Duration(ageThreshold) * 24 * time.Hour)
			if thresholdTime.After(now) {
				thresholdTime = now
			}
		}

		metadata := map[string]interface{}{
			"snapshotName":      snapshot.Name,
			"snapshotCreatedAt": snapshot.Time,
			"snapshotAgeDays":   ageDays,
			"snapshotAgeHours":  ageHours,
			"snapshotSizeBytes": snapshot.SizeBytes,
			"snapshotSizeGiB":   sizeGiB,
			"guestName":         guestName,
			"guestType":         guestType,
			"guestInstance":     snapshot.Instance,
			"guestNode":         snapshot.Node,
			"guestVmid":         snapshot.VMID,
			"triggeredMetrics":  triggeredStats,
			"primaryMetric":     "age",
		}
		if useSizePrimary {
			metadata["primaryMetric"] = "size"
		}
		if ageLevel != "" {
			metadata["thresholdDays"] = ageThreshold
		}
		if sizeLevel != "" {
			metadata["thresholdSizeGiB"] = sizeThreshold
		}

		resourceName := fmt.Sprintf("%s snapshot '%s'", guestName, snapshotName)

		m.mu.Lock()
		if existing, exists := m.activeAlerts[alertID]; exists {
			existing.LastSeen = now
			existing.Level = level
			existing.Value = alertValue
			existing.Threshold = alertThreshold
			existing.Message = message
			existing.ResourceName = resourceName
			if existing.Metadata == nil {
				existing.Metadata = make(map[string]interface{})
			}
			for k, v := range metadata {
				existing.Metadata[k] = v
			}
			m.mu.Unlock()
			continue
		}

		alert := &Alert{
			ID:           alertID,
			Type:         "snapshot-age",
			Level:        level,
			ResourceID:   snapshot.ID,
			ResourceName: resourceName,
			Node:         snapshot.Node,
			Instance:     snapshot.Instance,
			Message:      message,
			Value:        alertValue,
			Threshold:    alertThreshold,
			StartTime:    thresholdTime,
			LastSeen:     now,
			Metadata:     metadata,
		}

		m.preserveAlertState(alertID, alert)

		m.activeAlerts[alertID] = alert
		m.recentAlerts[alertID] = alert
		m.historyManager.AddAlert(*alert)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine (snapshot)")
				}
			}()
			if err := m.SaveActiveAlerts(); err != nil {
				log.Error().Err(err).Msg("Failed to save active alerts after snapshot alert creation")
			}
		}()

		if !m.checkRateLimit(alertID) {
			m.mu.Unlock()
			log.Debug().
				Str("alertID", alertID).
				Str("guest", guestName).
				Msg("Snapshot alert suppressed due to rate limit")
			continue
		}

		if m.onAlert != nil {
			nowCopy := now
			alert.LastNotified = &nowCopy
			if m.dispatchAlert(alert, true) {
				log.Info().
					Str("alertID", alertID).
					Str("guest", guestName).
					Msg("Snapshot age alert dispatched")
			} else {
				alert.LastNotified = nil
			}
		} else {
			log.Warn().
				Str("alertID", alertID).
				Msg("Snapshot age alert created but no onAlert callback set")
		}

		m.mu.Unlock()
	}

	m.mu.Lock()
	for alertID, alert := range m.activeAlerts {
		if alert == nil || alert.Type != "snapshot-age" {
			continue
		}
		if instanceName != "" && alert.Instance != instanceName {
			continue
		}
		if _, ok := validAlerts[alertID]; ok {
			continue
		}
		m.clearAlertNoLock(alertID)
	}
	m.mu.Unlock()
}

// CheckBackups evaluates storage, PBS, and PMG backups for age-based alerts.
func (m *Manager) CheckBackups(
	storageBackups []models.StorageBackup,
	pbsBackups []models.PBSBackup,
	pmgBackups []models.PMGBackup,
	guestsByKey map[string]GuestLookup,
	guestsByVMID map[string]GuestLookup,
) {
	m.mu.RLock()
	enabled := m.config.Enabled
	backupCfg := m.config.BackupDefaults
	m.mu.RUnlock()

	if !enabled || !backupCfg.Enabled {
		m.clearBackupAlerts()
		return
	}

	if backupCfg.WarningDays <= 0 && backupCfg.CriticalDays <= 0 {
		m.clearBackupAlerts()
		return
	}

	type backupRecord struct {
		key          string
		lookup       GuestLookup
		fallbackName string
		instance     string
		node         string
		source       string
		storage      string
		datastore    string
		backupType   string
		filename     string
		lastTime     time.Time
	}

	records := make(map[string]*backupRecord)

	updateRecord := func(key string, candidate backupRecord) {
		if key == "" {
			return
		}
		if existing, ok := records[key]; ok {
			if candidate.lastTime.After(existing.lastTime) {
				*existing = candidate
			}
			return
		}
		record := candidate
		records[key] = &record
	}

	now := time.Now()

	for _, backup := range storageBackups {
		if backup.Time.IsZero() {
			continue
		}

		key := BuildGuestKey(backup.Instance, backup.Node, backup.VMID)
		info := guestsByKey[key]
		displayName := info.Name
		if displayName == "" {
			displayName = fmt.Sprintf("%s-%d", sanitizeAlertKey(backup.Node), backup.VMID)
		}

		updateRecord(key, backupRecord{
			key:          key,
			lookup:       info,
			fallbackName: displayName,
			instance:     backup.Instance,
			node:         backup.Node,
			source:       "PVE storage",
			storage:      backup.Storage,
			backupType:   backup.Type,
			lastTime:     backup.Time,
		})
	}

	for _, backup := range pbsBackups {
		if backup.BackupTime.IsZero() {
			continue
		}
		if backup.VMID == "0" {
			// Host configuration backups - skip from age alerts
			continue
		}

		info, exists := guestsByVMID[backup.VMID]
		var key string
		var displayName string
		var instance string
		var node string

		if exists && info.Instance != "" && info.Node != "" {
			key = BuildGuestKey(info.Instance, info.Node, info.VMID)
			displayName = info.Name
			instance = info.Instance
			node = info.Node
		} else {
			key = fmt.Sprintf("pbs:%s:%s:%s", backup.Instance, backup.BackupType, backup.VMID)
			displayName = fmt.Sprintf("VMID %s", backup.VMID)
			instance = fmt.Sprintf("PBS:%s", backup.Instance)
			node = backup.Datastore
		}

		updateRecord(key, backupRecord{
			key:          key,
			lookup:       info,
			fallbackName: displayName,
			instance:     instance,
			node:         node,
			source:       "PBS",
			datastore:    backup.Datastore,
			backupType:   backup.BackupType,
			lastTime:     backup.BackupTime,
		})
	}

	for _, backup := range pmgBackups {
		if backup.BackupTime.IsZero() {
			continue
		}

		instanceLabel := strings.TrimSpace(backup.Instance)
		if instanceLabel == "" {
			instanceLabel = "PMG"
		}

		nodeName := strings.TrimSpace(backup.Node)
		keyComponent := nodeName
		if keyComponent == "" {
			keyComponent = strings.TrimSpace(backup.Filename)
		}
		if keyComponent == "" {
			keyComponent = "unknown"
		}

		displayName := nodeName
		if displayName == "" {
			displayName = instanceLabel
		}
		if displayName == "" {
			displayName = "PMG gateway"
		} else {
			displayName = fmt.Sprintf("PMG %s", displayName)
		}

		instanceField := fmt.Sprintf("PMG:%s", instanceLabel)
		key := fmt.Sprintf("pmg:%s:%s", instanceLabel, keyComponent)

		updateRecord(key, backupRecord{
			key:          key,
			fallbackName: displayName,
			instance:     instanceField,
			node:         nodeName,
			source:       "PMG",
			backupType:   "pmg",
			filename:     backup.Filename,
			lastTime:     backup.BackupTime,
		})
	}

	if len(records) == 0 {
		m.clearBackupAlerts()
		return
	}

	validAlerts := make(map[string]struct{})

	for key, record := range records {
		age := now.Sub(record.lastTime)
		if age < 0 {
			continue
		}

		ageDays := age.Hours() / 24
		if ageDays < 0 {
			continue
		}
		ageDaysRounded := math.Round(ageDays*10) / 10

		var level AlertLevel
		var threshold int
		switch {
		case backupCfg.CriticalDays > 0 && ageDays >= float64(backupCfg.CriticalDays):
			level = AlertLevelCritical
			threshold = backupCfg.CriticalDays
		case backupCfg.WarningDays > 0 && ageDays >= float64(backupCfg.WarningDays):
			level = AlertLevelWarning
			threshold = backupCfg.WarningDays
		default:
			continue
		}

		alertKey := sanitizeAlertKey(key)
		alertID := fmt.Sprintf("backup-age-%s", alertKey)
		validAlerts[alertID] = struct{}{}

		displayName := record.lookup.Name
		if displayName == "" {
			displayName = record.fallbackName
		}
		if displayName == "" {
			displayName = "Unknown guest"
		}

		node := record.node
		if node == "" {
			node = record.lookup.Node
		}
		instance := record.instance
		if instance == "" {
			instance = record.lookup.Instance
		}

		thresholdTime := record.lastTime.Add(time.Duration(threshold) * 24 * time.Hour)
		if thresholdTime.After(now) {
			thresholdTime = now
		}

		var sourceLabel string
		switch record.source {
		case "PBS":
			sourceLabel = fmt.Sprintf("PBS datastore %s on %s", record.datastore, strings.TrimPrefix(instance, "PBS:"))
		case "PMG":
			if node != "" {
				sourceLabel = fmt.Sprintf("PMG node %s", node)
			} else {
				sourceLabel = "PMG"
			}
		default:
			sourceLabel = fmt.Sprintf("storage %s on %s", record.storage, node)
		}

		message := fmt.Sprintf(
			"%s backup via %s is %.1f days old (threshold: %d days)",
			displayName,
			sourceLabel,
			ageDaysRounded,
			threshold,
		)

		metadata := map[string]interface{}{
			"source":         record.source,
			"lastBackupTime": record.lastTime,
			"ageDays":        ageDays,
			"thresholdDays":  threshold,
		}
		if record.storage != "" {
			metadata["storage"] = record.storage
		}
		if record.datastore != "" {
			metadata["datastore"] = record.datastore
		}
		if record.backupType != "" {
			metadata["backupType"] = record.backupType
		}
		if record.filename != "" {
			metadata["filename"] = record.filename
		}

		m.mu.Lock()
		if existing, exists := m.activeAlerts[alertID]; exists {
			existing.LastSeen = now
			existing.Level = level
			existing.Value = ageDays
			existing.Threshold = float64(threshold)
			existing.Message = message
			if existing.Metadata == nil {
				existing.Metadata = make(map[string]interface{})
			}
			for k, v := range metadata {
				existing.Metadata[k] = v
			}
			m.mu.Unlock()
			continue
		}

		alert := &Alert{
			ID:           alertID,
			Type:         "backup-age",
			Level:        level,
			ResourceID:   alertKey,
			ResourceName: fmt.Sprintf("%s backup", displayName),
			Node:         node,
			Instance:     instance,
			Message:      message,
			Value:        ageDays,
			Threshold:    float64(threshold),
			StartTime:    thresholdTime,
			LastSeen:     now,
			Metadata:     metadata,
		}

		m.preserveAlertState(alertID, alert)

		m.activeAlerts[alertID] = alert
		m.recentAlerts[alertID] = alert
		m.historyManager.AddAlert(*alert)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine (backup)")
				}
			}()
			if err := m.SaveActiveAlerts(); err != nil {
				log.Error().Err(err).Msg("Failed to save active alerts after backup alert creation")
			}
		}()

		if !m.checkRateLimit(alertID) {
			m.mu.Unlock()
			log.Debug().
				Str("alertID", alertID).
				Str("resource", displayName).
				Msg("Backup alert suppressed due to rate limit")
			continue
		}

		if m.onAlert != nil {
			notified := now
			alert.LastNotified = &notified
			if m.dispatchAlert(alert, true) {
				log.Info().
					Str("alertID", alertID).
					Str("resource", displayName).
					Msg("Backup age alert dispatched")
			} else {
				alert.LastNotified = nil
			}
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	for alertID, alert := range m.activeAlerts {
		if alert == nil || alert.Type != "backup-age" {
			continue
		}
		if _, ok := validAlerts[alertID]; ok {
			continue
		}
		m.clearAlertNoLock(alertID)
	}
	m.mu.Unlock()
}

// checkZFSPoolHealth checks ZFS pool for errors and degraded state
func (m *Manager) checkZFSPoolHealth(storage models.Storage) {
	pool := storage.ZFSPool
	if pool == nil {
		return
	}

	// Check pool state (DEGRADED, FAULTED, etc.)
	stateAlertID := fmt.Sprintf("zfs-pool-state-%s", storage.ID)
	if pool.State != "ONLINE" {
		level := AlertLevelWarning
		if pool.State == "FAULTED" || pool.State == "UNAVAIL" {
			level = AlertLevelCritical
		}

		m.mu.Lock()
		if _, exists := m.activeAlerts[stateAlertID]; !exists {
			alert := &Alert{
				ID:           stateAlertID,
				Type:         "zfs-pool-state",
				Level:        level,
				ResourceID:   storage.ID,
				ResourceName: fmt.Sprintf("%s (%s)", storage.Name, pool.Name),
				Node:         storage.Node,
				Instance:     storage.Instance,
				Message:      fmt.Sprintf("ZFS pool '%s' is %s", pool.Name, pool.State),
				Value:        0,
				Threshold:    0,
				StartTime:    time.Now(),
				LastSeen:     time.Now(),
				Metadata: map[string]interface{}{
					"pool_name":  pool.Name,
					"pool_state": pool.State,
				},
			}

			m.preserveAlertState(stateAlertID, alert)

			m.activeAlerts[stateAlertID] = alert
			m.recentAlerts[stateAlertID] = alert
			m.historyManager.AddAlert(*alert)

			m.dispatchAlert(alert, false)

			log.Warn().
				Str("pool", pool.Name).
				Str("state", pool.State).
				Str("node", storage.Node).
				Msg("ZFS pool is not healthy")
		}
		m.mu.Unlock()
	} else {
		// Clear state alert if pool is back online
		m.clearAlert(stateAlertID)
	}

	// Check for read/write/checksum errors
	totalErrors := pool.ReadErrors + pool.WriteErrors + pool.ChecksumErrors
	errorsAlertID := fmt.Sprintf("zfs-pool-errors-%s", storage.ID)
	if totalErrors > 0 {
		m.mu.Lock()
		existingAlert, exists := m.activeAlerts[errorsAlertID]

		// Only create new alert or update if error count increased
		if !exists || float64(totalErrors) > existingAlert.Value {
			alert := &Alert{
				ID:           errorsAlertID,
				Type:         "zfs-pool-errors",
				Level:        AlertLevelWarning,
				ResourceID:   storage.ID,
				ResourceName: fmt.Sprintf("%s (%s)", storage.Name, pool.Name),
				Node:         storage.Node,
				Instance:     storage.Instance,
				Message: fmt.Sprintf("ZFS pool '%s' has errors: %d read, %d write, %d checksum",
					pool.Name, pool.ReadErrors, pool.WriteErrors, pool.ChecksumErrors),
				Value:     float64(totalErrors),
				Threshold: 0,
				StartTime: time.Now(),
				LastSeen:  time.Now(),
				Metadata: map[string]interface{}{
					"pool_name":       pool.Name,
					"read_errors":     pool.ReadErrors,
					"write_errors":    pool.WriteErrors,
					"checksum_errors": pool.ChecksumErrors,
				},
			}

			if exists {
				// Preserve original start time when updating
				alert.StartTime = existingAlert.StartTime
			}

			m.preserveAlertState(errorsAlertID, alert)

			m.activeAlerts[errorsAlertID] = alert
			m.recentAlerts[errorsAlertID] = alert
			m.historyManager.AddAlert(*alert)

			m.dispatchAlert(alert, false)

			log.Error().
				Str("pool", pool.Name).
				Int64("read_errors", pool.ReadErrors).
				Int64("write_errors", pool.WriteErrors).
				Int64("checksum_errors", pool.ChecksumErrors).
				Str("node", storage.Node).
				Msg("ZFS pool has I/O errors")
		}
		m.mu.Unlock()
	} else {
		m.clearAlert(errorsAlertID)
	}

	// Check individual devices for errors
	m.mu.Lock()
	for _, device := range pool.Devices {
		alertID := fmt.Sprintf("zfs-device-%s-%s", storage.ID, device.Name)

		// Skip SPARE devices unless they have actual errors
		if (device.State != "ONLINE" && device.State != "SPARE") || device.ReadErrors > 0 || device.WriteErrors > 0 || device.ChecksumErrors > 0 {
			if _, exists := m.activeAlerts[alertID]; !exists {
				level := AlertLevelWarning
				if device.State == "FAULTED" || device.State == "UNAVAIL" {
					level = AlertLevelCritical
				}

				message := fmt.Sprintf("ZFS device '%s' in pool '%s'", device.Name, pool.Name)
				if device.State != "ONLINE" {
					message += fmt.Sprintf(" is %s", device.State)
				}
				if device.ReadErrors > 0 || device.WriteErrors > 0 || device.ChecksumErrors > 0 {
					message += fmt.Sprintf(" has errors: %d read, %d write, %d checksum",
						device.ReadErrors, device.WriteErrors, device.ChecksumErrors)
				}

				alert := &Alert{
					ID:           alertID,
					Type:         "zfs-device",
					Level:        level,
					ResourceID:   storage.ID,
					ResourceName: fmt.Sprintf("%s (%s/%s)", storage.Name, pool.Name, device.Name),
					Node:         storage.Node,
					Instance:     storage.Instance,
					Message:      message,
					Value:        float64(device.ReadErrors + device.WriteErrors + device.ChecksumErrors),
					Threshold:    0,
					StartTime:    time.Now(),
					LastSeen:     time.Now(),
					Metadata: map[string]interface{}{
						"pool_name":       pool.Name,
						"device_name":     device.Name,
						"device_state":    device.State,
						"read_errors":     device.ReadErrors,
						"write_errors":    device.WriteErrors,
						"checksum_errors": device.ChecksumErrors,
					},
				}

				m.preserveAlertState(alertID, alert)

				m.activeAlerts[alertID] = alert
				m.recentAlerts[alertID] = alert
				m.historyManager.AddAlert(*alert)

				m.dispatchAlert(alert, false)

				log.Warn().
					Str("pool", pool.Name).
					Str("device", device.Name).
					Str("state", device.State).
					Int64("errors", device.ReadErrors+device.WriteErrors+device.ChecksumErrors).
					Str("node", storage.Node).
					Msg("ZFS device has issues")
			}
		} else {
			// Clear device alert if it's back to normal
			m.clearAlertNoLock(alertID)
		}
	}
	m.mu.Unlock()
}

// clearAlert removes an alert if it exists
func (m *Manager) clearAlert(alertID string) {
	m.mu.Lock()
	alert, exists := m.activeAlerts[alertID]
	if exists {
		m.removeActiveAlertNoLock(alertID)
	}
	m.mu.Unlock()

	if !exists {
		return
	}

	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}

	m.addRecentlyResolvedUnlocked(alertID, resolvedAlert)

	m.safeCallResolvedCallback(alertID, false)

	log.Info().
		Str("alertID", alertID).
		Msg("Alert cleared")
}

// getTimeThreshold determines the delay to apply for a metric/resource combination.
func (m *Manager) getTimeThreshold(_ string, resourceType, metricType string) int {
	if delay, ok := m.getMetricTimeThreshold(resourceType, metricType); ok {
		return delay
	}

	base, hasTypeSpecific := m.getBaseTimeThreshold(resourceType)

	if !hasTypeSpecific {
		if delay, ok := m.getGlobalMetricTimeThreshold(metricType); ok {
			return delay
		}
	}

	return base
}

// getMetricTimeThreshold returns a metric-specific delay if configured at the resource-type level.
func (m *Manager) getMetricTimeThreshold(resourceType, metricType string) (int, bool) {
	if len(m.config.MetricTimeThresholds) == 0 {
		return 0, false
	}

	metricKey := strings.ToLower(strings.TrimSpace(metricType))
	if metricKey == "" {
		return 0, false
	}

	for _, typeKey := range canonicalResourceTypeKeys(resourceType) {
		perType, ok := m.config.MetricTimeThresholds[typeKey]
		if !ok || len(perType) == 0 {
			continue
		}

		if delay, ok := perType[metricKey]; ok {
			return delay, true
		}
		if delay, ok := perType["default"]; ok {
			return delay, true
		}
		if delay, ok := perType["_default"]; ok {
			return delay, true
		}
		if delay, ok := perType["*"]; ok {
			return delay, true
		}
	}

	return 0, false
}

// getBaseTimeThreshold returns the resource-type level delay.
func (m *Manager) getBaseTimeThreshold(resourceType string) (int, bool) {
	if m.config.TimeThresholds != nil {
		for _, key := range canonicalResourceTypeKeys(resourceType) {
			if delay, ok := m.config.TimeThresholds[key]; ok {
				return delay, true
			}
		}
		if delay, ok := m.config.TimeThresholds["all"]; ok {
			return delay, false
		}
	}

	return m.config.TimeThreshold, false
}

func (m *Manager) getGlobalMetricTimeThreshold(metricType string) (int, bool) {
	if len(m.config.MetricTimeThresholds) == 0 {
		return 0, false
	}

	perType, ok := m.config.MetricTimeThresholds["all"]
	if !ok || len(perType) == 0 {
		return 0, false
	}

	metricKey := strings.ToLower(strings.TrimSpace(metricType))
	if metricKey == "" {
		return 0, false
	}

	if delay, ok := perType[metricKey]; ok {
		return delay, true
	}
	if delay, ok := perType["default"]; ok {
		return delay, true
	}
	if delay, ok := perType["_default"]; ok {
		return delay, true
	}
	if delay, ok := perType["*"]; ok {
		return delay, true
	}

	return 0, false
}

func canonicalResourceTypeKeys(resourceType string) []string {
	typeKey := strings.ToLower(strings.TrimSpace(resourceType))

	addUnique := func(slice []string, value string) []string {
		if value == "" {
			return slice
		}
		for _, existing := range slice {
			if existing == value {
				return slice
			}
		}
		return append(slice, value)
	}

	var keys []string
	switch typeKey {
	case "guest", "qemu", "vm", "ct", "container", "lxc":
		keys = addUnique(keys, "guest")
	case "docker", "docker container", "dockercontainer":
		keys = addUnique(keys, "docker")
		keys = addUnique(keys, "guest")
	case "docker host", "dockerhost":
		keys = addUnique(keys, "dockerhost")
		keys = addUnique(keys, "docker")
		keys = addUnique(keys, "node")
	case "node":
		keys = addUnique(keys, "node")
	case "pbs", "pbs server", "pbsserver":
		keys = addUnique(keys, "pbs")
		keys = addUnique(keys, "node")
	case "storage":
		keys = addUnique(keys, "storage")
	default:
		keys = addUnique(keys, typeKey)
	}

	return keys
}

// checkMetric checks a single metric against its threshold with hysteresis
type metricOptions struct {
	Metadata map[string]interface{}
	Message  string
}

func (m *Manager) checkMetric(resourceID, resourceName, node, instance, resourceType, metricType string, value float64, threshold *HysteresisThreshold, opts *metricOptions) {
	if threshold == nil || threshold.Trigger <= 0 {
		return
	}

	log.Debug().
		Str("resource", resourceName).
		Str("metric", metricType).
		Float64("value", value).
		Float64("trigger", threshold.Trigger).
		Float64("clear", threshold.Clear).
		Bool("exceeds", value >= threshold.Trigger).
		Msg("Checking metric threshold")

	alertID := fmt.Sprintf("%s-%s", resourceID, metricType)

	m.mu.Lock()
	defer m.mu.Unlock()

	existingAlert, exists := m.activeAlerts[alertID]

	// Check for suppression
	if suppressUntil, suppressed := m.suppressedUntil[alertID]; suppressed && time.Now().Before(suppressUntil) {
		log.Debug().
			Str("alertID", alertID).
			Time("suppressedUntil", suppressUntil).
			Msg("Alert suppressed")
		return
	}

	if value >= threshold.Trigger {
		// Threshold exceeded
		if !exists {
			alertStartTime := time.Now()

			// Determine the appropriate time threshold based on resource/metric type
			timeThreshold := m.getTimeThreshold(resourceID, resourceType, metricType)

			// Check if we have a time threshold configured
			if timeThreshold > 0 {
				// Check if this threshold was already pending
				if pendingTime, isPending := m.pendingAlerts[alertID]; isPending {
					// Check if enough time has passed
					if time.Since(pendingTime) >= time.Duration(timeThreshold)*time.Second {
						// Time threshold met, proceed with alert
						delete(m.pendingAlerts, alertID)
						if !pendingTime.IsZero() {
							alertStartTime = pendingTime
						}
						log.Debug().
							Str("alertID", alertID).
							Int("timeThreshold", timeThreshold).
							Dur("elapsed", time.Since(pendingTime)).
							Msg("Time threshold met, triggering alert")
					} else {
						// Still waiting for time threshold
						log.Debug().
							Str("alertID", alertID).
							Int("timeThreshold", timeThreshold).
							Dur("elapsed", time.Since(pendingTime)).
							Msg("Threshold exceeded but waiting for time threshold")
						return
					}
				} else {
					// First time exceeding threshold, start tracking
					m.pendingAlerts[alertID] = alertStartTime
					log.Debug().
						Str("alertID", alertID).
						Int("timeThreshold", timeThreshold).
						Msg("Threshold exceeded, starting time threshold tracking")
					return
				}
			}

			// Check for recent similar alert to prevent spam
			if recent, hasRecent := m.recentAlerts[alertID]; hasRecent {
				// Check minimum delta
				if m.config.MinimumDelta > 0 &&
					time.Since(recent.StartTime) < time.Duration(m.config.SuppressionWindow)*time.Minute &&
					abs(recent.Value-value) < m.config.MinimumDelta {
					log.Debug().
						Str("alertID", alertID).
						Float64("recentValue", recent.Value).
						Float64("currentValue", value).
						Float64("delta", abs(recent.Value-value)).
						Float64("minimumDelta", m.config.MinimumDelta).
						Msg("Alert suppressed due to minimum delta")

					// Set suppression window
					m.suppressedUntil[alertID] = time.Now().Add(time.Duration(m.config.SuppressionWindow) * time.Minute)
					return
				}
			}

			// New alert
			message := ""
			var unit string
			if opts != nil && opts.Message != "" {
				message = opts.Message
			} else {
				switch metricType {
				case "usage":
					message = fmt.Sprintf("%s at %.1f%%", resourceType, value)
				case "diskRead", "diskWrite", "networkIn", "networkOut":
					message = fmt.Sprintf("%s %s at %.1f MB/s", resourceType, metricType, value)
					unit = "MB/s"
				case "temperature":
					message = fmt.Sprintf("%s %s at %.1f°C", resourceType, metricType, value)
					unit = "°C"
				default:
					message = fmt.Sprintf("%s %s at %.1f%%", resourceType, metricType, value)
				}
			}

			alertMetadata := map[string]interface{}{
				"resourceType":   resourceType,
				"clearThreshold": threshold.Clear,
			}
			if unit != "" {
				alertMetadata["unit"] = unit
			}
			if opts != nil && opts.Metadata != nil {
				for k, v := range opts.Metadata {
					alertMetadata[k] = v
				}
			}

			alert := &Alert{
				ID:           alertID,
				Type:         metricType,
				Level:        AlertLevelWarning,
				ResourceID:   resourceID,
				ResourceName: resourceName,
				Node:         node,
				Instance:     instance,
				Message:      message,
				Value:        value,
				Threshold:    threshold.Trigger,
				StartTime:    alertStartTime,
				LastSeen:     time.Now(),
				Metadata:     alertMetadata,
			}

			// Set level based on how much over threshold
			if value >= threshold.Trigger+10 {
				alert.Level = AlertLevelCritical
			}

			log.Debug().
				Str("alertID", alertID).
				Time("alertStartTime", alertStartTime).
				Time("now", time.Now()).
				Dur("initialDuration", time.Since(alertStartTime)).
				Msg("Creating new alert with start time")

			m.preserveAlertState(alertID, alert)

			m.activeAlerts[alertID] = alert
			m.recentAlerts[alertID] = alert
			m.historyManager.AddAlert(*alert)

			// Save active alerts after adding new one
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine")
					}
				}()
				if err := m.SaveActiveAlerts(); err != nil {
					log.Error().Err(err).Msg("Failed to save active alerts after creation")
				}
			}()

			log.Warn().
				Str("alertID", alertID).
				Str("resource", resourceName).
				Str("metric", metricType).
				Float64("value", value).
				Float64("trigger", threshold.Trigger).
				Float64("clear", threshold.Clear).
				Int("activeAlerts", len(m.activeAlerts)).
				Msg("Alert triggered")

			// Check rate limit (but don't remove alert from tracking)
			if !m.checkRateLimit(alertID) {
				log.Debug().
					Str("alertID", alertID).
					Int("maxPerHour", m.config.Schedule.MaxAlertsHour).
					Msg("Alert notification suppressed due to rate limit")
				// Don't delete the alert, just suppress notifications
				return
			}

			// Notify callback (may be suppressed by quiet hours)
			if m.onAlert != nil {
				now := time.Now()
				alert.LastNotified = &now
				if m.dispatchAlert(alert, true) {
					log.Info().Str("alertID", alertID).Msg("Calling onAlert callback")
				} else {
					alert.LastNotified = nil
				}
			} else {
				log.Warn().Msg("No onAlert callback set!")
			}
		} else {
			// Update existing alert
			existingAlert.LastSeen = time.Now()
			existingAlert.Value = value
			if existingAlert.Metadata == nil {
				existingAlert.Metadata = map[string]interface{}{}
			}
			existingAlert.Metadata["resourceType"] = resourceType
			existingAlert.Metadata["clearThreshold"] = threshold.Clear
			if opts != nil {
				if opts.Message != "" {
					existingAlert.Message = opts.Message
				}
				if opts.Metadata != nil {
					for k, v := range opts.Metadata {
						existingAlert.Metadata[k] = v
					}
				}
			}

			// Update level if needed
			oldLevel := existingAlert.Level
			if value >= threshold.Trigger+10 {
				existingAlert.Level = AlertLevelCritical
			} else {
				existingAlert.Level = AlertLevelWarning
			}

			// Check if we should re-notify based on cooldown period
			shouldRenotify := false
			if m.shouldNotifyAfterCooldown(existingAlert) {
				shouldRenotify = true
				log.Debug().
					Str("alertID", alertID).
					Dur("cooldown", time.Duration(m.config.Schedule.Cooldown)*time.Minute).
					Msg("Cooldown period has passed, will re-notify")
			} else if oldLevel != existingAlert.Level && existingAlert.Level == AlertLevelCritical {
				// Always re-notify if alert escalated to critical
				shouldRenotify = true
				log.Debug().
					Str("alertID", alertID).
					Msg("Alert escalated to critical, will re-notify despite cooldown")
			}

			// Send re-notification if appropriate (may be suppressed by quiet hours)
			if shouldRenotify && m.onAlert != nil {
				now := time.Now()
				existingAlert.LastNotified = &now
				if m.dispatchAlert(existingAlert, false) {
					log.Info().
						Str("alertID", alertID).
						Str("level", string(existingAlert.Level)).
						Msg("Re-notifying for existing alert")
				} else {
					existingAlert.LastNotified = nil
				}
			}
		}
	} else {
		// Value is below trigger threshold
		// Clear any pending alert for this metric
		if _, isPending := m.pendingAlerts[alertID]; isPending {
			delete(m.pendingAlerts, alertID)
			log.Debug().
				Str("alertID", alertID).
				Msg("Value dropped below threshold, clearing pending alert")
		}

		if exists {
			// Use hysteresis for resolution - only resolve if below clear threshold
			clearThreshold := threshold.Clear
			if clearThreshold <= 0 {
				clearThreshold = threshold.Trigger // Fallback to trigger if clear not set
			}

			if value <= clearThreshold {
				// Threshold cleared with hysteresis - auto resolve
				resolvedAlert := &ResolvedAlert{
					Alert:        existingAlert,
					ResolvedTime: time.Now(),
				}

				// Remove from active alerts
				m.removeActiveAlertNoLock(alertID)

				// Save active alerts after resolution
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine (resolution)")
						}
					}()
					if err := m.SaveActiveAlerts(); err != nil {
						log.Error().Err(err).Msg("Failed to save active alerts after resolution")
					}
				}()

				// Add to recently resolved while preventing lock-order inversions
				m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

				log.Info().
					Str("alertID", alertID).
					Msg("Added alert to recently resolved")

				log.Info().
					Str("resource", resourceName).
					Str("metric", metricType).
					Float64("value", value).
					Float64("clearThreshold", clearThreshold).
					Bool("wasAcknowledged", existingAlert.Acknowledged).
					Msg("Alert resolved with hysteresis")

				if m.onResolved != nil {
					go m.onResolved(alertID)
				}
			}
		}
	}
}

func sanitizeAlertKey(label string) string {
	trimmed := strings.TrimSpace(label)
	if trimmed == "" {
		return ""
	}

	if trimmed == "/" {
		return "root"
	}

	trimmed = strings.Trim(trimmed, "/\\ ")
	if trimmed == "" {
		trimmed = "root"
	}

	lower := strings.ToLower(trimmed)
	var builder strings.Builder
	builder.Grow(len(lower))
	prevDash := false
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			prevDash = false
			continue
		}
		if r == '.' {
			builder.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			builder.WriteRune('-')
			prevDash = true
		}
	}

	sanitized := strings.Trim(builder.String(), "-.")
	if sanitized == "" {
		sanitized = "disk"
	}

	return sanitized
}

// abs returns the absolute value of a float64
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// AcknowledgeAlert acknowledges an alert
func (m *Manager) AcknowledgeAlert(alertID, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return fmt.Errorf("alert not found: %s", alertID)
	}

	alert.Acknowledged = true
	now := time.Now()
	alert.AckTime = &now
	alert.AckUser = user

	// Write the modified alert back to the map
	m.activeAlerts[alertID] = alert
	m.ackState[alertID] = ackRecord{
		acknowledged: true,
		user:         user,
		time:         now,
	}

	log.Debug().
		Str("alertID", alertID).
		Str("user", user).
		Time("ackTime", now).
		Msg("Alert acknowledgment recorded")

	return nil
}

// UnacknowledgeAlert removes the acknowledged status from an alert
func (m *Manager) UnacknowledgeAlert(alertID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return fmt.Errorf("alert not found: %s", alertID)
	}

	alert.Acknowledged = false
	alert.AckTime = nil
	alert.AckUser = ""

	// Write the modified alert back to the map
	m.activeAlerts[alertID] = alert
	delete(m.ackState, alertID)

	log.Info().
		Str("alertID", alertID).
		Msg("Alert unacknowledged")

	return nil
}

// preserveAlertState copies acknowledgement and escalation metadata from an existing alert
// into a freshly constructed alert before it replaces the existing entry in the map. This
// prevents UI state from regressing when alerts are rebuilt during polling.
func (m *Manager) preserveAlertState(alertID string, updated *Alert) {
	if updated == nil {
		return
	}

	existing, exists := m.activeAlerts[alertID]
	if exists && existing != nil {
		// Preserve the original start time so duration calculations are correct
		updated.StartTime = existing.StartTime
		updated.Acknowledged = existing.Acknowledged
		updated.AckUser = existing.AckUser
		if existing.AckTime != nil {
			t := *existing.AckTime
			updated.AckTime = &t
		} else {
			updated.AckTime = nil
		}
		updated.LastEscalation = existing.LastEscalation
		if len(existing.EscalationTimes) > 0 {
			updated.EscalationTimes = append([]time.Time(nil), existing.EscalationTimes...)
		} else {
			updated.EscalationTimes = nil
		}

		log.Debug().
			Str("alertID", alertID).
			Time("originalStartTime", existing.StartTime).
			Dur("currentDuration", time.Since(existing.StartTime)).
			Msg("Preserving alert state including StartTime")
		return
	}

	// Fall back to previously recorded acknowledgement state for this alert ID (e.g., flapping alerts)
	if record, ok := m.ackState[alertID]; ok && record.acknowledged {
		updated.Acknowledged = true
		updated.AckUser = record.user
		t := record.time
		updated.AckTime = &t
	}
}

func (m *Manager) removeActiveAlertNoLock(alertID string) {
	delete(m.activeAlerts, alertID)
	delete(m.ackState, alertID)
}

// GetActiveAlerts returns all active alerts
func (m *Manager) GetActiveAlerts() []Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	alerts := make([]Alert, 0, len(m.activeAlerts))
	for _, alert := range m.activeAlerts {
		alerts = append(alerts, *alert)
	}
	return alerts
}

// NotifyExistingAlert re-dispatches a notification for an existing active alert
// Used when activation state changes from pending to active
func (m *Manager) NotifyExistingAlert(alertID string) {
	m.mu.RLock()
	alert, exists := m.activeAlerts[alertID]
	m.mu.RUnlock()

	if !exists {
		return
	}

	// Dispatch notification for existing alert
	m.dispatchAlert(alert, true)
}

// GetRecentlyResolved returns recently resolved alerts
func (m *Manager) GetRecentlyResolved() []models.ResolvedAlert {
	m.resolvedMutex.RLock()
	defer m.resolvedMutex.RUnlock()

	resolved := make([]models.ResolvedAlert, 0, len(m.recentlyResolved))
	for _, alert := range m.recentlyResolved {
		resolved = append(resolved, models.ResolvedAlert{
			Alert: models.Alert{
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
			},
			ResolvedTime: alert.ResolvedTime,
		})
	}
	return resolved
}

// GetAlertHistory returns alert history
func (m *Manager) GetAlertHistory(limit int) []Alert {
	return m.historyManager.GetAllHistory(limit)
}

// GetAlertHistorySince returns alert history entries created after the provided time.
func (m *Manager) GetAlertHistorySince(since time.Time, limit int) []Alert {
	if since.IsZero() {
		return m.GetAlertHistory(limit)
	}

	return m.historyManager.GetHistory(since, limit)
}

// ClearAlertHistory clears all alert history
func (m *Manager) ClearAlertHistory() error {
	return m.historyManager.ClearAllHistory()
}

// checkNodeOffline creates an alert for offline nodes after confirmation
func (m *Manager) checkNodeOffline(node models.Node) {
	alertID := fmt.Sprintf("node-offline-%s", node.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if node connectivity alerts are disabled
	if override, exists := m.config.Overrides[node.ID]; exists && override.DisableConnectivity {
		// Node connectivity alerts are disabled, clear any existing alert and return
		if _, alertExists := m.activeAlerts[alertID]; alertExists {
			m.clearAlertNoLock(alertID)
			log.Debug().
				Str("node", node.Name).
				Msg("Node offline alert cleared (connectivity alerts disabled)")
		}
		delete(m.nodeOfflineCount, node.ID)
		return
	}

	// Check if alert already exists
	if _, exists := m.activeAlerts[alertID]; exists {
		// Alert already exists, just update time
		m.activeAlerts[alertID].StartTime = time.Now()
		return
	}

	// Increment offline count
	m.nodeOfflineCount[node.ID]++
	offlineCount := m.nodeOfflineCount[node.ID]

	log.Debug().
		Str("node", node.Name).
		Str("instance", node.Instance).
		Int("offlineCount", offlineCount).
		Msg("Node offline detection count")

	// Require 3 consecutive offline polls (~15 seconds) before alerting
	// This prevents false positives from transient cluster communication issues
	const requiredOfflineCount = 3
	if offlineCount < requiredOfflineCount {
		log.Info().
			Str("node", node.Name).
			Int("count", offlineCount).
			Int("required", requiredOfflineCount).
			Msg("Node appears offline, waiting for confirmation")
		return
	}

	// Create new offline alert after confirmation
	alert := &Alert{
		ID:           alertID,
		Type:         "connectivity",
		Level:        AlertLevelCritical, // Node offline is always critical
		ResourceID:   node.ID,
		ResourceName: node.Name,
		Node:         node.Name,
		Instance:     node.Instance,
		Message:      fmt.Sprintf("Node '%s' is offline", node.Name),
		Value:        0, // Not applicable for offline status
		Threshold:    0, // Not applicable for offline status
		StartTime:    time.Now(),
		Acknowledged: false,
	}

	m.preserveAlertState(alertID, alert)

	m.activeAlerts[alertID] = alert
	m.recentAlerts[alertID] = alert

	// Add to history
	m.historyManager.AddAlert(*alert)

	// Send notification after confirmation
	m.dispatchAlert(alert, false)

	// Log the critical event
	log.Error().
		Str("node", node.Name).
		Str("instance", node.Instance).
		Str("status", node.Status).
		Str("connectionHealth", node.ConnectionHealth).
		Int("confirmedAfter", requiredOfflineCount).
		Msg("CRITICAL: Node is offline (confirmed)")
}

// clearNodeOfflineAlert removes offline alert when node comes back online
func (m *Manager) clearNodeOfflineAlert(node models.Node) {
	alertID := fmt.Sprintf("node-offline-%s", node.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset offline count when node comes back online
	if m.nodeOfflineCount[node.ID] > 0 {
		log.Debug().
			Str("node", node.Name).
			Int("previousCount", m.nodeOfflineCount[node.ID]).
			Msg("Node back online, resetting offline count")
		delete(m.nodeOfflineCount, node.ID)
	}

	// Check if offline alert exists
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	// Remove from active alerts
	m.removeActiveAlertNoLock(alertID)

	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}
	m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

	// Send recovery notification
	m.safeCallResolvedCallback(alertID, false)

	// Log recovery
	log.Info().
		Str("node", node.Name).
		Str("instance", node.Instance).
		Dur("downtime", time.Since(alert.StartTime)).
		Msg("Node is back online")
}

// checkPBSOffline creates an alert for offline PBS instances
func (m *Manager) checkPBSOffline(pbs models.PBSInstance) {
	alertID := fmt.Sprintf("pbs-offline-%s", pbs.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if PBS offline alerts are disabled via disableConnectivity flag
	if override, exists := m.config.Overrides[pbs.ID]; exists && (override.Disabled || override.DisableConnectivity) {
		// PBS connectivity alerts are disabled, clear any existing alert and return
		if _, alertExists := m.activeAlerts[alertID]; alertExists {
			m.clearAlertNoLock(alertID)
			log.Debug().
				Str("pbs", pbs.Name).
				Msg("PBS offline alert cleared (connectivity alerts disabled)")
		}
		return
	}

	// Track confirmation count for this PBS
	m.offlineConfirmations[pbs.ID]++

	// Require 3 consecutive offline polls (~15 seconds) before alerting
	if m.offlineConfirmations[pbs.ID] < 3 {
		log.Debug().
			Str("pbs", pbs.Name).
			Int("confirmations", m.offlineConfirmations[pbs.ID]).
			Msg("PBS offline detected, waiting for confirmation")
		return
	}

	// Check if alert already exists
	if _, exists := m.activeAlerts[alertID]; exists {
		// Update last seen time
		m.activeAlerts[alertID].LastSeen = time.Now()
		return
	}

	// Create new offline alert after confirmation
	alert := &Alert{
		ID:           alertID,
		Type:         "offline",
		Level:        AlertLevelCritical,
		ResourceID:   pbs.ID,
		ResourceName: pbs.Name,
		Node:         pbs.Host,
		Instance:     pbs.Name,
		Message:      fmt.Sprintf("PBS instance %s is offline", pbs.Name),
		Value:        0,
		Threshold:    0,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
	}

	m.preserveAlertState(alertID, alert)

	m.activeAlerts[alertID] = alert

	// Log and notify
	log.Error().
		Str("pbs", pbs.Name).
		Str("host", pbs.Host).
		Int("confirmations", m.offlineConfirmations[pbs.ID]).
		Msg("PBS instance is offline")

	m.dispatchAlert(alert, true)
}

// clearPBSOfflineAlert removes offline alert when PBS comes back online
func (m *Manager) clearPBSOfflineAlert(pbs models.PBSInstance) {
	alertID := fmt.Sprintf("pbs-offline-%s", pbs.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset offline confirmation count
	if count, exists := m.offlineConfirmations[pbs.ID]; exists && count > 0 {
		log.Debug().
			Str("pbs", pbs.Name).
			Int("previousCount", count).
			Msg("PBS is online, resetting offline confirmation count")
		delete(m.offlineConfirmations, pbs.ID)
	}

	// Check if offline alert exists
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	// Remove from active alerts
	m.removeActiveAlertNoLock(alertID)

	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}
	m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

	// Send recovery notification
	m.safeCallResolvedCallback(alertID, false)

	// Log recovery
	log.Info().
		Str("pbs", pbs.Name).
		Str("host", pbs.Host).
		Dur("downtime", time.Since(alert.StartTime)).
		Msg("PBS instance is back online")
}

// checkPMGOffline creates an alert for offline PMG instances
func (m *Manager) checkPMGOffline(pmg models.PMGInstance) {
	alertID := fmt.Sprintf("pmg-offline-%s", pmg.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if PMG offline alerts are disabled via disableConnectivity flag
	if override, exists := m.config.Overrides[pmg.ID]; exists && (override.Disabled || override.DisableConnectivity) {
		// PMG connectivity alerts are disabled, clear any existing alert and return
		if _, alertExists := m.activeAlerts[alertID]; alertExists {
			m.clearAlertNoLock(alertID)
			log.Debug().
				Str("pmg", pmg.Name).
				Msg("PMG offline alert cleared (connectivity alerts disabled)")
		}
		return
	}

	// Track confirmation count for this PMG
	m.offlineConfirmations[pmg.ID]++

	// Require 3 consecutive offline polls (~15 seconds) before alerting
	if m.offlineConfirmations[pmg.ID] < 3 {
		log.Debug().
			Str("pmg", pmg.Name).
			Int("confirmations", m.offlineConfirmations[pmg.ID]).
			Msg("PMG offline detected, waiting for confirmation")
		return
	}

	// Check if alert already exists
	if _, exists := m.activeAlerts[alertID]; exists {
		// Update last seen time
		m.activeAlerts[alertID].LastSeen = time.Now()
		return
	}

	// Create new offline alert after confirmation
	alert := &Alert{
		ID:           alertID,
		Type:         "offline",
		Level:        AlertLevelCritical,
		ResourceID:   pmg.ID,
		ResourceName: pmg.Name,
		Node:         pmg.Host,
		Instance:     pmg.Name,
		Message:      fmt.Sprintf("PMG instance %s is offline", pmg.Name),
		Value:        0,
		Threshold:    0,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
	}

	m.preserveAlertState(alertID, alert)

	m.activeAlerts[alertID] = alert

	// Log and notify
	log.Error().
		Str("pmg", pmg.Name).
		Str("host", pmg.Host).
		Int("confirmations", m.offlineConfirmations[pmg.ID]).
		Msg("PMG instance is offline")

	m.dispatchAlert(alert, true)
}

// clearPMGOfflineAlert removes offline alert when PMG comes back online
func (m *Manager) clearPMGOfflineAlert(pmg models.PMGInstance) {
	alertID := fmt.Sprintf("pmg-offline-%s", pmg.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset offline confirmation count
	if count, exists := m.offlineConfirmations[pmg.ID]; exists && count > 0 {
		log.Debug().
			Str("pmg", pmg.Name).
			Int("previousCount", count).
			Msg("PMG is online, resetting offline confirmation count")
		delete(m.offlineConfirmations, pmg.ID)
	}

	// Check if offline alert exists
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	// Remove from active alerts
	m.removeActiveAlertNoLock(alertID)

	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}
	m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

	// Send recovery notification
	m.safeCallResolvedCallback(alertID, false)

	// Log recovery
	log.Info().
		Str("pmg", pmg.Name).
		Str("host", pmg.Host).
		Dur("downtime", time.Since(alert.StartTime)).
		Msg("PMG instance is back online")
}

// checkPMGQueueDepths checks PMG mail queue depths and creates alerts
// Evaluates all queue types (total, deferred, hold) independently
func (m *Manager) checkPMGQueueDepths(pmg models.PMGInstance, defaults PMGThresholdConfig) {
	// Aggregate queue totals across all nodes
	var totalQueue, totalDeferred, totalHold int

	for _, node := range pmg.Nodes {
		if node.QueueStatus != nil {
			totalQueue += node.QueueStatus.Total
			totalDeferred += node.QueueStatus.Deferred
			totalHold += node.QueueStatus.Hold
		}
	}

	// Check total queue depth
	if defaults.QueueTotalWarning > 0 || defaults.QueueTotalCritical > 0 {
		alertID := fmt.Sprintf("%s-queue-total", pmg.ID)
		var level AlertLevel
		var threshold int
		var shouldAlert bool

		if defaults.QueueTotalCritical > 0 && totalQueue >= defaults.QueueTotalCritical {
			level = AlertLevelCritical
			threshold = defaults.QueueTotalCritical
			shouldAlert = true
		} else if defaults.QueueTotalWarning > 0 && totalQueue >= defaults.QueueTotalWarning {
			level = AlertLevelWarning
			threshold = defaults.QueueTotalWarning
			shouldAlert = true
		}

		if !shouldAlert {
			m.clearAlert(alertID)
		} else {
			m.mu.Lock()
			if alert, exists := m.activeAlerts[alertID]; exists {
				alert.LastSeen = time.Now()
				alert.Value = float64(totalQueue)
				alert.Threshold = float64(threshold)
				alert.Level = level
			} else {
				alert := &Alert{
					ID:           alertID,
					Type:         "queue-depth",
					Level:        level,
					ResourceID:   pmg.ID,
					ResourceName: pmg.Name,
					Node:         pmg.Host,
					Instance:     pmg.Name,
					Message:      fmt.Sprintf("PMG %s has %d total messages in queue (threshold: %d)", pmg.Name, totalQueue, threshold),
					Value:        float64(totalQueue),
					Threshold:    float64(threshold),
					StartTime:    time.Now(),
					LastSeen:     time.Now(),
				}
				m.activeAlerts[alertID] = alert
				m.dispatchAlert(alert, true)
				log.Warn().
					Str("pmg", pmg.Name).
					Int("total_queue", totalQueue).
					Int("threshold", threshold).
					Str("level", string(level)).
					Msg("PMG total queue depth alert triggered")
			}
			m.mu.Unlock()
		}
	}

	// Check deferred queue depth
	if defaults.DeferredQueueWarn > 0 || defaults.DeferredQueueCritical > 0 {
		alertID := fmt.Sprintf("%s-queue-deferred", pmg.ID)
		var level AlertLevel
		var threshold int
		var shouldAlert bool

		if defaults.DeferredQueueCritical > 0 && totalDeferred >= defaults.DeferredQueueCritical {
			level = AlertLevelCritical
			threshold = defaults.DeferredQueueCritical
			shouldAlert = true
		} else if defaults.DeferredQueueWarn > 0 && totalDeferred >= defaults.DeferredQueueWarn {
			level = AlertLevelWarning
			threshold = defaults.DeferredQueueWarn
			shouldAlert = true
		}

		if !shouldAlert {
			m.clearAlert(alertID)
		} else {
			m.mu.Lock()
			if alert, exists := m.activeAlerts[alertID]; exists {
				alert.LastSeen = time.Now()
				alert.Value = float64(totalDeferred)
				alert.Threshold = float64(threshold)
				alert.Level = level
			} else {
				alert := &Alert{
					ID:           alertID,
					Type:         "queue-deferred",
					Level:        level,
					ResourceID:   pmg.ID,
					ResourceName: pmg.Name,
					Node:         pmg.Host,
					Instance:     pmg.Name,
					Message:      fmt.Sprintf("PMG %s has %d deferred messages (threshold: %d)", pmg.Name, totalDeferred, threshold),
					Value:        float64(totalDeferred),
					Threshold:    float64(threshold),
					StartTime:    time.Now(),
					LastSeen:     time.Now(),
				}
				m.activeAlerts[alertID] = alert
				m.dispatchAlert(alert, true)
				log.Warn().
					Str("pmg", pmg.Name).
					Int("deferred_queue", totalDeferred).
					Int("threshold", threshold).
					Str("level", string(level)).
					Msg("PMG deferred queue depth alert triggered")
			}
			m.mu.Unlock()
		}
	}

	// Check hold queue depth
	if defaults.HoldQueueWarn > 0 || defaults.HoldQueueCritical > 0 {
		alertID := fmt.Sprintf("%s-queue-hold", pmg.ID)
		var level AlertLevel
		var threshold int
		var shouldAlert bool

		if defaults.HoldQueueCritical > 0 && totalHold >= defaults.HoldQueueCritical {
			level = AlertLevelCritical
			threshold = defaults.HoldQueueCritical
			shouldAlert = true
		} else if defaults.HoldQueueWarn > 0 && totalHold >= defaults.HoldQueueWarn {
			level = AlertLevelWarning
			threshold = defaults.HoldQueueWarn
			shouldAlert = true
		}

		if !shouldAlert {
			m.clearAlert(alertID)
		} else {
			m.mu.Lock()
			if alert, exists := m.activeAlerts[alertID]; exists {
				alert.LastSeen = time.Now()
				alert.Value = float64(totalHold)
				alert.Threshold = float64(threshold)
				alert.Level = level
			} else {
				alert := &Alert{
					ID:           alertID,
					Type:         "queue-hold",
					Level:        level,
					ResourceID:   pmg.ID,
					ResourceName: pmg.Name,
					Node:         pmg.Host,
					Instance:     pmg.Name,
					Message:      fmt.Sprintf("PMG %s has %d held messages (threshold: %d)", pmg.Name, totalHold, threshold),
					Value:        float64(totalHold),
					Threshold:    float64(threshold),
					StartTime:    time.Now(),
					LastSeen:     time.Now(),
				}
				m.activeAlerts[alertID] = alert
				m.dispatchAlert(alert, true)
				log.Warn().
					Str("pmg", pmg.Name).
					Int("hold_queue", totalHold).
					Int("threshold", threshold).
					Str("level", string(level)).
					Msg("PMG hold queue depth alert triggered")
			}
			m.mu.Unlock()
		}
	}
}

// checkPMGOldestMessage checks oldest queued message age and creates alerts
func (m *Manager) checkPMGOldestMessage(pmg models.PMGInstance, defaults PMGThresholdConfig) {
	if defaults.OldestMessageWarnMins <= 0 && defaults.OldestMessageCritMins <= 0 {
		return
	}

	// Find the oldest message age across all nodes
	var oldestAge int64 // in seconds
	for _, node := range pmg.Nodes {
		if node.QueueStatus != nil && node.QueueStatus.OldestAge > oldestAge {
			oldestAge = node.QueueStatus.OldestAge
		}
	}

	if oldestAge == 0 {
		// No messages in queue, clear any existing alert
		m.clearAlert(fmt.Sprintf("%s-oldest-message", pmg.ID))
		return
	}

	alertID := fmt.Sprintf("%s-oldest-message", pmg.ID)
	oldestMinutes := oldestAge / 60

	var level AlertLevel
	var threshold int64

	if defaults.OldestMessageCritMins > 0 && oldestMinutes >= int64(defaults.OldestMessageCritMins) {
		level = AlertLevelCritical
		threshold = int64(defaults.OldestMessageCritMins)
	} else if defaults.OldestMessageWarnMins > 0 && oldestMinutes >= int64(defaults.OldestMessageWarnMins) {
		level = AlertLevelWarning
		threshold = int64(defaults.OldestMessageWarnMins)
	} else {
		// Oldest message is below thresholds, clear any existing alert
		m.clearAlert(alertID)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if alert already exists
	if alert, exists := m.activeAlerts[alertID]; exists {
		// Update existing alert
		alert.LastSeen = time.Now()
		alert.Value = float64(oldestMinutes)
		alert.Threshold = float64(threshold)
		alert.Level = level
		return
	}

	// Create new alert
	alert := &Alert{
		ID:           alertID,
		Type:         "message-age",
		Level:        level,
		ResourceID:   pmg.ID,
		ResourceName: pmg.Name,
		Node:         pmg.Host,
		Instance:     pmg.Name,
		Message:      fmt.Sprintf("PMG %s has messages queued for %d minutes (threshold: %d minutes)", pmg.Name, oldestMinutes, threshold),
		Value:        float64(oldestMinutes),
		Threshold:    float64(threshold),
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
	}

	m.activeAlerts[alertID] = alert
	m.dispatchAlert(alert, true)

	log.Warn().
		Str("pmg", pmg.Name).
		Int64("oldest_minutes", oldestMinutes).
		Int64("threshold", threshold).
		Str("level", string(level)).
		Msg("PMG oldest message age alert triggered")
}

// checkPMGNodeQueues checks individual PMG node queue health
// Uses scaled thresholds (60% warn, 80% crit) and outlier detection
func (m *Manager) checkPMGNodeQueues(pmg models.PMGInstance, defaults PMGThresholdConfig) {
	if len(pmg.Nodes) == 0 {
		return
	}

	// Calculate median queue values across nodes for outlier detection
	nodeQueueTotals := make([]int, 0, len(pmg.Nodes))
	nodeQueueDeferred := make([]int, 0, len(pmg.Nodes))
	nodeQueueHold := make([]int, 0, len(pmg.Nodes))

	for _, node := range pmg.Nodes {
		if node.QueueStatus != nil {
			nodeQueueTotals = append(nodeQueueTotals, node.QueueStatus.Total)
			nodeQueueDeferred = append(nodeQueueDeferred, node.QueueStatus.Deferred)
			nodeQueueHold = append(nodeQueueHold, node.QueueStatus.Hold)
		}
	}

	medianTotal := calculateMedianInt(nodeQueueTotals)
	medianDeferred := calculateMedianInt(nodeQueueDeferred)
	medianHold := calculateMedianInt(nodeQueueHold)

	// Scaled thresholds: 60% for warning, 80% for critical (computed once, used for all nodes)
	scaledQueueWarn := scaleThreshold(defaults.QueueTotalWarning, 0.6)
	scaledQueueCrit := scaleThreshold(defaults.QueueTotalCritical, 0.8)
	scaledDeferredWarn := scaleThreshold(defaults.DeferredQueueWarn, 0.6)
	scaledDeferredCrit := scaleThreshold(defaults.DeferredQueueCritical, 0.8)
	scaledHoldWarn := scaleThreshold(defaults.HoldQueueWarn, 0.6)
	scaledHoldCrit := scaleThreshold(defaults.HoldQueueCritical, 0.8)
	scaledAgeWarn := scaleThreshold(defaults.OldestMessageWarnMins, 0.6)
	scaledAgeCrit := scaleThreshold(defaults.OldestMessageCritMins, 0.8)

	// Check each node
	for _, node := range pmg.Nodes {
		if node.QueueStatus == nil {
			continue
		}

		// Check total queue - always check thresholds
		if scaledQueueWarn > 0 || scaledQueueCrit > 0 {
			total := node.QueueStatus.Total
			alertID := fmt.Sprintf("%s-%s-queue-total", pmg.ID, node.Name)
			var level AlertLevel
			var threshold int

			if scaledQueueCrit > 0 && total >= scaledQueueCrit {
				level = AlertLevelCritical
				threshold = scaledQueueCrit
			} else if scaledQueueWarn > 0 && total >= scaledQueueWarn {
				level = AlertLevelWarning
				threshold = scaledQueueWarn
			} else {
				m.clearAlert(alertID)
				continue
			}

			// Add outlier indicator to message if applicable
			isOutlier := isQueueOutlier(total, medianTotal)
			outlierNote := ""
			if isOutlier {
				outlierNote = ", outlier"
			}

			m.createOrUpdateNodeAlert(alertID, pmg, node.Name, "queue-total", level, float64(total), float64(threshold),
				fmt.Sprintf("PMG node %s on %s has %d total messages in queue (threshold: %d%s)",
					node.Name, pmg.Name, total, threshold, outlierNote))
		}

		// Check deferred queue - always check thresholds
		if scaledDeferredWarn > 0 || scaledDeferredCrit > 0 {
			deferred := node.QueueStatus.Deferred
			alertID := fmt.Sprintf("%s-%s-queue-deferred", pmg.ID, node.Name)
			var level AlertLevel
			var threshold int

			if scaledDeferredCrit > 0 && deferred >= scaledDeferredCrit {
				level = AlertLevelCritical
				threshold = scaledDeferredCrit
			} else if scaledDeferredWarn > 0 && deferred >= scaledDeferredWarn {
				level = AlertLevelWarning
				threshold = scaledDeferredWarn
			} else {
				m.clearAlert(alertID)
				continue
			}

			// Add outlier indicator to message if applicable
			isOutlier := isQueueOutlier(deferred, medianDeferred)
			outlierNote := ""
			if isOutlier {
				outlierNote = ", outlier"
			}

			m.createOrUpdateNodeAlert(alertID, pmg, node.Name, "queue-deferred", level, float64(deferred), float64(threshold),
				fmt.Sprintf("PMG node %s on %s has %d deferred messages (threshold: %d%s)",
					node.Name, pmg.Name, deferred, threshold, outlierNote))
		}

		// Check hold queue - always check thresholds
		if scaledHoldWarn > 0 || scaledHoldCrit > 0 {
			hold := node.QueueStatus.Hold
			alertID := fmt.Sprintf("%s-%s-queue-hold", pmg.ID, node.Name)
			var level AlertLevel
			var threshold int

			if scaledHoldCrit > 0 && hold >= scaledHoldCrit {
				level = AlertLevelCritical
				threshold = scaledHoldCrit
			} else if scaledHoldWarn > 0 && hold >= scaledHoldWarn {
				level = AlertLevelWarning
				threshold = scaledHoldWarn
			} else {
				m.clearAlert(alertID)
				continue
			}

			// Add outlier indicator to message if applicable
			isOutlier := isQueueOutlier(hold, medianHold)
			outlierNote := ""
			if isOutlier {
				outlierNote = ", outlier"
			}

			m.createOrUpdateNodeAlert(alertID, pmg, node.Name, "queue-hold", level, float64(hold), float64(threshold),
				fmt.Sprintf("PMG node %s on %s has %d held messages (threshold: %d%s)",
					node.Name, pmg.Name, hold, threshold, outlierNote))
		}

		// Check oldest message age per node
		if scaledAgeWarn > 0 || scaledAgeCrit > 0 {
			oldestAge := node.QueueStatus.OldestAge
			if oldestAge > 0 {
				oldestMinutes := oldestAge / 60
				alertID := fmt.Sprintf("%s-%s-oldest-message", pmg.ID, node.Name)
				var level AlertLevel
				var threshold int64

				if scaledAgeCrit > 0 && oldestMinutes >= int64(scaledAgeCrit) {
					level = AlertLevelCritical
					threshold = int64(scaledAgeCrit)
				} else if scaledAgeWarn > 0 && oldestMinutes >= int64(scaledAgeWarn) {
					level = AlertLevelWarning
					threshold = int64(scaledAgeWarn)
				} else {
					m.clearAlert(alertID)
					continue
				}

				m.createOrUpdateNodeAlert(alertID, pmg, node.Name, "message-age", level, float64(oldestMinutes), float64(threshold),
					fmt.Sprintf("PMG node %s on %s has messages queued for %d minutes (threshold: %d min, node-specific)",
						node.Name, pmg.Name, oldestMinutes, threshold))
			}
		}
	}
}

// isQueueOutlier determines if a node's queue value is a significant outlier
// Returns true if value is >40% above the median across all nodes
func isQueueOutlier(value, median int) bool {
	if median == 0 {
		return value > 0
	}
	percentAboveMedian := float64(value-median) / float64(median) * 100
	return percentAboveMedian > 40
}

// scaleThreshold applies a scaling factor to a threshold and ensures minimum value of 1
// Uses ceiling to avoid truncation issues with small thresholds
func scaleThreshold(threshold int, scaleFactor float64) int {
	if threshold <= 0 {
		return 0
	}
	scaled := int(math.Ceil(float64(threshold) * scaleFactor))
	if scaled < 1 {
		return 1
	}
	return scaled
}

// calculateMedianInt calculates median of integer slice
func calculateMedianInt(values []int) int {
	if len(values) == 0 {
		return 0
	}

	// Copy and sort
	sorted := make([]int, len(values))
	copy(sorted, values)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

// createOrUpdateNodeAlert creates or updates a per-node alert
func (m *Manager) createOrUpdateNodeAlert(alertID string, pmg models.PMGInstance, nodeName, alertType string, level AlertLevel, value, threshold float64, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if alert already exists
	if alert, exists := m.activeAlerts[alertID]; exists {
		alert.LastSeen = time.Now()
		alert.Value = value
		alert.Threshold = threshold
		alert.Level = level
		alert.Message = message
		return
	}

	// Create new alert
	alert := &Alert{
		ID:           alertID,
		Type:         alertType,
		Level:        level,
		ResourceID:   pmg.ID,
		ResourceName: pmg.Name,
		Node:         nodeName,
		Instance:     pmg.Name,
		Message:      message,
		Value:        value,
		Threshold:    threshold,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
	}

	m.activeAlerts[alertID] = alert
	m.dispatchAlert(alert, true)

	log.Warn().
		Str("pmg", pmg.Name).
		Str("node", nodeName).
		Str("type", alertType).
		Float64("value", value).
		Float64("threshold", threshold).
		Str("level", string(level)).
		Msg("PMG per-node alert triggered")
}

// checkPMGQuarantineBacklog checks quarantine backlog and growth rates
func (m *Manager) checkPMGQuarantineBacklog(pmg models.PMGInstance, defaults PMGThresholdConfig) {
	if pmg.Quarantine == nil {
		m.clearAlert(fmt.Sprintf("%s-quarantine-spam", pmg.ID))
		m.clearAlert(fmt.Sprintf("%s-quarantine-virus", pmg.ID))
		return
	}

	now := time.Now()
	currentSpam := pmg.Quarantine.Spam
	currentVirus := pmg.Quarantine.Virus

	// Store current snapshot
	m.mu.Lock()
	snapshot := pmgQuarantineSnapshot{
		Spam:      currentSpam,
		Virus:     currentVirus,
		Timestamp: now,
	}

	// Get or create history for this PMG instance
	history := m.pmgQuarantineHistory[pmg.ID]
	history = append(history, snapshot)

	// Clean old snapshots (keep last 3 hours)
	cutoff := now.Add(-3 * time.Hour)
	validSnapshots := make([]pmgQuarantineSnapshot, 0, len(history))
	for _, snap := range history {
		if snap.Timestamp.After(cutoff) {
			validSnapshots = append(validSnapshots, snap)
		}
	}
	m.pmgQuarantineHistory[pmg.ID] = validSnapshots
	m.mu.Unlock()

	// Find snapshot from ~2 hours ago (within ±15 min tolerance)
	var twoHoursAgo *pmgQuarantineSnapshot
	targetTime := now.Add(-2 * time.Hour)
	minDiff := 15 * time.Minute

	for i := range validSnapshots {
		snap := &validSnapshots[i]
		diff := snap.Timestamp.Sub(targetTime)
		if diff < 0 {
			diff = -diff
		}
		if diff < minDiff {
			minDiff = diff
			twoHoursAgo = snap
		}
	}

	// Check spam quarantine
	m.checkQuarantineMetric(pmg, "spam", currentSpam, twoHoursAgo, defaults)

	// Check virus quarantine
	m.checkQuarantineMetric(pmg, "virus", currentVirus, twoHoursAgo, defaults)
}

// checkQuarantineMetric checks a single quarantine metric (spam or virus)
func (m *Manager) checkQuarantineMetric(pmg models.PMGInstance, metricType string, current int, twoHoursAgo *pmgQuarantineSnapshot, defaults PMGThresholdConfig) {
	alertID := fmt.Sprintf("%s-quarantine-%s", pmg.ID, metricType)

	var absoluteWarn, absoluteCrit int
	var previousCount int

	// Get thresholds and previous count based on metric type
	if metricType == "spam" {
		absoluteWarn = defaults.QuarantineSpamWarn
		absoluteCrit = defaults.QuarantineSpamCritical
		if twoHoursAgo != nil {
			previousCount = twoHoursAgo.Spam
		}
	} else { // virus
		absoluteWarn = defaults.QuarantineVirusWarn
		absoluteCrit = defaults.QuarantineVirusCritical
		if twoHoursAgo != nil {
			previousCount = twoHoursAgo.Virus
		}
	}

	var level AlertLevel
	var message string
	var threshold int
	var alertTriggered bool

	// Check absolute thresholds first
	if absoluteCrit > 0 && current >= absoluteCrit {
		level = AlertLevelCritical
		threshold = absoluteCrit
		message = fmt.Sprintf("PMG %s has %d %s messages in quarantine (threshold: %d)", pmg.Name, current, metricType, threshold)
		alertTriggered = true
	} else if absoluteWarn > 0 && current >= absoluteWarn {
		level = AlertLevelWarning
		threshold = absoluteWarn
		message = fmt.Sprintf("PMG %s has %d %s messages in quarantine (threshold: %d)", pmg.Name, current, metricType, threshold)
		alertTriggered = true
	}

	// Check growth thresholds if we have historical data
	if twoHoursAgo != nil && previousCount > 0 {
		growth := current - previousCount
		growthPct := (float64(growth) / float64(previousCount)) * 100

		// Critical growth: ≥50% AND ≥500 messages
		if defaults.QuarantineGrowthCritPct > 0 && defaults.QuarantineGrowthCritMin > 0 {
			if growthPct >= float64(defaults.QuarantineGrowthCritPct) && growth >= defaults.QuarantineGrowthCritMin {
				if level != AlertLevelCritical { // Only override if not already critical from absolute
					level = AlertLevelCritical
					threshold = previousCount + defaults.QuarantineGrowthCritMin
					message = fmt.Sprintf("PMG %s %s quarantine growing rapidly: +%d messages (+%.1f%%) in 2 hours", pmg.Name, metricType, growth, growthPct)
					alertTriggered = true
				}
			}
		}

		// Warning growth: ≥25% AND ≥250 messages (if not already critical)
		if level != AlertLevelCritical && defaults.QuarantineGrowthWarnPct > 0 && defaults.QuarantineGrowthWarnMin > 0 {
			if growthPct >= float64(defaults.QuarantineGrowthWarnPct) && growth >= defaults.QuarantineGrowthWarnMin {
				level = AlertLevelWarning
				threshold = previousCount + defaults.QuarantineGrowthWarnMin
				message = fmt.Sprintf("PMG %s %s quarantine growing: +%d messages (+%.1f%%) in 2 hours", pmg.Name, metricType, growth, growthPct)
				alertTriggered = true
			}
		}
	}

	// Clear alert if no thresholds exceeded
	if !alertTriggered {
		m.clearAlert(alertID)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if alert already exists
	if alert, exists := m.activeAlerts[alertID]; exists {
		// Update existing alert
		alert.LastSeen = time.Now()
		alert.Value = float64(current)
		alert.Threshold = float64(threshold)
		alert.Level = level
		alert.Message = message
		return
	}

	// Create new alert
	alert := &Alert{
		ID:           alertID,
		Type:         fmt.Sprintf("quarantine-%s", metricType),
		Level:        level,
		ResourceID:   pmg.ID,
		ResourceName: pmg.Name,
		Node:         pmg.Host,
		Instance:     pmg.Name,
		Message:      message,
		Value:        float64(current),
		Threshold:    float64(threshold),
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
	}

	m.activeAlerts[alertID] = alert
	m.dispatchAlert(alert, true)

	log.Warn().
		Str("pmg", pmg.Name).
		Str("type", metricType).
		Int("current", current).
		Int("threshold", threshold).
		Str("level", string(level)).
		Msg("PMG quarantine backlog alert triggered")
}

// calculateTrimmedBaseline computes a robust baseline from historical samples
// using trimmed mean with median fallback for statistical robustness
func calculateTrimmedBaseline(samples []float64) (baseline float64, trustworthy bool) {
	sampleCount := len(samples)

	// Need at least 12 samples for trustworthy baseline (warmup period)
	if sampleCount < 12 {
		return 0, false
	}

	// For full 24-sample baseline, use trimmed mean
	if sampleCount >= 24 {
		// Create a copy for sorting
		sorted := make([]float64, len(samples))
		copy(sorted, samples)

		// Sort samples
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[i] > sorted[j] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}

		// Calculate median
		var median float64
		mid := len(sorted) / 2
		if len(sorted)%2 == 0 {
			median = (sorted[mid-1] + sorted[mid]) / 2
		} else {
			median = sorted[mid]
		}

		// Calculate trimmed mean: drop top and bottom 2, average remaining 20
		if len(sorted) >= 24 {
			trimmed := sorted[2 : len(sorted)-2]
			sum := 0.0
			for _, val := range trimmed {
				sum += val
			}
			trimmedMean := sum / float64(len(trimmed))

			// Fallback rule: if trimmed mean differs from median by >40%, use median
			diff := trimmedMean - median
			if diff < 0 {
				diff = -diff
			}
			percentDiff := (diff / median) * 100

			if percentDiff > 40 {
				return median, true
			}
			return trimmedMean, true
		}
	}

	// For 12-23 samples, use simple mean (not enough for trimming)
	sum := 0.0
	for _, val := range samples {
		sum += val
	}
	return sum / float64(len(samples)), true
}

// checkPMGAnomalies detects spam/virus rate anomalies using trimmed baseline
func (m *Manager) checkPMGAnomalies(pmg models.PMGInstance, defaults PMGThresholdConfig) {
	// Need mail count data
	if len(pmg.MailCount) == 0 {
		return
	}

	// Get the latest hourly sample (most recent)
	latest := pmg.MailCount[len(pmg.MailCount)-1]
	now := time.Now()

	// Get or create anomaly tracker for this PMG instance
	m.mu.Lock()
	tracker := m.pmgAnomalyTrackers[pmg.ID]
	if tracker == nil {
		tracker = &pmgAnomalyTracker{
			Samples:   make([]pmgMailMetricSample, 0, 48),
			Baselines: make(map[string]pmgBaselineCache),
		}
		m.pmgAnomalyTrackers[pmg.ID] = tracker
	}

	// Create sample from latest mail count
	sample := pmgMailMetricSample{
		SpamIn:    latest.SpamIn,
		SpamOut:   latest.SpamOut,
		VirusIn:   latest.VirusIn,
		VirusOut:  latest.VirusOut,
		Timestamp: latest.Timestamp,
	}

	// Check for duplicate timestamp (already processed this sample)
	if !tracker.LastSampleTime.IsZero() && !sample.Timestamp.After(tracker.LastSampleTime) {
		m.mu.Unlock()
		return
	}

	// Check for timestamp gaps (>90 min indicates data discontinuity)
	if !tracker.LastSampleTime.IsZero() {
		gap := sample.Timestamp.Sub(tracker.LastSampleTime)
		if gap > 90*time.Minute {
			// Discard old samples - data gap detected
			log.Debug().
				Str("pmg", pmg.Name).
				Dur("gap", gap).
				Msg("PMG mail count data gap detected, resetting anomaly history")
			tracker.Samples = make([]pmgMailMetricSample, 0, 48)
			tracker.SampleCount = 0
		}
	}

	// Add sample to ring buffer
	tracker.Samples = append(tracker.Samples, sample)
	tracker.SampleCount++
	tracker.LastSampleTime = sample.Timestamp

	// Maintain ring buffer size (keep last 48)
	if len(tracker.Samples) > 48 {
		tracker.Samples = tracker.Samples[len(tracker.Samples)-48:]
	}

	sampleCount := len(tracker.Samples)
	m.mu.Unlock()

	// Need at least 12 samples for baseline warmup
	if sampleCount < 12 {
		log.Debug().
			Str("pmg", pmg.Name).
			Int("samples", sampleCount).
			Msg("PMG anomaly detection warming up (need 12 samples)")
		return
	}

	// Calculate baselines and check each metric
	metrics := []struct {
		name      string
		current   float64
		extractor func(pmgMailMetricSample) float64
	}{
		{"spamIn", sample.SpamIn, func(s pmgMailMetricSample) float64 { return s.SpamIn }},
		{"spamOut", sample.SpamOut, func(s pmgMailMetricSample) float64 { return s.SpamOut }},
		{"virusIn", sample.VirusIn, func(s pmgMailMetricSample) float64 { return s.VirusIn }},
		{"virusOut", sample.VirusOut, func(s pmgMailMetricSample) float64 { return s.VirusOut }},
	}

	for _, metric := range metrics {
		m.checkAnomalyMetric(pmg, tracker, metric.name, metric.current, metric.extractor, now)
	}
}

// checkAnomalyMetric checks a single spam/virus metric for anomalies
func (m *Manager) checkAnomalyMetric(pmg models.PMGInstance, tracker *pmgAnomalyTracker, metricName string, current float64, extractor func(pmgMailMetricSample) float64, now time.Time) {
	// Extract historical values for this metric (excluding current sample)
	m.mu.RLock()
	samples := tracker.Samples
	m.mu.RUnlock()

	if len(samples) < 2 {
		return
	}

	// Get previous 24 samples (or all available if less than 25 total)
	startIdx := 0
	if len(samples) > 25 {
		startIdx = len(samples) - 25
	}
	historicalSamples := samples[startIdx : len(samples)-1] // Exclude current (last) sample

	// Extract metric values
	values := make([]float64, 0, len(historicalSamples))
	for _, s := range historicalSamples {
		values = append(values, extractor(s))
	}

	// Calculate baseline
	baseline, trustworthy := calculateTrimmedBaseline(values)
	if !trustworthy {
		return
	}

	// Handle zero baseline edge case
	if baseline == 0 && current > 0 {
		baseline = 1.0 // Treat as 1 for ratio math
	}

	// Determine warning and critical thresholds
	var warnRatio, critRatio float64
	var warnDelta, critDelta float64

	if baseline < 40 {
		// Quiet site: use minimum absolute deltas
		warnRatio = 0
		critRatio = 0
		warnDelta = baseline + 60
		critDelta = baseline + 120
	} else {
		// Normal site: use ratio + absolute delta
		warnRatio = 1.8
		critRatio = 2.5
		warnDelta = baseline + 150
		critDelta = baseline + 300
	}

	alertID := fmt.Sprintf("%s-anomaly-%s", pmg.ID, metricName)
	pendingKey := fmt.Sprintf("pmg-anomaly-%s-%s", pmg.ID, metricName)

	var level AlertLevel
	var triggered bool
	var ratio float64

	if baseline > 0 {
		ratio = current / baseline
	}

	// Check critical threshold
	if critRatio > 0 && ratio >= critRatio && current >= critDelta {
		level = AlertLevelCritical
		triggered = true
	} else if warnRatio > 0 && ratio >= warnRatio && current >= warnDelta {
		level = AlertLevelWarning
		triggered = true
	} else if baseline < 40 {
		// Quiet site absolute check
		if current >= critDelta {
			level = AlertLevelCritical
			triggered = true
		} else if current >= warnDelta {
			level = AlertLevelWarning
			triggered = true
		}
	}

	// Two-sample confirmation using pendingAlerts
	if triggered {
		m.mu.Lock()
		firstSeen, pending := m.pendingAlerts[pendingKey]
		if !pending {
			// First sample above threshold - mark as pending
			m.pendingAlerts[pendingKey] = now
			m.mu.Unlock()
			log.Debug().
				Str("pmg", pmg.Name).
				Str("metric", metricName).
				Float64("current", current).
				Float64("baseline", baseline).
				Msg("PMG anomaly pending confirmation (first sample)")
			return
		}
		m.mu.Unlock()

		// Second consecutive sample above threshold - issue alert
		log.Debug().
			Str("pmg", pmg.Name).
			Str("metric", metricName).
			Float64("current", current).
			Float64("baseline", baseline).
			Dur("pending", now.Sub(firstSeen)).
			Msg("PMG anomaly confirmed (second sample)")

		m.mu.Lock()
		delete(m.pendingAlerts, pendingKey) // Clear pending

		// Check if alert already exists
		if alert, exists := m.activeAlerts[alertID]; exists {
			alert.LastSeen = now
			alert.Value = current
			alert.Threshold = baseline
			alert.Level = level
			m.mu.Unlock()
			return
		}

		// Create new alert
		message := fmt.Sprintf("PMG %s anomaly detected: %s is %.1f messages/hour (%.1fx baseline of %.1f)",
			pmg.Name, metricName, current, ratio, baseline)

		alert := &Alert{
			ID:           alertID,
			Type:         fmt.Sprintf("anomaly-%s", metricName),
			Level:        level,
			ResourceID:   pmg.ID,
			ResourceName: pmg.Name,
			Node:         pmg.Host,
			Instance:     pmg.Name,
			Message:      message,
			Value:        current,
			Threshold:    baseline,
			StartTime:    now,
			LastSeen:     now,
		}

		m.activeAlerts[alertID] = alert
		m.mu.Unlock()
		m.dispatchAlert(alert, true)

		log.Warn().
			Str("pmg", pmg.Name).
			Str("metric", metricName).
			Float64("current", current).
			Float64("baseline", baseline).
			Float64("ratio", ratio).
			Str("level", string(level)).
			Msg("PMG anomaly alert triggered")
	} else {
		// Below threshold - clear pending and alert
		m.mu.Lock()
		delete(m.pendingAlerts, pendingKey)
		m.mu.Unlock()
		m.clearAlert(alertID)
	}
}

// checkStorageOffline creates an alert for offline/unavailable storage
func (m *Manager) checkStorageOffline(storage models.Storage) {
	alertID := fmt.Sprintf("storage-offline-%s", storage.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if storage offline alerts are disabled
	if override, exists := m.config.Overrides[storage.ID]; exists && override.Disabled {
		// Storage alerts are disabled, clear any existing alert and return
		if _, alertExists := m.activeAlerts[alertID]; alertExists {
			m.clearAlertNoLock(alertID)
			log.Debug().
				Str("storage", storage.Name).
				Msg("Storage offline alert cleared (alerts disabled)")
		}
		return
	}

	// Track confirmation count for this storage
	m.offlineConfirmations[storage.ID]++

	// Require 2 consecutive offline polls (~10 seconds) before alerting for storage
	// (less than nodes since storage status can be more transient)
	if m.offlineConfirmations[storage.ID] < 2 {
		log.Debug().
			Str("storage", storage.Name).
			Int("confirmations", m.offlineConfirmations[storage.ID]).
			Msg("Storage offline detected, waiting for confirmation")
		return
	}

	// Check if alert already exists
	if _, exists := m.activeAlerts[alertID]; exists {
		// Update last seen time
		m.activeAlerts[alertID].LastSeen = time.Now()
		return
	}

	// Create new offline alert after confirmation
	alert := &Alert{
		ID:           alertID,
		Type:         "offline",
		Level:        AlertLevelWarning, // Storage offline is Warning, not Critical
		ResourceID:   storage.ID,
		ResourceName: storage.Name,
		Node:         storage.Node,
		Instance:     storage.Instance,
		Message:      fmt.Sprintf("Storage %s on node %s is unavailable", storage.Name, storage.Node),
		Value:        0,
		Threshold:    0,
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
	}

	m.preserveAlertState(alertID, alert)

	m.activeAlerts[alertID] = alert

	// Log and notify
	log.Warn().
		Str("storage", storage.Name).
		Str("node", storage.Node).
		Int("confirmations", m.offlineConfirmations[storage.ID]).
		Msg("Storage is offline/unavailable")

	m.dispatchAlert(alert, true)
}

// clearStorageOfflineAlert removes offline alert when storage comes back online
func (m *Manager) clearStorageOfflineAlert(storage models.Storage) {
	alertID := fmt.Sprintf("storage-offline-%s", storage.ID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset offline confirmation count
	if count, exists := m.offlineConfirmations[storage.ID]; exists && count > 0 {
		log.Debug().
			Str("storage", storage.Name).
			Int("previousCount", count).
			Msg("Storage is online, resetting offline confirmation count")
		delete(m.offlineConfirmations, storage.ID)
	}

	// Check if offline alert exists
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	// Remove from active alerts
	m.removeActiveAlertNoLock(alertID)

	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}
	m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

	// Send recovery notification
	m.safeCallResolvedCallback(alertID, false)

	// Log recovery
	log.Info().
		Str("storage", storage.Name).
		Str("node", storage.Node).
		Dur("downtime", time.Since(alert.StartTime)).
		Msg("Storage is back online")
}

// checkGuestPoweredOff creates an alert for powered-off guests
func (m *Manager) checkGuestPoweredOff(guestID, name, node, instanceName, guestType string) {
	alertID := fmt.Sprintf("guest-powered-off-%s", guestID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Get thresholds to check if powered-off alerts are disabled
	var thresholds ThresholdConfig
	if override, exists := m.config.Overrides[guestID]; exists {
		thresholds = override
	} else {
		thresholds = m.config.GuestDefaults
	}

	severity := normalizePoweredOffSeverity(thresholds.PoweredOffSeverity)

	// Check if powered-off alerts are disabled for this guest
	if thresholds.Disabled || thresholds.DisableConnectivity {
		// Powered-off alerts are disabled, clear any existing alert and return
		if _, alertExists := m.activeAlerts[alertID]; alertExists {
			m.clearAlertNoLock(alertID)
			log.Debug().
				Str("guest", name).
				Msg("Guest powered-off alert cleared (alerts disabled)")
		}
		delete(m.offlineConfirmations, guestID)
		return
	}

	// Check if alert already exists
	if alert, exists := m.activeAlerts[alertID]; exists {
		// Alert already exists, just update LastSeen
		alert.LastSeen = time.Now()
		alert.Level = severity
		return
	}

	// Increment confirmation count
	m.offlineConfirmations[guestID]++
	confirmCount := m.offlineConfirmations[guestID]

	log.Debug().
		Str("guest", name).
		Str("type", guestType).
		Int("confirmations", confirmCount).
		Msg("Guest powered-off detected")

	// Require 2 consecutive powered-off polls (~10 seconds) before alerting
	// This prevents false positives from transient states
	const requiredConfirmations = 2
	if confirmCount < requiredConfirmations {
		log.Debug().
			Str("guest", name).
			Int("count", confirmCount).
			Int("required", requiredConfirmations).
			Msg("Guest appears powered-off, waiting for confirmation")
		return
	}

	// Create new powered-off alert after confirmation
	alert := &Alert{
		ID:           alertID,
		Type:         "powered-off",
		Level:        severity,
		ResourceID:   guestID,
		ResourceName: name,
		Node:         node,
		Instance:     instanceName,
		Message:      fmt.Sprintf("%s '%s' is powered off", guestType, name),
		Value:        0, // Not applicable for powered-off status
		Threshold:    0, // Not applicable for powered-off status
		StartTime:    time.Now(),
		LastSeen:     time.Now(),
		Acknowledged: false,
	}

	m.preserveAlertState(alertID, alert)

	m.activeAlerts[alertID] = alert
	m.recentAlerts[alertID] = alert

	// Add to history
	m.historyManager.AddAlert(*alert)

	// Send notification after confirmation
	m.dispatchAlert(alert, false)

	// Log the event
	log.Warn().
		Str("guest", name).
		Str("type", guestType).
		Str("node", node).
		Str("instance", instanceName).
		Int("confirmedAfter", requiredConfirmations).
		Msg("Guest is powered off (confirmed)")
}

// clearGuestPoweredOffAlert removes powered-off alert when guest starts running
func (m *Manager) clearGuestPoweredOffAlert(guestID, name string) {
	alertID := fmt.Sprintf("guest-powered-off-%s", guestID)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset confirmation count when guest comes back online
	if count, exists := m.offlineConfirmations[guestID]; exists && count > 0 {
		log.Debug().
			Str("guest", name).
			Int("previousCount", count).
			Msg("Guest is running, resetting powered-off confirmation count")
		delete(m.offlineConfirmations, guestID)
	}

	// Check if powered-off alert exists
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	// Remove from active alerts
	m.removeActiveAlertNoLock(alertID)

	downtime := time.Since(alert.StartTime)
	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}
	m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

	// Send recovery notification
	m.safeCallResolvedCallback(alertID, false)

	// Log recovery
	log.Info().
		Str("guest", name).
		Dur("downtime", downtime).
		Msg("Guest is now running")
}

// ClearAlert removes an alert from active alerts (but keeps in history)
func (m *Manager) ClearAlert(alertID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove from active alerts only
	m.removeActiveAlertNoLock(alertID)

	if m.onResolved != nil {
		go m.onResolved(alertID)
	}
}

// Cleanup removes old acknowledged alerts and cleans up tracking maps
func (m *Manager) Cleanup(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	// Clean up acknowledged alerts
	for id, alert := range m.activeAlerts {
		if alert.Acknowledged && alert.AckTime != nil && now.Sub(*alert.AckTime) > maxAge {
			m.removeActiveAlertNoLock(id)
		}
	}

	// Clean up recent alerts older than suppression window
	suppressionWindow := time.Duration(m.config.SuppressionWindow) * time.Minute
	if suppressionWindow == 0 {
		suppressionWindow = 5 * time.Minute // Default
	}

	for id, alert := range m.recentAlerts {
		if now.Sub(alert.StartTime) > suppressionWindow {
			delete(m.recentAlerts, id)
		}
	}

	// Clean up expired suppressions
	for id, suppressUntil := range m.suppressedUntil {
		if now.After(suppressUntil) {
			delete(m.suppressedUntil, id)
		}
	}

	// Clean up old rate limit entries (older than 1 hour)
	cutoff := now.Add(-1 * time.Hour)
	for alertID, times := range m.alertRateLimit {
		var recentTimes []time.Time
		for _, t := range times {
			if t.After(cutoff) {
				recentTimes = append(recentTimes, t)
			}
		}
		if len(recentTimes) == 0 {
			// No recent alerts, remove the entry entirely
			delete(m.alertRateLimit, alertID)
		} else {
			// Update with only recent times
			m.alertRateLimit[alertID] = recentTimes
		}
	}

	// Clean up old recently resolved alerts (older than 5 minutes)
	fiveMinutesAgo := now.Add(-5 * time.Minute)
	m.resolvedMutex.Lock()
	for alertID, resolved := range m.recentlyResolved {
		if resolved.ResolvedTime.Before(fiveMinutesAgo) {
			delete(m.recentlyResolved, alertID)
		}
	}
	m.resolvedMutex.Unlock()

	// Clean up stale pending alerts (older than max time threshold window)
	// This prevents memory leak from deleted resources that never triggered alerts
	maxPendingAge := 10 * time.Minute // Longest time threshold + safety buffer
	for id, pendingTime := range m.pendingAlerts {
		if now.Sub(pendingTime) > maxPendingAge {
			delete(m.pendingAlerts, id)
			log.Debug().
				Str("resourceID", id).
				Dur("age", now.Sub(pendingTime)).
				Msg("Cleaned up stale pending alert entry")
		}
	}

	// Clean up old Docker restart tracking (containers not seen in 24h)
	// Prevents memory leak from ephemeral containers in CI/CD environments
	for resourceID, record := range m.dockerRestartTracking {
		if now.Sub(record.lastChecked) > 24*time.Hour {
			delete(m.dockerRestartTracking, resourceID)
			log.Debug().
				Str("resourceID", resourceID).
				Msg("Cleaned up stale Docker restart tracking entry")
		}
	}
}

// convertLegacyThreshold converts a legacy float64 threshold to HysteresisThreshold
func (m *Manager) convertLegacyThreshold(legacy *float64) *HysteresisThreshold {
	if legacy == nil || *legacy <= 0 {
		return nil
	}
	margin := m.config.HysteresisMargin
	if margin <= 0 {
		margin = 5.0 // Default 5% margin
	}
	return &HysteresisThreshold{
		Trigger: *legacy,
		Clear:   *legacy - margin,
	}
}

func cloneThreshold(threshold *HysteresisThreshold) *HysteresisThreshold {
	if threshold == nil {
		return nil
	}
	clone := *threshold
	return &clone
}

func (m *Manager) applyThresholdOverride(base ThresholdConfig, override ThresholdConfig) ThresholdConfig {
	result := base

	if override.Disabled {
		result.Disabled = true
	}
	if override.DisableConnectivity {
		result.DisableConnectivity = true
	}

	if override.CPU != nil {
		result.CPU = ensureHysteresisThreshold(cloneThreshold(override.CPU))
	} else if override.CPULegacy != nil {
		result.CPU = m.convertLegacyThreshold(override.CPULegacy)
	}

	if override.Memory != nil {
		result.Memory = ensureHysteresisThreshold(cloneThreshold(override.Memory))
	} else if override.MemoryLegacy != nil {
		result.Memory = m.convertLegacyThreshold(override.MemoryLegacy)
	}

	if override.Disk != nil {
		result.Disk = ensureHysteresisThreshold(cloneThreshold(override.Disk))
	} else if override.DiskLegacy != nil {
		result.Disk = m.convertLegacyThreshold(override.DiskLegacy)
	}

	if override.DiskRead != nil {
		result.DiskRead = ensureHysteresisThreshold(cloneThreshold(override.DiskRead))
	} else if override.DiskReadLegacy != nil {
		result.DiskRead = m.convertLegacyThreshold(override.DiskReadLegacy)
	}

	if override.DiskWrite != nil {
		result.DiskWrite = ensureHysteresisThreshold(cloneThreshold(override.DiskWrite))
	} else if override.DiskWriteLegacy != nil {
		result.DiskWrite = m.convertLegacyThreshold(override.DiskWriteLegacy)
	}

	if override.NetworkIn != nil {
		result.NetworkIn = ensureHysteresisThreshold(cloneThreshold(override.NetworkIn))
	} else if override.NetworkInLegacy != nil {
		result.NetworkIn = m.convertLegacyThreshold(override.NetworkInLegacy)
	}

	if override.NetworkOut != nil {
		result.NetworkOut = ensureHysteresisThreshold(cloneThreshold(override.NetworkOut))
	} else if override.NetworkOutLegacy != nil {
		result.NetworkOut = m.convertLegacyThreshold(override.NetworkOutLegacy)
	}

	if override.Temperature != nil {
		result.Temperature = ensureHysteresisThreshold(cloneThreshold(override.Temperature))
	}

	if override.Usage != nil {
		result.Usage = ensureHysteresisThreshold(cloneThreshold(override.Usage))
	}

	return result
}

// ensureHysteresisThreshold ensures a threshold has hysteresis configured
func ensureHysteresisThreshold(threshold *HysteresisThreshold) *HysteresisThreshold {
	if threshold == nil {
		return nil
	}
	if threshold.Clear <= 0 {
		threshold.Clear = threshold.Trigger - 5.0 // Default 5% margin
	}
	return threshold
}

// evaluateFilterCondition evaluates a single filter condition against a guest
func (m *Manager) evaluateFilterCondition(guest interface{}, condition FilterCondition) bool {
	switch g := guest.(type) {
	case models.VM:
		return m.evaluateVMCondition(g, condition)
	case models.Container:
		return m.evaluateContainerCondition(g, condition)
	default:
		return false
	}
}

// evaluateVMCondition evaluates a filter condition against a VM
func (m *Manager) evaluateVMCondition(vm models.VM, condition FilterCondition) bool {
	switch condition.Type {
	case "metric":
		value := 0.0
		switch strings.ToLower(condition.Field) {
		case "cpu":
			value = vm.CPU * 100
		case "memory":
			value = vm.Memory.Usage
		case "disk":
			value = vm.Disk.Usage
		case "diskread":
			value = float64(vm.DiskRead) / 1024 / 1024 // Convert to MB/s
		case "diskwrite":
			value = float64(vm.DiskWrite) / 1024 / 1024
		case "networkin":
			value = float64(vm.NetworkIn) / 1024 / 1024
		case "networkout":
			value = float64(vm.NetworkOut) / 1024 / 1024
		default:
			return false
		}

		condValue, ok := condition.Value.(float64)
		if !ok {
			// Try to convert from int
			if intVal, ok := condition.Value.(int); ok {
				condValue = float64(intVal)
			} else {
				return false
			}
		}

		switch condition.Operator {
		case ">":
			return value > condValue
		case "<":
			return value < condValue
		case ">=":
			return value >= condValue
		case "<=":
			return value <= condValue
		case "=", "==":
			return value >= condValue-0.5 && value <= condValue+0.5
		}

	case "text":
		searchValue := strings.ToLower(fmt.Sprintf("%v", condition.Value))
		switch strings.ToLower(condition.Field) {
		case "name":
			return strings.Contains(strings.ToLower(vm.Name), searchValue)
		case "node":
			return strings.Contains(strings.ToLower(vm.Node), searchValue)
		case "vmid":
			return strings.Contains(vm.ID, searchValue)
		}

	case "raw":
		if condition.RawText != "" {
			term := strings.ToLower(condition.RawText)
			return strings.Contains(strings.ToLower(vm.Name), term) ||
				strings.Contains(vm.ID, term) ||
				strings.Contains(strings.ToLower(vm.Node), term) ||
				strings.Contains(strings.ToLower(vm.Status), term)
		}
	}

	return false
}

// evaluateContainerCondition evaluates a filter condition against a Container
func (m *Manager) evaluateContainerCondition(ct models.Container, condition FilterCondition) bool {
	// Similar logic to evaluateVMCondition but for Container type
	switch condition.Type {
	case "metric":
		value := 0.0
		switch strings.ToLower(condition.Field) {
		case "cpu":
			value = ct.CPU * 100
		case "memory":
			value = ct.Memory.Usage
		case "disk":
			value = ct.Disk.Usage
		case "diskread":
			value = float64(ct.DiskRead) / 1024 / 1024
		case "diskwrite":
			value = float64(ct.DiskWrite) / 1024 / 1024
		case "networkin":
			value = float64(ct.NetworkIn) / 1024 / 1024
		case "networkout":
			value = float64(ct.NetworkOut) / 1024 / 1024
		default:
			return false
		}

		condValue, ok := condition.Value.(float64)
		if !ok {
			if intVal, ok := condition.Value.(int); ok {
				condValue = float64(intVal)
			} else {
				return false
			}
		}

		switch condition.Operator {
		case ">":
			return value > condValue
		case "<":
			return value < condValue
		case ">=":
			return value >= condValue
		case "<=":
			return value <= condValue
		case "=", "==":
			return value >= condValue-0.5 && value <= condValue+0.5
		}

	case "text":
		searchValue := strings.ToLower(fmt.Sprintf("%v", condition.Value))
		switch strings.ToLower(condition.Field) {
		case "name":
			return strings.Contains(strings.ToLower(ct.Name), searchValue)
		case "node":
			return strings.Contains(strings.ToLower(ct.Node), searchValue)
		case "vmid":
			return strings.Contains(ct.ID, searchValue)
		}

	case "raw":
		if condition.RawText != "" {
			term := strings.ToLower(condition.RawText)
			return strings.Contains(strings.ToLower(ct.Name), term) ||
				strings.Contains(ct.ID, term) ||
				strings.Contains(strings.ToLower(ct.Node), term) ||
				strings.Contains(strings.ToLower(ct.Status), term)
		}
	}

	return false
}

// evaluateFilterStack evaluates a filter stack against a guest
func (m *Manager) evaluateFilterStack(guest interface{}, stack FilterStack) bool {
	if len(stack.Filters) == 0 {
		return true
	}

	results := make([]bool, len(stack.Filters))
	for i, filter := range stack.Filters {
		results[i] = m.evaluateFilterCondition(guest, filter)
	}

	// Apply logical operator
	if stack.LogicalOperator == "AND" {
		for _, result := range results {
			if !result {
				return false
			}
		}
		return true
	} else { // OR
		for _, result := range results {
			if result {
				return true
			}
		}
		return false
	}
}

// getGuestThresholds returns the appropriate thresholds for a guest
// Priority: Guest-specific overrides > Custom rules (by priority) > Global defaults
func (m *Manager) getGuestThresholds(guest interface{}, guestID string) ThresholdConfig {
	// Start with defaults
	thresholds := m.config.GuestDefaults

	// Check custom rules (sorted by priority, highest first)
	var applicableRule *CustomAlertRule
	highestPriority := -1

	for i := range m.config.CustomRules {
		rule := &m.config.CustomRules[i]
		if !rule.Enabled {
			continue
		}

		// Check if this rule applies to the guest
		if m.evaluateFilterStack(guest, rule.FilterConditions) {
			if rule.Priority > highestPriority {
				applicableRule = rule
				highestPriority = rule.Priority
			}
		}
	}

	// Apply custom rule thresholds if found
	if applicableRule != nil {
		if applicableRule.Thresholds.CPU != nil {
			thresholds.CPU = ensureHysteresisThreshold(applicableRule.Thresholds.CPU)
		} else if applicableRule.Thresholds.CPULegacy != nil {
			thresholds.CPU = m.convertLegacyThreshold(applicableRule.Thresholds.CPULegacy)
		}
		if applicableRule.Thresholds.Memory != nil {
			thresholds.Memory = ensureHysteresisThreshold(applicableRule.Thresholds.Memory)
		} else if applicableRule.Thresholds.MemoryLegacy != nil {
			thresholds.Memory = m.convertLegacyThreshold(applicableRule.Thresholds.MemoryLegacy)
		}
		if applicableRule.Thresholds.Disk != nil {
			thresholds.Disk = ensureHysteresisThreshold(applicableRule.Thresholds.Disk)
		} else if applicableRule.Thresholds.DiskLegacy != nil {
			thresholds.Disk = m.convertLegacyThreshold(applicableRule.Thresholds.DiskLegacy)
		}
		if applicableRule.Thresholds.DiskRead != nil {
			thresholds.DiskRead = ensureHysteresisThreshold(applicableRule.Thresholds.DiskRead)
		} else if applicableRule.Thresholds.DiskReadLegacy != nil {
			thresholds.DiskRead = m.convertLegacyThreshold(applicableRule.Thresholds.DiskReadLegacy)
		}
		if applicableRule.Thresholds.DiskWrite != nil {
			thresholds.DiskWrite = ensureHysteresisThreshold(applicableRule.Thresholds.DiskWrite)
		} else if applicableRule.Thresholds.DiskWriteLegacy != nil {
			thresholds.DiskWrite = m.convertLegacyThreshold(applicableRule.Thresholds.DiskWriteLegacy)
		}
		if applicableRule.Thresholds.NetworkIn != nil {
			thresholds.NetworkIn = ensureHysteresisThreshold(applicableRule.Thresholds.NetworkIn)
		} else if applicableRule.Thresholds.NetworkInLegacy != nil {
			thresholds.NetworkIn = m.convertLegacyThreshold(applicableRule.Thresholds.NetworkInLegacy)
		}
		if applicableRule.Thresholds.NetworkOut != nil {
			thresholds.NetworkOut = ensureHysteresisThreshold(applicableRule.Thresholds.NetworkOut)
		} else if applicableRule.Thresholds.NetworkOutLegacy != nil {
			thresholds.NetworkOut = m.convertLegacyThreshold(applicableRule.Thresholds.NetworkOutLegacy)
		}
		if applicableRule.Thresholds.DisableConnectivity {
			thresholds.DisableConnectivity = true
		}

		log.Debug().
			Str("guest", guestID).
			Str("rule", applicableRule.Name).
			Int("priority", applicableRule.Priority).
			Msg("Applied custom alert rule")
	}

	// Finally check guest-specific overrides (highest priority)
	if override, exists := m.config.Overrides[guestID]; exists {
		// Apply the disabled flag if set
		if override.Disabled {
			thresholds.Disabled = true
		}
		if override.DisableConnectivity {
			thresholds.DisableConnectivity = true
		}

		if override.CPU != nil {
			thresholds.CPU = ensureHysteresisThreshold(override.CPU)
		} else if override.CPULegacy != nil {
			thresholds.CPU = m.convertLegacyThreshold(override.CPULegacy)
		}
		if override.Memory != nil {
			thresholds.Memory = ensureHysteresisThreshold(override.Memory)
		} else if override.MemoryLegacy != nil {
			thresholds.Memory = m.convertLegacyThreshold(override.MemoryLegacy)
		}
		if override.Disk != nil {
			thresholds.Disk = ensureHysteresisThreshold(override.Disk)
		} else if override.DiskLegacy != nil {
			thresholds.Disk = m.convertLegacyThreshold(override.DiskLegacy)
		}
		if override.DiskRead != nil {
			thresholds.DiskRead = ensureHysteresisThreshold(override.DiskRead)
		} else if override.DiskReadLegacy != nil {
			thresholds.DiskRead = m.convertLegacyThreshold(override.DiskReadLegacy)
		}
		if override.DiskWrite != nil {
			thresholds.DiskWrite = ensureHysteresisThreshold(override.DiskWrite)
		} else if override.DiskWriteLegacy != nil {
			thresholds.DiskWrite = m.convertLegacyThreshold(override.DiskWriteLegacy)
		}
		if override.NetworkIn != nil {
			thresholds.NetworkIn = ensureHysteresisThreshold(override.NetworkIn)
		} else if override.NetworkInLegacy != nil {
			thresholds.NetworkIn = m.convertLegacyThreshold(override.NetworkInLegacy)
		}
		if override.NetworkOut != nil {
			thresholds.NetworkOut = ensureHysteresisThreshold(override.NetworkOut)
		} else if override.NetworkOutLegacy != nil {
			thresholds.NetworkOut = m.convertLegacyThreshold(override.NetworkOutLegacy)
		}
	}

	return thresholds
}

// checkRateLimit checks if an alert has exceeded rate limit
func (m *Manager) checkRateLimit(alertID string) bool {
	if m.config.Schedule.MaxAlertsHour <= 0 {
		return true // No rate limit
	}

	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)

	// Clean old entries and count recent alerts
	var recentAlerts []time.Time
	if times, exists := m.alertRateLimit[alertID]; exists {
		for _, t := range times {
			if t.After(cutoff) {
				recentAlerts = append(recentAlerts, t)
			}
		}
	}

	// Check if we've hit the limit
	if len(recentAlerts) >= m.config.Schedule.MaxAlertsHour {
		return false
	}

	// Add current time
	recentAlerts = append(recentAlerts, now)
	m.alertRateLimit[alertID] = recentAlerts

	return true
}

// escalationChecker runs periodically to check for alerts that need escalation and cleanup
func (m *Manager) escalationChecker() {
	ticker := time.NewTicker(1 * time.Minute)
	cleanupTicker := time.NewTicker(10 * time.Minute) // Run cleanup every 10 minutes
	defer ticker.Stop()
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ticker.C:
			m.checkEscalations()
		case <-cleanupTicker.C:
			m.Cleanup(24 * time.Hour) // Clean up acknowledged alerts older than 24 hours
		case <-m.escalationStop:
			return
		}
	}
}

// checkEscalations checks all active alerts for escalation
func (m *Manager) checkEscalations() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.config.Schedule.Escalation.Enabled {
		return
	}

	now := time.Now()
	for _, alert := range m.activeAlerts {
		// Skip acknowledged alerts
		if alert.Acknowledged {
			continue
		}

		// Check each escalation level
		for i, level := range m.config.Schedule.Escalation.Levels {
			// Skip if we've already escalated to this level
			if alert.LastEscalation >= i+1 {
				continue
			}

			// Check if it's time to escalate
			escalateTime := alert.StartTime.Add(time.Duration(level.After) * time.Minute)
			if now.After(escalateTime) {
				// Update alert escalation state
				alert.LastEscalation = i + 1
				alert.EscalationTimes = append(alert.EscalationTimes, now)

				log.Info().
					Str("alertID", alert.ID).
					Int("level", i+1).
					Str("notify", level.Notify).
					Msg("Alert escalated")

				// Trigger escalation callback
				m.safeCallEscalateCallback(alert, i+1)
			}
		}
	}
}

// Stop stops the alert manager and saves history
func (m *Manager) Stop() {
	close(m.escalationStop)
	m.historyManager.Stop()

	// Give background goroutines time to exit cleanly
	time.Sleep(100 * time.Millisecond)

	// Save active alerts before stopping
	if err := m.SaveActiveAlerts(); err != nil {
		log.Error().Err(err).Msg("Failed to save active alerts on stop")
	}
}

// SaveActiveAlerts persists active alerts to disk
func (m *Manager) SaveActiveAlerts() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Create directory if it doesn't exist
	alertsDir := filepath.Join(utils.GetDataDir(), "alerts")
	if err := os.MkdirAll(alertsDir, 0755); err != nil {
		return fmt.Errorf("failed to create alerts directory: %w", err)
	}

	// Convert map to slice for JSON encoding
	alerts := make([]*Alert, 0, len(m.activeAlerts))
	for _, alert := range m.activeAlerts {
		alerts = append(alerts, alert)
	}

	data, err := json.MarshalIndent(alerts, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal active alerts: %w", err)
	}

	// Write to temporary file first, then rename (atomic operation)
	tmpFile := filepath.Join(alertsDir, "active-alerts.json.tmp")
	finalFile := filepath.Join(alertsDir, "active-alerts.json")

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write active alerts: %w", err)
	}

	if err := os.Rename(tmpFile, finalFile); err != nil {
		return fmt.Errorf("failed to rename active alerts file: %w", err)
	}

	log.Info().Int("count", len(alerts)).Msg("Saved active alerts to disk")
	return nil
}

// LoadActiveAlerts restores active alerts from disk
func (m *Manager) LoadActiveAlerts() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alertsFile := filepath.Join(utils.GetDataDir(), "alerts", "active-alerts.json")
	data, err := os.ReadFile(alertsFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info().Msg("No active alerts file found, starting fresh")
			return nil
		}
		return fmt.Errorf("failed to read active alerts: %w", err)
	}

	var alerts []*Alert
	if err := json.Unmarshal(data, &alerts); err != nil {
		return fmt.Errorf("failed to unmarshal active alerts: %w", err)
	}

	// Restore alerts to the map with deduplication
	now := time.Now()
	restoredCount := 0
	duplicateCount := 0
	seen := make(map[string]bool)

	for _, alert := range alerts {
		// Skip duplicates
		if seen[alert.ID] {
			duplicateCount++
			log.Warn().Str("alertID", alert.ID).Msg("Skipping duplicate alert during restore")
			continue
		}
		seen[alert.ID] = true

		// Skip very old alerts (older than 24 hours)
		if now.Sub(alert.StartTime) > 24*time.Hour {
			log.Debug().Str("alertID", alert.ID).Msg("Skipping old alert during restore")
			continue
		}

		// Skip acknowledged alerts older than 1 hour
		if alert.Acknowledged && alert.AckTime != nil && now.Sub(*alert.AckTime) > time.Hour {
			log.Debug().Str("alertID", alert.ID).Msg("Skipping old acknowledged alert")
			continue
		}

		m.activeAlerts[alert.ID] = alert
		if alert.Acknowledged {
			ackTime := alert.StartTime
			if alert.AckTime != nil {
				ackTime = *alert.AckTime
			}
			m.ackState[alert.ID] = ackRecord{
				acknowledged: true,
				user:         alert.AckUser,
				time:         ackTime,
			}
		}
		restoredCount++

		// For critical alerts that are still active after restart, send notifications
		// This ensures users are notified about ongoing critical issues even after service restarts
		// Only notify for alerts that started recently (within last 2 hours) to avoid spam
		if alert.Level == AlertLevelCritical && now.Sub(alert.StartTime) < 2*time.Hour {
			// Use a goroutine and add a small delay to avoid notification spam on startup
			alertCopy := alert.Clone()
			go func(a *Alert) {
				time.Sleep(10 * time.Second) // Wait for system to stabilize after restart
				log.Info().
					Str("alertID", a.ID).
					Str("resource", a.ResourceName).
					Msg("Attempting to send notification for restored critical alert")
				m.dispatchAlert(a, false) // Use dispatchAlert to respect activation state and quiet hours
			}(alertCopy)
		}
	}

	log.Info().
		Int("restored", restoredCount).
		Int("total", len(alerts)).
		Int("duplicates", duplicateCount).
		Msg("Restored active alerts from disk")
	return nil
}

// CleanupAlertsForNodes removes alerts for nodes that no longer exist
func (m *Manager) CleanupAlertsForNodes(existingNodes map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	log.Info().
		Int("totalAlerts", len(m.activeAlerts)).
		Int("existingNodes", len(existingNodes)).
		Interface("nodes", existingNodes).
		Msg("Starting alert cleanup for non-existent nodes")

	removedCount := 0
	for alertID, alert := range m.activeAlerts {
		if alert == nil {
			continue
		}

		// Skip alerts that are not tied to Proxmox nodes. Docker and PBS resources use
		// synthetic node identifiers that won't appear in the Proxmox node list, so we
		// must preserve their alerts here.
		if strings.HasPrefix(alertID, "docker-") || strings.HasPrefix(alert.ResourceID, "docker:") {
			continue
		}
		if strings.HasPrefix(alertID, "pbs-") || alert.Type == "pbs-offline" {
			continue
		}
		// Use the Node field from the alert itself, which is more reliable
		node := alert.Node

		// If we couldn't get a node or the node doesn't exist, remove the alert
		if node == "" || !existingNodes[node] {
			m.removeActiveAlertNoLock(alertID)
			removedCount++
			log.Debug().Str("alertID", alertID).Str("node", node).Msg("Removed alert for non-existent node")
		}
	}

	if removedCount > 0 {
		log.Info().Int("removed", removedCount).Int("remaining", len(m.activeAlerts)).Msg("Cleaned up alerts for non-existent nodes")
		// Save the cleaned up state
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine (cleanup)")
				}
			}()
			if err := m.SaveActiveAlerts(); err != nil {
				log.Error().Err(err).Msg("Failed to save alerts after cleanup")
			}
		}()
	} else {
		log.Info().Msg("No alerts needed cleanup")
	}
}

// ClearActiveAlerts removes all active and pending alerts, resetting the manager state.
func (m *Manager) ClearActiveAlerts() {
	m.mu.Lock()
	if len(m.activeAlerts) == 0 && len(m.pendingAlerts) == 0 {
		m.mu.Unlock()
		return
	}
	m.activeAlerts = make(map[string]*Alert)
	m.pendingAlerts = make(map[string]time.Time)
	m.recentAlerts = make(map[string]*Alert)
	m.suppressedUntil = make(map[string]time.Time)
	m.alertRateLimit = make(map[string][]time.Time)
	m.nodeOfflineCount = make(map[string]int)
	m.offlineConfirmations = make(map[string]int)
	m.dockerOfflineCount = make(map[string]int)
	m.dockerStateConfirm = make(map[string]int)
	m.ackState = make(map[string]ackRecord)
	m.mu.Unlock()

	m.resolvedMutex.Lock()
	m.recentlyResolved = make(map[string]*ResolvedAlert)
	m.resolvedMutex.Unlock()

	log.Info().Msg("Cleared all active and pending alerts")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Msg("Panic in SaveActiveAlerts goroutine (clear)")
			}
		}()
		if err := m.SaveActiveAlerts(); err != nil {
			log.Error().Err(err).Msg("Failed to persist cleared alerts")
		}
	}()
}

// periodicSaveAlerts saves active alerts to disk periodically
func (m *Manager) periodicSaveAlerts() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.SaveActiveAlerts(); err != nil {
				log.Error().Err(err).Msg("Failed to save active alerts during periodic save")
			}
		case <-m.escalationStop:
			return
		}
	}
}

// CheckDiskHealth checks disk health and creates alerts if needed
func (m *Manager) CheckDiskHealth(instance, node string, disk proxmox.Disk) {
	// Create unique alert ID for this disk
	alertID := fmt.Sprintf("disk-health-%s-%s-%s", instance, node, disk.DevPath)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if disk health is not PASSED
	normalizedHealth := strings.ToUpper(strings.TrimSpace(disk.Health))
	if normalizedHealth != "" && normalizedHealth != "UNKNOWN" && normalizedHealth != "PASSED" && normalizedHealth != "OK" {
		// Check if alert already exists
		if _, exists := m.activeAlerts[alertID]; !exists {
			// Create new health alert
			alert := &Alert{
				ID:           alertID,
				Type:         "disk-health",
				Level:        AlertLevelCritical,
				ResourceID:   fmt.Sprintf("%s-%s", node, disk.DevPath),
				ResourceName: fmt.Sprintf("%s (%s)", disk.Model, disk.DevPath),
				Node:         node,
				Instance:     instance,
				Message:      fmt.Sprintf("Disk health check failed: %s", disk.Health),
				Value:        0, // Not applicable for health status
				Threshold:    0,
				StartTime:    time.Now(),
				LastSeen:     time.Now(),
				Metadata: map[string]interface{}{
					"disk_path":   disk.DevPath,
					"disk_model":  disk.Model,
					"disk_serial": disk.Serial,
					"disk_type":   disk.Type,
					"disk_health": disk.Health,
					"disk_size":   disk.Size,
				},
			}

			m.preserveAlertState(alertID, alert)

			m.activeAlerts[alertID] = alert
			m.recentAlerts[alertID] = alert
			m.historyManager.AddAlert(*alert)

			m.dispatchAlert(alert, false)

			log.Error().
				Str("node", node).
				Str("disk", disk.DevPath).
				Str("model", disk.Model).
				Str("health", disk.Health).
				Msg("Disk health alert created")
		}
	} else {
		// Disk is healthy, clear alert if it exists
		m.clearAlertNoLock(alertID)
	}

	// Check for low wearout (SSD life remaining)
	if disk.Wearout > 0 && disk.Wearout < 10 {
		wearoutAlertID := fmt.Sprintf("disk-wearout-%s-%s-%s", instance, node, disk.DevPath)
		message := fmt.Sprintf("SSD has less than 10%% life remaining (%d%% wearout)", disk.Wearout)
		resourceID := fmt.Sprintf("%s-%s", node, disk.DevPath)
		resourceName := fmt.Sprintf("%s (%s)", disk.Model, disk.DevPath)

		if existing, exists := m.activeAlerts[wearoutAlertID]; exists {
			// Refresh details so legacy alerts pick up updated wording and metadata
			existing.LastSeen = time.Now()
			existing.Value = float64(disk.Wearout)
			existing.Message = message
			existing.ResourceID = resourceID
			existing.ResourceName = resourceName
			existing.Node = node
			existing.Instance = instance
			if existing.Metadata == nil {
				existing.Metadata = map[string]interface{}{}
			}
			existing.Metadata["disk_path"] = disk.DevPath
			existing.Metadata["disk_model"] = disk.Model
			existing.Metadata["disk_serial"] = disk.Serial
			existing.Metadata["disk_type"] = disk.Type
			existing.Metadata["disk_wearout"] = disk.Wearout
			delete(existing.Metadata, "disk_wearout_used")
		} else {
			// Create wearout alert
			alert := &Alert{
				ID:           wearoutAlertID,
				Type:         "disk-wearout",
				Level:        AlertLevelWarning,
				ResourceID:   resourceID,
				ResourceName: resourceName,
				Node:         node,
				Instance:     instance,
				Message:      message,
				Value:        float64(disk.Wearout),
				Threshold:    10.0,
				StartTime:    time.Now(),
				LastSeen:     time.Now(),
				Metadata: map[string]interface{}{
					"disk_path":    disk.DevPath,
					"disk_model":   disk.Model,
					"disk_serial":  disk.Serial,
					"disk_type":    disk.Type,
					"disk_wearout": disk.Wearout,
				},
			}

			m.preserveAlertState(wearoutAlertID, alert)

			m.activeAlerts[wearoutAlertID] = alert
			m.recentAlerts[wearoutAlertID] = alert
			m.historyManager.AddAlert(*alert)

			m.dispatchAlert(alert, false)

			log.Warn().
				Str("node", node).
				Str("disk", disk.DevPath).
				Str("model", disk.Model).
				Int("wearout", disk.Wearout).
				Msg("Disk wearout alert created")
		}
	} else if disk.Wearout >= 10 {
		// Wearout is acceptable, clear alert if it exists
		wearoutAlertID := fmt.Sprintf("disk-wearout-%s-%s-%s", instance, node, disk.DevPath)
		m.clearAlertNoLock(wearoutAlertID)
	}
}

// clearAlertNoLock clears an alert without locking (must be called with lock held)
func (m *Manager) clearAlertNoLock(alertID string) {
	alert, exists := m.activeAlerts[alertID]
	if !exists {
		return
	}

	m.removeActiveAlertNoLock(alertID)
	resolvedAlert := &ResolvedAlert{
		Alert:        alert,
		ResolvedTime: time.Now(),
	}

	m.addRecentlyResolvedWithPrimaryLock(alertID, resolvedAlert)

	m.safeCallResolvedCallback(alertID, true) // Make async to prevent deadlock

	log.Info().
		Str("alertID", alertID).
		Msg("Alert cleared")
}

func (m *Manager) clearSnapshotAlertsForInstance(instance string) {
	m.mu.Lock()
	m.clearSnapshotAlertsForInstanceLocked(instance)
	m.mu.Unlock()
}

func (m *Manager) clearSnapshotAlertsForInstanceLocked(instance string) {
	for alertID, alert := range m.activeAlerts {
		if alert == nil || alert.Type != "snapshot-age" {
			continue
		}
		if instance != "" && alert.Instance != instance {
			continue
		}
		m.clearAlertNoLock(alertID)
	}
}

func (m *Manager) clearBackupAlerts() {
	m.mu.Lock()
	m.clearBackupAlertsLocked()
	m.mu.Unlock()
}

func (m *Manager) clearBackupAlertsLocked() {
	for alertID, alert := range m.activeAlerts {
		if alert == nil || alert.Type != "backup-age" {
			continue
		}
		m.clearAlertNoLock(alertID)
	}
}
