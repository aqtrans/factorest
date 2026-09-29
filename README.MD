# Factorest

Factorio-style automation for your Restic backups.

The server acts as the orchestrator. A server-side TOML configuration file is served via HTTP as JSON. Clients connect to the server using a unique client ID and fetch their filtered Restic configuration. Jobs are the backups to be performed; they can be assigned to multiple clients via `assigned_to`.

## Architecture

```
┌─────────────────────┐         ┌──────────────────────────────┐
│   Client Daemon     │         │       Server (Orchestrator)  │
│                     │         │                              │
│  - Heartbeat loop   │◄──────►│  - Heartbeat tracking        │
│  - Config fetch     │         │  - Backup slot management    │
│  - Restic backups   │         │  - Scheduled trigger logic   │
│  - Hook execution   │         │  - Config hot-reload (fsnotify)
│  - Status reports   │         │  - Auth (X-Auth-Token)       │
└─────────────────────┘         └──────────────────────────────┘
```

## How It Works

1. **Client daemon** — runs continuously, sending a heartbeat to the server every N seconds (configurable via `-heartbeat` flag, default 10s) with its unique client ID.
2. **Server validation** — the server validates the client ID against the configured `[[clients]]` list and records the last-seen timestamp.
3. **Scheduled trigger** — each client can define a `schedule_time` (e.g. `"4:40 pm"`). On the first heartbeat *after* that time (and only if no backup is already in progress), the server responds with `status: "backup"`.
4. **Manual trigger** — `POST /clients/{id}/backup` queues a backup for the client; it is consumed on the client's next heartbeat. Returns `409` if a different client is already holding the backup slot.
5. **Config fetch** — when the client receives `status: "backup"`, it fetches `GET /config.json?client_id={id}`. The server returns only the jobs assigned to that client (or public jobs), plus the client's own hook configuration and any client-level repository override.
6. **Backup execution** — the client runs its jobs one-by-one, executing pre-hooks, the `restic backup` command, and post-hooks. It reports `running` / `completed` / `failed` status to the server via `POST /clients/{id}/backup/status`.
7. **Backup slot** — only one client can hold the backup slot at a time. The server tracks the active slot and releases it on `completed`/`failed` reports, or automatically if the client goes stale (no heartbeat for > 5 minutes).
8. **Failure hooks** — if any job fails, the client runs its `onFailure` hooks with `FAILED_JOBS` set to the failing job name.

## Configuration

The server reads a TOML file (default `config.toml`, overridable via `-config` flag). The file is hot-reloaded via `fsnotify` — no restart needed.

```toml
pwfile = "/path/to/restic-password"
repo = "/path/to/restic-repo"
debug = false
dry-run = false
listen_address = "0.0.0.0:8080"
auth_token = "your-shared-secret"

[[jobs]]
  name = "home-server"
  tag = ["home", "server"]
  files = ["/home", "/etc"]
  preHook = ["echo 'starting home-server backup'"]
  postHook = ["echo 'home-server backup done'"]
  assigned_to = ["laptop1", "nas"]

[[jobs]]
  name = "public-data"
  tag = ["shared"]
  files = ["/data"]
  # No assigned_to → public, available to all clients

[[clients]]
  name = "laptop1"
  schedule_time = "4:40 pm"
  repo = "/path/to/alternate-repo"  # optional per-client override
  preHook = ["echo 'laptop1 pre-hook'"]
  postHook = ["echo 'laptop1 post-hook'"]
  onFailure = ["echo 'laptop1 backup failed'"]

[[clients]]
  name = "nas"
  schedule_time = "11:30 pm"
```

### Config Fields

| Field | Type | Description |
|-------|------|-------------|
| `pwfile` | string | Path to the Restic password file |
| `repo` | string | Restic repository path |
| `debug` | bool | Enable debug logging |
| `dry-run` | bool | Run `restic backup --dry-run` instead of real backups |
| `listen_address` | string | Address to bind the HTTP server (e.g. `0.0.0.0:8080`) |
| `auth_token` | string | Shared secret for `X-Auth-Token` header (empty = auth disabled) |

### Job Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Job identifier |
| `tag` | []string | Restic `--tag` values applied to the backup |
| `files` | []string | Directories/files to back up |
| `preHook` | []string | Shell commands run before this job's backup |
| `postHook` | []string | Shell commands run after this job's backup |
| `assigned_to` | []string | Client names allowed to run this job (empty = public) |

### Client Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique client ID |
| `repo` | string | Optional per-client repository override |
| `schedule_time` | string | Local time to trigger backup (e.g. `"4:40 pm"`) |
| `preHook` | []string | Commands run once before all jobs start |
| `postHook` | []string | Commands run once after all jobs succeed |
| `onFailure` | []string | Commands run if any job fails |

## Web UI

The server ships a built-in dashboard at `GET /` (also `GET /ui`). It is a single self-contained HTML page embedded in the binary (no external assets, no build step). It polls the JSON endpoints and shows:

- **Clients** — every configured client with its live status (`online` / `offline` / `never seen`), last-seen time, schedule time, and whether it holds the backup slot (with the active job). A **Trigger backup** button queues a manual backup per client.
- **Jobs** — the configured jobs with their tags, files, and which clients they are assigned to (or "public" if unassigned).
- **Server health** — a live indicator from `GET /health`.

The page shell is public (no secrets in it). When `auth_token` is set, enter the token in the UI's "Auth token" field; the browser attaches it as `X-Auth-Token` to the data fetches (`/clients`, `/jobs`, `/health`, and the backup trigger). The token is stored in the browser's `localStorage` for convenience.

Open it in a browser at `http://<server-host>:<port>/`.

## HTTP API

All endpoints require the `X-Auth-Token` header when `auth_token` is configured, **except** the web UI shell (`GET /` and `GET /ui`), which is public.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Web UI dashboard (also `GET /ui`) |
| `GET` | `/health` | Health check; returns `{"status":"ok","server_time":...}` |
| `GET` | `/config.json?client_id={id}` | Returns filtered config (jobs + client hooks) for the requesting client |
| `GET` | `/jobs` | List all configured jobs (name, tags, files, assigned_to, public) |
| `POST` | `/reload` | Manually reload the TOML config from disk |
| `POST` | `/clients/{id}/status` | Heartbeat; returns `{"status":"ok"}` or `{"status":"backup"}` |
| `POST` | `/clients/{id}/backup` | Queue a manual backup for the client |
| `POST` | `/clients/{id}/backup/status` | Report backup state: `{"status":"running"|"completed"|"failed","job":"name"}` |
| `GET` | `/clients` | List all configured clients with live status, last-seen, schedule, and slot status |
| `GET` | `/clients/{id}` | Single client view: last seen, active job, in-slot status |

## Building & Running

### Server

```bash
cd server
go build -o factorest-server .
./factorest-server -config /path/to/config.toml
```

### Client

```bash
cd client
go build -o factorest-client .
./factorest-client -client-id laptop1 -server http://localhost:8080 -heartbeat 10 -token your-shared-secret
```

### Client Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-client-id` | *(required)* | The client name to present to the server |
| `-server` | `http://localhost:8080` | Address of the server |
| `-heartbeat` | `10` | Seconds between heartbeats |
| `-token` | *(empty)* | Shared auth token (must match server `auth_token`) |

## Project Structure

```
factorest/
├── go.mod
├── README.MD
├── server/
│   ├── main.go        # Entry point, config loading, fsnotify watcher, HTTP server
│   ├── config.go      # ConfigStore (TOML parsing, RWMutex-protected)
│   ├── handlers.go    # HTTP handlers: health, config.json, jobs, reload
│   ├── heartbeat.go   # Heartbeat tracking, backup slot, scheduled triggers, client views
│   ├── ui.go          # Web UI handler (serves embedded dashboard)
│   └── web/
│       └── index.html # Self-contained dashboard (embedded via go:embed)
├── client/
│   └── main.go        # Client daemon: heartbeat loop, config fetch, restic execution, hooks
├── types/
│   └── types.go       # Shared config/job/client structs
└── plans/
    └── analysis.md    # Codebase analysis and issue tracking
```

## Security Notes

- **Auth token**: when `auth_token` is set, every request must include a matching `X-Auth-Token` header. Comparison is constant-time to prevent timing attacks.
- **Bind address**: the default `listen_address` should be set to a specific interface or `0.0.0.0` for remote clients. Binding to `127.0.0.1` restricts access to localhost only.
- **No TLS**: the server uses plain HTTP. For production use over a network, consider placing it behind a reverse proxy with TLS termination.
