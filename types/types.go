package types

// Config holds all configuration loaded from config.toml
// Struct field names must be exported (capitalized) for TOML unmarshaling to work
type Config struct {
	PasswordFile string         `toml:"pwfile"`
	Repository   string         `toml:"repo"`
	Debug        bool           `toml:"debug"`
	DryRun       bool           `toml:"dry-run"`
	ListenPort   string         `toml:"listen_port"`
	Jobs         []JobConfig    `toml:"jobs"`
	Clients      []ClientConfig `toml:"clients"`
	Client       ClientConfig   `toml:"-"` // populated by server per-request, not from TOML
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
	Name         string   `toml:"name"`
	Repository   string   `toml:"repo"`          // optional override
	ScheduleTime string   `toml:"schedule_time"` // e.g. "4:40 pm" — local time when client starts
	PreHook      []string `toml:"preHook"`       // commands to run before all jobs start
	PostHook     []string `toml:"postHook"`      // commands to run after all jobs succeed
	OnFailure    []string `toml:"onFailure"`     // commands to run if any job fails
}

type BackupStatus struct {
	Job      string `json:"job"`
	Progress int    `json:"progress"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	ClientId string `json:"client_id,omitempty"`
}
