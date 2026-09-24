package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// Config holds all configuration loaded from config.toml
// Struct field names must be exported (capitalized) for TOML unmarshaling to work
type Config struct {
	PasswordFile string         `toml:"pwfile"`
	Repository   string         `toml:"repo"`
	Debug        bool           `toml:"debug"`
	Jobs         []JobConfig    `toml:"jobs"`
	Clients      []ClientConfig `toml:"clients"`
}

// JobCfg represents a backup job configuration
type JobConfig struct {
	Name       string   `toml:"name"`
	Tag        []string `toml:"tag"`
	Files      []string `toml:"files"`
	PreHook    []string `toml:"preHook"`
	PostHook   []string `toml:"postHook"`
	AssignedTo []string `toml:"assigned_to"`
}

type ClientConfig struct {
	Name                  string `toml:"name"`
	ScheduleStartHour     int    `toml:"schedule_start_hour"`     // HH:00 UTC when client starts
	ScheduleOffsetMinutes int    `toml:"schedule_offset_minutes"` // Delay after start hour
	PreferredHourRange    []int  `toml:"preferred_hour_range"`    // Preferred window [start, end]
	MaxConcurrentJobs     int    `toml:"max_concurrent_jobs"`     // Max parallel jobs for this client
	TimeoutSeconds        int    `toml:"timeout_seconds"`         // Override global timeout
}

// runResticBackup executes a restic backup with the given job configuration
func runResticBackup(g Config, j JobConfig) error {
	// Set a hard timeout for the backup window
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// Grab hostname for the default tag
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	// Create a list of "--tag" flags to throw to Restic
	tagWithFlags := make([]string, len(j.Tag))
	for _, tag := range j.Tag {
		tagWithFlags = append([]string{"--tag", tag})
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

// runHooks executes a pre/post hook command
func runHooks(c string) error {
	// Set a hard timeout for the hook execution
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// Target the restic binary with args.
	// Separating keys and flags protects against command injection vulnerabilities.
	slog.Info("Executing prehook: " + c)
	cmd := exec.CommandContext(ctx, "sh", "-c", c)

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

	slog.Info("Hook completed successfully!")

	return nil
}

func runJobs(cfg Config) error {
	for _, job := range cfg.Jobs {
		var err error

		if len(job.PreHook) > 0 {
			for _, preHook := range job.PreHook {
				err = runHooks(preHook)
				if err != nil {
					slog.Error("Error running preHook", slog.Any("err", err))
					return err
				}
			}
		}

		err = runResticBackup(cfg, job)
		if err != nil {
			slog.Error("Error running backup", slog.Any("err", err))
			return err
		}

		if len(job.PostHook) > 0 {
			for _, postHook := range job.PostHook {
				err = runHooks(postHook)
				if err != nil {
					slog.Error("Error running postHook", slog.Any("err", err))
					return err
				}
			}
		}
	}
	return nil
}

func GetConfig(serverAddress, clientID string) (Config, error) {

	// Fetch job config from server - all values come from server response
	config := Config{}

	resp, err := http.Get(fmt.Sprintf("%s/config.json?client_id=%s", serverAddress, clientID))
	if err != nil {
		return config, fmt.Errorf("failed to fetch config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return config, fmt.Errorf("config error: %s - %s", resp.Status, string(body))
	}

	log.Println(resp.Body)

	json.NewDecoder(resp.Body).Decode(&config)

	log.Println(config)

	fmt.Printf("Client [%s] fetched job: %s\n", clientID, config.Jobs)
	return config, nil
}

func main() {

	clientID := flag.String("client-id", "", "the client name to present to the server")
	serverAddress := flag.String("server", "http://localhost:8080", "address of the server")

	flag.Parse()

	cfg, err := GetConfig(*serverAddress, *clientID)
	if err != nil {
		slog.Error("error getting config", slog.Any("err", err))
		os.Exit(1)
	}

	// Enable debug logging:
	slog.SetLogLoggerLevel(slog.LevelDebug)

	slog.Debug("config loaded", slog.Any("cfg", cfg))
	slog.Info("Password file:", slog.String("pwfile", cfg.PasswordFile))
	slog.Info("Repository:", slog.String("repo", cfg.Repository))
	slog.Info("Debug mode:", slog.Bool("debug", cfg.Debug))
	slog.Info("Jobs loaded:", slog.Int("count", len(cfg.Jobs)))

	slog.Debug("Jobs config:", slog.Any("jobs", cfg.Jobs))

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	fmt.Println("Client running. Connecting to " + *serverAddress)
	<-done // Will block here until user hits ctrl+c
	slog.Info("Shutting down.")

}
