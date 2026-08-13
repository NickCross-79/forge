#!/usr/bin/env bash
#
# End-to-end check of the forge CLI against a scratch project.
#
# This complements the Go tests rather than duplicating them: it exercises the
# real binary the way a person would, including exit codes and the HTTP server,
# which the in-process tests cannot cover.
#
#   ./scripts/e2e.sh [path-to-forge-binary]

set -euo pipefail

FORGE="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/forge}"

if [[ ! -x "$FORGE" ]]; then
  echo "forge binary not found at $FORGE — run 'make build' first" >&2
  exit 1
fi
FORGE="$(cd "$(dirname "$FORGE")" && pwd)/$(basename "$FORGE")"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
PORT="${FORGE_E2E_PORT:-7911}"
SERVER_PID=""

cleanup() {
  local status=$?
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT INT TERM

pass=0
step()  { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }
ok()    { printf '  \033[32m✔\033[0m %s\n' "$1"; pass=$((pass + 1)); }
die()   { printf '  \033[31m✘ %s\033[0m\n' "$1" >&2; exit 1; }

cd "$WORK"

# --- scaffolding ------------------------------------------------------------

step "forge init"
"$FORGE" init >/dev/null
[[ -f forge.yaml   ]] || die "forge.yaml was not created"
[[ -f pipeline.yml ]] || die "pipeline.yml was not created"
[[ -f .forge/secrets.env ]] || die ".forge/secrets.env was not created"
ok "scaffolded forge.yaml, pipeline.yml and .forge/"

# The secrets template must not be readable by anyone else.
perms="$(stat -c '%a' .forge/secrets.env 2>/dev/null || stat -f '%Lp' .forge/secrets.env)"
[[ "$perms" == "600" ]] || die "secrets.env has mode $perms, want 600"
ok "secrets.env is mode 600"

step "forge validate"
"$FORGE" validate pipeline.yml >/dev/null || die "the scaffolded pipeline does not validate"
ok "the scaffolded pipeline validates"

# Every shipped example must validate too.
for example in "$REPO_ROOT"/examples/*.yml; do
  "$FORGE" validate "$example" >/dev/null || die "example $(basename "$example") does not validate"
done
ok "all shipped examples validate"

# A broken pipeline must be rejected, with a non-zero exit.
cat > broken.yml <<'YAML'
name: broken
jobs:
  a:
    needs: [b]
    commands: [echo a]
  b:
    needs: [a]
    commands: [echo b]
YAML
if "$FORGE" validate broken.yml >/dev/null 2>&1; then
  die "a cyclic pipeline was accepted"
fi
ok "a cyclic pipeline is rejected with a non-zero exit"

# --- running ----------------------------------------------------------------

step "forge run"
"$FORGE" run pipeline.yml >/dev/null || die "the example pipeline failed"
ok "the example pipeline succeeded"

# The artifact the pipeline declared must exist, with the right bytes.
artifact="$(find .forge/artifacts -name output.txt -print -quit)"
[[ -n "$artifact" ]] || die "dist/output.txt was not collected"
[[ "$(cat "$artifact")" == "hello" ]] || die "artifact content is wrong: $(cat "$artifact")"
ok "the declared artifact was collected with the expected content"

step "forge runs / run-status / logs / artifacts"
"$FORGE" runs --json | grep -q '"status": "success"' || die "forge runs does not report the run"
ok "forge runs lists the run"

"$FORGE" run-status 1 >/dev/null || die "forge run-status failed"
ok "forge run-status reports the run"

# The `test` job read the artifact the `build` job produced, in its own
# workspace. That output proves artifacts flow across isolated workspaces.
"$FORGE" logs 1 --job test | grep -q "hello" \
  || die "the test job did not receive the build job's artifact"
ok "artifacts flow along 'needs' into a separate workspace"

"$FORGE" artifacts 1 | grep -q "dist/output.txt" || die "forge artifacts does not list the file"
ok "forge artifacts lists the collected file"

mkdir -p extracted
"$FORGE" artifacts 1 --output extracted >/dev/null
[[ -f extracted/dist/output.txt ]] || die "artifact extraction did not produce the file"
ok "forge artifacts --output extracts the file"

step "a failing pipeline exits non-zero"
cat > failing.yml <<'YAML'
name: failing
jobs:
  boom:
    commands:
      - echo "about to fail"
      - exit 7
  downstream:
    needs: [boom]
    commands: [echo "unreachable"]
YAML
if "$FORGE" run failing.yml >/dev/null 2>&1; then
  die "a failing pipeline exited 0"
fi
ok "a failing pipeline exits non-zero"

"$FORGE" runs --json | grep -q '"status": "failed"' || die "the failed run was not recorded"
ok "the failure was recorded"

# The dependent must have been skipped, not run.
"$FORGE" run-status 2 --json | grep -q '"status": "skipped"' \
  || die "the downstream job was not skipped after its dependency failed"
ok "a downstream job is skipped when its dependency fails"

step "state survives a restart"
# Every command above ran in its own process, so the run history being readable
# at all proves the database persists. Confirm the count explicitly.
runs="$("$FORGE" runs --json | grep -c '"id":' || true)"
[[ "$runs" -ge 2 ]] || die "expected at least 2 runs in history, found $runs"
ok "run history survives across processes"

# --- serving ----------------------------------------------------------------

step "forge serve"
"$FORGE" serve --port "$PORT" >serve.log 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/api/v1/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

curl -fsS "http://127.0.0.1:$PORT/api/v1/health" | grep -q '"status":"ok"' \
  || die "the health endpoint did not report ok"
ok "the API is serving and healthy"

curl -fsS "http://127.0.0.1:$PORT/api/v1/runs" | grep -q '"runs"' || die "the runs endpoint failed"
ok "the runs endpoint responds"

curl -fsS "http://127.0.0.1:$PORT/metrics" | grep -q "forge_runs_started_total" \
  || die "the Prometheus endpoint failed"
ok "the Prometheus endpoint responds"

# Secrets must never appear in the settings payload.
if curl -fsS "http://127.0.0.1:$PORT/api/v1/settings" | grep -q "replace-me"; then
  die "the settings endpoint leaked a secret value"
fi
ok "the settings endpoint exposes no secret values"

status="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/")"
[[ "$status" == "200" ]] || die "the dashboard returned HTTP $status"
ok "the dashboard is served"

status="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/api/v1/nonexistent")"
[[ "$status" == "404" ]] || die "an unknown API path returned HTTP $status, want 404"
ok "unknown API paths return 404"

printf '\n\033[32m✔ %d checks passed\033[0m\n' "$pass"
