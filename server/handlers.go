package main

import (
	"encoding/json"
	"factorest/types"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// jobState records a job the server believes a client is currently running
type jobState struct {
	name  string
	since time.Time
}

// heartbeatStore keeps track of each client's last heartbeat, the
// jobs the server believes each client is currently running, and the
// local date on which a backup trigger was last issued for the client
// (the guard window)
type store struct {
	sync.Mutex
	clients map[string]types.Heartbeat
	running map[string]map[string]jobState
	pending map[string]string
}

var heartbeatStore = &store{
	clients: make(map[string]types.Heartbeat),
	running: make(map[string]map[string]jobState),
	pending: make(map[string]string),
}

func (s *store) hasRunningJob(clientID string) bool {
	return len(s.running[clientID]) > 0
}

func (s *store) markRunning(clientID, job string) {
	if s.running[clientID] == nil {
		s.running[clientID] = make(map[string]jobState)
	}
	s.running[clientID][job] = jobState{name: job, since: time.Now()}
}

func (s *store) clearRunning(clientID, job string) {
	if s.running[clientID] != nil {
		delete(s.running[clientID], job)
	}
}

// localDate renders a time as its local YYYY-MM-DD date
func localDate(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02")
}

// markPending records the local date on which the server last issued a
// backup trigger for this client; the guard then suppresses repeat
// triggers for the rest of that client's local day
func (s *store) markPending(clientID string) {
	s.pending[clientID] = localDate(time.Now())
}

// clearPending removes the guard entry for a client
func (s *store) clearPending(clientID string) {
	delete(s.pending, clientID)
}

// guardActive reports whether the backup guard is currently suppressing
// repeat triggers for this client (a trigger already went out today)
func (s *store) guardActive(clientID string) bool {
	at, ok := s.pending[clientID]
	return ok && at == localDate(time.Now())
}

// sweepStale clears running state for clients whose heartbeats went silent
func (s *store) sweepStale() {
	s.Lock()
	defer s.Unlock()
	for id := range s.running {
		hb, ok := s.clients[id]
		if !ok || time.Since(hb.Timestamp) > 60*time.Second {
			for job := range s.running[id] {
				s.clearRunning(id, job)
			}
			slog.Warn("cleared stale running state", slog.String("client_id", id))
		}
	}
}

// ServeHeartbeat records the client's heartbeat in memory
func ServeHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var hb types.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		http.Error(w, fmt.Sprintf("invalid heartbeat: %v", err), http.StatusBadRequest)
		return
	}

	if hb.ClientID == "" {
		http.Error(w, "no client_id provided", http.StatusBadRequest)
		return
	}

	heartbeatStore.Lock()
	defer heartbeatStore.Unlock()

	// The server stamps the heartbeat with its own clock; the client's
	// timestamp is ignored so it can't fake freshness or staleness.
	hb.Timestamp = time.Now()

	heartbeatStore.clients[hb.ClientID] = hb

	// If the client says it's free but the server still thinks it's running,
	// trust the client and clear the server-side running state.
	// The guard is intentionally NOT cleared here: it stays active until the
	// client's local day rolls over, so each client gets at most one trigger
	// per day (a failed job clears it, allowing a same-day retry).
	if !hb.JobsRunning && heartbeatStore.hasRunningJob(hb.ClientID) {
		slog.Warn("heartbeat reports no jobs, clearing server state",
			slog.String("client_id", hb.ClientID))
		for job := range heartbeatStore.running[hb.ClientID] {
			heartbeatStore.clearRunning(hb.ClientID, job)
		}
	}

	// Schedule backups here:
	// Just setting Status to do_it triggers the client to fetch
	// config from config.json, and do the rest
	status := "ok"
	if timeToBackup(hb.ClientID) && !heartbeatStore.hasRunningJob(hb.ClientID) && !heartbeatStore.guardActive(hb.ClientID) {
		slog.Info("it is time for backups...")
		status = "do_it"
		heartbeatStore.markPending(hb.ClientID)
		cfg := configStore.Current()
		for _, job := range cfg.Jobs {
			if isClientAllowedToJob(job, hb.ClientID) {
				heartbeatStore.markRunning(hb.ClientID, job.Name)
			}
		}
	}

	slog.Debug("Heartbeat received", slog.String("client_id", hb.ClientID), slog.String("name", hb.Name))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&types.HeartbeatResponse{
		Status:    status,
		ClientID:  hb.ClientID,
		Timestamp: time.Now().Unix(),
	})

}

// ServeJobStatus records the client's job lifecycle reports
// ("started", "completed", "failed") so the server knows when clients
// are running jobs
func ServeJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var js types.JobStatus
	if err := json.NewDecoder(r.Body).Decode(&js); err != nil {
		http.Error(w, fmt.Sprintf("invalid job status: %v", err), http.StatusBadRequest)
		return
	}

	if js.ClientID == "" {
		http.Error(w, "no client_id provided", http.StatusBadRequest)
		return
	}
	if js.Status != "started" && js.Status != "completed" && js.Status != "failed" {
		http.Error(w, "unknown status", http.StatusBadRequest)
		return
	}

	heartbeatStore.Lock()
	defer heartbeatStore.Unlock()
	switch js.Status {
	case "started":
		heartbeatStore.markRunning(js.ClientID, js.Job)
	case "completed":
		heartbeatStore.clearRunning(js.ClientID, js.Job)
	case "failed":
		heartbeatStore.clearRunning(js.ClientID, js.Job)
		// A failed backup clears the guard so the client can be
		// re-triggered again today
		heartbeatStore.clearPending(js.ClientID)
	}

	slog.Debug("Job status recorded",
		slog.String("client_id", js.ClientID),
		slog.String("job", js.Job),
		slog.String("status", js.Status))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "recorded"})
}

// clientView pairs a client's last heartbeat with the jobs the server
// believes are currently running
type clientView struct {
	types.Heartbeat
	RunningJobs []string `json:"running_jobs"`
	Pending     bool     `json:"pending"`
}

// ListHeartbeats returns the clients seen so far with their last heartbeat
// and the jobs the server believes are currently running per client
func ListHeartbeats(w http.ResponseWriter, r *http.Request) {
	heartbeatStore.Lock()
	views := make(map[string]clientView, len(heartbeatStore.clients))
	for id, hb := range heartbeatStore.clients {
		jobs := []string{}
		for name := range heartbeatStore.running[id] {
			jobs = append(jobs, name)
		}
		views[id] = clientView{Heartbeat: hb, RunningJobs: jobs, Pending: heartbeatStore.guardActive(id)}
	}
	heartbeatStore.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

func ServeHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","server_time":%d}`, now.Unix())
}

// ServeConfig returns global config and jobs for the specific client ID
func ServeConfig(r *http.Request) (types.Config, error) {
	// Extract client ID from header or query parameter
	clientId := r.Header.Get("X-Client-ID")
	if clientId == "" {
		clientId = r.URL.Query().Get("client_id")
	}
	if clientId == "" {
		return types.Config{}, fmt.Errorf("no client_id provided in header or query parameter")
	}

	return GetClientConfig(clientId)
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
	if err := configStore.Reload(configPath); err != nil {
		http.Error(w, fmt.Sprintf("failed to reload config: %v", err), http.StatusInternalServerError)
		return
	}

	cfg := configStore.Current()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "reloaded",
		"message": "Config reloaded successfully",
		"jobs":    len(cfg.Jobs),
		"clients": len(cfg.Clients),
	})
}
