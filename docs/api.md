# API reference

forge serves a REST API and two SSE streams from the same process as the
dashboard. Base URL: `http://127.0.0.1:7777`, versioned at `/api/v1`.

```bash
forge serve
curl -s http://127.0.0.1:7777/api/v1/health | jq
```

## Conventions

- Request and response bodies are JSON. Timestamps are RFC 3339; durations are
  milliseconds in `*_ms` fields.
- Lists are always arrays, never `null`.
- Unknown fields in a request body are **rejected**, so a typo is reported rather
  than silently ignored.
- Errors share one shape:

  ```json
  { "error": { "code": "not_found", "message": "run 42: not found" } }
  ```

| Status | When |
|---|---|
| `200` | success |
| `202` | accepted — the work continues in the background |
| `204` | success, no body |
| `400` | malformed path parameter or request body |
| `401` | missing or wrong token |
| `403` | refused for a safety reason (see artifact download) |
| `404` | no such resource or endpoint |
| `409` | the resource is in the wrong state (cancelling a finished run) |
| `422` | a stored pipeline no longer parses |
| `500` | internal error |

## Authentication

None by default, because the server binds to loopback. When `server.token` (or
`FORGE_TOKEN`) is set, every `/api/` and `/metrics` request needs it:

```bash
curl -H "Authorization: Bearer $FORGE_TOKEN" http://127.0.0.1:7777/api/v1/runs
```

`EventSource` cannot set headers, so SSE endpoints also accept `?token=`.
Comparison is constant-time.

The dashboard's own assets stay public — without them a browser could never load
the page that presents the token.

---

## Endpoints

| Method | Path | |
|---|---|---|
| GET | `/api/v1/health` | liveness and schema version |
| GET | `/api/v1/settings` | effective configuration |
| GET | `/api/v1/metrics` | counters and aggregates |
| GET | `/metrics` | Prometheus text format |
| GET | `/api/v1/pipelines` | list pipelines |
| GET | `/api/v1/pipelines/{id}` | pipeline, jobs, graph, recent runs |
| POST | `/api/v1/pipelines/{id}/runs` | start a run |
| GET | `/api/v1/runs` | list runs |
| GET | `/api/v1/runs/{id}` | run, jobs, artifacts, graph |
| POST | `/api/v1/runs/{id}/cancel` | cancel |
| DELETE | `/api/v1/runs/{id}` | delete a run and its artifacts |
| GET | `/api/v1/runs/{id}/jobs` | jobs |
| POST | `/api/v1/runs/{id}/jobs/{name}/approve` | release a manual job |
| GET | `/api/v1/runs/{id}/artifacts` | artifacts |
| GET | `/api/v1/runs/{id}/events` | timeline |
| GET | `/api/v1/runs/{id}/events/stream` | **SSE** live timeline |
| GET | `/api/v1/jobs/{id}` | job, attempts, logs, artifacts |
| GET | `/api/v1/jobs/{id}/logs` | captured output |
| GET | `/api/v1/jobs/{id}/logs/stream` | **SSE** live output |
| GET | `/api/v1/artifacts/{id}` | artifact metadata |
| GET | `/api/v1/artifacts/{id}/download` | artifact bytes |

---

### `GET /api/v1/health`

```json
{
  "status": "ok",
  "schema_version": "0001_init",
  "active_runs": [7],
  "time": "2026-08-12T21:00:57Z"
}
```

### `GET /api/v1/settings`

The effective configuration. **Secret values are never included** — only their
names, so the UI can show what is available.

```json
{
  "project_root": "/home/you/project",
  "home": "/home/you/project/.forge",
  "loopback_only": true,
  "auth_required": false,
  "secret_names": ["API_TOKEN"],
  "runner": { "concurrency": 4, "default_executor": "local", "default_timeout": "30m0s" },
  "workspace": { "mode": "isolated", "keep_runs": 50 },
  "retention": { "artifacts": "720h0m0s", "logs": "720h0m0s" }
}
```

### `GET /api/v1/metrics`

Three views: `process` (counters since this process started), `totals`
(everything in the database), and `durations` (recent finished runs, oldest
first, for a trend).

```json
{
  "process": { "uptime_seconds": 412.5, "active_jobs": 2, "queued_jobs": 1, "jobs_retried": 3 },
  "totals": { "runs_total": 128, "runs_succeeded": 119, "success_rate": 0.93, "avg_run_duration_ms": 4210 },
  "durations": [{ "run_id": 120, "number": 40, "pipeline": "web-app", "status": "success", "duration_ms": 3900 }]
}
```

`GET /metrics` returns the same counters in Prometheus text format, so a local
Prometheus — or `curl | grep` — can read them:

```
# HELP forge_jobs_active Job attempts currently executing.
# TYPE forge_jobs_active gauge
forge_jobs_active 2
forge_executor_jobs_total{executor="local"} 341
```

---

### `GET /api/v1/pipelines`

```json
{
  "pipelines": [{
    "id": 1,
    "name": "web-app",
    "source_path": "pipeline.yml",
    "stats": { "runs": 42, "successes": 39, "failures": 3, "last_status": "success", "last_run_id": 42, "avg_duration_ms": 4210 }
  }]
}
```

### `GET /api/v1/pipelines/{id}`

Adds `jobs` (the static description of each job), `stages`, `layers` (topological
layers for drawing the graph) and `runs`. If the stored definition no longer
parses, `spec_error` explains why instead.

```json
{
  "pipeline": { "id": 1, "name": "web-app", "spec_yaml": "name: web-app\n…" },
  "jobs": [{ "name": "build", "stage": "build", "needs": [], "commands": ["make"], "executor": "local", "when": "on_success", "retries": 0, "implicit_needs": false }],
  "stages": ["build", "test"],
  "layers": [["build"], ["test"]],
  "runs": []
}
```

### `POST /api/v1/pipelines/{id}/runs`

Starts a run and returns `202` immediately — the pipeline continues in the
background, so the request does not stay open for its duration.

```json
{
  "concurrency": 4,
  "variables": { "DEPLOY_ENV": "staging" },
  "approved": ["deploy"],
  "manual": "wait"
}
```

Every field is optional. `manual` is `wait` (default here, since the dashboard
can approve) or `skip`. Variable overrides apply to this run only and are never
written back to the pipeline.

Responds with the created run and a `Location` header.

---

### `GET /api/v1/runs`

| Query | |
|---|---|
| `pipeline_id` | filter by pipeline |
| `status` | repeatable, or comma-separated; unknown values are ignored |
| `limit` | default 50, max 500 |
| `offset` | for paging |

```bash
curl "…/api/v1/runs?status=failed&limit=10"
curl "…/api/v1/runs?status=running,queued,retrying"
```

```json
{ "runs": [ … ], "total": 128, "limit": 50, "offset": 0 }
```

### `GET /api/v1/runs/{id}`

The run, its jobs, its artifacts, whether it is `active` in this process, and the
graph (`layers` and `edges`) taken from the **snapshot stored with the run** — so
editing the pipeline file afterwards does not rewrite history.

```json
{
  "run": { "id": 7, "number": 3, "status": "running", "trigger": "api", "duration_ms": 0 },
  "jobs": [{ "id": 21, "name": "build", "status": "running", "needs": [], "attempts": 1, "max_attempts": 1 }],
  "artifacts": [],
  "active": true,
  "layers": [["build"], ["test"]],
  "edges": [{ "from": "build", "to": "test", "implicit": false }]
}
```

### `POST /api/v1/runs/{id}/cancel`

`202` on success. `409` if the run is not executing **in this process** —
cancellation acts on in-memory state, so it only means something where the run
actually lives. `404` if there is no such run.

### `DELETE /api/v1/runs/{id}`

Deletes the run, its jobs, logs and artifacts. `409` if it is still active.
`204` on success.

### `POST /api/v1/runs/{id}/jobs/{name}/approve`

Releases a job blocked on a manual gate. `202` on success; `409` if the run is
not active or the job is not awaiting approval.

---

### `GET /api/v1/jobs/{id}`

The job with its `attempts`, `logs` (metadata and paths) and `artifacts`.

### `GET /api/v1/jobs/{id}/logs`

| Query | |
|---|---|
| `stream` | `stdout`, `stderr`, or both (default) |
| `offset` | start byte, for incremental reads |
| `limit` | maximum bytes (default 1 MiB) |

```json
{
  "logs": [{
    "stream": "stdout",
    "attempt_id": 31,
    "content": "$ echo hi\nhi\n",
    "bytes": 13,
    "offset": 13,
    "truncated": false
  }]
}
```

`offset` in the response is where to resume. Secrets are already masked — they
are redacted before bytes reach disk, so there is nothing to filter here.

---

### Artifacts

`GET /api/v1/artifacts/{id}` returns metadata. `GET …/download` streams the
bytes with `Content-Disposition`, `Content-Length` and `X-Artifact-SHA256`.

The stored path is **re-validated against the artifact store root on the way
out**, so even a tampered database row cannot turn this endpoint into an
arbitrary file read — that case returns `403`.

```bash
curl -OJ http://127.0.0.1:7777/api/v1/artifacts/12/download
```

---

## Server-sent events

Two streams. Both send `event:` and `id:` lines, and terminate with a `done`
event when the run or job finishes — a finished run's stream delivers its full
history and then closes rather than hanging open.

Neither stream carries state on the server. The event carries an ID, the client
sends it back on reconnect via `Last-Event-ID`, and the handler resumes from
exactly there.

### `GET /api/v1/runs/{id}/events/stream`

Every state transition in the run.

```
id: 14
event: event
data: {"id":14,"run_id":7,"job_id":21,"job_name":"build","type":"job_status","from":"queued","to":"running","created_at":"2026-08-12T21:00:57Z"}

event: done
data: {"run_id":7}
```

`Last-Event-ID` is the event ID; `?after=N` does the same for non-EventSource
clients.

```js
const es = new EventSource("/api/v1/runs/7/events/stream");
es.addEventListener("event", (e) => console.log(JSON.parse(e.data)));
es.addEventListener("done", () => es.close());
```

### `GET /api/v1/jobs/{id}/logs/stream`

Live output for one stream of a job. `?stream=stdout|stderr`, default `stdout`.

```
id: 512
event: log
data: {"stream":"stdout","offset":512,"content":"compiling module 3\n"}
```

Here `Last-Event-ID` is a **byte offset**, so resuming never duplicates or drops
output. `?offset=N` is the non-EventSource equivalent. The stream begins from
offset 0, delivering everything already on disk before tailing, so a client does
not need a separate fetch first.

A comment line (`: keep-alive`) is sent every 25 seconds so idle connections are
not dropped by an intermediary.

---

## Worked example

Start a run and follow it to completion:

```bash
BASE=http://127.0.0.1:7777/api/v1

RUN=$(curl -s -X POST "$BASE/pipelines/1/runs" \
        -H 'Content-Type: application/json' \
        -d '{"manual":"skip"}' | jq -r '.run.id')

curl -sN "$BASE/runs/$RUN/events/stream" | while read -r line; do
  case "$line" in
    data:*) echo "${line#data: }" | jq -r 'select(.type=="job_status") | "\(.job_name) → \(.to)"' ;;
  esac
done

curl -s "$BASE/runs/$RUN" | jq '.run.status, .artifacts'
```

Poll instead of streaming, if that is simpler:

```bash
while :; do
  status=$(curl -s "$BASE/runs/$RUN" | jq -r '.run.status')
  echo "$status"
  case "$status" in success|failed|cancelled) break ;; esac
  sleep 1
done
```
