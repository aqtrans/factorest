package main

import (
	"encoding/json"
	"factorest/types"
	"fmt"
	"net/http"
	"time"
)

func ServeHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","server_time":%d}`, now.Unix())
}

// ServeConfig returns global config and
func ServeConfig(r *http.Request) (types.Config, error) {
	config, err := LoadConfig()
	if err != nil {
		return types.Config{}, fmt.Errorf("failed to load config: %w", err)
	}

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

	return *config, nil
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

// ServeConfigJSON returns JSON-encoded list of jobs for the client
func ServeConfigJSON(w http.ResponseWriter, r *http.Request) {
	jobs, err := ServeConfig(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jobs)
}

// ServeReload - manually trigger config reload (for testing or CLI tools)
func ServeReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Trigger a reload
	config, err := LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reload config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "reloaded",
		"message": "Config reloaded successfully",
		"jobs":    len(config.Jobs),
		"clients": len(config.Clients),
	})
}

// ServeProgress returns a placeholder backup status with the client ID
func ServeProgress(w http.ResponseWriter, r *http.Request) (types.BackupStatus, error) {
	config, err := LoadConfig()
	if err != nil {
		return types.BackupStatus{Job: "default-job", Progress: 0, Status: "idle", Message: ""}, nil
	}

	status := types.BackupStatus{
		Job:      config.Jobs[0].Name,
		Progress: 0,
		Status:   "idle",
		Message:  "",
	}

	// Add client ID if present
	clientId := r.Header.Get("X-Client-ID")
	if clientId == "" {
		clientId = r.URL.Query().Get("client_id")
	}
	status.ClientId = clientId

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
	return status, nil
}
