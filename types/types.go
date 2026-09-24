package types

// Config holds all configuration loaded from config.toml
// Struct field names must be exported (capitalized) for TOML unmarshaling to work
type Config struct {
	PasswordFile string         `toml:"pwfile"`
	Repository   string         `toml:"repo"`
	Debug        bool           `toml:"debug"`
	ListenPort   string         `toml:"listen_port"`
	Jobs         []JobConfig    `toml:"jobs"`
	Clients      []ClientConfig `toml:"clients"`
}

// JobCfg represents a backup job configuration
type JobConfig struct {
	Name       string   `toml:"name"`
	Tag        []string `toml:"tag"`
	Files      []string `toml:"files"`
	PreHook    []string `toml:"preHook"`
	PostHook   []string `toml:"postHook"`
	AssignedTo []string `toml:"assigned_to"`
}

type ClientConfig struct {
	Name                  string `toml:"name"`
	ScheduleStartHour     int    `toml:"schedule_start_hour"`     // HH:00 UTC when client starts
	ScheduleOffsetMinutes int    `toml:"schedule_offset_minutes"` // Delay after start hour
	PreferredHourRange    []int  `toml:"preferred_hour_range"`    // Preferred window [start, end]
	MaxConcurrentJobs     int    `toml:"max_concurrent_jobs"`     // Max parallel jobs for this client
	TimeoutSeconds        int    `toml:"timeout_seconds"`         // Override global timeout
}

type BackupStatus struct {
	Job      string `json:"job"`
	Progress int    `json:"progress"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	ClientId string `json:"client_id,omitempty"`
}
