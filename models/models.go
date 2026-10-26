package models

// User represents a user in the system
type User struct {
	Username     string `json:"username" db:"username"`
	PasswordHash string `json:"-" db:"password_hash"`
	Role         string `json:"role" db:"role"`
	TOTPSecret   string `json:"-" db:"totp_secret"`
	TOTPEnabled  bool   `json:"totp_enabled" db:"totp_enabled"`
}

// Lab represents a project
type Lab struct {
	Project string `json:"project" db:"project"`
	Owner   string `json:"owner" db:"owner"`
}

// ActivityLog entry
type ActivityLog struct {
	ID        int    `json:"id" db:"id"`
	Timestamp string `json:"timestamp" db:"timestamp"`
	Username  string `json:"username" db:"username"`
	Action    string `json:"action" db:"action"`
	Target    string `json:"target" db:"target"`
	Details   string `json:"details" db:"details"`
}

// Agent represents an external monitoring agent
type Agent struct {
	AgentID   string `json:"agent_id" db:"agent_id"`
	Name      string `json:"name" db:"name"`
	Token     string `json:"token" db:"token"`
	CreatedBy string `json:"created_by" db:"created_by"`
	CreatedAt string `json:"created_at" db:"created_at"`
	LastSeen  string `json:"last_seen" db:"last_seen"`
	Status    string `json:"status" db:"status"`
	Hostname  string `json:"hostname" db:"hostname"`
	OSInfo    string `json:"os_info" db:"os_info"`
	IPAddress string `json:"ip_address" db:"ip_address"`
	LastData  string `json:"last_data" db:"last_data"`
}

// BackupSchedule represents a backup schedule
type BackupSchedule struct {
	ID             int    `json:"id" db:"id"`
	Project        string `json:"project" db:"project"`
	VolumeName     string `json:"volume_name" db:"volume_name"`
	CronExpr       string `json:"cron_expr" db:"cron_expr"`
	RetentionCount int    `json:"retention_count" db:"retention_count"`
	Enabled        bool   `json:"enabled" db:"enabled"`
	LastRun        string `json:"last_run" db:"last_run"`
	CreatedBy      string `json:"created_by" db:"created_by"`
	CreatedAt      string `json:"created_at" db:"created_at"`
}

// BackupHistory represents a backup entry
type BackupHistory struct {
	ID         int    `json:"id" db:"id"`
	Project    string `json:"project" db:"project"`
	VolumeName string `json:"volume_name" db:"volume_name"`
	Filename   string `json:"filename" db:"filename"`
	SizeBytes  int64  `json:"size_bytes" db:"size_bytes"`
	Status     string `json:"status" db:"status"`
	CreatedAt  string `json:"created_at" db:"created_at"`
	CreatedBy  string `json:"created_by" db:"created_by"`
}

// ContainerLog represents a centralized log entry
type ContainerLog struct {
	ID            int    `json:"id" db:"id"`
	Timestamp     string `json:"timestamp" db:"timestamp"`
	ContainerID   string `json:"container_id" db:"container_id"`
	ContainerName string `json:"container_name" db:"container_name"`
	Project       string `json:"project" db:"project"`
	Severity      string `json:"severity" db:"severity"`
	Message       string `json:"message" db:"message"`
}

// DockerHost represents a remote Docker host
type DockerHost struct {
	HostID    string `json:"host_id" db:"host_id"`
	Name      string `json:"name" db:"name"`
	URL       string `json:"url" db:"url"`
	AuthType  string `json:"auth_type" db:"auth_type"`
	TLSCa     string `json:"tls_ca,omitempty" db:"tls_ca"`
	TLSCert   string `json:"tls_cert,omitempty" db:"tls_cert"`
	TLSKey    string `json:"tls_key,omitempty" db:"tls_key"`
	Status    string `json:"status" db:"status"`
	LastCheck string `json:"last_check" db:"last_check"`
	AddedBy   string `json:"added_by" db:"added_by"`
	AddedAt   string `json:"added_at" db:"added_at"`
}

// MetricsHistory represents a time-series metrics entry (compatible with Python schema)
type MetricsHistory struct {
	ID         int     `json:"id" db:"id"`
	Timestamp  int64   `json:"timestamp" db:"timestamp"`
	SourceType string  `json:"source_type" db:"source_type"`
	SourceID   string  `json:"source_id" db:"source_id"`
	SourceName string  `json:"source_name" db:"source_name"`
	Project    string  `json:"project" db:"project"`
	CPU        float64 `json:"cpu" db:"cpu"`
	Mem        float64 `json:"mem" db:"mem"`
	MemMB      float64 `json:"mem_mb" db:"mem_mb"`
	NetRx      float64 `json:"net_rx" db:"net_rx"`
	NetTx      float64 `json:"net_tx" db:"net_tx"`
}

// ContainerInfo for dashboard/supervision responses
type ContainerInfo struct {
	ID      string  `json:"id"`
	FullID  string  `json:"full_id"`
	Name    string  `json:"name"`
	Image   string  `json:"image"`
	State   string  `json:"state"`
	Project string  `json:"project"`
	Ports   string  `json:"ports"`
	CPU     float64 `json:"cpu_percent,omitempty"`
	MemMB   float64 `json:"mem_mb,omitempty"`
	MemLim  float64 `json:"mem_limit_mb,omitempty"`
	NetRx   int64   `json:"net_rx,omitempty"`
	NetTx   int64   `json:"net_tx,omitempty"`
}

// ProjectInfo for dashboard
type ProjectInfo struct {
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Containers []ContainerInfo `json:"containers"`
	Role       string          `json:"role"`
}

// SupervisionSummary for the supervision endpoint
type SupervisionSummary struct {
	TotalRunning   int     `json:"total_running"`
	GlobalCPU      float64 `json:"global_cpu"`
	GlobalMemMB    float64 `json:"global_mem_mb"`
	GlobalMemLimMB float64 `json:"global_mem_limit_mb"`
	GlobalMemPct   float64 `json:"global_mem_percent"`
	Alerts         int     `json:"alerts"`
	AgentsOnline   int     `json:"agents_online"`
	AgentsTotal    int     `json:"agents_total"`
}

// CatalogueEntry for custom services
type CatalogueEntry struct {
	Key         string `json:"key" db:"key"`
	Image       string `json:"image" db:"image"`
	Category    string `json:"category" db:"category"`
	Description string `json:"description" db:"description"`
	AddedBy     string `json:"added_by" db:"added_by"`
	AddedAt     string `json:"added_at" db:"added_at"`
}
