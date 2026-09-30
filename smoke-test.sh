#!/usr/bin/env bash
#
# smoke-test.sh — End-to-end smoke test for factorest
#
# What it does:
#   1. Builds the server and client binaries.
#   2. Runs the Go unit tests.
#   3. Creates a throwaway restic repository + password file.
#   4. Generates a temporary server config (TOML) with three clients and
#      three jobs (one public, one assigned to "alpha", one failing job
#      assigned to "gamma").
#   5. Starts the server on a free port with an auth token.
#   6. Exercises every HTTP endpoint (auth on/off, health, jobs, clients,
#      config filtering, unknown client, manual-trigger 409 conflict).
#   7. Runs a real client daemon that performs a full backup cycle
#      (heartbeat -> manual trigger -> config fetch -> restic backup ->
#      status reports -> slot release) and verifies the snapshot landed.
#   8. Runs a second client to exercise the failure path (failing job ->
#      "failed" status -> onFailure hook -> slot release).
#   9. Cleans up everything and reports PASS/FAIL.
#
# Usage:
#   ./smoke-test.sh            # run the full test, then clean up
#   ./smoke-test.sh --keep     # keep the temp dir + server running for manual inspection
#
# Requirements: go, restic, curl, jq
#
set -u

# ---------------------------------------------------------------------------
# Globals
# ---------------------------------------------------------------------------
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

PASS=0
FAIL=0
SERVER_PID=""
CLIENT_PID=""
TMPDIR=""

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
pass() { printf '   \033[1;32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
fail() { printf '   \033[1;31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# check_cmd <name> — ensure a required binary is present
check_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}

# http <method> <path> [token] [body] -> prints body; sets HTTP_CODE
http() {
  local method="$1" path="$2" token="${3:-}" body="${4:-}"
  local args=(-s -o /tmp/.smoke_body -w '%{http_code}' -X "$method" "$BASE$path")
  [ -n "$token" ] && args+=(-H "X-Auth-Token: $token")
  [ -n "$body" ]  && args+=(-H "Content-Type: application/json" -d "$body")
  HTTP_CODE="$(curl "${args[@]}")"
  cat /tmp/.smoke_body
}

# wait_for_slot <client> <want> <timeout_s>
#   want=true  -> wait until in_backup_slot becomes true
#   want=false -> wait until in_backup_slot becomes false/absent
wait_for_slot() {
  local client="$1" want="$2" timeout="$3"
  local i=0 slot
  while [ "$i" -lt "$timeout" ]; do
    slot="$(http GET "/clients/$client" "$TOKEN" | jq -r '.in_backup_slot // "null"')"
    if [ "$want" = "true" ]  && [ "$slot" = "true" ]; then return 0; fi
    if [ "$want" = "false" ] && [ "$slot" != "true" ]; then return 0; fi
    sleep 1; i=$((i+1))
  done
  return 1
}

# snapshot_count -> number of snapshots in the temp repo
snapshot_count() {
  RESTIC_REPOSITORY="$REPO" RESTIC_PASSWORD_FILE="$PWFILE" \
    restic snapshots --json 2>/dev/null | jq 'length'
}

cleanup() {
  [ -n "$CLIENT_PID" ] && kill "$CLIENT_PID" 2>/dev/null
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  wait 2>/dev/null
  if [ "$KEEP" -eq 0 ] && [ -n "$TMPDIR" ]; then
    rm -rf "$TMPDIR"
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 0. Preflight
# ---------------------------------------------------------------------------
log "Preflight"
for c in go restic curl jq; do check_cmd "$c"; done
info "go:     $(go version)"
info "restic: $(restic version 2>/dev/null | head -1)"

# ---------------------------------------------------------------------------
# 1. Build + unit tests
# ---------------------------------------------------------------------------
log "Build & unit tests"
SERVER_BIN="$(mktemp -d)/factorest-server"
CLIENT_BIN="$(mktemp -d)/factorest-client"
( cd "$ROOT/server"  && go build -o "$SERVER_BIN" . ) || die "server build failed"
( cd "$ROOT/client"  && go build -o "$CLIENT_BIN" . ) || die "client build failed"
pass "server + client binaries built"

if ( cd "$ROOT/server" && go test ./... ) >/tmp/.smoke_tests 2>&1; then
  pass "go unit tests passed"
else
  fail "go unit tests failed (see /tmp/.smoke_tests)"
  cat /tmp/.smoke_tests
fi

# ---------------------------------------------------------------------------
# 2. Temp workspace: restic repo, password, data, config
# ---------------------------------------------------------------------------
log "Temp workspace"
TMPDIR="$(mktemp -d /tmp/factorest-smoke.XXXXXX)"
REPO="$TMPDIR/repo"
PWFILE="$TMPDIR/restic.passwd"
DATA="$TMPDIR/data"
CONFIG="$TMPDIR/config.toml"
TOKEN="smoke-test-token"

mkdir -p "$DATA"
echo "hello factorest" > "$DATA/hello.txt"
echo "smoke-test-password" > "$PWFILE"

RESTIC_REPOSITORY="$REPO" RESTIC_PASSWORD_FILE="$PWFILE" restic init >/dev/null 2>&1 \
  || die "failed to initialize restic repo"
pass "restic repo initialized at $REPO"

# Find a free port
PORT=18080
while (echo > /dev/tcp/127.0.0.1/$PORT) 2>/dev/null; do PORT=$((PORT+1)); done
BASE="http://127.0.0.1:$PORT"
info "using port $PORT (base $BASE)"

# Generate the TOML config (quoted heredoc -> no shell expansion; sed fills placeholders)
cat > "$CONFIG" <<'EOF'
pwfile = "__PWFILE__"
repo = "__REPO__"
debug = true
listen_address = "127.0.0.1:__PORT__"
auth_token = "__TOKEN__"

[[jobs]]
name = "public-job"
tag = ["public"]
files = ["__DATA__"]
preHook  = ["echo 'public-job pre-hook'"]
postHook = ["echo 'public-job post-hook'"]
# no assigned_to -> public, visible to every client

[[jobs]]
name = "assigned-job"
tag = ["assigned"]
files = ["__DATA__"]
preHook  = ["echo 'assigned-job pre-hook'"]
postHook = ["echo 'assigned-job post-hook'"]
assigned_to = ["alpha"]

[[jobs]]
name = "failing-job"
tag = ["failing"]
files = ["__DATA__"]
preHook = ["sh -c 'echo about to fail; exit 1'"]
assigned_to = ["gamma"]

[[clients]]
name = "alpha"
schedule_time = "11:59 pm"
preHook  = ["echo \"alpha pre-hook CLIENT_ID=$CLIENT_ID JOB_COUNT=$JOB_COUNT\""]
postHook = ["echo 'alpha post-hook'"]
onFailure = ["echo 'alpha onFailure hook ran'"]

[[clients]]
name = "beta"
schedule_time = "11:59 pm"

[[clients]]
name = "gamma"
schedule_time = "11:59 pm"
onFailure = ["echo 'gamma onFailure hook ran'"]
EOF
sed -i \
  -e "s|__PWFILE__|$PWFILE|g" \
  -e "s|__REPO__|$REPO|g" \
  -e "s|__DATA__|$DATA|g" \
  -e "s|__PORT__|$PORT|g" \
  -e "s|__TOKEN__|$TOKEN|g" \
  "$CONFIG"
pass "wrote temp config at $CONFIG"

# ---------------------------------------------------------------------------
# 3. Start the server
# ---------------------------------------------------------------------------
log "Start server"
"$SERVER_BIN" -config "$CONFIG" > "$TMPDIR/server.log" 2>&1 &
SERVER_PID=$!

# Wait for the server to come up (health endpoint, with auth token)
up=0
for i in $(seq 1 20); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "$BASE/health")"
  if [ "$code" = "200" ]; then up=1; break; fi
  sleep 0.5
done
[ "$up" -eq 1 ] || { fail "server did not come up"; cat "$TMPDIR/server.log"; exit 1; }
pass "server is up (pid $SERVER_PID)"

# ---------------------------------------------------------------------------
# 4. Endpoint tests
# ---------------------------------------------------------------------------
log "Endpoint tests"

# 4a. Auth: no token -> 401 on a protected endpoint
http GET "/health" "" >/dev/null
[ "$HTTP_CODE" = "401" ] && pass "auth: /health without token -> 401" \
                          || fail "auth: expected 401, got $HTTP_CODE"

# 4b. Auth: wrong token -> 401
http GET "/health" "wrong-token" >/dev/null
[ "$HTTP_CODE" = "401" ] && pass "auth: /health with wrong token -> 401" \
                          || fail "auth: expected 401, got $HTTP_CODE"

# 4c. Auth: correct token -> 200
http GET "/health" "$TOKEN" >/dev/null
[ "$HTTP_CODE" = "200" ] && pass "auth: /health with correct token -> 200" \
                          || fail "auth: expected 200, got $HTTP_CODE"

# 4d. Health body
body="$(http GET "/health" "$TOKEN")"
echo "$body" | jq -e '.status == "ok"' >/dev/null 2>&1 \
  && pass "health: body has status=ok" \
  || fail "health: unexpected body: $body"

# 4e. Web UI shell is public (no token needed)
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/")"
[ "$code" = "200" ] && pass "ui: GET / public -> 200" || fail "ui: expected 200, got $code"
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/ui")"
[ "$code" = "200" ] && pass "ui: GET /ui public -> 200" || fail "ui: expected 200, got $code"

# 4f. Jobs list
jobs="$(http GET "/jobs" "$TOKEN")"
njobs="$(echo "$jobs" | jq 'length')"
[ "$njobs" = "3" ] && pass "jobs: 3 jobs listed" || fail "jobs: expected 3, got $njobs"
echo "$jobs" | jq -e '.[] | select(.name=="public-job") | .public == true' >/dev/null 2>&1 \
  && pass "jobs: public-job flagged public" || fail "jobs: public-job not flagged public"

# 4g. Clients list
clients="$(http GET "/clients" "$TOKEN")"
nclients="$(echo "$clients" | jq 'length')"
[ "$nclients" = "3" ] && pass "clients: 3 clients listed" || fail "clients: expected 3, got $nclients"

# 4h. Config filtering
for cid in alpha beta gamma; do
  cfg="$(http GET "/config.json?client_id=$cid" "$TOKEN")"
  n="$(echo "$cfg" | jq '.Jobs | length')"
  case "$cid" in
    alpha) want=2 ;;  # public-job + assigned-job
    beta)  want=1 ;;  # public-job only
    gamma) want=2 ;;  # public-job + failing-job
  esac
  [ "$n" = "$want" ] && pass "config: $cid sees $n job(s) (expected $want)" \
                      || fail "config: $cid sees $n job(s), expected $want"
done

# 4i. Config for alpha includes its own client hooks
cfg="$(http GET "/config.json?client_id=alpha" "$TOKEN")"
echo "$cfg" | jq -e '.Client.Name == "alpha" and (.Client.PreHook | length) == 1' >/dev/null 2>&1 \
  && pass "config: alpha has its own client hooks attached" \
  || fail "config: alpha client hooks missing"

# 4j. Unknown client -> 404
http POST "/clients/does-not-exist/status" "$TOKEN" >/dev/null
[ "$HTTP_CODE" = "404" ] && pass "heartbeat: unknown client -> 404" \
                          || fail "heartbeat: expected 404, got $HTTP_CODE"

# 4k. Manual trigger for a valid client -> 200 queued
http POST "/clients/alpha/backup" "$TOKEN" >/dev/null
[ "$HTTP_CODE" = "200" ] && pass "trigger: POST /clients/alpha/backup -> 200 queued" \
                          || fail "trigger: expected 200, got $HTTP_CODE"

# 4l. Manual trigger for a DIFFERENT client while alpha holds the slot -> 409
http POST "/clients/beta/backup" "$TOKEN" >/dev/null
[ "$HTTP_CODE" = "409" ] && pass "trigger: beta while alpha holds slot -> 409" \
                          || fail "trigger: expected 409, got $HTTP_CODE"

# 4m. Reload endpoint
body="$(http POST "/reload" "$TOKEN")"
echo "$body" | jq -e '.status == "reloaded"' >/dev/null 2>&1 \
  && pass "reload: POST /reload -> reloaded" \
  || fail "reload: unexpected body: $body"

# ---------------------------------------------------------------------------
# 5. Happy path: run a real client, full backup cycle
# ---------------------------------------------------------------------------
log "Happy path: client 'alpha' full backup cycle"
CLIENT_LOG="$TMPDIR/client-alpha.log"
"$CLIENT_BIN" -client-id alpha -server "$BASE" -heartbeat 2 -token "$TOKEN" \
  > "$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
info "client alpha started (pid $CLIENT_PID)"

# The manual trigger from 4k is already pending; the client should pick it up
# on its next heartbeat and run the backup.
if wait_for_slot alpha true 30; then
  pass "backup: alpha acquired the backup slot"
else
  fail "backup: alpha never acquired the backup slot (timeout)"
fi

if wait_for_slot alpha false 60; then
  pass "backup: alpha released the backup slot (completed)"
else
  fail "backup: alpha did not release the slot within 60s"
fi

# Verify the backup actually landed in the repo
snap="$(snapshot_count)"
[ "$snap" -ge 1 ] && pass "backup: $snap snapshot(s) present in repo" \
                  || fail "backup: no snapshots in repo"

# Verify hooks ran (client-level + job-level) and env vars were passed
grep -q "alpha pre-hook CLIENT_ID=alpha JOB_COUNT=2" "$CLIENT_LOG" \
  && pass "hooks: alpha client pre-hook ran with CLIENT_ID/JOB_COUNT env" \
  || fail "hooks: alpha client pre-hook env not found in log"
grep -q "public-job pre-hook" "$CLIENT_LOG" \
  && pass "hooks: public-job pre-hook ran" || fail "hooks: public-job pre-hook missing"
grep -q "assigned-job post-hook" "$CLIENT_LOG" \
  && pass "hooks: assigned-job post-hook ran" || fail "hooks: assigned-job post-hook missing"
grep -q "alpha post-hook" "$CLIENT_LOG" \
  && pass "hooks: alpha client post-hook ran" || fail "hooks: alpha client post-hook missing"
grep -q "job successful" "$CLIENT_LOG" \
  && pass "backup: jobs reported successful" || fail "backup: no 'job successful' in log"

kill "$CLIENT_PID" 2>/dev/null; wait "$CLIENT_PID" 2>/dev/null; CLIENT_PID=""

# ---------------------------------------------------------------------------
# 6. Failure path: run client 'gamma' with a failing job
# ---------------------------------------------------------------------------
log "Failure path: client 'gamma' failing job"
CLIENT_LOG="$TMPDIR/client-gamma.log"
"$CLIENT_BIN" -client-id gamma -server "$BASE" -heartbeat 2 -token "$TOKEN" \
  > "$CLIENT_LOG" 2>&1 &
CLIENT_PID=$!
info "client gamma started (pid $CLIENT_PID)"

# Queue a manual backup for gamma
http POST "/clients/gamma/backup" "$TOKEN" >/dev/null
[ "$HTTP_CODE" = "200" ] && pass "trigger: gamma backup queued" \
                          || fail "trigger: gamma queue expected 200, got $HTTP_CODE"

if wait_for_slot gamma true 30; then
  pass "failure: gamma acquired the backup slot"
else
  fail "failure: gamma never acquired the slot (timeout)"
fi

if wait_for_slot gamma false 60; then
  pass "failure: gamma released the slot after failure"
else
  fail "failure: gamma did not release the slot within 60s"
fi

grep -q "gamma onFailure hook ran" "$CLIENT_LOG" \
  && pass "failure: gamma onFailure hook ran" \
  || fail "failure: gamma onFailure hook did not run"
grep -q "backup run failed" "$CLIENT_LOG" \
  && pass "failure: client logged 'backup run failed'" \
  || fail "failure: no 'backup run failed' in log"
grep -q "failing-job" "$CLIENT_LOG" \
  && pass "failure: failing-job named in log" \
  || fail "failure: failing-job not named in log"

kill "$CLIENT_PID" 2>/dev/null; wait "$CLIENT_PID" 2>/dev/null; CLIENT_PID=""

# ---------------------------------------------------------------------------
# 7. Summary
# ---------------------------------------------------------------------------
log "Summary"
printf '   \033[1;32m%d passed\033[0m, \033[1;31m%d failed\033[0m\n' "$PASS" "$FAIL"

if [ "$KEEP" -eq 1 ]; then
  info "KEEP mode: server still running (pid $SERVER_PID), config at $CONFIG"
  info "Open the UI at $BASE/  (auth token: $TOKEN)"
  info "Repo: $REPO   Password: $PWFILE"
else
  info "cleaned up temp dir $TMPDIR"
fi

[ "$FAIL" -eq 0 ] || exit 1
exit 0
