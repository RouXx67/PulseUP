package models

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// State represents the current state of all monitored resources
type State struct {
	mu               sync.RWMutex
	Nodes            []Node           `json:"nodes"`
	VMs              []VM             `json:"vms"`
	Containers       []Container      `json:"containers"`
	DockerHosts      []DockerHost     `json:"dockerHosts"`
	Hosts            []Host           `json:"hosts"`
	Storage          []Storage        `json:"storage"`
	CephClusters     []CephCluster    `json:"cephClusters"`
	PhysicalDisks    []PhysicalDisk   `json:"physicalDisks"`
	PBSInstances     []PBSInstance    `json:"pbs"`
	PMGInstances     []PMGInstance    `json:"pmg"`
	PBSBackups       []PBSBackup      `json:"pbsBackups"`
	PMGBackups       []PMGBackup      `json:"pmgBackups"`
	Backups          Backups          `json:"backups"`
	ReplicationJobs  []ReplicationJob `json:"replicationJobs"`
	Metrics          []Metric         `json:"metrics"`
	PVEBackups       PVEBackups       `json:"pveBackups"`
	Performance      Performance      `json:"performance"`
	ConnectionHealth map[string]bool  `json:"connectionHealth"`
	Stats            Stats            `json:"stats"`
	ActiveAlerts     []Alert          `json:"activeAlerts"`
	RecentlyResolved []ResolvedAlert  `json:"recentlyResolved"`
	LastUpdate       time.Time        `json:"lastUpdate"`
}

// Alert represents an active alert (simplified for State)
type Alert struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Level        string     `json:"level"`
	ResourceID   string     `json:"resourceId"`
	ResourceName string     `json:"resourceName"`
	Node         string     `json:"node"`
	Instance     string     `json:"instance"`
	Message      string     `json:"message"`
	Value        float64    `json:"value"`
	Threshold    float64    `json:"threshold"`
	StartTime    time.Time  `json:"startTime"`
	Acknowledged bool       `json:"acknowledged"`
	AckTime      *time.Time `json:"ackTime,omitempty"`
	AckUser      string     `json:"ackUser,omitempty"`
}

// ResolvedAlert represents a recently resolved alert
type ResolvedAlert struct {
	Alert
	ResolvedTime time.Time `json:"resolvedTime"`
}

// Node represents a Proxmox VE node
type Node struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	DisplayName      string       `json:"displayName,omitempty"`
	Instance         string       `json:"instance"`
	Host             string       `json:"host"` // Full host URL from config
	Status           string       `json:"status"`
	Type             string       `json:"type"`
	CPU              float64      `json:"cpu"`
	Memory           Memory       `json:"memory"`
	Disk             Disk         `json:"disk"`
	Uptime           int64        `json:"uptime"`
	LoadAverage      []float64    `json:"loadAverage"`
	KernelVersion    string       `json:"kernelVersion"`
	PVEVersion       string       `json:"pveVersion"`
	CPUInfo          CPUInfo      `json:"cpuInfo"`
	Temperature      *Temperature `json:"temperature,omitempty"` // CPU/NVMe temperatures
	LastSeen         time.Time    `json:"lastSeen"`
	ConnectionHealth string       `json:"connectionHealth"`
	IsClusterMember  bool         `json:"isClusterMember"` // True if part of a cluster
	ClusterName      string       `json:"clusterName"`     // Name of cluster (empty if standalone)
}

// VM represents a virtual machine
type VM struct {
	ID                string                  `json:"id"`
	VMID              int                     `json:"vmid"`
	Name              string                  `json:"name"`
	Node              string                  `json:"node"`
	Instance          string                  `json:"instance"`
	Status            string                  `json:"status"`
	Type              string                  `json:"type"`
	CPU               float64                 `json:"cpu"`
	CPUs              int                     `json:"cpus"`
	Memory            Memory                  `json:"memory"`
	Disk              Disk                    `json:"disk"`
	Disks             []Disk                  `json:"disks,omitempty"`
	DiskStatusReason  string                  `json:"diskStatusReason,omitempty"` // Why disk stats are unavailable
	IPAddresses       []string                `json:"ipAddresses,omitempty"`
	OSName            string                  `json:"osName,omitempty"`
	OSVersion         string                  `json:"osVersion,omitempty"`
	AgentVersion      string                  `json:"agentVersion,omitempty"`
	NetworkInterfaces []GuestNetworkInterface `json:"networkInterfaces,omitempty"`
	NetworkIn         int64                   `json:"networkIn"`
	NetworkOut        int64                   `json:"networkOut"`
	DiskRead          int64                   `json:"diskRead"`
	DiskWrite         int64                   `json:"diskWrite"`
	Uptime            int64                   `json:"uptime"`
	Template          bool                    `json:"template"`
	LastBackup        time.Time               `json:"lastBackup,omitempty"`
	Tags              []string                `json:"tags,omitempty"`
	Lock              string                  `json:"lock,omitempty"`
	LastSeen          time.Time               `json:"lastSeen"`
}

// Container represents an LXC container
type Container struct {
	ID                string                  `json:"id"`
	VMID              int                     `json:"vmid"`
	Name              string                  `json:"name"`
	Node              string                  `json:"node"`
	Instance          string                  `json:"instance"`
	Status            string                  `json:"status"`
	Type              string                  `json:"type"`
	CPU               float64                 `json:"cpu"`
	CPUs              int                     `json:"cpus"`
	Memory            Memory                  `json:"memory"`
	Disk              Disk                    `json:"disk"`
	Disks             []Disk                  `json:"disks,omitempty"`
	NetworkIn         int64                   `json:"networkIn"`
	NetworkOut        int64                   `json:"networkOut"`
	DiskRead          int64                   `json:"diskRead"`
	DiskWrite         int64                   `json:"diskWrite"`
	Uptime            int64                   `json:"uptime"`
	Template          bool                    `json:"template"`
	LastBackup        time.Time               `json:"lastBackup,omitempty"`
	Tags              []string                `json:"tags,omitempty"`
	Lock              string                  `json:"lock,omitempty"`
	LastSeen          time.Time               `json:"lastSeen"`
	IPAddresses       []string                `json:"ipAddresses,omitempty"`
	NetworkInterfaces []GuestNetworkInterface `json:"networkInterfaces,omitempty"`
}

// Host represents a generic infrastructure host reporting via external agents.
type Host struct {
	ID                string                 `json:"id"`
	Hostname          string                 `json:"hostname"`
	DisplayName       string                 `json:"displayName,omitempty"`
	Platform          string                 `json:"platform,omitempty"`
	OSName            string                 `json:"osName,omitempty"`
	OSVersion         string                 `json:"osVersion,omitempty"`
	KernelVersion     string                 `json:"kernelVersion,omitempty"`
	Architecture      string                 `json:"architecture,omitempty"`
	CPUCount          int                    `json:"cpuCount,omitempty"`
	CPUUsage          float64                `json:"cpuUsage,omitempty"`
	Memory            Memory                 `json:"memory"`
	LoadAverage       []float64              `json:"loadAverage,omitempty"`
	Disks             []Disk                 `json:"disks,omitempty"`
	NetworkInterfaces []HostNetworkInterface `json:"networkInterfaces,omitempty"`
	Sensors           HostSensorSummary      `json:"sensors,omitempty"`
	Status            string                 `json:"status"`
	UptimeSeconds     int64                  `json:"uptimeSeconds,omitempty"`
	IntervalSeconds   int                    `json:"intervalSeconds,omitempty"`
	LastSeen          time.Time              `json:"lastSeen"`
	AgentVersion      string                 `json:"agentVersion,omitempty"`
	TokenID           string                 `json:"tokenId,omitempty"`
	TokenName         string                 `json:"tokenName,omitempty"`
	TokenHint         string                 `json:"tokenHint,omitempty"`
	TokenLastUsedAt   *time.Time             `json:"tokenLastUsedAt,omitempty"`
	Tags              []string               `json:"tags,omitempty"`
}

// HostNetworkInterface describes a host network adapter summary.
type HostNetworkInterface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	RXBytes   uint64   `json:"rxBytes,omitempty"`
	TXBytes   uint64   `json:"txBytes,omitempty"`
	SpeedMbps *int64   `json:"speedMbps,omitempty"`
}

// HostSensorSummary captures optional per-host sensor readings.
type HostSensorSummary struct {
	TemperatureCelsius map[string]float64 `json:"temperatureCelsius,omitempty"`
	FanRPM             map[string]float64 `json:"fanRpm,omitempty"`
	Additional         map[string]float64 `json:"additional,omitempty"`
}

// DockerHost represents a Docker host reporting metrics via the external agent.
type DockerHost struct {
	ID               string                   `json:"id"`
	AgentID          string                   `json:"agentId"`
	Hostname         string                   `json:"hostname"`
	DisplayName      string                   `json:"displayName"`
	MachineID        string                   `json:"machineId,omitempty"`
	OS               string                   `json:"os,omitempty"`
	KernelVersion    string                   `json:"kernelVersion,omitempty"`
	Architecture     string                   `json:"architecture,omitempty"`
	DockerVersion    string                   `json:"dockerVersion,omitempty"`
	CPUs             int                      `json:"cpus"`
	TotalMemoryBytes int64                    `json:"totalMemoryBytes"`
	UptimeSeconds    int64                    `json:"uptimeSeconds"`
	Status           string                   `json:"status"`
	LastSeen         time.Time                `json:"lastSeen"`
	IntervalSeconds  int                      `json:"intervalSeconds"`
	AgentVersion     string                   `json:"agentVersion,omitempty"`
	Containers       []DockerContainer        `json:"containers"`
	TokenID          string                   `json:"tokenId,omitempty"`
	TokenName        string                   `json:"tokenName,omitempty"`
	TokenHint        string                   `json:"tokenHint,omitempty"`
	TokenLastUsedAt  *time.Time               `json:"tokenLastUsedAt,omitempty"`
	Hidden           bool                     `json:"hidden"`
	PendingUninstall bool                     `json:"pendingUninstall"`
	Command          *DockerHostCommandStatus `json:"command,omitempty"`
}

// DockerContainer represents the state of a Docker container on a monitored host.
type DockerContainer struct {
	ID            string                       `json:"id"`
	Name          string                       `json:"name"`
	Image         string                       `json:"image"`
	State         string                       `json:"state"`
	Status        string                       `json:"status"`
	Health        string                       `json:"health,omitempty"`
	CPUPercent    float64                      `json:"cpuPercent"`
	MemoryUsage   int64                        `json:"memoryUsageBytes"`
	MemoryLimit   int64                        `json:"memoryLimitBytes"`
	MemoryPercent float64                      `json:"memoryPercent"`
	UptimeSeconds int64                        `json:"uptimeSeconds"`
	RestartCount  int                          `json:"restartCount"`
	ExitCode      int                          `json:"exitCode"`
	CreatedAt     time.Time                    `json:"createdAt"`
	StartedAt     *time.Time                   `json:"startedAt,omitempty"`
	FinishedAt    *time.Time                   `json:"finishedAt,omitempty"`
	Ports         []DockerContainerPort        `json:"ports,omitempty"`
	Labels        map[string]string            `json:"labels,omitempty"`
	Networks      []DockerContainerNetworkLink `json:"networks,omitempty"`
}

// DockerContainerPort describes an exposed container port mapping.
type DockerContainerPort struct {
	PrivatePort int    `json:"privatePort"`
	PublicPort  int    `json:"publicPort,omitempty"`
	Protocol    string `json:"protocol"`
	IP          string `json:"ip,omitempty"`
}

// DockerContainerNetworkLink summarises container network addresses per network.
type DockerContainerNetworkLink struct {
	Name string `json:"name"`
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// DockerHostCommandStatus tracks the lifecycle of a control command issued to a Docker host.
type DockerHostCommandStatus struct {
	ID             string     `json:"id"`
	Type           string     `json:"type"`
	Status         string     `json:"status"`
	Message        string     `json:"message,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	DispatchedAt   *time.Time `json:"dispatchedAt,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	FailedAt       *time.Time `json:"failedAt,omitempty"`
	FailureReason  string     `json:"failureReason,omitempty"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
}

// Storage represents a storage resource
type Storage struct {
	ID        string   `json:"id"`
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
	Free      int64    `json:"free"`
	Usage     float64  `json:"usage"`
	Content   string   `json:"content"`
	Shared    bool     `json:"shared"`
	Enabled   bool     `json:"enabled"`
	Active    bool     `json:"active"`
	ZFSPool   *ZFSPool `json:"zfsPool,omitempty"` // ZFS pool details if this is ZFS storage
}

// ZFSPool represents a ZFS pool with health and error information
type ZFSPool struct {
	Name           string      `json:"name"`
	State          string      `json:"state"`  // ONLINE, DEGRADED, FAULTED, OFFLINE, REMOVED, UNAVAIL
	Status         string      `json:"status"` // Healthy, Degraded, Faulted, etc.
	Scan           string      `json:"scan"`   // Current scan status (scrub, resilver, none)
	ReadErrors     int64       `json:"readErrors"`
	WriteErrors    int64       `json:"writeErrors"`
	ChecksumErrors int64       `json:"checksumErrors"`
	Devices        []ZFSDevice `json:"devices"`
}

// ZFSDevice represents a device in a ZFS pool
type ZFSDevice struct {
	Name           string `json:"name"`
	Type           string `json:"type"`  // disk, mirror, raidz, raidz2, raidz3, spare, log, cache
	State          string `json:"state"` // ONLINE, DEGRADED, FAULTED, OFFLINE, REMOVED, UNAVAIL
	ReadErrors     int64  `json:"readErrors"`
	WriteErrors    int64  `json:"writeErrors"`
	ChecksumErrors int64  `json:"checksumErrors"`
	Message        string `json:"message,omitempty"` // Additional message provided by Proxmox (if any)
}

// CephCluster represents the health and capacity information for a Ceph cluster
type CephCluster struct {
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
	LastUpdated    time.Time           `json:"lastUpdated"`
}

// CephPool represents usage statistics for a Ceph pool
type CephPool struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	StoredBytes    int64   `json:"storedBytes"`
	AvailableBytes int64   `json:"availableBytes"`
	Objects        int64   `json:"objects"`
	PercentUsed    float64 `json:"percentUsed"`
}

// CephServiceStatus summarises daemon health for a Ceph service type (e.g. mon, mgr)
type CephServiceStatus struct {
	Type    string `json:"type"`
	Running int    `json:"running"`
	Total   int    `json:"total"`
	Message string `json:"message,omitempty"`
}

// PhysicalDisk represents a physical disk on a node
type PhysicalDisk struct {
	ID          string    `json:"id"` // "{instance}-{node}-{devpath}"
	Node        string    `json:"node"`
	Instance    string    `json:"instance"`
	DevPath     string    `json:"devPath"` // /dev/nvme0n1, /dev/sda
	Model       string    `json:"model"`
	Serial      string    `json:"serial"`
	Type        string    `json:"type"`        // nvme, sata, sas
	Size        int64     `json:"size"`        // bytes
	Health      string    `json:"health"`      // PASSED, FAILED, UNKNOWN
	Wearout     int       `json:"wearout"`     // SSD wear metric from Proxmox (0-100, -1 when unavailable)
	Temperature int       `json:"temperature"` // Celsius (if available)
	RPM         int       `json:"rpm"`         // 0 for SSDs
	Used        string    `json:"used"`        // Filesystem or partition usage
	LastChecked time.Time `json:"lastChecked"`
}

// PBSInstance represents a Proxmox Backup Server instance
type PBSInstance struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Host             string          `json:"host"`
	Status           string          `json:"status"`
	Version          string          `json:"version"`
	CPU              float64         `json:"cpu"`         // CPU usage percentage
	Memory           float64         `json:"memory"`      // Memory usage percentage
	MemoryUsed       int64           `json:"memoryUsed"`  // Memory used in bytes
	MemoryTotal      int64           `json:"memoryTotal"` // Total memory in bytes
	Uptime           int64           `json:"uptime"`      // Uptime in seconds
	Datastores       []PBSDatastore  `json:"datastores"`
	BackupJobs       []PBSBackupJob  `json:"backupJobs"`
	SyncJobs         []PBSSyncJob    `json:"syncJobs"`
	VerifyJobs       []PBSVerifyJob  `json:"verifyJobs"`
	PruneJobs        []PBSPruneJob   `json:"pruneJobs"`
	GarbageJobs      []PBSGarbageJob `json:"garbageJobs"`
	ConnectionHealth string          `json:"connectionHealth"`
	LastSeen         time.Time       `json:"lastSeen"`
}

// PBSDatastore represents a PBS datastore
type PBSDatastore struct {
	Name                string         `json:"name"`
	Total               int64          `json:"total"`
	Used                int64          `json:"used"`
	Free                int64          `json:"free"`
	Usage               float64        `json:"usage"`
	Status              string         `json:"status"`
	Error               string         `json:"error,omitempty"`
	Namespaces          []PBSNamespace `json:"namespaces,omitempty"`
	DeduplicationFactor float64        `json:"deduplicationFactor,omitempty"`
}

// PBSNamespace represents a PBS namespace
type PBSNamespace struct {
	Path   string `json:"path"`
	Parent string `json:"parent,omitempty"`
	Depth  int    `json:"depth"`
}

// PBSBackup represents a backup stored on PBS
type PBSBackup struct {
	ID         string    `json:"id"`       // Unique ID combining PBS instance, namespace, type, vmid, and time
	Instance   string    `json:"instance"` // PBS instance name
	Datastore  string    `json:"datastore"`
	Namespace  string    `json:"namespace"`
	BackupType string    `json:"backupType"` // "vm" or "ct"
	VMID       string    `json:"vmid"`
	BackupTime time.Time `json:"backupTime"`
	Size       int64     `json:"size"`
	Protected  bool      `json:"protected"`
	Verified   bool      `json:"verified"`
	Comment    string    `json:"comment,omitempty"`
	Files      []string  `json:"files,omitempty"`
	Owner      string    `json:"owner,omitempty"` // User who created the backup
}

// PBSBackupJob represents a PBS backup job
type PBSBackupJob struct {
	ID         string    `json:"id"`
	Store      string    `json:"store"`
	Type       string    `json:"type"`
	VMID       string    `json:"vmid,omitempty"`
	LastBackup time.Time `json:"lastBackup"`
	NextRun    time.Time `json:"nextRun,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
}

// PBSSyncJob represents a PBS sync job
type PBSSyncJob struct {
	ID       string    `json:"id"`
	Store    string    `json:"store"`
	Remote   string    `json:"remote"`
	Status   string    `json:"status"`
	LastSync time.Time `json:"lastSync"`
	NextRun  time.Time `json:"nextRun,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// PBSVerifyJob represents a PBS verification job
type PBSVerifyJob struct {
	ID         string    `json:"id"`
	Store      string    `json:"store"`
	Status     string    `json:"status"`
	LastVerify time.Time `json:"lastVerify"`
	NextRun    time.Time `json:"nextRun,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// PBSPruneJob represents a PBS prune job
type PBSPruneJob struct {
	ID        string    `json:"id"`
	Store     string    `json:"store"`
	Status    string    `json:"status"`
	LastPrune time.Time `json:"lastPrune"`
	NextRun   time.Time `json:"nextRun,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// PBSGarbageJob represents a PBS garbage collection job
type PBSGarbageJob struct {
	ID           string    `json:"id"`
	Store        string    `json:"store"`
	Status       string    `json:"status"`
	LastGarbage  time.Time `json:"lastGarbage"`
	NextRun      time.Time `json:"nextRun,omitempty"`
	RemovedBytes int64     `json:"removedBytes,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// PMGInstance represents a Proxmox Mail Gateway connection
type PMGInstance struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	Host             string               `json:"host"`
	Status           string               `json:"status"`
	Version          string               `json:"version"`
	Nodes            []PMGNodeStatus      `json:"nodes,omitempty"`
	MailStats        *PMGMailStats        `json:"mailStats,omitempty"`
	MailCount        []PMGMailCountPoint  `json:"mailCount,omitempty"`
	SpamDistribution []PMGSpamBucket      `json:"spamDistribution,omitempty"`
	Quarantine       *PMGQuarantineTotals `json:"quarantine,omitempty"`
	ConnectionHealth string               `json:"connectionHealth"`
	LastSeen         time.Time            `json:"lastSeen"`
	LastUpdated      time.Time            `json:"lastUpdated"`
}

// PMGNodeStatus represents the status of a PMG cluster node
type PMGNodeStatus struct {
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	Role        string          `json:"role,omitempty"`
	Uptime      int64           `json:"uptime,omitempty"`
	LoadAvg     string          `json:"loadAvg,omitempty"`
	QueueStatus *PMGQueueStatus `json:"queueStatus,omitempty"` // Postfix queue status for this node
}

// PMGBackup represents a configuration backup generated by a PMG node.
type PMGBackup struct {
	ID         string    `json:"id"`
	Instance   string    `json:"instance"`
	Node       string    `json:"node"`
	Filename   string    `json:"filename"`
	BackupTime time.Time `json:"backupTime"`
	Size       int64     `json:"size"`
}

// Backups aggregates backup collections by source type.
type Backups struct {
	PVE PVEBackups  `json:"pve"`
	PBS []PBSBackup `json:"pbs"`
	PMG []PMGBackup `json:"pmg"`
}

// PMGMailStats summarizes aggregated mail statistics for a timeframe
type PMGMailStats struct {
	Timeframe            string    `json:"timeframe"`
	CountTotal           float64   `json:"countTotal"`
	CountIn              float64   `json:"countIn"`
	CountOut             float64   `json:"countOut"`
	SpamIn               float64   `json:"spamIn"`
	SpamOut              float64   `json:"spamOut"`
	VirusIn              float64   `json:"virusIn"`
	VirusOut             float64   `json:"virusOut"`
	BouncesIn            float64   `json:"bouncesIn"`
	BouncesOut           float64   `json:"bouncesOut"`
	BytesIn              float64   `json:"bytesIn"`
	BytesOut             float64   `json:"bytesOut"`
	GreylistCount        float64   `json:"greylistCount"`
	JunkIn               float64   `json:"junkIn"`
	AverageProcessTimeMs float64   `json:"averageProcessTimeMs"`
	RBLRejects           float64   `json:"rblRejects"`
	PregreetRejects      float64   `json:"pregreetRejects"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

// PMGMailCountPoint represents a point-in-time mail counter snapshot
type PMGMailCountPoint struct {
	Timestamp   time.Time `json:"timestamp"`
	Count       float64   `json:"count"`
	CountIn     float64   `json:"countIn"`
	CountOut    float64   `json:"countOut"`
	SpamIn      float64   `json:"spamIn"`
	SpamOut     float64   `json:"spamOut"`
	VirusIn     float64   `json:"virusIn"`
	VirusOut    float64   `json:"virusOut"`
	RBLRejects  float64   `json:"rblRejects"`
	Pregreet    float64   `json:"pregreet"`
	BouncesIn   float64   `json:"bouncesIn"`
	BouncesOut  float64   `json:"bouncesOut"`
	Greylist    float64   `json:"greylist"`
	Index       int       `json:"index"`
	Timeframe   string    `json:"timeframe"`
	WindowStart time.Time `json:"windowStart,omitempty"`
	WindowEnd   time.Time `json:"windowEnd,omitempty"`
}

// PMGSpamBucket represents spam distribution counts by score
type PMGSpamBucket struct {
	Score string  `json:"score"`
	Count float64 `json:"count"`
}

// PMGQuarantineTotals summarizes quarantine counts per category
type PMGQuarantineTotals struct {
	Spam        int `json:"spam"`
	Virus       int `json:"virus"`
	Attachment  int `json:"attachment"`
	Blacklisted int `json:"blacklisted"`
}

// PMGQueueStatus represents the Postfix mail queue status for a PMG instance
type PMGQueueStatus struct {
	Active    int       `json:"active"`    // Messages currently being delivered
	Deferred  int       `json:"deferred"`  // Messages waiting for retry
	Hold      int       `json:"hold"`      // Messages on hold
	Incoming  int       `json:"incoming"`  // Messages in incoming queue
	Total     int       `json:"total"`     // Total messages in all queues
	OldestAge int64     `json:"oldestAge"` // Age of oldest message in seconds (0 if queue empty)
	UpdatedAt time.Time `json:"updatedAt"` // When this queue data was collected
}

// Memory represents memory usage
type Memory struct {
	Total     int64   `json:"total"`
	Used      int64   `json:"used"`
	Free      int64   `json:"free"`
	Usage     float64 `json:"usage"`
	Balloon   int64   `json:"balloon,omitempty"`
	SwapUsed  int64   `json:"swapUsed,omitempty"`
	SwapTotal int64   `json:"swapTotal,omitempty"`
}

type GuestNetworkInterface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	RXBytes   int64    `json:"rxBytes,omitempty"`
	TXBytes   int64    `json:"txBytes,omitempty"`
}

// Disk represents disk usage
type Disk struct {
	Total      int64   `json:"total"`
	Used       int64   `json:"used"`
	Free       int64   `json:"free"`
	Usage      float64 `json:"usage"`
	Mountpoint string  `json:"mountpoint,omitempty"`
	Type       string  `json:"type,omitempty"`
	Device     string  `json:"device,omitempty"`
}

// CPUInfo represents CPU information
type CPUInfo struct {
	Model   string `json:"model"`
	Cores   int    `json:"cores"`
	Sockets int    `json:"sockets"`
	MHz     string `json:"mhz"`
}

// Temperature represents temperature sensors data
type Temperature struct {
	CPUPackage   float64    `json:"cpuPackage,omitempty"`   // CPU package temperature (primary metric)
	CPUMax       float64    `json:"cpuMax,omitempty"`       // Highest core temperature
	CPUMin       float64    `json:"cpuMin,omitempty"`       // Minimum recorded CPU temperature (since monitoring started)
	CPUMaxRecord float64    `json:"cpuMaxRecord,omitempty"` // Maximum recorded CPU temperature (since monitoring started)
	MinRecorded  time.Time  `json:"minRecorded,omitempty"`  // When minimum temperature was recorded
	MaxRecorded  time.Time  `json:"maxRecorded,omitempty"`  // When maximum temperature was recorded
	Cores        []CoreTemp `json:"cores,omitempty"`        // Individual core temperatures
	NVMe         []NVMeTemp `json:"nvme,omitempty"`         // NVMe drive temperatures
	Available    bool       `json:"available"`              // Whether any temperature data is available
	HasCPU       bool       `json:"hasCPU"`                 // Whether CPU temperature data is available
	HasNVMe      bool       `json:"hasNVMe"`                // Whether NVMe temperature data is available
	LastUpdate   time.Time  `json:"lastUpdate"`             // When this data was collected
}

// CoreTemp represents a CPU core temperature
type CoreTemp struct {
	Core int     `json:"core"`
	Temp float64 `json:"temp"`
}

// NVMeTemp represents an NVMe drive temperature
type NVMeTemp struct {
	Device string  `json:"device"`
	Temp   float64 `json:"temp"`
}

// Metric represents a time-series metric
type Metric struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	ID        string                 `json:"id"`
	Values    map[string]interface{} `json:"values"`
}

// PVEBackups represents PVE backup information
type PVEBackups struct {
	BackupTasks    []BackupTask    `json:"backupTasks"`
	StorageBackups []StorageBackup `json:"storageBackups"`
	GuestSnapshots []GuestSnapshot `json:"guestSnapshots"`
}

// BackupTask represents a PVE backup task
type BackupTask struct {
	ID        string    `json:"id"`
	Node      string    `json:"node"`
	Type      string    `json:"type"`
	VMID      int       `json:"vmid"`
	Status    string    `json:"status"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime,omitempty"`
	Size      int64     `json:"size,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// StorageBackup represents a backup file in storage
type StorageBackup struct {
	ID           string    `json:"id"`
	Storage      string    `json:"storage"`
	Node         string    `json:"node"`
	Instance     string    `json:"instance"` // Unique instance identifier (for nodes with duplicate names)
	Type         string    `json:"type"`
	VMID         int       `json:"vmid"`
	Time         time.Time `json:"time"`
	CTime        int64     `json:"ctime"` // Unix timestamp for compatibility
	Size         int64     `json:"size"`
	Format       string    `json:"format"`
	Notes        string    `json:"notes,omitempty"`
	Protected    bool      `json:"protected"`
	Volid        string    `json:"volid"`                  // Volume ID for compatibility
	IsPBS        bool      `json:"isPBS"`                  // Indicates if backup is on PBS storage
	Verified     bool      `json:"verified"`               // PBS verification status
	Verification string    `json:"verification,omitempty"` // Verification details
}

// GuestSnapshot represents a VM/CT snapshot
type GuestSnapshot struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Node        string    `json:"node"`
	Instance    string    `json:"instance"` // Unique instance identifier (for nodes with duplicate names)
	Type        string    `json:"type"`
	VMID        int       `json:"vmid"`
	Time        time.Time `json:"time"`
	Description string    `json:"description,omitempty"`
	Parent      string    `json:"parent,omitempty"`
	VMState     bool      `json:"vmstate"`
	SizeBytes   int64     `json:"sizeBytes,omitempty"`
}

// ReplicationJob represents the status of a Proxmox storage replication job.
type ReplicationJob struct {
	ID                      string     `json:"id"`
	Instance                string     `json:"instance"`
	JobID                   string     `json:"jobId"`
	JobNumber               int        `json:"jobNumber,omitempty"`
	Guest                   string     `json:"guest,omitempty"`
	GuestID                 int        `json:"guestId,omitempty"`
	GuestName               string     `json:"guestName,omitempty"`
	GuestType               string     `json:"guestType,omitempty"`
	GuestNode               string     `json:"guestNode,omitempty"`
	SourceNode              string     `json:"sourceNode,omitempty"`
	SourceStorage           string     `json:"sourceStorage,omitempty"`
	TargetNode              string     `json:"targetNode,omitempty"`
	TargetStorage           string     `json:"targetStorage,omitempty"`
	Schedule                string     `json:"schedule,omitempty"`
	Type                    string     `json:"type,omitempty"`
	Enabled                 bool       `json:"enabled"`
	State                   string     `json:"state,omitempty"`
	Status                  string     `json:"status,omitempty"`
	LastSyncStatus          string     `json:"lastSyncStatus,omitempty"`
	LastSyncTime            *time.Time `json:"lastSyncTime,omitempty"`
	LastSyncUnix            int64      `json:"lastSyncUnix,omitempty"`
	LastSyncDurationSeconds int        `json:"lastSyncDurationSeconds,omitempty"`
	LastSyncDurationHuman   string     `json:"lastSyncDurationHuman,omitempty"`
	NextSyncTime            *time.Time `json:"nextSyncTime,omitempty"`
	NextSyncUnix            int64      `json:"nextSyncUnix,omitempty"`
	DurationSeconds         int        `json:"durationSeconds,omitempty"`
	DurationHuman           string     `json:"durationHuman,omitempty"`
	FailCount               int        `json:"failCount,omitempty"`
	Error                   string     `json:"error,omitempty"`
	Comment                 string     `json:"comment,omitempty"`
	RemoveJob               string     `json:"removeJob,omitempty"`
	RateLimitMbps           *float64   `json:"rateLimitMbps,omitempty"`
	LastPolled              time.Time  `json:"lastPolled"`
}

// Performance represents performance metrics
type Performance struct {
	APICallDuration  map[string]float64 `json:"apiCallDuration"`
	LastPollDuration float64            `json:"lastPollDuration"`
	PollingStartTime time.Time          `json:"pollingStartTime"`
	TotalAPICalls    int                `json:"totalApiCalls"`
	FailedAPICalls   int                `json:"failedApiCalls"`
}

// Stats represents runtime statistics
type Stats struct {
	StartTime        time.Time `json:"startTime"`
	Uptime           int64     `json:"uptime"`
	PollingCycles    int       `json:"pollingCycles"`
	WebSocketClients int       `json:"webSocketClients"`
	Version          string    `json:"version"`
}

// NewState creates a new State instance
func NewState() *State {
	pveBackups := PVEBackups{
		BackupTasks:    make([]BackupTask, 0),
		StorageBackups: make([]StorageBackup, 0),
		GuestSnapshots: make([]GuestSnapshot, 0),
	}

	state := &State{
		Nodes:         make([]Node, 0),
		VMs:           make([]VM, 0),
		Containers:    make([]Container, 0),
		DockerHosts:   make([]DockerHost, 0),
		Storage:       make([]Storage, 0),
		PhysicalDisks: make([]PhysicalDisk, 0),
		PBSInstances:  make([]PBSInstance, 0),
		PMGInstances:  make([]PMGInstance, 0),
		PBSBackups:    make([]PBSBackup, 0),
		PMGBackups:    make([]PMGBackup, 0),
		Backups: Backups{
			PVE: pveBackups,
			PBS: make([]PBSBackup, 0),
			PMG: make([]PMGBackup, 0),
		},
		ReplicationJobs:  make([]ReplicationJob, 0),
		Metrics:          make([]Metric, 0),
		PVEBackups:       pveBackups,
		ConnectionHealth: make(map[string]bool),
		ActiveAlerts:     make([]Alert, 0),
		RecentlyResolved: make([]ResolvedAlert, 0),
		LastUpdate:       time.Now(),
	}

	state.syncBackupsLocked()
	return state
}

// syncBackupsLocked updates the aggregated backups structure.
func (s *State) syncBackupsLocked() {
	s.Backups = Backups{
		PVE: s.PVEBackups,
		PBS: append([]PBSBackup(nil), s.PBSBackups...),
		PMG: append([]PMGBackup(nil), s.PMGBackups...),
	}
}

// UpdateActiveAlerts updates the active alerts in the state
func (s *State) UpdateActiveAlerts(alerts []Alert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ActiveAlerts = alerts
}

// UpdateRecentlyResolved updates the recently resolved alerts in the state
func (s *State) UpdateRecentlyResolved(resolved []ResolvedAlert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RecentlyResolved = resolved
}

// UpdateNodes updates the nodes in the state
func (s *State) UpdateNodes(nodes []Node) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Sort nodes by name to ensure consistent ordering
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})

	s.Nodes = nodes
	s.LastUpdate = time.Now()
}

// UpdateNodesForInstance updates nodes for a specific instance, merging with existing nodes
func (s *State) UpdateNodesForInstance(instanceName string, nodes []Node) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing nodes, excluding those from this instance
	nodeMap := make(map[string]Node)
	for _, node := range s.Nodes {
		if node.Instance != instanceName {
			nodeMap[node.ID] = node
		}
	}

	// Add or update nodes from this instance
	for _, node := range nodes {
		nodeMap[node.ID] = node
	}

	// Convert map back to slice
	newNodes := make([]Node, 0, len(nodeMap))
	for _, node := range nodeMap {
		newNodes = append(newNodes, node)
	}

	// Sort nodes by name to ensure consistent ordering
	sort.Slice(newNodes, func(i, j int) bool {
		return newNodes[i].Name < newNodes[j].Name
	})

	s.Nodes = newNodes
	s.LastUpdate = time.Now()
}

// UpdateVMs updates the VMs in the state
func (s *State) UpdateVMs(vms []VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.VMs = vms
	s.LastUpdate = time.Now()
}

// UpdateVMsForInstance updates VMs for a specific instance, merging with existing VMs
func (s *State) UpdateVMsForInstance(instanceName string, vms []VM) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing VMs, excluding those from this instance
	vmMap := make(map[string]VM)
	for _, vm := range s.VMs {
		if vm.Instance != instanceName {
			vmMap[vm.ID] = vm
		}
	}

	// Add or update VMs from this instance
	for _, vm := range vms {
		vmMap[vm.ID] = vm
	}

	// Convert map back to slice
	newVMs := make([]VM, 0, len(vmMap))
	for _, vm := range vmMap {
		newVMs = append(newVMs, vm)
	}

	// Sort VMs by VMID to ensure consistent ordering
	sort.Slice(newVMs, func(i, j int) bool {
		return newVMs[i].VMID < newVMs[j].VMID
	})

	s.VMs = newVMs
	s.LastUpdate = time.Now()
}

// UpdateContainers updates the containers in the state
func (s *State) UpdateContainers(containers []Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Containers = containers
	s.LastUpdate = time.Now()
}

// UpdateContainersForInstance updates containers for a specific instance, merging with existing containers
func (s *State) UpdateContainersForInstance(instanceName string, containers []Container) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing containers, excluding those from this instance
	containerMap := make(map[string]Container)
	for _, container := range s.Containers {
		if container.Instance != instanceName {
			containerMap[container.ID] = container
		}
	}

	// Add or update containers from this instance
	for _, container := range containers {
		containerMap[container.ID] = container
	}

	// Convert map back to slice
	newContainers := make([]Container, 0, len(containerMap))
	for _, container := range containerMap {
		newContainers = append(newContainers, container)
	}

	// Sort containers by VMID to ensure consistent ordering
	sort.Slice(newContainers, func(i, j int) bool {
		return newContainers[i].VMID < newContainers[j].VMID
	})

	s.Containers = newContainers
	s.LastUpdate = time.Now()
}

// UpsertDockerHost inserts or updates a Docker host in state.
func (s *State) UpsertDockerHost(host DockerHost) {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := false
	for i, existing := range s.DockerHosts {
		if existing.ID == host.ID {
			s.DockerHosts[i] = host
			updated = true
			break
		}
	}

	if !updated {
		s.DockerHosts = append(s.DockerHosts, host)
	}

	sort.Slice(s.DockerHosts, func(i, j int) bool {
		return s.DockerHosts[i].Hostname < s.DockerHosts[j].Hostname
	})

	s.LastUpdate = time.Now()
}

// RemoveDockerHost removes a docker host by ID and returns the removed host.
func (s *State) RemoveDockerHost(hostID string) (DockerHost, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.DockerHosts {
		if host.ID == hostID {
			// Remove the host while preserving slice order
			s.DockerHosts = append(s.DockerHosts[:i], s.DockerHosts[i+1:]...)
			s.LastUpdate = time.Now()
			return host, true
		}
	}

	return DockerHost{}, false
}

// SetDockerHostStatus updates the status of a docker host if present.
func (s *State) SetDockerHostStatus(hostID, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for i, host := range s.DockerHosts {
		if host.ID == hostID {
			if host.Status != status {
				host.Status = status
				s.DockerHosts[i] = host
				s.LastUpdate = time.Now()
			}
			changed = true
			break
		}
	}

	return changed
}

// SetDockerHostHidden updates the hidden status of a docker host and returns the updated host.
func (s *State) SetDockerHostHidden(hostID string, hidden bool) (DockerHost, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.DockerHosts {
		if host.ID == hostID {
			host.Hidden = hidden
			s.DockerHosts[i] = host
			s.LastUpdate = time.Now()
			return host, true
		}
	}

	return DockerHost{}, false
}

// SetDockerHostPendingUninstall updates the pending uninstall status of a docker host and returns the updated host.
func (s *State) SetDockerHostPendingUninstall(hostID string, pending bool) (DockerHost, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.DockerHosts {
		if host.ID == hostID {
			host.PendingUninstall = pending
			s.DockerHosts[i] = host
			s.LastUpdate = time.Now()
			return host, true
		}
	}

	return DockerHost{}, false
}

// SetDockerHostCommand updates the active command status for a docker host.
func (s *State) SetDockerHostCommand(hostID string, command *DockerHostCommandStatus) (DockerHost, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.DockerHosts {
		if host.ID == hostID {
			host.Command = command
			s.DockerHosts[i] = host
			s.LastUpdate = time.Now()
			return host, true
		}
	}

	return DockerHost{}, false
}

// TouchDockerHost updates the last seen timestamp for a docker host.
func (s *State) TouchDockerHost(hostID string, ts time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.DockerHosts {
		if host.ID == hostID {
			host.LastSeen = ts
			s.DockerHosts[i] = host
			s.LastUpdate = time.Now()
			return true
		}
	}

	return false
}

// RemoveStaleDockerHosts removes docker hosts that haven't been seen since cutoff.
func (s *State) RemoveStaleDockerHosts(cutoff time.Time) []DockerHost {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := make([]DockerHost, 0)
	fresh := make([]DockerHost, 0, len(s.DockerHosts))
	for _, host := range s.DockerHosts {
		if host.LastSeen.Before(cutoff) && cutoff.After(host.LastSeen) {
			removed = append(removed, host)
			continue
		}
		fresh = append(fresh, host)
	}

	if len(removed) > 0 {
		s.DockerHosts = fresh
		s.LastUpdate = time.Now()
	}

	return removed
}

// GetDockerHosts returns a copy of docker hosts.
func (s *State) GetDockerHosts() []DockerHost {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hosts := make([]DockerHost, len(s.DockerHosts))
	copy(hosts, s.DockerHosts)
	return hosts
}

// UpsertHost inserts or updates a generic host in state.
func (s *State) UpsertHost(host Host) {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := false
	for i, existing := range s.Hosts {
		if existing.ID == host.ID {
			s.Hosts[i] = host
			updated = true
			break
		}
	}

	if !updated {
		s.Hosts = append(s.Hosts, host)
	}

	sort.Slice(s.Hosts, func(i, j int) bool {
		return s.Hosts[i].Hostname < s.Hosts[j].Hostname
	})

	s.LastUpdate = time.Now()
}

// GetHosts returns a copy of all generic hosts.
func (s *State) GetHosts() []Host {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hosts := make([]Host, len(s.Hosts))
	copy(hosts, s.Hosts)
	return hosts
}

// RemoveHost removes a host by ID and returns the removed entry.
func (s *State) RemoveHost(hostID string) (Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.Hosts {
		if host.ID == hostID {
			s.Hosts = append(s.Hosts[:i], s.Hosts[i+1:]...)
			s.LastUpdate = time.Now()
			return host, true
		}
	}

	return Host{}, false
}

// SetHostStatus updates the status of a host if present.
func (s *State) SetHostStatus(hostID, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.Hosts {
		if host.ID == hostID {
			if host.Status != status {
				host.Status = status
				s.Hosts[i] = host
				s.LastUpdate = time.Now()
			}
			return true
		}
	}
	return false
}

// TouchHost updates the last seen timestamp for a host.
func (s *State) TouchHost(hostID string, ts time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, host := range s.Hosts {
		if host.ID == hostID {
			host.LastSeen = ts
			s.Hosts[i] = host
			s.LastUpdate = time.Now()
			return true
		}
	}
	return false
}

// UpdateStorage updates the storage in the state
func (s *State) UpdateStorage(storage []Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Storage = storage
	s.LastUpdate = time.Now()
}

// UpdatePhysicalDisks updates physical disks for a specific instance
func (s *State) UpdatePhysicalDisks(instanceName string, disks []PhysicalDisk) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing disks, excluding those from this instance
	diskMap := make(map[string]PhysicalDisk)
	for _, disk := range s.PhysicalDisks {
		if disk.Instance != instanceName {
			diskMap[disk.ID] = disk
		}
	}

	// Add or update disks from this instance
	for _, disk := range disks {
		diskMap[disk.ID] = disk
	}

	// Convert map back to slice
	newDisks := make([]PhysicalDisk, 0, len(diskMap))
	for _, disk := range diskMap {
		newDisks = append(newDisks, disk)
	}

	// Sort by node and dev path for consistent ordering
	sort.Slice(newDisks, func(i, j int) bool {
		if newDisks[i].Node != newDisks[j].Node {
			return newDisks[i].Node < newDisks[j].Node
		}
		return newDisks[i].DevPath < newDisks[j].DevPath
	})

	s.PhysicalDisks = newDisks
	s.LastUpdate = time.Now()
}

// UpdateStorageForInstance updates storage for a specific instance, merging with existing storage
func (s *State) UpdateStorageForInstance(instanceName string, storage []Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing storage, excluding those from this instance
	storageMap := make(map[string]Storage)
	for _, st := range s.Storage {
		if st.Instance != instanceName {
			storageMap[st.ID] = st
		}
	}

	// Add or update storage from this instance
	for _, st := range storage {
		storageMap[st.ID] = st
	}

	// Convert map back to slice
	newStorage := make([]Storage, 0, len(storageMap))
	for _, st := range storageMap {
		newStorage = append(newStorage, st)
	}

	// Sort storage by name to ensure consistent ordering
	sort.Slice(newStorage, func(i, j int) bool {
		if newStorage[i].Instance == newStorage[j].Instance {
			return newStorage[i].Name < newStorage[j].Name
		}
		return newStorage[i].Instance < newStorage[j].Instance
	})

	s.Storage = newStorage
	s.LastUpdate = time.Now()
}

// UpdateCephClustersForInstance updates Ceph cluster information for a specific instance
func (s *State) UpdateCephClustersForInstance(instanceName string, clusters []CephCluster) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Preserve clusters from other instances
	filtered := make([]CephCluster, 0, len(s.CephClusters))
	for _, cluster := range s.CephClusters {
		if cluster.Instance != instanceName {
			filtered = append(filtered, cluster)
		}
	}

	// Add updated clusters (if any) for this instance
	if len(clusters) > 0 {
		filtered = append(filtered, clusters...)
	}

	// Sort for stable ordering in UI
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Instance == filtered[j].Instance {
			if filtered[i].Name == filtered[j].Name {
				return filtered[i].ID < filtered[j].ID
			}
			return filtered[i].Name < filtered[j].Name
		}
		return filtered[i].Instance < filtered[j].Instance
	})

	s.CephClusters = filtered
	s.LastUpdate = time.Now()
}

// UpdatePBSInstances updates the PBS instances in the state
func (s *State) UpdatePBSInstances(instances []PBSInstance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PBSInstances = instances
	s.LastUpdate = time.Now()
}

// UpdatePBSInstance updates a single PBS instance in the state, merging with existing instances
func (s *State) UpdatePBSInstance(instance PBSInstance) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find and update existing instance or append new one
	found := false
	for i, existing := range s.PBSInstances {
		if existing.ID == instance.ID {
			s.PBSInstances[i] = instance
			found = true
			break
		}
	}

	if !found {
		s.PBSInstances = append(s.PBSInstances, instance)
	}

	s.LastUpdate = time.Now()
}

// UpdatePMGInstances replaces the entire PMG instance list
func (s *State) UpdatePMGInstances(instances []PMGInstance) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.PMGInstances = instances
	s.LastUpdate = time.Now()
}

// UpdatePMGInstance updates or inserts a PMG instance record
func (s *State) UpdatePMGInstance(instance PMGInstance) {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := false
	for i := range s.PMGInstances {
		if s.PMGInstances[i].ID == instance.ID || strings.EqualFold(s.PMGInstances[i].Name, instance.Name) {
			s.PMGInstances[i] = instance
			updated = true
			break
		}
	}

	if !updated {
		s.PMGInstances = append(s.PMGInstances, instance)
	}

	s.LastUpdate = time.Now()
}

// UpdateBackupTasksForInstance updates backup tasks for a specific instance, merging with existing tasks
func (s *State) UpdateBackupTasksForInstance(instanceName string, tasks []BackupTask) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing tasks, excluding those from this instance
	taskMap := make(map[string]BackupTask)
	for _, task := range s.PVEBackups.BackupTasks {
		// Check if task ID contains the instance name
		if !strings.HasPrefix(task.ID, instanceName+"-") {
			taskMap[task.ID] = task
		}
	}

	// Add or update tasks from this instance
	for _, task := range tasks {
		taskMap[task.ID] = task
	}

	// Convert map back to slice
	newTasks := make([]BackupTask, 0, len(taskMap))
	for _, task := range taskMap {
		newTasks = append(newTasks, task)
	}

	// Sort by start time descending
	sort.Slice(newTasks, func(i, j int) bool {
		return newTasks[i].StartTime.After(newTasks[j].StartTime)
	})

	s.PVEBackups.BackupTasks = newTasks
	s.syncBackupsLocked()
	s.LastUpdate = time.Now()
}

// UpdateStorageBackupsForInstance updates storage backups for a specific instance, merging with existing backups
func (s *State) UpdateStorageBackupsForInstance(instanceName string, backups []StorageBackup) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// When storage is shared across nodes, backups can appear under whichever node reported the content.
	// Align each backup with the guest's current node so the frontend column matches the VM/CT placement.
	guestNodeByVMID := make(map[int]string)
	for _, vm := range s.VMs {
		if vm.Instance == instanceName && vm.Node != "" {
			guestNodeByVMID[vm.VMID] = vm.Node
		}
	}
	for _, ct := range s.Containers {
		if ct.Instance == instanceName && ct.Node != "" {
			guestNodeByVMID[ct.VMID] = ct.Node
		}
	}

	normalizedBackups := make([]StorageBackup, 0, len(backups))
	for _, backup := range backups {
		if backup.VMID > 0 {
			if node, ok := guestNodeByVMID[backup.VMID]; ok {
				backup.Node = node
			}
		}
		normalizedBackups = append(normalizedBackups, backup)
	}

	// Create a map of existing backups, excluding those from this instance
	backupMap := make(map[string]StorageBackup)
	for _, backup := range s.PVEBackups.StorageBackups {
		// Check if backup ID contains the instance name
		if !strings.HasPrefix(backup.ID, instanceName+"-") {
			backupMap[backup.ID] = backup
		}
	}

	// Add or update backups from this instance
	for _, backup := range normalizedBackups {
		backupMap[backup.ID] = backup
	}

	// Convert map back to slice
	newBackups := make([]StorageBackup, 0, len(backupMap))
	for _, backup := range backupMap {
		newBackups = append(newBackups, backup)
	}

	// Sort by time descending
	sort.Slice(newBackups, func(i, j int) bool {
		return newBackups[i].Time.After(newBackups[j].Time)
	})

	s.PVEBackups.StorageBackups = newBackups
	s.syncBackupsLocked()
	s.LastUpdate = time.Now()
}

// UpdateReplicationJobsForInstance updates replication jobs for a specific instance.
func (s *State) UpdateReplicationJobsForInstance(instanceName string, jobs []ReplicationJob) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]ReplicationJob, 0, len(s.ReplicationJobs))
	for _, job := range s.ReplicationJobs {
		if job.Instance != instanceName {
			filtered = append(filtered, job)
		}
	}

	now := time.Now()
	for _, job := range jobs {
		if job.Instance == "" {
			job.Instance = instanceName
		}
		if job.JobID == "" {
			job.JobID = job.ID
		}
		if job.LastPolled.IsZero() {
			job.LastPolled = now
		}
		filtered = append(filtered, job)
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Instance == filtered[j].Instance {
			if filtered[i].GuestID == filtered[j].GuestID {
				if filtered[i].JobNumber == filtered[j].JobNumber {
					if filtered[i].JobID == filtered[j].JobID {
						return filtered[i].ID < filtered[j].ID
					}
					return filtered[i].JobID < filtered[j].JobID
				}
				return filtered[i].JobNumber < filtered[j].JobNumber
			}
			if filtered[i].GuestID == 0 || filtered[j].GuestID == 0 {
				return filtered[i].Guest < filtered[j].Guest
			}
			return filtered[i].GuestID < filtered[j].GuestID
		}
		return filtered[i].Instance < filtered[j].Instance
	})

	s.ReplicationJobs = filtered
	s.LastUpdate = now
}

// UpdateGuestSnapshotsForInstance updates guest snapshots for a specific instance, merging with existing snapshots
func (s *State) UpdateGuestSnapshotsForInstance(instanceName string, snapshots []GuestSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing snapshots, excluding those from this instance
	snapshotMap := make(map[string]GuestSnapshot)
	for _, snapshot := range s.PVEBackups.GuestSnapshots {
		// Check if snapshot ID contains the instance name
		if !strings.HasPrefix(snapshot.ID, instanceName+"-") {
			snapshotMap[snapshot.ID] = snapshot
		}
	}

	// Add or update snapshots from this instance
	for _, snapshot := range snapshots {
		snapshotMap[snapshot.ID] = snapshot
	}

	// Convert map back to slice
	newSnapshots := make([]GuestSnapshot, 0, len(snapshotMap))
	for _, snapshot := range snapshotMap {
		newSnapshots = append(newSnapshots, snapshot)
	}

	// Sort by time descending
	sort.Slice(newSnapshots, func(i, j int) bool {
		return newSnapshots[i].Time.After(newSnapshots[j].Time)
	})

	s.PVEBackups.GuestSnapshots = newSnapshots
	s.syncBackupsLocked()
	s.LastUpdate = time.Now()
}

// SetConnectionHealth updates the connection health for an instance
func (s *State) SetConnectionHealth(instanceID string, healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ConnectionHealth[instanceID] = healthy
}

// RemoveConnectionHealth removes a connection health entry if it exists.
func (s *State) RemoveConnectionHealth(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ConnectionHealth, instanceID)
}

// UpdatePBSBackups updates PBS backups for a specific instance
func (s *State) UpdatePBSBackups(instanceName string, backups []PBSBackup) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a map of existing backups excluding ones from this instance
	backupMap := make(map[string]PBSBackup)
	for _, backup := range s.PBSBackups {
		if backup.Instance != instanceName {
			backupMap[backup.ID] = backup
		}
	}

	// Add new backups from this instance
	for _, backup := range backups {
		backupMap[backup.ID] = backup
	}

	// Convert map back to slice
	newBackups := make([]PBSBackup, 0, len(backupMap))
	for _, backup := range backupMap {
		newBackups = append(newBackups, backup)
	}

	// Sort by backup time (newest first)
	sort.Slice(newBackups, func(i, j int) bool {
		return newBackups[i].BackupTime.After(newBackups[j].BackupTime)
	})

	s.PBSBackups = newBackups
	s.syncBackupsLocked()
	s.LastUpdate = time.Now()
}

// UpdatePMGBackups updates PMG backups for a specific instance.
func (s *State) UpdatePMGBackups(instanceName string, backups []PMGBackup) {
	s.mu.Lock()
	defer s.mu.Unlock()

	combined := make([]PMGBackup, 0, len(s.PMGBackups)+len(backups))
	for _, backup := range s.PMGBackups {
		if backup.Instance != instanceName {
			combined = append(combined, backup)
		}
	}
	if len(backups) > 0 {
		combined = append(combined, backups...)
	}

	if len(combined) > 1 {
		sort.Slice(combined, func(i, j int) bool {
			return combined[i].BackupTime.After(combined[j].BackupTime)
		})
	}

	s.PMGBackups = combined
	s.syncBackupsLocked()
	s.LastUpdate = time.Now()
}
