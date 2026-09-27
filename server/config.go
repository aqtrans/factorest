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
