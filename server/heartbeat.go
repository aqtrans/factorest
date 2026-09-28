package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Heartbeat state. Only touched from HTTP handlers, guarded by heartbeatMu.
var (
	heartbeatMu        sync.Mutex
	clientLastSeen     = map[string]time.Time{}
	pendingBackup      = map[string]bool{}
	scheduledTriggered = map[string]string{} // clientID -> date string (e.g. "2026-09-28")
	backupInProgress   string                // clientID currently backing up, "" if none
)

// clientExists validates a client id against the configured clients.
func clientExists(clientID string) bool {
	cfgStore.RLock()
	defer cfgStore.RUnlock()
	if cfgStore.cfg == nil {
		return false
	}
	for _, c := range cfgStore.cfg.Clients {
		if c.Name == clientID {
			return true
		}
	}
	return false
}

// isItTimeToBackup checks whether the current time has reached or passed the
// client's scheduled backup time, the trigger has not already fired today,
// and no backup is currently in progress. Returns true only on the first
// heartbeat after the scheduled time for a given day.
func isItTimeToBackup(clientID string) bool {
	cfgStore.RLock()
	var scheduleTime string
	if cfgStore.cfg != nil {
		for _, c := range cfgStore.cfg.Clients {
			if c.Name == clientID {
				scheduleTime = c.ScheduleTime
				break
			}
		}
	}
	cfgStore.RUnlock()

	if scheduleTime == "" {
		return false
	}

	// Parse "4:40 pm" style time
	sched, err := time.Parse("3:04 pm", scheduleTime)
	if err != nil {
		slog.Debug("failed to parse schedule_time", slog.String("client_id", clientID), slog.String("value", scheduleTime), slog.Any("err", err))
		return false
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	// Build today's scheduled start time
	schedToday := time.Date(now.Year(), now.Month(), now.Day(), sched.Hour(), sched.Minute(), 0, 0, now.Location())

	// Only trigger if current time has reached or passed the scheduled time
	if now.Before(schedToday) {
		return false
	}

	// Check if already triggered today or a backup is already in progress
	heartbeatMu.Lock()
	defer heartbeatMu.Unlock()
	if scheduledTriggered[clientID] == today {
		return false
	}
	if backupInProgress != "" {
		return false
	}
	scheduledTriggered[clientID] = today
	backupInProgress = clientID
	return true
}

// serveClientStatus is the heartbeat endpoint: POST /clients/{id}/status
// It records the client's last seen time and responds with status "ok",
// or "backup" if a manual backup is pending or the scheduled time has been reached.
func serveClientStatus(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if !clientExists(clientID) {
		http.Error(w, "unknown client: "+clientID, http.StatusNotFound)
		return
	}

	heartbeatMu.Lock()
	clientLastSeen[clientID] = time.Now()
	pending := pendingBackup[clientID]
	pendingBackup[clientID] = false
	// Clear the backup lock if this client is checking in after finishing its backup
	if backupInProgress == clientID && !pending {
		backupInProgress = ""
	}
	heartbeatMu.Unlock()

	// Also trigger if the scheduled time has been reached (once per day)
	if !pending && isItTimeToBackup(clientID) {
		pending = true
	}

	status := "ok"
	if pending {
		status = "backup"
	}
	slog.Debug("heartbeat received", slog.String("client_id", clientID), slog.String("status", status))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":      status,
		"server_time": time.Now().Unix(),
	})
}

// serveClientBackup is the manual trigger: POST /clients/{id}/backup
// Queues a backup for the client; it is consumed on the client's next heartbeat.
func serveClientBackup(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if !clientExists(clientID) {
		http.Error(w, "unknown client: "+clientID, http.StatusNotFound)
		return
	}

	heartbeatMu.Lock()
	pendingBackup[clientID] = true
	heartbeatMu.Unlock()

	slog.Info("manual backup queued", slog.String("client_id", clientID))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "queued", "client_id": clientID})
}

// clientView is the JSON representation of one client's live state.
type clientView struct {
	ClientID string `json:"client_id"`
	Status   string `json:"status"`
	LastSeen int64  `json:"last_seen,omitempty"`
}

// serveClients lists the server's live view of all clients: GET /clients
func serveClients(w http.ResponseWriter, r *http.Request) {
	heartbeatMu.Lock()
	views := make([]clientView, 0, len(clientLastSeen))
	for id, last := range clientLastSeen {
		views = append(views, clientView{ClientID: id, Status: "online", LastSeen: last.Unix()})
	}
	heartbeatMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

// serveClient is the single-client view: GET /clients/{id}
func serveClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if !clientExists(clientID) {
		http.Error(w, "unknown client: "+clientID, http.StatusNotFound)
		return
	}

	heartbeatMu.Lock()
	last, seen := clientLastSeen[clientID]
	heartbeatMu.Unlock()

	view := clientView{ClientID: clientID, Status: "never seen"}
	if seen {
		view.Status = "online"
		view.LastSeen = last.Unix()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view)
}
