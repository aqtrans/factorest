package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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

// parseScheduleTime parses a time string like "4:40 pm", "10:30 am", "12:00 pm".
// Returns the hour and minute in 24-hour format.
func parseScheduleTime(s string) (hour, minute int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}

	// Split on space to separate time and am/pm
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, 0, false
	}

	timePart := parts[0]
	meridiem := strings.ToLower(parts[1])
	if meridiem != "am" && meridiem != "pm" {
		return 0, 0, false
	}

	// Split time part on colon
	colonParts := strings.Split(timePart, ":")
	if len(colonParts) != 2 {
		return 0, 0, false
	}

	h, err := strconv.Atoi(colonParts[0])
	if err != nil || h < 1 || h > 12 {
		return 0, 0, false
	}
	m, err := strconv.Atoi(colonParts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, false
	}

	// Convert to 24-hour
	if meridiem == "pm" && h != 12 {
		h += 12
	}
	if meridiem == "am" && h == 12 {
		h = 0
	}

	return h, m, true
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

	hour, minute, ok := parseScheduleTime(scheduleTime)
	if !ok {
		slog.Debug("failed to parse schedule_time", slog.String("client_id", clientID), slog.String("value", scheduleTime))
		return false
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	// Build today's scheduled start time
	schedToday := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())

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
	scheduledTriggered[clientID] = today
	slot = backupSlot{ClientID: clientID, StartedAt: time.Now(), LastSeen: time.Now()}
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
	pendingBackup[clientID] = false

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
			slot.LastSeen = time.Now()
			if req.Job != "" {
				slot.Job = req.Job
			}
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
		}
	case "failed":
		if slot.ClientID == clientID {
			slog.Warn("backup failed", slog.String("client_id", clientID), slog.String("job", req.Job))
			slot = backupSlot{}
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
}

// serveClients lists the server's live view of all clients: GET /clients
func serveClients(w http.ResponseWriter, r *http.Request) {
	heartbeatMu.Lock()
	views := make([]clientView, 0, len(clientLastSeen))
	for id, last := range clientLastSeen {
		v := clientView{ClientID: id, Status: "online", LastSeen: last.Unix()}
		if slot.ClientID == id {
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

	view := clientView{ClientID: clientID, Status: "never seen"}
	if seen {
		view.Status = "online"
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

// Ensure fmt is used (for potential future use in error messages)
var _ = fmt.Sprintf
