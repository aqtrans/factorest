package main

import (
	"factorest/types"
	"log"
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
