package main

import (
	"factorest/types"
	"fmt"
	"os"
	"sync"

	"github.com/pelletier/go-toml/v2"
)

// ConfigStore holds the currently active server config.
// Readers take the read lock; reloads take the write lock, and the
// last known good config is kept when a reload fails.
type configStore struct {
	sync.RWMutex
	cfg *types.Config
}

func loadConfig() (*types.Config, error) {
	var config types.Config

	// Read TOML config file and unmarshal directly into our struct
	// TODO: Figure out the best way to integrate this
	configBytes, err := os.ReadFile(configPath)
	if err == nil && len(configBytes) > 0 {
		err = toml.Unmarshal(configBytes, &config)
		return &config, err
	}

	return &config, nil
}

func (s *configStore) Get() error {
	var config types.Config

	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config %q: %w", configPath, err)
	}
	if len(configBytes) == 0 {
		return fmt.Errorf("config file %q is empty", configPath)
	}
	if err := toml.Unmarshal(configBytes, &config); err != nil {
		return fmt.Errorf("failed to parse config %q: %w", configPath, err)
	}

	s.Lock()
	s.cfg = &config
	s.Unlock()
	return nil
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

func toClientConfig(jobs []types.JobConfig) types.JobConfig {
	if len(jobs) > 0 {
		return jobs[0]
	}

	// Fallback: only use defaults if no config file exists
	cfg, _ := loadConfig()
	if cfg != nil && len(cfg.Jobs) > 0 {
		return cfg.Jobs[0]
	}

	// Absolute last resort default
	return types.JobConfig{Name: "unknown-job", Files: []string{""}}
}
