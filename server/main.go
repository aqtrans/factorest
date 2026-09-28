package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/fsnotify/fsnotify"
)

var configPath = "config.toml"

// Package-level so heartbeat handlers can read the active config.
var cfgStore configStore

func main() {

	// Allow overriding config path
	config := flag.String("config", "config.toml", "the path to the TOML configuration file")
	flag.Parse()

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
				log.Printf("Event received: %s", event)

				// Evaluate specific operation types
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					slog.Info("config file modified")
					err = cfgStore.Get()
					if err != nil {
						slog.Error("error loading config", slog.Any("cfg", err))
						os.Exit(1)
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("Error:", slog.Any("cfg", err))
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

	// slog.Debug("config loaded", slog.Any("cfg", cfg))
	slog.Debug("Password file:", slog.String("pwfile", cfgStore.cfg.PasswordFile))
	slog.Debug("Repository:", slog.String("repo", cfgStore.cfg.Repository))
	slog.Debug("Debug mode:", slog.Bool("debug", cfgStore.cfg.Debug))
	slog.Debug("Jobs loaded:", slog.Int("count", len(cfgStore.cfg.Jobs)))

	slog.Info("Server starting on port localhost:" + cfgStore.cfg.ListenPort)

	mux := http.NewServeMux()

	// Fetch all jobs assigned to this client
	mux.HandleFunc("GET /health", serveHealth)
	mux.HandleFunc("GET /config.json", serveConfigJSON)
	mux.HandleFunc("POST /reload", serveReload)

	// Heartbeat and client state endpoints
	mux.HandleFunc("POST /clients/{id}/status", serveClientStatus)
	mux.HandleFunc("POST /clients/{id}/backup", serveClientBackup)
	mux.HandleFunc("GET /clients", serveClients)
	mux.HandleFunc("GET /clients/{id}", serveClient)

	fmt.Printf("Endpoints:\n  GET  /health             - health check\n  GET  /config.json        - fetch backup config (JSON)\n  POST /clients/{id}/status - client heartbeat\n  POST /clients/{id}/backup - manual backup trigger\n  GET  /clients            - live client list\n  GET  /clients/{id}       - single client view\n")

	err = http.ListenAndServe("localhost:"+cfgStore.cfg.ListenPort, mux)
	if err != nil {
		slog.Error("HTTP listen error", slog.Any("http", err))
		os.Exit(1)
	}
}
