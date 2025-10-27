package models

// Frontend-friendly type aliases with proper JSON tags
// These extend the base types with additional computed fields

// NodeFrontend represents a Node with frontend-friendly field names
type NodeFrontend struct {
	ID               string       `json:"id"`
	Node             string       `json:"node"` // Maps to Name
	Name             string       `json:"name"`
	DisplayName      string       `json:"displayName"`
	Instance         string       `json:"instance"`
	Host             string       `json:"host,omitempty"`
	Status           string       `json:"status"`
	Type             string       `json:"type"`
	CPU              float64      `json:"cpu"`
	Memory           *Memory      `json:"memory,omitempty"` // Full memory object with usage percentage
	Mem              int64        `json:"mem"`              // Maps to Memory.Used (kept for backward compat)
	MaxMem           int64        `json:"maxmem"`           // Maps to Memory.Total (kept for backward compat)
	Disk             *Disk        `json:"disk,omitempty"`   // Full disk object with usage percentage
	MaxDisk          int64        `json:"maxdisk"`          // Maps to Disk.Total (kept for backward compat)
	Uptime           int64        `json:"uptime"`
	LoadAverage      []float64    `json:"loadAverage"`
	KernelVersion    string       `json:"kernelVersion"`
	PVEVersion       string       `json:"pveVersion"`
	CPUInfo          CPUInfo      `json:"cpuInfo"`
	Temperature      *Temperature `json:"temperature,omitempty"` // CPU/NVMe temperatures
	LastSeen         int64        `json:"lastSeen"`              // Unix timestamp
	ConnectionHealth string       `json:"connectionHealth"`
	IsClusterMember  bool         `json:"isClusterMember,omitempty"`
	ClusterName      string       `json:"clusterName,omitempty"`
}

// VMFrontend represents a VM with frontend-friendly field names
type VMFrontend struct {
	ID                string                  `json:"id"`
	VMID              int                     `json:"vmid"`
	Name              string                  `json:"name"`
	Node              string                  `json:"node"`
	Instance          string                  `json:"instance"`
	Status            string                  `json:"status"`
	Type              string                  `json:"type"`
	CPU               float64                 `json:"cpu"`
	CPUs              int                     `json:"cpus"`
	Memory            *Memory                 `json:"memory,omitempty"`           // Full memory object
	Mem               int64                   `json:"mem"`                        // Maps to Memory.Used
	MaxMem            int64                   `json:"maxmem"`                     // Maps to Memory.Total
	DiskObj           *Disk                   `json:"disk,omitempty"`             // Full disk object
	Disks             []Disk                  `json:"disks,omitempty"`            // Individual filesystem/disk usage
	DiskStatusReason  string                  `json:"diskStatusReason,omitempty"` // Why disk stats are unavailable
	OSName            string                  `json:"osName,omitempty"`
	OSVersion         string                  `json:"osVersion,omitempty"`
	AgentVersion      string                  `json:"agentVersion,omitempty"`
	NetworkInterfaces []GuestNetworkInterface `json:"networkInterfaces,omitempty"`
	IPAddresses       []string                `json:"ipAddresses,omitempty"`
	NetIn             int64                   `json:"networkIn"`  // Maps to NetworkIn (camelCase for frontend)
	NetOut            int64                   `json:"networkOut"` // Maps to NetworkOut (camelCase for frontend)
	DiskRead          int64                   `json:"diskRead"`   // Maps to DiskRead (camelCase for frontend)
	DiskWrite         int64                   `json:"diskWrite"`  // Maps to DiskWrite (camelCase for frontend)
	Uptime            int64                   `json:"uptime"`
	Template          bool                    `json:"template"`
	LastBackup        int64                   `json:"lastBackup,omitempty"` // Unix timestamp
	Tags              string                  `json:"tags,omitempty"`       // Joined string
	Lock              string                  `json:"lock,omitempty"`
	LastSeen          int64                   `json:"lastSeen"` // Unix timestamp
}

// ContainerFrontend represents a Container with frontend-friendly field names
type ContainerFrontend struct {
	ID                string                  `json:"id"`
	VMID              int                     `json:"vmid"`
	Name              string                  `json:"name"`
	Node              string                  `json:"node"`
	Instance          string                  `json:"instance"`
	Status            string                  `json:"status"`
	Type              string                  `json:"type"`
	CPU               float64                 `json:"cpu"`
	CPUs              int                     `json:"cpus"`
	Memory            *Memory                 `json:"memory,omitempty"` // Full memory object
	Mem               int64                   `json:"mem"`              // Maps to Memory.Used
	MaxMem            int64                   `json:"maxmem"`           // Maps to Memory.Total
	DiskObj           *Disk                   `json:"disk,omitempty"`   // Full disk object
	Disks             []Disk                  `json:"disks,omitempty"`  // Individual filesystem/disk usage
	NetworkInterfaces []GuestNetworkInterface `json:"networkInterfaces,omitempty"`
	IPAddresses       []string                `json:"ipAddresses,omitempty"`
	NetIn             int64                   `json:"networkIn"`  // Maps to NetworkIn (camelCase for frontend)
	NetOut            int64                   `json:"networkOut"` // Maps to NetworkOut (camelCase for frontend)
	DiskRead          int64                   `json:"diskRead"`   // Maps to DiskRead (camelCase for frontend)
	DiskWrite         int64                   `json:"diskWrite"`  // Maps to DiskWrite (camelCase for frontend)
	Uptime            int64                   `json:"uptime"`
	Template          bool                    `json:"template"`
	LastBackup        int64                   `json:"lastBackup,omitempty"` // Unix timestamp
	Tags              string                  `json:"tags,omitempty"`       // Joined string
	Lock              string                  `json:"lock,omitempty"`
	LastSeen          int64                   `json:"lastSeen"` // Unix timestamp
}

// DockerHostFrontend represents a Docker host with frontend-friendly fields
type DockerHostFrontend struct {
	ID               string                     `json:"id"`
	AgentID          string                     `json:"agentId"`
	Hostname         string                     `json:"hostname"`
	DisplayName      string                     `json:"displayName"`
	MachineID        string                     `json:"machineId,omitempty"`
	OS               string                     `json:"os,omitempty"`
	KernelVersion    string                     `json:"kernelVersion,omitempty"`
	Architecture     string                     `json:"architecture,omitempty"`
	DockerVersion    string                     `json:"dockerVersion,omitempty"`
	CPUs             int                        `json:"cpus"`
	TotalMemoryBytes int64                      `json:"totalMemoryBytes"`
	UptimeSeconds    int64                      `json:"uptimeSeconds"`
	Status           string                     `json:"status"`
	LastSeen         int64                      `json:"lastSeen"`
	IntervalSeconds  int                        `json:"intervalSeconds"`
	AgentVersion     string                     `json:"agentVersion,omitempty"`
	Containers       []DockerContainerFrontend  `json:"containers"`
	TokenID          string                     `json:"tokenId,omitempty"`
	TokenName        string                     `json:"tokenName,omitempty"`
	TokenHint        string                     `json:"tokenHint,omitempty"`
	TokenLastUsedAt  *int64                     `json:"tokenLastUsedAt,omitempty"`
	PendingUninstall bool                       `json:"pendingUninstall"`
	Command          *DockerHostCommandFrontend `json:"command,omitempty"`
}

// DockerContainerFrontend represents a Docker container for the frontend
type DockerContainerFrontend struct {
	ID            string                           `json:"id"`
	Name          string                           `json:"name"`
	Image         string                           `json:"image"`
	State         string                           `json:"state"`
	Status        string                           `json:"status"`
	Health        string                           `json:"health,omitempty"`
	CPUPercent    float64                          `json:"cpuPercent"`
	MemoryUsage   int64                            `json:"memoryUsageBytes"`
	MemoryLimit   int64                            `json:"memoryLimitBytes"`
	MemoryPercent float64                          `json:"memoryPercent"`
	UptimeSeconds int64                            `json:"uptimeSeconds"`
	RestartCount  int                              `json:"restartCount"`
	ExitCode      int                              `json:"exitCode"`
	CreatedAt     int64                            `json:"createdAt"`
	StartedAt     *int64                           `json:"startedAt,omitempty"`
	FinishedAt    *int64                           `json:"finishedAt,omitempty"`
	Ports         []DockerContainerPortFrontend    `json:"ports,omitempty"`
	Labels        map[string]string                `json:"labels,omitempty"`
	Networks      []DockerContainerNetworkFrontend `json:"networks,omitempty"`
}

// DockerContainerPortFrontend represents a container port mapping
type DockerContainerPortFrontend struct {
	PrivatePort int    `json:"privatePort"`
	PublicPort  int    `json:"publicPort,omitempty"`
	Protocol    string `json:"protocol"`
	IP          string `json:"ip,omitempty"`
}

// DockerContainerNetworkFrontend represents container network addresses
type DockerContainerNetworkFrontend struct {
	Name string `json:"name"`
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// DockerHostCommandFrontend exposes docker host command state to the UI.
type DockerHostCommandFrontend struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	Message        string `json:"message,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
	DispatchedAt   *int64 `json:"dispatchedAt,omitempty"`
	AcknowledgedAt *int64 `json:"acknowledgedAt,omitempty"`
	CompletedAt    *int64 `json:"completedAt,omitempty"`
	FailedAt       *int64 `json:"failedAt,omitempty"`
	FailureReason  string `json:"failureReason,omitempty"`
	ExpiresAt      *int64 `json:"expiresAt,omitempty"`
}

// HostFrontend represents a generic infrastructure host exposed to the UI.
type HostFrontend struct {
	ID                string                     `json:"id"`
	Hostname          string                     `json:"hostname"`
	DisplayName       string                     `json:"displayName"`
	Platform          string                     `json:"platform,omitempty"`
	OSName            string                     `json:"osName,omitempty"`
	OSVersion         string                     `json:"osVersion,omitempty"`
	KernelVersion     string                     `json:"kernelVersion,omitempty"`
	Architecture      string                     `json:"architecture,omitempty"`
	CPUCount          int                        `json:"cpuCount,omitempty"`
	CPUUsage          float64                    `json:"cpuUsage,omitempty"`
	LoadAverage       []float64                  `json:"loadAverage,omitempty"`
	Memory            *Memory                    `json:"memory,omitempty"`
	Disks             []Disk                     `json:"disks,omitempty"`
	NetworkInterfaces []HostNetworkInterface     `json:"networkInterfaces,omitempty"`
	Sensors           *HostSensorSummaryFrontend `json:"sensors,omitempty"`
	Status            string                     `json:"status"`
	UptimeSeconds     int64                      `json:"uptimeSeconds,omitempty"`
	LastSeen          int64                      `json:"lastSeen"`
	IntervalSeconds   int                        `json:"intervalSeconds,omitempty"`
	AgentVersion      string                     `json:"agentVersion,omitempty"`
	TokenID           string                     `json:"tokenId,omitempty"`
	TokenName         string                     `json:"tokenName,omitempty"`
	TokenHint         string                     `json:"tokenHint,omitempty"`
	TokenLastUsedAt   *int64                     `json:"tokenLastUsedAt,omitempty"`
	Tags              []string                   `json:"tags,omitempty"`
}

// HostSensorSummaryFrontend mirrors HostSensorSummary with primitives for the frontend.
type HostSensorSummaryFrontend struct {
	TemperatureCelsius map[string]float64 `json:"temperatureCelsius,omitempty"`
	FanRPM             map[string]float64 `json:"fanRpm,omitempty"`
	Additional         map[string]float64 `json:"additional,omitempty"`
}

// StorageFrontend represents Storage with frontend-friendly field names
type StorageFrontend struct {
	ID        string   `json:"id"`
	Storage   string   `json:"storage"` // Maps to Name
	Name      string   `json:"name"`
	Node      string   `json:"node"`
	Instance  string   `json:"instance"`
	Nodes     []string `json:"nodes,omitempty"`
	NodeIDs   []string `json:"nodeIds,omitempty"`
	NodeCount int      `json:"nodeCount,omitempty"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	Total     int64    `json:"total"`
	Used      int64    `json:"used"`
	Avail     int64    `json:"avail"` // Maps to Free
	Free      int64    `json:"free"`
	Usage     float64  `json:"usage"`
	Content   string   `json:"content"`
	Shared    bool     `json:"shared"`
	Enabled   bool     `json:"enabled"`
	Active    bool     `json:"active"`
}

// CephClusterFrontend represents a Ceph cluster with frontend-friendly field names
type CephClusterFrontend struct {
	ID             string              `json:"id"`
	Instance       string              `json:"instance"`
	Name           string              `json:"name"`
	FSID           string              `json:"fsid,omitempty"`
	Health         string              `json:"health"`
	HealthMessage  string              `json:"healthMessage,omitempty"`
	TotalBytes     int64               `json:"totalBytes"`
	UsedBytes      int64               `json:"usedBytes"`
	AvailableBytes int64               `json:"availableBytes"`
	UsagePercent   float64             `json:"usagePercent"`
	NumMons        int                 `json:"numMons"`
	NumMgrs        int                 `json:"numMgrs"`
	NumOSDs        int                 `json:"numOsds"`
	NumOSDsUp      int                 `json:"numOsdsUp"`
	NumOSDsIn      int                 `json:"numOsdsIn"`
	NumPGs         int                 `json:"numPGs"`
	Pools          []CephPool          `json:"pools,omitempty"`
	Services       []CephServiceStatus `json:"services,omitempty"`
	LastUpdated    int64               `json:"lastUpdated"`
}

// ReplicationJobFrontend represents a replication job for the frontend.
type ReplicationJobFrontend struct {
	ID                      string   `json:"id"`
	Instance                string   `json:"instance"`
	JobID                   string   `json:"jobId"`
	JobNumber               int      `json:"jobNumber,omitempty"`
	Guest                   string   `json:"guest,omitempty"`
	GuestID                 int      `json:"guestId,omitempty"`
	GuestName               string   `json:"guestName,omitempty"`
	GuestType               string   `json:"guestType,omitempty"`
	GuestNode               string   `json:"guestNode,omitempty"`
	SourceNode              string   `json:"sourceNode,omitempty"`
	SourceStorage           string   `json:"sourceStorage,omitempty"`
	TargetNode              string   `json:"targetNode,omitempty"`
	TargetStorage           string   `json:"targetStorage,omitempty"`
	Schedule                string   `json:"schedule,omitempty"`
	Type                    string   `json:"type,omitempty"`
	Enabled                 bool     `json:"enabled"`
	State                   string   `json:"state,omitempty"`
	Status                  string   `json:"status,omitempty"`
	LastSyncStatus          string   `json:"lastSyncStatus,omitempty"`
	LastSyncTime            int64    `json:"lastSyncTime,omitempty"`
	LastSyncUnix            int64    `json:"lastSyncUnix,omitempty"`
	LastSyncDurationSeconds int      `json:"lastSyncDurationSeconds,omitempty"`
	LastSyncDurationHuman   string   `json:"lastSyncDurationHuman,omitempty"`
	NextSyncTime            int64    `json:"nextSyncTime,omitempty"`
	NextSyncUnix            int64    `json:"nextSyncUnix,omitempty"`
	DurationSeconds         int      `json:"durationSeconds,omitempty"`
	DurationHuman           string   `json:"durationHuman,omitempty"`
	FailCount               int      `json:"failCount,omitempty"`
	Error                   string   `json:"error,omitempty"`
	Comment                 string   `json:"comment,omitempty"`
	RemoveJob               string   `json:"removeJob,omitempty"`
	RateLimitMbps           *float64 `json:"rateLimitMbps,omitempty"`
	PolledAt                int64    `json:"polledAt,omitempty"`
}

// StateFrontend represents the state with frontend-friendly field names
type StateFrontend struct {
	Nodes            []NodeFrontend           `json:"nodes"`
	VMs              []VMFrontend             `json:"vms"`
	Containers       []ContainerFrontend      `json:"containers"`
	DockerHosts      []DockerHostFrontend     `json:"dockerHosts"`
	Hosts            []HostFrontend           `json:"hosts"`
	Storage          []StorageFrontend        `json:"storage"`
	CephClusters     []CephClusterFrontend    `json:"cephClusters"`
	PhysicalDisks    []PhysicalDisk           `json:"physicalDisks"`
	PBS              []PBSInstance            `json:"pbs"` // Keep as is
	PMG              []PMGInstance            `json:"pmg"`
	PBSBackups       []PBSBackup              `json:"pbsBackups"`
	PMGBackups       []PMGBackup              `json:"pmgBackups"`
	Backups          Backups                  `json:"backups"`
	ReplicationJobs  []ReplicationJobFrontend `json:"replicationJobs"`
	ActiveAlerts     []Alert                  `json:"activeAlerts"`     // Active alerts
	Metrics          map[string]any           `json:"metrics"`          // Empty object for now
	PVEBackups       PVEBackups               `json:"pveBackups"`       // Keep as is
	Performance      map[string]any           `json:"performance"`      // Empty object for now
	ConnectionHealth map[string]bool          `json:"connectionHealth"` // Keep as is
	Stats            map[string]any           `json:"stats"`            // Empty object for now
	LastUpdate       int64                    `json:"lastUpdate"`       // Unix timestamp
}
