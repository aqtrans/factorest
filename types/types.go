package types

import "time"

// Heartbeat is sent by clients to report their health to the server
// It carries the client's ID, display name, and the time it was sent
type Heartbeat struct {
	ClientID    string    `json:"client_id"`
	Name        string    `json:"name"`
	Timestamp   time.Time `json:"timestamp"`
	JobsRunning bool      `json:"running"`
}

// JobStatus reports a job lifecycle event from a client to the server
type JobStatus struct {
	Job      string `json:"job"`
	ClientID string `json:"client_id"`
	Status   string `json:"status"` // "started", "completed", "failed"
}

type HeartbeatResponse struct {
	Status    string `json:"status"`
	ClientID  string `json:"client_id"`
	Timestamp int64  `json:"server_time"`
}

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
	Name           string `toml:"name"`
	ScheduleTime   string `toml:"schedule_time"`   // AM/PM
	TimeoutSeconds int    `toml:"timeout_seconds"` // Override global timeout
}

type BackupStatus struct {
	Job      string `json:"job"`
	Progress int    `json:"progress"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	ClientId string `json:"client_id,omitempty"`
}
