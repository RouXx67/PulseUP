package monitoring

import (
	"testing"
	"time"

	"github.com/RouXx67/PulseUp/internal/alerts"
	"github.com/RouXx67/PulseUp/internal/config"
	"github.com/RouXx67/PulseUp/internal/models"
	agentsdocker "github.com/RouXx67/PulseUp/pkg/agents/docker"
)

func newTestMonitor(t *testing.T) *Monitor {
	t.Helper()

	return &Monitor{
		state:              models.NewState(),
		alertManager:       alerts.NewManager(),
		removedDockerHosts: make(map[string]time.Time),
	}
}

func TestApplyDockerReportGeneratesUniqueIDsForCollidingHosts(t *testing.T) {
	monitor := newTestMonitor(t)

	baseTimestamp := time.Now().UTC()
	baseReport := agentsdocker.Report{
		Agent: agentsdocker.AgentInfo{
			Version:         "1.0.0",
			IntervalSeconds: 30,
		},
		Host: agentsdocker.HostInfo{
			Hostname:         "docker-host",
			Name:             "Docker Host",
			MachineID:        "machine-duplicate",
			DockerVersion:    "26.0.0",
			TotalCPU:         4,
			TotalMemoryBytes: 8 << 30,
			UptimeSeconds:    120,
		},
		Containers: []agentsdocker.Container{
			{ID: "container-1", Name: "nginx"},
		},
		Timestamp: baseTimestamp,
	}

	token1 := &config.APITokenRecord{ID: "token-host-1", Name: "Host 1"}
	host1, err := monitor.ApplyDockerReport(baseReport, token1)
	if err != nil {
		t.Fatalf("ApplyDockerReport host1: %v", err)
	}
	if host1.ID == "" {
		t.Fatalf("expected host1 to have an identifier")
	}

	hosts := monitor.state.GetDockerHosts()
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host after first report, got %d", len(hosts))
	}

	secondReport := baseReport
	secondReport.Host.Name = "Docker Host Clone"
	secondReport.Timestamp = baseTimestamp.Add(45 * time.Second)

	token2 := &config.APITokenRecord{ID: "token-host-2", Name: "Host 2"}
	host2, err := monitor.ApplyDockerReport(secondReport, token2)
	if err != nil {
		t.Fatalf("ApplyDockerReport host2: %v", err)
	}
	if host2.ID == "" {
		t.Fatalf("expected host2 to have an identifier")
	}
	if host2.ID == host1.ID {
		t.Fatalf("expected unique identifiers, but both hosts share %q", host2.ID)
	}

	hosts = monitor.state.GetDockerHosts()
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts after second report, got %d", len(hosts))
	}

	secondReport.Timestamp = secondReport.Timestamp.Add(45 * time.Second)
	secondReport.Containers = append(secondReport.Containers, agentsdocker.Container{
		ID:   "container-2",
		Name: "redis",
	})

	updatedHost2, err := monitor.ApplyDockerReport(secondReport, token2)
	if err != nil {
		t.Fatalf("ApplyDockerReport host2 update: %v", err)
	}
	if updatedHost2.ID != host2.ID {
		t.Fatalf("expected host2 to retain identifier %q, got %q", host2.ID, updatedHost2.ID)
	}

	hosts = monitor.state.GetDockerHosts()
	var found models.DockerHost
	for _, h := range hosts {
		if h.ID == host2.ID {
			found = h
			break
		}
	}
	if found.ID == "" {
		t.Fatalf("failed to locate host2 in state after update")
	}
	if len(found.Containers) != 2 {
		t.Fatalf("expected host2 to have 2 containers after update, got %d", len(found.Containers))
	}
}
