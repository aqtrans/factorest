package main

import (
	"factorest/types"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

func LoadConfig() (*types.Config, error) {
	var config types.Config

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
func IsClientAllowed(job types.JobConfig, clientId string) bool {
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

func ToClientConfig(jobs []types.JobConfig) types.JobConfig {
	if len(jobs) > 0 {
		return jobs[0]
	}

	// Fallback: only use defaults if no config file exists
	cfg, _ := LoadConfig()
	if cfg != nil && len(cfg.Jobs) > 0 {
		return cfg.Jobs[0]
	}

	// Absolute last resort default
	return types.JobConfig{Name: "unknown-job", Files: []string{""}}
}

// ServeConfig returns global config and jobs for the specific client ID
// GetClientEntry returns the ClientConfig for a single client by name,
// or false if no client with that name exists.
func GetClientEntry(cfg types.Config, clientID string) (types.ClientConfig, bool) {
	for _, c := range cfg.Clients {
		if c.Name == clientID {
			return c, true
		}
	}
	return types.ClientConfig{}, false
}

func GetClientConfig(clientId string) (types.Config, error) {
	config, err := LoadConfig()
	if err != nil {
		return types.Config{}, fmt.Errorf("failed to load config: %w", err)
	}

	// Find all jobs assigned to this client (including public jobs)
	var matchingJobs []types.JobConfig
	for _, job := range config.Jobs {
		if isClientAllowedToJob(job, clientId) {
			matchingJobs = append(matchingJobs, job)
		}
	}

	config.Jobs = matchingJobs

	return *config, nil
}
