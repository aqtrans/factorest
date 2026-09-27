package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

// parseSchedule parses a client's schedule_time (e.g. "4:41 pm") as a
// wall-clock time in the local timezone
func parseSchedule(value string) (time.Time, error) {
	return time.ParseInLocation("3:04 pm", value, time.Local)
}

// timeToBackup reports whether a client's daily backup window is open:
// its scheduled time has passed for the client's local day, and no trigger
// has already been issued for that day. The window stays open until the
// client's local day rolls over, so a client that was offline at its
// scheduled time picks the backup up on its next heartbeat.
func timeToBackup(clientID string) bool {
	cfg, err := GetClientConfig(clientID)
	if err != nil {
		slog.Error("failed to load config", "err", err, "client_id", clientID)
		return false
	}

	clientCfg, ok := GetClientEntry(cfg, clientID)
	if !ok || clientCfg.ScheduleTime == "" {
		slog.Debug("schedule time is empty", "client", clientID)
		return false
	}

	want, err := parseSchedule(clientCfg.ScheduleTime)
	if err != nil {
		slog.Error("bad schedule_time format",
			"client_id", clientID,
			"value", clientCfg.ScheduleTime)
		return false
	}

	now := time.Now().In(time.Local)
	return now.After(want) || now.Equal(want)
}

func main() {
	// Load initial config
	flag.StringVar(&configPath, "config", "config.toml", "path to the server config file")
	flag.Parse()

	if err := configStore.Init(configPath); err != nil {
		slog.Error("error loading config", slog.Any("err", err))
		os.Exit(1)
	}

	// Create a new file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("error creating file watcher", slog.Any("err", err))
		os.Exit(1)
	}
	defer watcher.Close()

	// Start a goroutine to listen for events out of the channel
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				slog.Debug("Event received", "event", event)

				// Evaluate specific operation types
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					if err := configStore.Reload(configPath); err != nil {
						// Keep the last known good config; the store is unchanged.
						slog.Error("failed to reload config", slog.Any("err", err))
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("Config watcher error:", slog.Any("err", err))
			}
		}
	}()

	err = watcher.Add(configPath)
	if err != nil {
		slog.Error("Error watching config file", slog.Any("err", err))
		os.Exit(1)
	}

	// Enable debug logging:
	cfg := configStore.Current()
	slog.Debug("Debug mode:", slog.Bool("debug", cfg.Debug))
	if cfg.Debug {
		slog.Info("debug mode enabled")
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	slog.Debug("Password file:", slog.String("pwfile", cfg.PasswordFile))
	slog.Debug("Repository:", slog.String("repo", cfg.Repository))
	slog.Debug("Jobs loaded:", slog.Int("count", len(cfg.Jobs)))
	slog.Info("Server starting on port localhost:" + cfg.ListenPort)

	http.HandleFunc("/health", ServeHealth)

	// Client heartbeat (POST) - reports client health to the server
	http.HandleFunc("/heartbeat", ServeHeartbeat)

	// Client job lifecycle reports (started / completed / failed)
	http.HandleFunc("/job-status", ServeJobStatus)

	// Periodically drop running state for clients that have gone silent
	go func() {
		t := time.NewTicker(30 * time.Second)
		for range t.C {
			heartbeatStore.sweepStale()
		}
	}()

	// List of clients seen so far with their last heartbeat
	http.HandleFunc("/heartbeats", ListHeartbeats)

	// Fetch all jobs assigned to this client
	http.HandleFunc("/config.json", ServeConfigJSON)

	// Manual reload endpoint (for testing or CLI tools)
	http.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		ServeReload(w, r)
	})

	fmt.Printf("Endpoints:\n  GET  /health             - health check\n  GET  /config.json        - fetch backup config (TOML)\n  POST /heartbeat          - client heartbeat\n  POST /job-status         - job lifecycle report (started/completed/failed)\n  GET  /heartbeats         - list client heartbeats\n")

	err = http.ListenAndServe("localhost:"+cfg.ListenPort, nil)
	if err != nil {
		slog.Error("HTTP listen error", slog.Any("http", err))
		os.Exit(1)
	}
}
