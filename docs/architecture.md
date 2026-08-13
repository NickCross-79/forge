# Architecture

forge is a single Go binary with an embedded React dashboard, a SQLite database,
and no other moving parts. This document explains how the pieces fit and why they
are shaped the way they are.

## The shape of it

```
                       ┌──────────────┐
   forge run  ────────▶│              │
                       │  scheduler   │──▶ executor ──▶ shell / container
   HTTP POST ─────────▶│              │        │
                       └──────┬───────┘        │
                              │                ├──▶ logs      ──▶ .forge/logs/
                              │                └──▶ artifacts ──▶ .forge/artifacts/
                              ▼
                       ┌──────────────┐
                       │  store       │──────────────▶ .forge/forge.db
                       │  (SQLite)    │
                       └──────┬───────┘
                              │
                       ┌──────▼───────┐
                       │  api + SSE   │──▶ dashboard (embedded)
                       └──────────────┘
```

Every package has one job, and depends only on packages below it:

| Package | Responsibility |
|---|---|
| `internal/model` | The shared vocabulary: `Run`, `Job`, `Status`, `Artifact`. Depends on nothing. |
| `internal/config` | Loading and validating `forge.yaml`; resolving the on-disk layout. |
| `internal/pipeline` | Parsing YAML into a `Spec`, validating it, and building an acyclic `Graph`. Pure functions. |
| `internal/store` | SQLite: migrations, repositories, aggregate queries. |
| `internal/executor` | Running a job's commands. `LocalExecutor` and `DockerExecutor` behind one interface. |
| `internal/artifacts` | Selecting, storing and restoring files, with path-traversal defences. |
| `internal/logs` | Capturing stdout/stderr, masking secrets, and notifying live tailers. |
| `internal/secrets` | Sourcing secret values and exposing the set to redact. |
| `internal/scheduler` | The engine: dependency resolution, concurrency, retries, cancellation, manual gates. |
| `internal/metrics` | Process-lifetime counters and gauges. |
| `internal/api` | REST handlers, SSE streams, artifact downloads, the embedded dashboard. |
| `internal/cli` | Commands and terminal output. |
| `internal/observability` | Structured application logging. |

The seams are real. `executor.Executor` is a two-method interface, so a new
backend (podman, a remote runner, a WASM sandbox) is a new type and nothing else.
`store` is the only package that touches SQL. `pipeline` has no dependencies on
anything in forge, which is what makes `forge validate` instant and the parser
exhaustively testable.

## Execution model

### From file to run

1. `pipeline.Load` parses the YAML **strictly** — an unrecognised key is an
   error, because a typo'd key in CI config usually means the pipeline is not
   doing what its author believes.
2. Validation collects *every* problem before reporting, so you fix them in one
   pass rather than one at a time.
3. `Spec.Graph()` resolves dependencies and runs Kahn's algorithm. A cycle is
   rejected here, before any state is written or any command runs.
4. `scheduler.Prepare` writes the pipeline, the run, and one row per job.
5. `scheduler.Execute` runs the graph.

### Dependency resolution

A job's dependencies are either explicit (`needs:`) or implied by stages: a job
with no `needs` depends on every job in the nearest preceding stage that has any.
Empty stages are skipped rather than severing the chain, so an unused stage in
the list does not silently break ordering.

That means `stages` alone is a complete way to express a pipeline, and `needs`
alone is too. Mixing them works and is the common case.

### The engine loop

```
loop:
  if not cancelled:
    repeat until nothing changes:
      for each job in deterministic order:
        if every dependency is terminal:
          decide: run / skip / await approval
  if nothing is in flight: break
  wait for a result
```

The fixed-point inner loop matters: skipping one job can make its dependents
decidable in the same pass, so a chain of skips resolves immediately rather than
one iteration per link.

Ready jobs go onto a **buffered queue drained by a fixed worker pool**. The pool
size *is* the concurrency limit, and because jobs are enqueued in the pipeline's
stable order and workers take them in turn, queueing is fair. The loop itself
only ever blocks on results, so a long-running job cannot delay the release of
unrelated work.

Manual gates are awaited **off** the worker pool. If a gate occupied a worker, a
pipeline with concurrency 1 and one manual job would deadlock — nothing else
could run while the gate was open.

Exactly one result flows back per dispatched job. The gate either reports a skip
or cancellation itself, or hands the job to a worker which reports the real
outcome. That invariant is what keeps the in-flight count honest.

### Whether a job runs

Once every dependency is terminal, two independent gates decide:

- **`when`** looks at the outcome so far: `on_success` (default) needs its
  dependencies to have succeeded and the run to be healthy; `on_failure` needs
  something to have failed; `always` ignores both; `manual` adds approval;
  `never` is off.
- **`if`** looks at the environment, using a small expression language.

A dependency that was *skipped* blocks its dependents, which is how a skip
propagates down the graph without any explicit propagation code. A dependency
that failed with `allow_failure: true` does not block.

### Workspaces

Each run gets `.forge/runs/<id>/`. In the default **isolated** mode, each job
gets its own copy of the project tree inside that, seeded by copying the source
(honouring an ignore list) and then restoring the artifacts of every job it
`needs`.

This is the only model that is correct when independent jobs run concurrently:
two jobs in the same stage cannot see or clobber each other's files. It is also
what makes artifact flow meaningful — `test` reads `dist/output.txt` because
`build` produced it and `test` needs it, not because they happen to share a
directory.

`workspace.mode: shared` gives every job in a run one directory. It is faster on
large trees and it gives up that isolation. The trade is yours to make.

A retry re-seeds from scratch, so a failed attempt cannot leave debris that
changes the behaviour of the next one.

### Executors

```go
type Executor interface {
    Run(ctx context.Context, job Job) Result
}
```

Both backends assemble the job's commands into **one shell script** rather than
running each command separately. That is what makes `cd dist` affect the next
command, matching how you would run the same steps by hand. `set -e` stops at the
first failure so a broken step cannot be masked by a later successful one.

The script arrives on **stdin**, never as an argument — command lines are visible
to every user on the host via `ps`, and a job's commands may embed values that
should not be.

- **Local** runs the shell as a child process in its own **process group**, so a
  timeout or cancellation kills anything the job backgrounded rather than leaking
  orphans.
- **Docker** runs `docker run --rm` with the workspace bind-mounted and
  `--user <uid>:<gid>` so files written into the mount are not left root-owned.
  Environment values are forwarded **by name** (`--env KEY`, with the value in
  the docker CLI's own environment) rather than as `KEY=VALUE` arguments, for the
  same `ps` reason. Killing the docker CLI does not stop the container, so
  cancellation stops it explicitly by name.

### Logs

stdout and stderr are captured **separately**, so a job's diagnostics can be read
apart from its output.

Output is buffered per line before being written, which is what makes redaction
reliable: a secret is matched against a whole line rather than against whatever
fragment happened to arrive in one `write`. When a line is implausibly long the
buffer is flushed early, holding back a tail as long as the longest secret so a
value straddling the boundary is still caught.

**Redaction happens on the write path**, before bytes reach disk. A secret is
therefore never persisted even briefly, and cannot be recovered by reading the
log file directly.

Log bodies live on disk; the database holds only metadata and a path. A chatty
job cannot bloat the database.

### Live streaming

The notification broker carries **no payload**. Subscribers are told only that a
topic changed, and then read the authoritative source themselves — the log file
at their byte offset, or the events table after their last seen ID.

Three things fall out of that:

- A slow HTTP client can never block a running job.
- A dropped notification is harmless; the next one triggers a full catch-up.
- Reconnection is trivial. EventSource resends `Last-Event-ID`, which is a byte
  offset for logs and an event ID for run timelines, so a client resumes exactly
  where it stopped without duplicating or losing anything.

The broker also remembers recently-closed topics, bounded to the last few
thousand. Without that there is a race between "is this job still running?" and
subscribing, and losing it leaves a stream open on a source that has already
stopped.

## Persistence

SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — a pure
Go implementation. That choice is deliberate: `CGO_ENABLED=0`, no C toolchain to
install, and cross-compilation that works. The performance difference against the
CGO driver is irrelevant at the scale a local CI tool operates.

**Timestamps are Unix milliseconds in INTEGER columns**, not text or a
driver-specific date type. Ordering, comparison and arithmetic are then exact and
independent of how any particular driver parses dates.

The connection pool is limited to **one connection**. SQLite allows a single
writer, and serialising every statement removes an entire class of
"database is locked" failures at a cost that does not matter here: queries are
sub-millisecond and no transaction is held open across I/O.

Migrations are embedded `.sql` files applied in order, each in its own
transaction together with the row recording it. A failure leaves the database at
the last fully-applied version rather than halfway through, and re-running is a
no-op — which is what makes restarts safe.

### Surviving a crash

forge holds run state in memory while executing, so a run left as `running` in
the database can only be the residue of a crash or a kill. `forge serve`
reconciles those to `cancelled` at startup, with an explanation, so the database
never lies about what is happening.

### Schema

`pipelines` → `runs` → `jobs` → `job_attempts` → `logs`, plus `artifacts` and
`run_events`, with `ON DELETE CASCADE` throughout and foreign keys enabled.

Runs carry a per-pipeline monotonic `number`, allocated inside the same
transaction as the insert, so two runs started at the same moment cannot collide.
The CLI takes the friendly integer ID.

A run stores a **snapshot** of the pipeline as it was when the run started.
Editing the file afterwards does not rewrite the history of old runs, and the
dashboard can draw a graph for a run whose source file no longer exists.

## HTTP and the dashboard

The API uses Go 1.22+ `ServeMux` patterns (`GET /api/v1/runs/{id}`), so there is
no router dependency. Middleware is panic recovery → request logging → CORS →
authentication.

The dashboard is compiled into the binary with `go:embed`. `forge serve` is one
process serving both. Hashed asset filenames are cached immutably; `index.html`
never is, so a rebuilt dashboard is picked up on the next reload. Unknown paths
fall back to `index.html` for client-side routing — except under `/api/`, where a
wrong path is an honest JSON 404 rather than HTML.

In development, `make dev` runs Vite on `:5173` proxying to the API, and the
server allows that origin explicitly.

## What is deliberately not here

- **No agents or runners.** One process does everything.
- **No plugin system.** A pipeline runs commands; that is the extension point.
- **No matrix builds.** Generate the jobs, or run forge twice.
- **No distributed execution.** The whole premise is that this is your machine.
- **No expression language beyond `if`.** A condition that needs more is better
  written as a command that exits non-zero.

## Replacing a piece

- **A different executor** — implement `executor.Executor`, pass it in
  `scheduler.Options`. This is exactly how the tests substitute a fake backend.
- **A different database** — `internal/store` is the only package with SQL in it.
- **A different frontend** — the REST API is documented and versioned; the
  dashboard has no privileged access.
- **A different log sink** — `logs.Writer` is an `io.Writer` with a redactor.
