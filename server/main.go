package main

import (
	"crypto/subtle"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/fsnotify/fsnotify"
)

var configPath = "config.toml"

// Package-level so heartbeat handlers can read the active config.
var cfgStore configStore

func main() {

	// Allow overriding config path
	config := flag.String("config", "config.toml", "the path to the TOML configuration file")
	versionFlag := flag.Bool("version", false, "server version")
	flag.Parse()

	if *versionFlag {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			slog.Info("Version", "info", info.Main.Version)
		}
		return
	}

	configPath = *config

	// Load initial config
	err := cfgStore.Get()
	if err != nil {
		slog.Error("error loading config", slog.Any("cfg", err))
		os.Exit(1)
	}

	// Create a new file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
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
				slog.Debug("fsnotify event", slog.Any("event", event))

				// Evaluate specific operation types
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					slog.Info("config file modified")
					err = cfgStore.Get()
					if err != nil {
						// Log the error but keep the last known good config.
						// Do NOT exit — a transient file issue (editor save,
						// partial write) should not kill the server.
						slog.Error("error reloading config, keeping last known good config", slog.Any("err", err))
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("fsnotify error", slog.Any("err", err))
			}
		}
	}()

	err = watcher.Add(*config)
	if err != nil {
		slog.Error("Error watching config file", slog.Any("err", err))
		os.Exit(1)
	}

	// Enable debug logging:
	slog.SetLogLoggerLevel(slog.LevelDebug)

	slog.Debug("Password file:", slog.String("pwfile", cfgStore.cfg.PasswordFile))
	slog.Debug("Repository:", slog.String("repo", cfgStore.cfg.Repository))
	slog.Debug("Debug mode:", slog.Bool("debug", cfgStore.cfg.Debug))
	slog.Debug("Jobs loaded:", slog.Int("count", len(cfgStore.cfg.Jobs)))

	slog.Info("Server starting on " + cfgStore.cfg.ListenAddress)

	// Start background maintenance goroutines
	go startBackupTimeoutMonitor()
	go startMaintenanceLoop()

	mux := http.NewServeMux()

	// Web UI (exempt from auth — see requireAuth)
	mux.HandleFunc("GET /", serveUI)
	mux.HandleFunc("GET /ui", serveUI)

	// Core endpoints
	mux.HandleFunc("GET /health", serveHealth)
	mux.HandleFunc("GET /config.json", serveConfigJSON)
	mux.HandleFunc("GET /jobs", serveJobs)
	mux.HandleFunc("POST /reload", serveReload)

	// Heartbeat and client state endpoints
	mux.HandleFunc("POST /clients/{id}/status", serveClientStatus)
	mux.HandleFunc("POST /clients/{id}/backup", serveClientBackup)
	mux.HandleFunc("POST /clients/{id}/backup/status", serveBackupStatus)
	mux.HandleFunc("GET /clients", serveClients)
	mux.HandleFunc("GET /clients/{id}", serveClient)

	slog.Info("Endpoints:",
		slog.String("endpoints", "GET /, GET /ui, GET /health, GET /config.json, GET /jobs, POST /reload, POST /clients/{id}/status, POST /clients/{id}/backup, POST /clients/{id}/backup/status, GET /clients, GET /clients/{id}"))

	srv := &http.Server{
		Addr:         cfgStore.cfg.ListenAddress,
		Handler:      requireAuth(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	err = srv.ListenAndServe()
	if err != nil {
		slog.Error("HTTP listen error", slog.Any("http", err))
		os.Exit(1)
	}
}

// requireAuth wraps an http.Handler with token authentication.
// If the configured AuthToken is empty, authentication is disabled and
// all requests pass through. Otherwise, every request must include
// an X-Auth-Token header matching the configured token.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The web UI shell is public: it contains no secrets, and the
		// browser supplies the token for its data fetches.
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/ui") {
			next.ServeHTTP(w, r)
			return
		}

		cfgStore.RLock()
		token := cfgStore.cfg.AuthToken
		cfgStore.RUnlock()

		if token == "" {
			// Auth disabled — pass through
			next.ServeHTTP(w, r)
			return
		}

		provided := r.Header.Get("X-Auth-Token")
		// Constant-time comparison to prevent timing attacks
		match := subtle.ConstantTimeCompare([]byte(provided), []byte(token))
		if match != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
