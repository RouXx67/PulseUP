package hostagent

import "time"

// Report represents a single heartbeat from the Host agent to Pulse.
type Report struct {
	Agent     AgentInfo `json:"agent"`
	Host      HostInfo  `json:"host"`
	Timestamp time.Time `json:"timestamp"`
}

// AgentInfo describes the reporting agent instance.
type AgentInfo struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	IntervalSeconds int    `json:"intervalSeconds"`
}

// HostInfo contains metadata about the host where the agent runs.
type HostInfo struct {
	ID                 string             `json:"id,omitempty"`
	Hostname           string             `json:"hostname"`
	DisplayName        string             `json:"displayName,omitempty"`
	MachineID          string             `json:"machineId,omitempty"`
	Platform           string             `json:"platform,omitempty"`
	OSName             string             `json:"osName,omitempty"`
	OSVersion          string             `json:"osVersion,omitempty"`
	KernelVersion      string             `json:"kernelVersion,omitempty"`
	Architecture       string             `json:"architecture,omitempty"`
	CPUCount           int                `json:"cpuCount,omitempty"`
	TotalMemoryBytes   int64              `json:"totalMemoryBytes,omitempty"`
	LoadAverage        []float64          `json:"loadAverage,omitempty"`
	UptimeSeconds      int64              `json:"uptimeSeconds,omitempty"`
	Disks              []DiskInfo         `json:"disks,omitempty"`
	NetworkInterfaces  []NetworkInterface `json:"networkInterfaces,omitempty"`
}

// DiskInfo contains information about disk usage.
type DiskInfo struct {
	Device     string `json:"device"`
	Mountpoint string `json:"mountpoint"`
	Filesystem string `json:"filesystem"`
	SizeBytes  int64  `json:"sizeBytes"`
	UsedBytes  int64  `json:"usedBytes"`
	FreeBytes  int64  `json:"freeBytes"`
	UsedPercent float64 `json:"usedPercent"`
}

// NetworkInterface contains information about network interfaces.
type NetworkInterface struct {
	Name         string `json:"name"`
	HardwareAddr string `json:"hardwareAddr,omitempty"`
	MTU          int    `json:"mtu,omitempty"`
	Flags        string `json:"flags,omitempty"`
	Addresses    []string `json:"addresses,omitempty"`
}
