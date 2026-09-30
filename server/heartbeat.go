package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// backupSlot tracks the single client currently holding the backup slot.
type backupSlot struct {
	ClientID  string
	StartedAt time.Time
	LastSeen  time.Time
	Job       string // current job name (for observability)
}

// Heartbeat state. Only touched from HTTP handlers and the timeout monitor,
// guarded by heartbeatMu.
var (
	heartbeatMu        sync.Mutex
	clientLastSeen     = map[string]time.Time{}
	pendingBackup      = map[string]bool{}
	scheduledTriggered = map[string]string{} // clientID -> date string (e.g. "2026-09-28")
	slot               backupSlot            // the single active backup slot
)

// staleBackupTimeout is how long we wait before declaring a backup stale.
const staleBackupTimeout = 5 * time.Minute

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

	schedTime, err := time.Parse("3:04 pm", strings.TrimSpace(scheduleTime))
	if err != nil {
		slog.Debug("failed to parse schedule_time", slog.String("client_id", clientID), slog.String("value", scheduleTime))
		return false
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	// Build today's scheduled start time
	schedToday := time.Date(now.Year(), now.Month(), now.Day(), schedTime.Hour(), schedTime.Minute(), 0, 0, now.Location())

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
	if slot.ClientID != "" {
		return false
	}
	slot = backupSlot{ClientID: clientID, StartedAt: time.Now(), LastSeen: time.Now()}
	// Mark as triggered immediately so a failed "running" report
	// cannot cause the trigger to fire again on the next heartbeat.
	scheduledTriggered[clientID] = today
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
	_, seenBefore := clientLastSeen[clientID]
	clientLastSeen[clientID] = time.Now()
	pending := pendingBackup[clientID]

	// If this client is the one holding the slot, update LastSeen
	if slot.ClientID == clientID {
		slot.LastSeen = time.Now()
	}
	heartbeatMu.Unlock()

	// Only trigger the scheduled backup on subsequent heartbeats, not the first.
	// Initial connections always respond "ok" so the client can establish
	// itself before any backup is requested.
	if !pending && seenBefore && isItTimeToBackup(clientID) {
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
// Returns 409 if a different client is currently holding the backup slot.
func serveClientBackup(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if !clientExists(clientID) {
		http.Error(w, "unknown client: "+clientID, http.StatusNotFound)
		return
	}

	heartbeatMu.Lock()
	// Reject if a different client is actively backing up
	if slot.ClientID != "" && slot.ClientID != clientID {
		heartbeatMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error":     "another backup is in progress",
			"client_id": slot.ClientID,
		})
		return
	}
	pendingBackup[clientID] = true
	// Acquire the backup slot so the client's running/completed/failed
	// reports are processed and pendingBackup is cleared (mirrors the
	// scheduled-trigger path in isItTimeToBackup).
	now := time.Now()
	slot = backupSlot{ClientID: clientID, StartedAt: now, LastSeen: now}
	heartbeatMu.Unlock()

	slog.Info("manual backup queued", slog.String("client_id", clientID))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "queued", "client_id": clientID})
}

// serveBackupStatus handles explicit backup state reports from the client:
// POST /clients/{id}/backup/status
// Body: {"status": "running"|"completed"|"failed", "job": "optional job name"}
func serveBackupStatus(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if !clientExists(clientID) {
		http.Error(w, "unknown client: "+clientID, http.StatusNotFound)
		return
	}

	var req struct {
		Status string `json:"status"`
		Job    string `json:"job"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	heartbeatMu.Lock()
	switch req.Status {
	case "running":
		// Confirm the slot is held by this client and update LastSeen
		if slot.ClientID == clientID {
			now := time.Now()
			slot.LastSeen = now
			// Keep the client "online" in the dashboard while it is
			// actively backing up (heartbeats are paused during a backup).
			clientLastSeen[clientID] = now
			if req.Job != "" {
				slot.Job = req.Job
			}
			pendingBackup[clientID] = false
			scheduledTriggered[clientID] = now.Format("2006-01-02")
		} else {
			// Client reports running but doesn't hold the slot — ignore
			heartbeatMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"status": "ignored", "reason": "not holding backup slot"})
			return
		}
	case "completed":
		if slot.ClientID == clientID {
			slog.Info("backup completed", slog.String("client_id", clientID), slog.String("job", req.Job))
			slot = backupSlot{}
			// Safety net: clear pending in case the "running" report was lost
			pendingBackup[clientID] = false
		}
	case "failed":
		if slot.ClientID == clientID {
			slog.Warn("backup failed", slog.String("client_id", clientID), slog.String("job", req.Job))
			slot = backupSlot{}
			// Safety net: clear pending in case the "running" report was lost
			pendingBackup[clientID] = false
		}
	default:
		heartbeatMu.Unlock()
		http.Error(w, "unknown status: "+req.Status, http.StatusBadRequest)
		return
	}
	heartbeatMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ack", "client_id": clientID})
}

// startBackupTimeoutMonitor runs a goroutine that checks every 30 seconds
// whether the active backup slot is stale (no heartbeat for > 5 minutes).
// If stale, it releases the slot and logs a warning.
func startBackupTimeoutMonitor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		heartbeatMu.Lock()
		if slot.ClientID != "" && time.Since(slot.LastSeen) > staleBackupTimeout {
			slog.Warn("backup slot stale, releasing",
				slog.String("client_id", slot.ClientID),
				slog.String("last_seen", slot.LastSeen.Format(time.RFC3339)),
			)
			// Clear pending so a stale slot cannot cause the client to
			// immediately re-trigger a backup on its next heartbeat.
			pendingBackup[slot.ClientID] = false
			slot = backupSlot{}
		}
		heartbeatMu.Unlock()
	}
}

// clientView is the JSON representation of one client's live state.
type clientView struct {
	ClientID     string `json:"client_id"`
	Status       string `json:"status"`
	LastSeen     int64  `json:"last_seen,omitempty"`
	ActiveJob    string `json:"active_job,omitempty"`
	InBackupSlot bool   `json:"in_backup_slot,omitempty"`
	ScheduleTime string `json:"schedule_time,omitempty"`
}

// onlineThreshold is how recent a heartbeat must be for a client to be
// considered online. Clients that pause heartbeats during a backup are
// kept "online" while they hold the backup slot.
const onlineThreshold = 60 * time.Second

// clientStatus computes the live status for a client from its last-seen
// time and whether it currently holds the backup slot.
func clientStatus(last time.Time, seen bool, inSlot bool) string {
	if inSlot {
		return "online"
	}
	if !seen {
		return "never seen"
	}
	if time.Since(last) <= onlineThreshold {
		return "online"
	}
	return "offline"
}

// serveClients lists the server's live view of all configured clients,
// merged with their live heartbeat/slot state: GET /clients
func serveClients(w http.ResponseWriter, r *http.Request) {
	cfgStore.RLock()
	configured := make([]string, 0, len(cfgStore.cfg.Clients))
	schedule := make(map[string]string, len(cfgStore.cfg.Clients))
	for _, c := range cfgStore.cfg.Clients {
		configured = append(configured, c.Name)
		schedule[c.Name] = c.ScheduleTime
	}
	cfgStore.RUnlock()

	heartbeatMu.Lock()
	views := make([]clientView, 0, len(configured))
	for _, id := range configured {
		last, seen := clientLastSeen[id]
		inSlot := slot.ClientID == id
		v := clientView{
			ClientID:     id,
			Status:       clientStatus(last, seen, inSlot),
			ScheduleTime: schedule[id],
		}
		if seen {
			v.LastSeen = last.Unix()
		}
		if inSlot {
			v.InBackupSlot = true
			v.ActiveJob = slot.Job
		}
		views = append(views, v)
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
	inSlot := slot.ClientID == clientID
	activeJob := slot.Job
	heartbeatMu.Unlock()

	view := clientView{ClientID: clientID, Status: clientStatus(last, seen, inSlot)}
	if seen {
		view.LastSeen = last.Unix()
	}
	if inSlot {
		view.InBackupSlot = true
		view.ActiveJob = activeJob
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view)
}

// pruneScheduledTriggered removes entries from scheduledTriggered that are
// older than today. Called periodically to prevent unbounded growth.
func pruneScheduledTriggered() {
	today := time.Now().Format("2006-01-02")
	heartbeatMu.Lock()
	for id, date := range scheduledTriggered {
		if date != today {
			delete(scheduledTriggered, id)
		}
	}
	heartbeatMu.Unlock()
}

// startMaintenanceLoop runs periodic maintenance tasks (pruning, etc.)
func startMaintenanceLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		pruneScheduledTriggered()
	}
}
