package main

import (
	"context"
	"encoding/json"
	"factorest/types"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// runResticBackup executes a restic backup with the given job configuration
func runResticBackup(g types.Config, j types.JobConfig) error {
	// Set a hard timeout for the backup window
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// Grab hostname for the default tag
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	// Create a list of "--tag" flags to throw to Restic
	tagWithFlags := make([]string, 0, len(j.Tag)*2)
	for _, tag := range j.Tag {
		tagWithFlags = append(tagWithFlags, "--tag", tag)
	}

	// build the full command, should just be tags plus dirs
	var backupCmd []string
	// Debug mode: include --dry-run flag
	if g.Debug {
		backupCmd = append([]string{"backup", "--dry-run", "--tag", hostname}, tagWithFlags...)
	} else {
		backupCmd = append([]string{"backup", "--tag", hostname}, tagWithFlags...)
	}

	// Add directories to the backup command
	fullCmd := append(backupCmd, j.Files...)

	// Target the restic binary with args.
	// Separating keys and flags protects against command injection vulnerabilities.
	cmd := exec.CommandContext(ctx, "restic", fullCmd...)

	// Restic requires the repository password. Provide it via environment variables.
	cmd.Env = append(os.Environ(),
		"RESTIC_REPOSITORY="+g.Repository,
		"RESTIC_PASSWORD_FILE="+g.PasswordFile,
	)

	slog.Debug(cmd.String())

	// Stream restic's real-time progress straight to your terminal's stdout/stderr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run starts the process and blocks until it completes
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("restic backup timed out")
		}
		return fmt.Errorf("restic backup failed: %w", err)
	}

	slog.Info("Backup completed successfully!")

	return nil
}

// runHooks executes a hook command with a descriptive label for logging
func runHooks(label, c string) error {
	// Set a hard timeout for the hook execution
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	slog.Info("Executing " + label + ": " + c)
	cmd := exec.CommandContext(ctx, "sh", "-c", c)

	slog.Debug(cmd.String())

	// Stream output straight to your terminal's stdout/stderr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run starts the process and blocks until it completes
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("hook timed out: %s", label)
		}
		return fmt.Errorf("hook failed (%s): %w", label, err)
	}

	slog.Info("Hook completed successfully: " + label)

	return nil
}

func runJobs(cfg types.Config) error {
	// Client-level pre-hooks: run once before all jobs start
	for _, hook := range cfg.Client.PreHook {
		if err := runHooks("client pre-hook", hook); err != nil {
			slog.Error("Error running client pre-hook", slog.Any("err", err))
			return err
		}
	}

	failed := false
	for _, job := range cfg.Jobs {
		var err error

		if len(job.PreHook) > 0 {
			for _, preHook := range job.PreHook {
				err = runHooks("job pre-hook ("+job.Name+")", preHook)
				if err != nil {
					slog.Error("Error running preHook", slog.Any("err", err))
					failed = true
					break
				}
			}
		}
		if failed {
			break
		}

		err = runResticBackup(cfg, job)
		if err != nil {
			slog.Error("Error running backup", slog.Any("err", err))
			failed = true
			break
		}

		if len(job.PostHook) > 0 {
			for _, postHook := range job.PostHook {
				err = runHooks("job post-hook ("+job.Name+")", postHook)
				if err != nil {
					slog.Error("Error running postHook", slog.Any("err", err))
					failed = true
					break
				}
			}
		}
		if failed {
			break
		}
	}

	if failed {
		// Client-level failure hooks: notify that the run failed
		for _, hook := range cfg.Client.OnFailure {
			if err := runHooks("client onFailure-hook", hook); err != nil {
				slog.Error("Error running onFailure hook", slog.Any("err", err))
			}
		}
		return fmt.Errorf("backup run failed")
	}

	// Client-level post-hooks: run once after all jobs succeed
	for _, hook := range cfg.Client.PostHook {
		if err := runHooks("client post-hook", hook); err != nil {
			slog.Error("Error running client post-hook", slog.Any("err", err))
			return err
		}
	}

	return nil
}

func getConfig(serverAddress, clientID string) (types.Config, error) {

	// Fetch job config from server - all values come from server response
	config := types.Config{}

	resp, err := http.Get(fmt.Sprintf("%s/config.json?client_id=%s", serverAddress, clientID))
	if err != nil {
		return config, fmt.Errorf("failed to fetch config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return config, fmt.Errorf("config error: %s - %s", resp.Status, string(body))
	}

	json.NewDecoder(resp.Body).Decode(&config)

	fmt.Printf("Client [%s] fetched job: %s\n", clientID, config.Jobs)
	return config, nil
}

// sendHeartbeat POSTs a heartbeat to the server and returns the status
// from the response. The server responds with status "backup" when a
// backup should be started; "ok" otherwise.
func sendHeartbeat(serverAddress, clientID string) string {
	resp, err := http.Post(serverAddress+"/clients/"+clientID+"/status", "application/json", nil)
	if err != nil {
		slog.Error("heartbeat failed", slog.Any("err", err))
		return "error"
	}
	defer resp.Body.Close()

	var heartbeat struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&heartbeat); err != nil {
		slog.Error("failed to decode heartbeat response", slog.Any("err", err))
		return "error"
	}
	slog.Debug("heartbeat", slog.String("status", heartbeat.Status))
	return heartbeat.Status
}

func main() {

	clientID := flag.String("client-id", "", "the client name to present to the server")
	serverAddress := flag.String("server", "http://localhost:8080", "address of the server")
	heartbeatSeconds := flag.Int("heartbeat", 10, "seconds between heartbeats")

	flag.Parse()

	/* 	cfg, err := getConfig(*serverAddress, *clientID)
	   	if err != nil {
	   		slog.Error("error getting config", slog.Any("err", err))
	   		os.Exit(1)
	   	}
	*/

	// Enable debug logging:
	slog.SetLogLoggerLevel(slog.LevelDebug)

	// slog.Debug("config loaded", slog.Any("cfg", cfg))
	// slog.Debug("Password file:", slog.String("pwfile", cfg.PasswordFile))
	// slog.Debug("Repository:", slog.String("repo", cfg.Repository))
	// slog.Debug("Debug mode:", slog.Bool("debug", cfg.Debug))
	// slog.Debug("Jobs loaded:", slog.Int("count", len(cfg.Jobs)))

	// slog.Debug("Jobs config:", slog.Any("jobs", cfg.Jobs))

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	// Heartbeat interval, configurable via command-line, default 10s.
	interval := 10 * time.Second
	if *heartbeatSeconds > 0 {
		interval = time.Duration(*heartbeatSeconds) * time.Second
	}

	// Send initial heartbeat to check connection
	status := sendHeartbeat(*serverAddress, *clientID)
	if status == "error" {
		slog.Error("error sending heartbeat to server", slog.String("server", *serverAddress))
		os.Exit(1)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// busy tracks whether a backup run is in progress, so triggers while
	// jobs are running don't start overlapping runs.
	busy := false

	for {
		select {
		case <-done:
			slog.Info("Shutting down.")
			return
		case <-ticker.C:
			if status := sendHeartbeat(*serverAddress, *clientID); status == "backup" && !busy {
				busy = true
				// Fetch config from the server
				cfg, err := getConfig(*serverAddress, *clientID)
				if err != nil {
					slog.Error("error getting config from server", slog.String("server", *serverAddress), slog.Any("err", err))
					os.Exit(1)
				}
				go func() {
					defer func() { busy = false }()
					if err := runJobs(cfg); err != nil {
						slog.Error("backup run failed", slog.Any("err", err))
					}
				}()
			}
		}
	}
}
