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

func main() {

	// Allow overriding config path
	config := flag.String("config", "config.toml", "the path to the TOML configuration file")
	flag.Parse()

	configPath = *config

	// Load initial config
	var cfgStore configStore
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

	err = watcher.Add("config.toml")
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

	http.HandleFunc("/health", serveHealth)

	// Fetch all jobs assigned to this client
	http.HandleFunc("/config.json", serveConfigJSON)

	// Manual reload endpoint (for testing or CLI tools)
	http.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		serveReload(w, r)
	})

	fmt.Printf("Endpoints:\n  GET  /health             - health check\n  GET  /config.json        - fetch backup config (TOML)\n")

	err = http.ListenAndServe("localhost:"+cfgStore.cfg.ListenPort, nil)
	if err != nil {
		slog.Error("HTTP listen error", slog.Any("http", err))
		os.Exit(1)
	}
}
