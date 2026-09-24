package main

import (
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/fsnotify/fsnotify"
)

func main() {
	// Load initial config
	cfg, err := LoadConfig()
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
				if event.Has(fsnotify.Write) {
					log.Printf("Modified file: %s", event.Name)
					cfg, err = LoadConfig()
					if err != nil {
						slog.Error("error loading config", slog.Any("cfg", err))
						os.Exit(1)
					}
				}
				if event.Has(fsnotify.Create) {
					log.Printf("Created file: %s", event.Name)
					cfg, err = LoadConfig()
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

	err = watcher.Add("config.toml")
	if err != nil {
		slog.Error("Error watching", slog.Any("err", err))
		os.Exit(1)
	}

	// Enable debug logging:
	slog.SetLogLoggerLevel(slog.LevelDebug)

	slog.Debug("config loaded", slog.Any("cfg", cfg))
	slog.Info("Password file:", slog.String("pwfile", cfg.PasswordFile))
	slog.Info("Repository:", slog.String("repo", cfg.Repository))
	slog.Info("Debug mode:", slog.Bool("debug", cfg.Debug))
	slog.Info("Jobs loaded:", slog.Int("count", len(cfg.Jobs)))

	slog.Info("Server starting on port localhost:" + cfg.ListenPort)

	http.HandleFunc("/health", ServeHealth)

	// Fetch all jobs assigned to this client
	http.HandleFunc("/config.json", ServeConfigJSON)

	// Manual reload endpoint (for testing or CLI tools)
	http.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		ServeReload(w, r)
	})

	fmt.Printf("Endpoints:\n  GET  /health             - health check\n  GET  /config.json        - fetch backup config (TOML)\n")

	err = http.ListenAndServe("localhost:"+cfg.ListenPort, nil)
	if err != nil {
		slog.Error("HTTP listen error", slog.Any("http", err))
		os.Exit(1)
	}
}
