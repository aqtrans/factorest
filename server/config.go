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

// Get reloads the config from disk and updates the store.
// On failure, the last known good config is retained.
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
