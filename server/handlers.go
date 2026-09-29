package main

import (
	"encoding/json"
	"factorest/types"
	"fmt"
	"net/http"
	"time"
)

func serveHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","server_time":%d}`, now.Unix())
}

// serveConfig returns the global config filtered for the requesting client.
// It reads from the cached cfgStore (no file I/O per request).
func serveConfig(r *http.Request) (types.Config, error) {
	cfgStore.RLock()
	if cfgStore.cfg == nil {
		cfgStore.RUnlock()
		return types.Config{}, fmt.Errorf("config not loaded")
	}
	// Work on a local copy to avoid mutating shared state
	config := *cfgStore.cfg
	cfgStore.RUnlock()

	// Extract client ID from header or query parameter
	clientId := r.Header.Get("X-Client-ID")
	if clientId == "" {
		clientId = r.URL.Query().Get("client_id")
	}
	if clientId == "" {
		return types.Config{}, fmt.Errorf("no client_id provided in header or query parameter")
	}

	// Find all jobs assigned to this client (including public jobs)
	var matchingJobs []types.JobConfig
	for _, job := range config.Jobs {
		if isClientAllowedToJob(job, clientId) {
			matchingJobs = append(matchingJobs, job)
		}
	}

	config.Jobs = matchingJobs

	// Attach the matching client config so the client can run its own hooks
	for _, c := range config.Clients {
		if c.Name == clientId {
			config.Client = c
			if c.Repository != "" {
				config.Repository = c.Repository // client-level override
			}
			break
		}
	}

	return config, nil
}

// isClientAllowedToJob checks if a client_id can access this job
func isClientAllowedToJob(job types.JobConfig, clientId string) bool {
	if len(job.AssignedTo) == 0 {
		// Job is public (no restrictions)
		return true
	}
	for _, assigned := range job.AssignedTo {
		if assigned == "" || assigned == clientId {
			return true
		}
	}
	return false
}

// serveConfigJSON returns JSON-encoded config for the client
func serveConfigJSON(w http.ResponseWriter, r *http.Request) {
	config, err := serveConfig(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

// jobView is the JSON representation of a job for the dashboard overview.
type jobView struct {
	Name       string   `json:"name"`
	Tag        []string `json:"tag,omitempty"`
	Files      []string `json:"files,omitempty"`
	AssignedTo []string `json:"assigned_to,omitempty"`
	Public     bool     `json:"public"`
}

// serveJobs returns the configured jobs for the dashboard overview: GET /jobs
func serveJobs(w http.ResponseWriter, r *http.Request) {
	cfgStore.RLock()
	jobs := make([]jobView, 0, len(cfgStore.cfg.Jobs))
	for _, j := range cfgStore.cfg.Jobs {
		jobs = append(jobs, jobView{
			Name:       j.Name,
			Tag:        j.Tag,
			Files:      j.Files,
			AssignedTo: j.AssignedTo,
			Public:     len(j.AssignedTo) == 0,
		})
	}
	cfgStore.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jobs)
}

// serveReload manually triggers a config reload (for testing or CLI tools)
func serveReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Trigger a reload via the store (which manages its own locking)
	if err := cfgStore.Get(); err != nil {
		http.Error(w, fmt.Sprintf("failed to reload config: %v", err), http.StatusInternalServerError)
		return
	}

	cfgStore.RLock()
	jobs := len(cfgStore.cfg.Jobs)
	clients := len(cfgStore.cfg.Clients)
	cfgStore.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "reloaded",
		"message": "Config reloaded successfully",
		"jobs":    jobs,
		"clients": clients,
	})
}
