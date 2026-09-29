package main

import (
	"bytes"
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
	"sync"
	"syscall"
	"time"
)

// httpClient is a shared HTTP client with a reasonable timeout.
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
}

// runResticBackup executes a restic backup with the given job configuration
func runResticBackup(ctx context.Context, g types.Config, j types.JobConfig) error {
	// Set a hard timeout for the backup window
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
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
	// Dry run mode: include --dry-run flag
	if g.DryRun {
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

	slog.Info("job successful", "job", j.Name)

	return nil
}

// runHooks executes a hook command with a descriptive label for logging.
// extraEnv are additional KEY=VALUE environment variables available to the hook.
func runHooks(ctx context.Context, label, c string, extraEnv ...string) error {
	// Set a hard timeout for the hook execution
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()

	slog.Info("Executing " + label + ": " + c)
	cmd := exec.CommandContext(ctx, "sh", "-c", c)
	cmd.Env = append(os.Environ(), extraEnv...)

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

	slog.Info("hook completed successfully", "label", label)

	return nil
}

// runJobs executes all assigned jobs with their hooks.
// Returns the name of the failed job if any job fails, or "" on success.
func runJobs(ctx context.Context, cfg types.Config) (failedJob string, err error) {
	clientID := cfg.Client.Name
	jobCount := len(cfg.Jobs)

	// Base env vars available to all client-level hooks
	baseEnv := []string{
		"CLIENT_ID=" + clientID,
		"JOB_COUNT=" + fmt.Sprintf("%d", jobCount),
	}

	// Client-level pre-hooks: run once before all jobs start
	for _, hook := range cfg.Client.PreHook {
		if err := runHooks(ctx, "client pre-hook", hook, baseEnv...); err != nil {
			slog.Error("error running client pre-hook", "hook", hook, slog.Any("err", err))
			return "", err
		}
	}

	for _, job := range cfg.Jobs {
		var err error

		if len(job.PreHook) > 0 {
			for _, preHook := range job.PreHook {
				err = runHooks(ctx, "job pre-hook ("+job.Name+")", preHook, baseEnv...)
				if err != nil {
					slog.Error("error running preHook", "job", job.Name, "hook", preHook, slog.Any("err", err))
					return job.Name, err
				}
			}
		}

		err = runResticBackup(ctx, cfg, job)
		if err != nil {
			slog.Error("error running backup", "job", job.Name, slog.Any("err", err))
			return job.Name, err
		}

		if len(job.PostHook) > 0 {
			for _, postHook := range job.PostHook {
				err = runHooks(ctx, "job post-hook ("+job.Name+")", postHook, baseEnv...)
				if err != nil {
					slog.Error("error running postHook", "job", job.Name, slog.Any("err", err))
					return job.Name, err
				}
			}
		}
	}

	// Client-level post-hooks: run once after all jobs succeed
	for _, hook := range cfg.Client.PostHook {
		if err := runHooks(ctx, "client post-hook", hook, baseEnv...); err != nil {
			slog.Error("error running client post-hook", "hook", hook, slog.Any("err", err))
			return "", err
		}
	}

	return "", nil
}

// getConfig fetches job config from the server.
func getConfig(serverAddress, clientID string) (types.Config, error) {
	config := types.Config{}

	resp, err := httpClient.Get(fmt.Sprintf("%s/config.json?client_id=%s", serverAddress, clientID))
	if err != nil {
		return config, fmt.Errorf("failed to fetch config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return config, fmt.Errorf("config error: %s - %s", resp.Status, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return config, fmt.Errorf("failed to decode config: %w", err)
	}

	slog.Info("fetched config", slog.String("client_id", clientID), slog.Int("jobs", len(config.Jobs)))
	return config, nil
}

// sendHeartbeat POSTs a heartbeat to the server and returns the status
// from the response. The server responds with status "backup" when a
// backup should be started; "ok" otherwise.
func sendHeartbeat(serverAddress, clientID string) string {
	resp, err := httpClient.Post(serverAddress+"/clients/"+clientID+"/status", "application/json", nil)
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

// reportBackupStatus sends an explicit backup state report to the server.
// status is "running", "completed", or "failed". job is the job name (optional).
func reportBackupStatus(serverAddress, clientID, status, job string) {
	body, _ := json.Marshal(map[string]string{
		"status": status,
		"job":    job,
	})
	resp, err := httpClient.Post(serverAddress+"/clients/"+clientID+"/backup/status", "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to report backup status", slog.String("status", status), slog.Any("err", err))
		return
	}
	defer resp.Body.Close()
	slog.Debug("backup status reported", slog.String("status", status), slog.String("job", job))
	// If job is blank, this should be a client backup, so log as info
	if job == "" {
		slog.Info("client backup", "status", status)
	}
}

func main() {

	clientID := flag.String("client-id", "", "the client name to present to the server")
	serverAddress := flag.String("server", "http://localhost:8080", "address of the server")
	heartbeatSeconds := flag.Int("heartbeat", 10, "seconds between heartbeats")

	flag.Parse()

	// Enable debug logging:
	slog.SetLogLoggerLevel(slog.LevelDebug)

	// Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	// busyMu guards the busy flag to prevent data races between
	// the main loop and the backup goroutine.
	var busyMu sync.Mutex
	busy := false

	for {
		select {
		case <-done:
			slog.Info("Shutting down.")
			cancel()
			return
		case <-ticker.C:
			busyMu.Lock()
			isBusy := busy
			busyMu.Unlock()

			if status := sendHeartbeat(*serverAddress, *clientID); status == "backup" && !isBusy {
				busyMu.Lock()
				busy = true
				busyMu.Unlock()

				// Fetch config from the server
				cfg, err := getConfig(*serverAddress, *clientID)
				if err != nil {
					slog.Error("error getting config from server", slog.String("server", *serverAddress), slog.Any("err", err))
					busyMu.Lock()
					busy = false
					busyMu.Unlock()
					continue
				}

				go func() {
					defer func() {
						busyMu.Lock()
						busy = false
						busyMu.Unlock()
					}()

					// Report that we're starting
					reportBackupStatus(*serverAddress, *clientID, "running", "")

					failedJob, err := runJobs(ctx, cfg)
					if err != nil {
						slog.Error("backup run failed", slog.Any("err", err))
						reportBackupStatus(*serverAddress, *clientID, "failed", failedJob)

						// Client-level failure hooks
						failEnv := []string{
							"CLIENT_ID=" + cfg.Client.Name,
							"JOB_COUNT=" + fmt.Sprintf("%d", len(cfg.Jobs)),
							"FAILED_JOBS=" + failedJob,
						}
						for _, hook := range cfg.Client.OnFailure {
							if hookErr := runHooks(ctx, "client onFailure-hook", hook, failEnv...); hookErr != nil {
								slog.Error("error running onFailure hook", slog.Any("err", hookErr))
							}
						}
					} else {
						reportBackupStatus(*serverAddress, *clientID, "completed", "")
					}
				}()
			}
		}
	}
}
