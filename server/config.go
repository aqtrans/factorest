package main

import (
	"log"
	"os"

	"github.com/pelletier/go-toml/v2"
)

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

func LoadConfig() (*Config, error) {
	var config Config

	// Read TOML config file and unmarshal directly into our struct
	// TODO: Figure out the best way to integrate this
	configBytes, err := os.ReadFile("config.toml")
	if err == nil && len(configBytes) > 0 {
		err = toml.Unmarshal(configBytes, &config)
		return &config, err
	}

	return &config, nil
}

// Check if a client is allowed to run this job (internal use only)
// This function is no longer used - replaced by handlers.isClientAllowedToJob
func IsClientAllowed(job JobConfig, clientId string) bool {
	log.Println(job, clientId)
	if len(job.AssignedTo) == 0 {
		return true
	}
	for _, assigned := range job.AssignedTo {
		if assigned == "" || contains(clientId, assigned) {
			return true
		}
	}
	return false
}

// contains is a helper to check if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func ToClientConfig(jobs []JobConfig) JobConfig {
	if len(jobs) > 0 {
		return jobs[0]
	}

	// Fallback: only use defaults if no config file exists
	config, _ := LoadConfig()
	if config != nil && len(config.Jobs) > 0 {
		return config.Jobs[0]
	}

	// Absolute last resort default
	return JobConfig{Name: "unknown-job", Files: []string{""}}
}
