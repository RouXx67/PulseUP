package alerts

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RouXx67/PulseUp/internal/models"
)

func TestAcknowledgePersistsThroughCheckMetric(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()
	// Set config fields directly to bypass UpdateConfig's default value enforcement
	m.mu.Lock()
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.config.SuppressionWindow = 0
	m.config.MinimumDelta = 0
	m.mu.Unlock()

	threshold := &HysteresisThreshold{Trigger: 80, Clear: 70}
	m.checkMetric("res1", "Resource", "node1", "inst1", "guest", "usage", 90, threshold, nil)
	if _, exists := m.activeAlerts["res1-usage"]; !exists {
		t.Fatalf("expected alert to be created")
	}

	if err := m.AcknowledgeAlert("res1-usage", "tester"); err != nil {
		t.Fatalf("ack failed: %v", err)
	}

	if !m.activeAlerts["res1-usage"].Acknowledged {
		t.Fatalf("acknowledged flag not set")
	}

	alerts := m.GetActiveAlerts()
	if len(alerts) != 1 || !alerts[0].Acknowledged {
		t.Fatalf("GetActiveAlerts lost acknowledgement")
	}

	m.checkMetric("res1", "Resource", "node1", "inst1", "guest", "usage", 85, threshold, nil)
	if !m.activeAlerts["res1-usage"].Acknowledged {
		t.Fatalf("acknowledged flag lost after update")
	}
}

func TestCheckGuestSkipsAlertsWhenMetricDisabled(t *testing.T) {
	m := NewManager()

	vmID := "instance-node-101"
	instanceName := "instance"

	// Start with default configuration to allow CPU alerts.
	initialConfig := AlertConfig{
		Enabled: true,
		GuestDefaults: ThresholdConfig{
			CPU: &HysteresisThreshold{Trigger: 80, Clear: 75},
		},
		TimeThreshold:  0,
		TimeThresholds: map[string]int{},
		NodeDefaults: ThresholdConfig{
			CPU:    &HysteresisThreshold{Trigger: 80, Clear: 75},
			Memory: &HysteresisThreshold{Trigger: 85, Clear: 80},
			Disk:   &HysteresisThreshold{Trigger: 90, Clear: 85},
		},
		StorageDefault: HysteresisThreshold{Trigger: 85, Clear: 80},
		Overrides:      make(map[string]ThresholdConfig),
	}
	m.UpdateConfig(initialConfig)
	m.mu.Lock()
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.mu.Unlock()

	var dispatched []*Alert
	done := make(chan struct{}, 1)
	var resolved []string
	resolvedDone := make(chan struct{}, 1)
	m.SetAlertCallback(func(alert *Alert) {
		dispatched = append(dispatched, alert)
		select {
		case done <- struct{}{}:
		default:
		}
	})
	m.SetResolvedCallback(func(alertID string) {
		resolved = append(resolved, alertID)
		select {
		case resolvedDone <- struct{}{}:
		default:
		}
	})

	vm := models.VM{
		ID:       vmID,
		Name:     "test-vm",
		Node:     "node",
		Instance: instanceName,
		Status:   "running",
		CPU:      1.0, // 100% once multiplied by 100 inside CheckGuest
		Memory: models.Memory{
			Usage: 65,
		},
		Disk: models.Disk{
			Usage: 40,
		},
	}

	// Initial check should trigger an alert with default thresholds.
	m.CheckGuest(vm, instanceName)
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("did not receive initial alert dispatch")
	}
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 alert before disabling metric, got %d", len(dispatched))
	}

	// Apply override disabling CPU alerts for this VM.
	disabledConfig := initialConfig
	disabledConfig.Overrides = map[string]ThresholdConfig{
		vmID: {
			CPU: &HysteresisThreshold{Trigger: -1, Clear: 0},
		},
	}
	disabledConfig.TimeThreshold = 0
	disabledConfig.TimeThresholds = map[string]int{}
	m.UpdateConfig(disabledConfig)
	m.mu.Lock()
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.mu.Unlock()

	// Clear dispatched slice to capture only post-disable notifications.
	dispatched = dispatched[:0]
	done = make(chan struct{}, 1)

	// Re-run evaluation with high CPU; no alert should be dispatched.
	m.CheckGuest(vm, instanceName)
	select {
	case <-done:
		t.Fatalf("expected no alerts after disabling CPU metric, but callback fired")
	case <-time.After(100 * time.Millisecond):
		// No callback fired as expected.
	}

	// Active alerts should be cleared by the config update.
	m.mu.RLock()
	activeCount := len(m.activeAlerts)
	m.mu.RUnlock()
	if activeCount != 0 {
		t.Fatalf("expected active alerts to be cleared after disabling metric, got %d", activeCount)
	}

	select {
	case <-resolvedDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected resolved callback to fire after disabling metric")
	}
	if len(resolved) != 1 || resolved[0] != fmt.Sprintf("%s-cpu", vmID) {
		t.Fatalf("expected resolved callback for %s-cpu, got %v", vmID, resolved)
	}

	m.mu.RLock()
	_, isPending := m.pendingAlerts[fmt.Sprintf("%s-cpu", vmID)]
	m.mu.RUnlock()
	if isPending {
		t.Fatalf("expected pending alert entry to be cleared after disabling metric")
	}
}

func TestHandleDockerHostRemovedClearsAlertsAndTracking(t *testing.T) {
	m := NewManager()
	host := models.DockerHost{ID: "host1", DisplayName: "Host One", Hostname: "host-one"}
	containerResourceID := "docker:host1/container1"
	containerAlertID := "docker-container-state-" + containerResourceID
	hostAlertID := "docker-host-offline-host1"

	m.mu.Lock()
	m.activeAlerts[hostAlertID] = &Alert{ID: hostAlertID, ResourceID: "docker:host1"}
	m.activeAlerts[containerAlertID] = &Alert{ID: containerAlertID, ResourceID: containerResourceID}
	m.dockerOfflineCount[host.ID] = 2
	m.dockerStateConfirm[containerResourceID] = 1
	m.dockerRestartTracking[containerResourceID] = &dockerRestartRecord{}
	m.dockerLastExitCode[containerResourceID] = 137
	m.mu.Unlock()

	m.HandleDockerHostRemoved(host)

	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, exists := m.activeAlerts[containerAlertID]; exists {
		t.Fatalf("expected container alerts to be cleared")
	}
	if _, exists := m.activeAlerts[hostAlertID]; exists {
		t.Fatalf("expected host offline alert to be cleared")
	}
	if _, exists := m.dockerOfflineCount[host.ID]; exists {
		t.Fatalf("expected offline tracking to be cleared")
	}
	if _, exists := m.dockerStateConfirm[containerResourceID]; exists {
		t.Fatalf("expected state confirmation to be cleared")
	}
	if _, exists := m.dockerRestartTracking[containerResourceID]; exists {
		t.Fatalf("expected restart tracking to be cleared")
	}
	if _, exists := m.dockerLastExitCode[containerResourceID]; exists {
		t.Fatalf("expected last exit code tracking to be cleared")
	}
}

func TestCheckSnapshotsForInstanceCreatesAndClearsAlerts(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()

	cfg := AlertConfig{
		Enabled:        true,
		StorageDefault: HysteresisThreshold{Trigger: 85, Clear: 80},
		SnapshotDefaults: SnapshotAlertConfig{
			Enabled:         true,
			WarningDays:     7,
			CriticalDays:    14,
			WarningSizeGiB:  0,
			CriticalSizeGiB: 0,
		},
		Overrides: make(map[string]ThresholdConfig),
	}
	m.UpdateConfig(cfg)
	m.mu.Lock()
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.mu.Unlock()

	now := time.Now()
	snapshots := []models.GuestSnapshot{
		{
			ID:        "inst-node-100-weekly",
			Name:      "weekly",
			Node:      "node",
			Instance:  "inst",
			Type:      "qemu",
			VMID:      100,
			Time:      now.Add(-15 * 24 * time.Hour),
			SizeBytes: 60 << 30,
		},
	}
	guestNames := map[string]string{
		"inst-node-100": "app-server",
	}

	m.CheckSnapshotsForInstance("inst", snapshots, guestNames)

	m.mu.RLock()
	alert, exists := m.activeAlerts["snapshot-age-inst-node-100-weekly"]
	m.mu.RUnlock()
	if !exists {
		t.Fatalf("expected snapshot age alert to be created")
	}
	if alert.Level != AlertLevelCritical {
		t.Fatalf("expected critical level for old snapshot, got %s", alert.Level)
	}
	if alert.ResourceName != "app-server snapshot 'weekly'" {
		t.Fatalf("unexpected resource name: %s", alert.ResourceName)
	}

	m.CheckSnapshotsForInstance("inst", nil, guestNames)

	m.mu.RLock()
	_, exists = m.activeAlerts["snapshot-age-inst-node-100-weekly"]
	m.mu.RUnlock()
	if exists {
		t.Fatalf("expected snapshot alert to be cleared when snapshot missing")
	}
}

func TestCheckSnapshotsForInstanceTriggersOnSnapshotSize(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()

	cfg := AlertConfig{
		Enabled:        true,
		StorageDefault: HysteresisThreshold{Trigger: 85, Clear: 80},
		SnapshotDefaults: SnapshotAlertConfig{
			Enabled:         true,
			WarningDays:     0,
			CriticalDays:    0,
			WarningSizeGiB:  50,
			CriticalSizeGiB: 100,
		},
		Overrides: make(map[string]ThresholdConfig),
	}
	m.UpdateConfig(cfg)
	m.mu.Lock()
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.mu.Unlock()

	now := time.Now()
	snapshots := []models.GuestSnapshot{
		{
			ID:        "inst-node-200-sizey",
			Name:      "pre-maintenance",
			Node:      "node",
			Instance:  "inst",
			Type:      "qemu",
			VMID:      200,
			Time:      now.Add(-2 * time.Hour),
			SizeBytes: int64(120) << 30,
		},
	}
	guestNames := map[string]string{
		"inst-node-200": "db-server",
	}

	m.CheckSnapshotsForInstance("inst", snapshots, guestNames)

	m.mu.RLock()
	alert, exists := m.activeAlerts["snapshot-age-inst-node-200-sizey"]
	m.mu.RUnlock()
	if !exists {
		t.Fatalf("expected snapshot size alert to be created")
	}
	if alert.Level != AlertLevelCritical {
		t.Fatalf("expected critical level for large snapshot, got %s", alert.Level)
	}
	if alert.Value < 119.5 || alert.Value > 120.5 {
		t.Fatalf("expected alert value near 120 GiB, got %.2f", alert.Value)
	}
	if alert.Threshold != 100 {
		t.Fatalf("expected threshold 100 GiB, got %.2f", alert.Threshold)
	}
	if alert.Metadata == nil {
		t.Fatalf("expected metadata for snapshot alert")
	}
	if metric, ok := alert.Metadata["primaryMetric"].(string); !ok || metric != "size" {
		t.Fatalf("expected primary metric size, got %#v", alert.Metadata["primaryMetric"])
	}
	if sizeBytes, ok := alert.Metadata["snapshotSizeBytes"].(int64); !ok || sizeBytes == 0 {
		t.Fatalf("expected snapshotSizeBytes in metadata")
	}
	metrics, ok := alert.Metadata["triggeredMetrics"].([]string)
	if !ok {
		t.Fatalf("expected triggeredMetrics slice, got %#v", alert.Metadata["triggeredMetrics"])
	}
	foundSize := false
	for _, metric := range metrics {
		if metric == "size" {
			foundSize = true
			break
		}
	}
	if !foundSize {
		t.Fatalf("expected size metric recorded in metadata")
	}
}

func TestCheckSnapshotsForInstanceIncludesAgeAndSizeReasons(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()

	cfg := AlertConfig{
		Enabled:        true,
		StorageDefault: HysteresisThreshold{Trigger: 85, Clear: 80},
		SnapshotDefaults: SnapshotAlertConfig{
			Enabled:         true,
			WarningDays:     5,
			CriticalDays:    10,
			WarningSizeGiB:  40,
			CriticalSizeGiB: 80,
		},
		Overrides: make(map[string]ThresholdConfig),
	}
	m.UpdateConfig(cfg)
	m.mu.Lock()
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.mu.Unlock()

	now := time.Now()
	snapshots := []models.GuestSnapshot{
		{
			ID:        "inst-node-300-combined",
			Name:      "long-running",
			Node:      "node",
			Instance:  "inst",
			Type:      "qemu",
			VMID:      300,
			Time:      now.Add(-15 * 24 * time.Hour),
			SizeBytes: int64(90) << 30,
		},
	}
	guestNames := map[string]string{
		"inst-node-300": "app-server",
	}

	m.CheckSnapshotsForInstance("inst", snapshots, guestNames)

	m.mu.RLock()
	alert, exists := m.activeAlerts["snapshot-age-inst-node-300-combined"]
	m.mu.RUnlock()
	if !exists {
		t.Fatalf("expected combined snapshot alert to be created")
	}
	if alert.Level != AlertLevelCritical {
		t.Fatalf("expected critical level, got %s", alert.Level)
	}
	if !strings.Contains(alert.Message, "days old") || !strings.Contains(strings.ToLower(alert.Message), "gib") {
		t.Fatalf("expected alert message to reference age and size, got %q", alert.Message)
	}
	if alert.Metadata == nil {
		t.Fatalf("expected metadata for combined alert")
	}
	metrics, ok := alert.Metadata["triggeredMetrics"].([]string)
	if !ok {
		t.Fatalf("expected triggeredMetrics slice, got %#v", alert.Metadata["triggeredMetrics"])
	}
	if len(metrics) < 2 {
		t.Fatalf("expected both age and size metrics recorded, got %v", metrics)
	}
	if metric, ok := alert.Metadata["primaryMetric"].(string); !ok || metric != "age" {
		t.Fatalf("expected primary metric age, got %#v", alert.Metadata["primaryMetric"])
	}
}

func TestCheckBackupsCreatesAndClearsAlerts(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()

	m.mu.Lock()
	m.config.Enabled = true
	m.config.BackupDefaults = BackupAlertConfig{
		Enabled:      true,
		WarningDays:  7,
		CriticalDays: 14,
	}
	m.config.TimeThreshold = 0
	m.config.TimeThresholds = map[string]int{}
	m.mu.Unlock()

	now := time.Now()
	storageBackups := []models.StorageBackup{
		{
			ID:       "inst-node-100-backup",
			Storage:  "local",
			Node:     "node",
			Instance: "inst",
			Type:     "qemu",
			VMID:     100,
			Time:     now.Add(-15 * 24 * time.Hour),
		},
	}

	key := BuildGuestKey("inst", "node", 100)
	guestsByKey := map[string]GuestLookup{
		key: {
			Name:     "app-server",
			Instance: "inst",
			Node:     "node",
			Type:     "qemu",
			VMID:     100,
		},
	}
	guestsByVMID := map[string]GuestLookup{
		"100": guestsByKey[key],
	}

	m.CheckBackups(storageBackups, nil, nil, guestsByKey, guestsByVMID)

	m.mu.RLock()
	alert, exists := m.activeAlerts["backup-age-"+sanitizeAlertKey(key)]
	m.mu.RUnlock()
	if !exists {
		t.Fatalf("expected backup age alert to be created")
	}
	if alert.Level != AlertLevelCritical {
		t.Fatalf("expected critical backup alert, got %s", alert.Level)
	}

	// Recent backup clears alert
	storageBackups[0].Time = now
	m.CheckBackups(storageBackups, nil, nil, guestsByKey, guestsByVMID)

	m.mu.RLock()
	_, exists = m.activeAlerts["backup-age-"+sanitizeAlertKey(key)]
	m.mu.RUnlock()
	if exists {
		t.Fatalf("expected backup-age alert to clear after fresh backup")
	}
}

func TestCheckBackupsHandlesPbsOnlyGuests(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()

	m.mu.Lock()
	m.config.Enabled = true
	m.config.BackupDefaults = BackupAlertConfig{
		Enabled:      true,
		WarningDays:  3,
		CriticalDays: 5,
	}
	m.mu.Unlock()

	now := time.Now()
	pbsBackups := []models.PBSBackup{
		{
			ID:         "pbs-backup-999-0",
			Instance:   "pbs-main",
			Datastore:  "backup-store",
			BackupType: "qemu",
			VMID:       "999",
			BackupTime: now.Add(-6 * 24 * time.Hour),
		},
	}

	m.CheckBackups(nil, pbsBackups, nil, map[string]GuestLookup{}, map[string]GuestLookup{})

	m.mu.RLock()
	found := false
	for id, alert := range m.activeAlerts {
		if strings.HasPrefix(id, "backup-age-") {
			found = true
			if alert.Level != AlertLevelCritical {
				t.Fatalf("expected PBS backup alert to be critical")
			}
			break
		}
	}
	m.mu.RUnlock()
	if !found {
		t.Fatalf("expected PBS backup alert to be created")
	}
}

func TestCheckBackupsHandlesPmgBackups(t *testing.T) {
	m := NewManager()
	m.ClearActiveAlerts()

	m.mu.Lock()
	m.config.Enabled = true
	m.config.BackupDefaults = BackupAlertConfig{
		Enabled:      true,
		WarningDays:  5,
		CriticalDays: 7,
	}
	m.mu.Unlock()

	now := time.Now()
	pmgBackups := []models.PMGBackup{
		{
			ID:         "pmg-backup-mail-01",
			Instance:   "mail",
			Node:       "mail-gateway",
			Filename:   "pmg-backup_2024-01-01.tgz",
			BackupTime: now.Add(-8 * 24 * time.Hour),
			Size:       123456,
		},
	}

	m.CheckBackups(nil, nil, pmgBackups, map[string]GuestLookup{}, map[string]GuestLookup{})

	m.mu.RLock()
	found := false
	for id, alert := range m.activeAlerts {
		if strings.HasPrefix(id, "backup-age-") {
			found = true
			if alert.Level != AlertLevelCritical {
				t.Fatalf("expected PMG backup alert to be critical")
			}
			break
		}
	}
	m.mu.RUnlock()
	if !found {
		t.Fatalf("expected PMG backup alert to be created")
	}
}

func TestCheckDockerHostIgnoresContainersByPrefix(t *testing.T) {
	m := NewManager()

	m.mu.Lock()
	m.config.DockerIgnoredContainerPrefixes = []string{"runner-"}
	m.mu.Unlock()

	container := models.DockerContainer{
		ID:     "1234567890ab",
		Name:   "runner-auto-1",
		State:  "exited",
		Status: "Exited (0) 3 seconds ago",
	}

	host := models.DockerHost{
		ID:          "host-ephemeral",
		Hostname:    "ci-host",
		DisplayName: "CI Host",
		Containers:  []models.DockerContainer{container},
	}

	resourceID := dockerResourceID(host.ID, container.ID)
	alertID := fmt.Sprintf("docker-container-state-%s", resourceID)

	// Run twice to satisfy the confirmation threshold when not ignored
	m.CheckDockerHost(host)
	m.CheckDockerHost(host)

	if _, exists := m.activeAlerts[alertID]; exists {
		t.Fatalf("expected no state alert for ignored container")
	}
	if _, exists := m.dockerStateConfirm[resourceID]; exists {
		t.Fatalf("expected no state confirmation tracking for ignored container")
	}
}

func TestNormalizeDockerIgnoredPrefixes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "nil input",
			input:    nil,
			expected: nil,
		},
		{
			name:     "blank entries removed",
			input:    []string{"", "   ", "\t"},
			expected: nil,
		},
		{
			name:     "trims and deduplicates preserving first occurrence casing",
			input:    []string{"  Foo ", "foo", "Bar", " bar ", "Baz"},
			expected: []string{"Foo", "Bar", "Baz"},
		},
		{
			name:     "already normalized list remains unchanged",
			input:    []string{"alpha", "beta"},
			expected: []string{"alpha", "beta"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := NormalizeDockerIgnoredPrefixes(tc.input)
			if !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func TestCheckDockerHostIgnoredPrefixClearsExistingAlerts(t *testing.T) {
	m := NewManager()

	container := models.DockerContainer{
		ID:     "abc123456789",
		Name:   "runner-job-1",
		State:  "exited",
		Status: "Exited (1) 10 seconds ago",
	}
	host := models.DockerHost{
		ID:          "docker-host",
		DisplayName: "Docker Host",
		Hostname:    "docker-host.local",
		Containers:  []models.DockerContainer{container},
	}
	resourceID := dockerResourceID(host.ID, container.ID)
	stateAlertID := fmt.Sprintf("docker-container-state-%s", resourceID)
	healthAlertID := fmt.Sprintf("docker-container-health-%s", resourceID)
	restartAlertID := fmt.Sprintf("docker-container-restart-loop-%s", resourceID)

	m.mu.Lock()
	m.config.Enabled = true
	m.config.DockerIgnoredContainerPrefixes = []string{"runner-"}
	m.activeAlerts[stateAlertID] = &Alert{ID: stateAlertID, ResourceID: resourceID}
	m.activeAlerts[healthAlertID] = &Alert{ID: healthAlertID, ResourceID: resourceID}
	m.activeAlerts[restartAlertID] = &Alert{ID: restartAlertID, ResourceID: resourceID}
	m.dockerStateConfirm[resourceID] = 2
	m.dockerRestartTracking[resourceID] = &dockerRestartRecord{}
	m.dockerLastExitCode[resourceID] = 137
	m.mu.Unlock()

	m.CheckDockerHost(host)

	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, exists := m.activeAlerts[stateAlertID]; exists {
		t.Fatalf("expected state alert cleared for ignored container")
	}
	if _, exists := m.activeAlerts[healthAlertID]; exists {
		t.Fatalf("expected health alert cleared for ignored container")
	}
	if _, exists := m.activeAlerts[restartAlertID]; exists {
		t.Fatalf("expected restart alert cleared for ignored container")
	}
	if _, exists := m.dockerStateConfirm[resourceID]; exists {
		t.Fatalf("expected state confirmation tracking cleared")
	}
	if _, exists := m.dockerRestartTracking[resourceID]; exists {
		t.Fatalf("expected restart tracking cleared")
	}
	if _, exists := m.dockerLastExitCode[resourceID]; exists {
		t.Fatalf("expected last exit code cleared")
	}
}

func TestUpdateConfigNormalizesDockerIgnoredPrefixes(t *testing.T) {
	t.Parallel()

	t.Run("nil input remains nil", func(t *testing.T) {
		t.Parallel()

		m := NewManager()
		m.UpdateConfig(AlertConfig{})

		m.mu.RLock()
		defer m.mu.RUnlock()

		if m.config.DockerIgnoredContainerPrefixes != nil {
			t.Fatalf("expected nil prefixes, got %v", m.config.DockerIgnoredContainerPrefixes)
		}
	})

	t.Run("duplicates trimmed and deduplicated", func(t *testing.T) {
		t.Parallel()

		m := NewManager()
		cfg := AlertConfig{
			DockerIgnoredContainerPrefixes: []string{
				"  Foo ",
				"foo",
				"Bar",
			},
		}

		m.UpdateConfig(cfg)

		m.mu.RLock()
		defer m.mu.RUnlock()

		expected := []string{"Foo", "Bar"}
		if !reflect.DeepEqual(m.config.DockerIgnoredContainerPrefixes, expected) {
			t.Fatalf("expected normalized prefixes %v, got %v", expected, m.config.DockerIgnoredContainerPrefixes)
		}
	})
}

func TestMatchesDockerIgnoredPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		containerName string
		containerID   string
		prefixes      []string
		want          bool
	}{
		{name: "empty prefixes", containerName: "runner-123", containerID: "abc", prefixes: nil, want: false},
		{name: "match with name", containerName: "runner-123", containerID: "abc", prefixes: []string{"runner-"}, want: true},
		{name: "match with id", containerName: "app", containerID: "abc123", prefixes: []string{"abc"}, want: true},
		{name: "trimmed comparison", containerName: "runner-job", containerID: "abc", prefixes: []string{"  runner- "}, want: true},
		{name: "case insensitive", containerName: "Runner-Job", containerID: "abc", prefixes: []string{"runner-"}, want: true},
		{name: "no match", containerName: "service", containerID: "xyz", prefixes: []string{"runner-"}, want: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := matchesDockerIgnoredPrefix(tc.containerName, tc.containerID, tc.prefixes); got != tc.want {
				t.Fatalf("matchesDockerIgnoredPrefix(%q, %q, %v) = %v, want %v", tc.containerName, tc.containerID, tc.prefixes, got, tc.want)
			}
		})
	}
}

func TestDockerInstanceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host models.DockerHost
		want string
	}{
		{name: "uses display name", host: models.DockerHost{DisplayName: "Prod Host"}, want: "Docker:Prod Host"},
		{name: "falls back to hostname", host: models.DockerHost{Hostname: "docker.local"}, want: "Docker:docker.local"},
		{name: "defaults when empty", host: models.DockerHost{}, want: "Docker"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dockerInstanceName(tc.host); got != tc.want {
				t.Fatalf("dockerInstanceName(%+v) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestDockerContainerDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		container models.DockerContainer
		want      string
	}{
		{name: "trims whitespace", container: models.DockerContainer{Name: "  app  "}, want: "app"},
		{name: "strips leading slash", container: models.DockerContainer{Name: "/runner"}, want: "runner"},
		{name: "falls back to id truncated", container: models.DockerContainer{ID: "0123456789abcdef"}, want: "0123456789ab"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dockerContainerDisplayName(tc.container); got != tc.want {
				t.Fatalf("dockerContainerDisplayName(%+v) = %q, want %q", tc.container, got, tc.want)
			}
		})
	}
}

func TestDockerResourceID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		hostID      string
		containerID string
		want        string
	}{
		{name: "both ids present", hostID: "host1", containerID: "abc", want: "docker:host1/abc"},
		{name: "missing host id", hostID: "", containerID: "abc", want: "docker:container/abc"},
		{name: "missing container id", hostID: "host1", containerID: "", want: "docker:host1"},
		{name: "both missing", hostID: "", containerID: "", want: "docker:unknown"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := dockerResourceID(tc.hostID, tc.containerID); got != tc.want {
				t.Fatalf("dockerResourceID(%q, %q) = %q, want %q", tc.hostID, tc.containerID, got, tc.want)
			}
		})
	}
}
