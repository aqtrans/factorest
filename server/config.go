package main

import (
	"factorest/types"
	"fmt"
	"os"
	"sync"

	"github.com/pelletier/go-toml/v2"
)

// configPath is the path of the server config file. Overridable with -config.
var configPath = "config.toml"

// ConfigStore holds the currently active server config.
// Readers take the read lock; reloads take the write lock, and the
// last known good config is kept when a reload fails.
type ConfigStore struct {
	sync.RWMutex
	cfg *types.Config
}

var configStore = &ConfigStore{}

// LoadConfigFile reads and parses the TOML config file.
// A missing or empty file is an error, not a silent zero config.
func LoadConfigFile(path string) (*types.Config, error) {
	var config types.Config

	configBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %q: %w", path, err)
	}
	if len(configBytes) == 0 {
		return nil, fmt.Errorf("config file %q is empty", path)
	}
	if err := toml.Unmarshal(configBytes, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config %q: %w", path, err)
	}
	return &config, nil
}

// Init loads the config file into the store at startup.
func (s *ConfigStore) Init(path string) error {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return err
	}
	s.Lock()
	s.cfg = cfg
	s.Unlock()
	return nil
}

// Current returns the active config, safe for concurrent readers.
func (s *ConfigStore) Current() *types.Config {
	s.RLock()
	defer s.RUnlock()
	return s.cfg
}

// Reload re-reads the file and swaps in the new config.
// If the new file is invalid, the last known good config is kept.
func (s *ConfigStore) Reload(path string) error {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return err
	}
	s.Lock()
	s.cfg = cfg
	s.Unlock()
	return nil
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

// GetClientConfig returns the active config filtered to the jobs
// assigned to the client (including public jobs).
func GetClientConfig(clientID string) (types.Config, error) {
	cfg := configStore.Current()
	if cfg == nil {
		return types.Config{}, fmt.Errorf("no active config")
	}

	config := *cfg
	var matchingJobs []types.JobConfig
	for _, job := range config.Jobs {
		if isClientAllowedToJob(job, clientID) {
			matchingJobs = append(matchingJobs, job)
		}
	}
	config.Jobs = matchingJobs

	return config, nil
}
