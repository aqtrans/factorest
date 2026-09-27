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
	heartbeatMu    sync.Mutex
	clientLastSeen = map[string]time.Time{}
	pendingBackup  = map[string]bool{}
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

// serveClientStatus is the heartbeat endpoint: POST /clients/{id}/status
// It records the client's last seen time and responds with status "ok",
// or "backup" if a manual backup is pending for the client.
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
	heartbeatMu.Unlock()

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
