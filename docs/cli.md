# CLI reference

```
forge init                     scaffold forge.yaml and an example pipeline
forge validate pipeline.yml    check a pipeline without running it
forge run pipeline.yml         execute it
forge pipelines                list known pipelines
forge runs                     list recent runs
forge run-status <id>          inspect one run
forge logs <id>                show captured output
forge artifacts <id>           list or extract collected files
forge approve <id> <job>       release a manual job
forge cancel <id>              stop a running pipeline
forge serve                    start the API and dashboard
forge prune                    apply retention
```

## Global flags

| Flag | Meaning |
|---|---|
| `-c, --config PATH` | config file to use (default: the nearest `forge.yaml`) |
| `-C, --dir PATH` | project directory (default: the current one) |
| `--json` | machine-readable output |
| `--no-color` | disable ANSI colour |
| `-v, --verbose` | show debug logging |
| `-q, --quiet` | only warnings and errors |

Colour is disabled automatically when output is not a terminal, when `NO_COLOR`
is set, or when `TERM=dumb`.

forge finds the project root by walking up from the current directory looking for
`forge.yaml` or `.forge/`, the way git does — so any command works from a
subdirectory.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | the run failed or was cancelled, the pipeline is invalid, or the command errored |

`forge run` exits non-zero when the run does not succeed, so it works directly as
a git hook or in a script:

```bash
forge run pipeline.yml || exit 1
```

---

## `forge init`

Creates `forge.yaml` (with commented defaults), `pipeline.yml`, `.forge/` and a
`secrets.env` template at mode 0600.

| Flag | |
|---|---|
| `-f, --force` | overwrite existing files |

Existing files are left alone otherwise, and reported.

## `forge validate [pipeline.yml...]`

Parses and checks one or more pipelines. Nothing is executed and no state is
written, so this is safe to run anywhere.

It prints the **execution plan** — which jobs run in parallel, and what each one
waits for:

```
✔ pipeline.yml — pipeline example, 3 job(s)

  execution plan
  wave 1
    • build [build]
      artifacts: dist/
  wave 2
    • test [test]
      needs build
  wave 3
    • package [package]
      needs test
```

Every problem is reported at once. Exits 1 if any file is invalid.

## `forge run [pipeline.yml]`

Runs a pipeline to completion. With no argument it looks for `pipeline.yml` then
`pipeline.yaml` in the project root.

| Flag | |
|---|---|
| `-j, --concurrency N` | maximum jobs at once (default: from config) |
| `-e, --var KEY=VALUE` | override a pipeline variable (repeatable) |
| `--approve JOB` | pre-approve manual jobs (repeatable) |
| `--wait-manual` | block on manual jobs instead of skipping them |
| `--follow` | stream job output as it happens (default true) |

Output is each job's stdout, prefixed with the job name — without the prefix,
interleaved output from parallel jobs is impossible to attribute — followed by a
summary table.

`Ctrl-C` cancels the run rather than killing forge outright: jobs are stopped
cleanly, whole process groups are killed, and the outcome is still recorded.

Variable overrides apply to that run only. They are never written back to the
stored pipeline definition.

```bash
forge run pipeline.yml -j 8
forge run pipeline.yml -e DEPLOY_ENV=production --approve deploy
forge run pipeline.yml --wait-manual        # then approve elsewhere
forge run pipeline.yml --json | jq '.run.status'
```

## `forge pipelines`

Every pipeline forge has seen, with run counts, pass/fail totals, average
duration and last outcome. Aliases: `pipeline`, `pl`.

## `forge runs`

Recent runs, newest first. Aliases: `history`, `ls`.

| Flag | |
|---|---|
| `-n, --limit N` | maximum to show (default 20) |
| `-s, --status S` | filter by status (repeatable) |
| `-p, --pipeline NAME` | filter by pipeline |

```bash
forge runs -s failed
forge runs -p web-app -n 50
```

## `forge run-status <id>`

One run in detail: per-job status, duration, attempts, executor and failure
reason, plus artifacts and anything awaiting approval. Aliases: `status`, `show`.

The `#` prefix is accepted, so a value copied from the UI works: `forge run-status #7`.

## `forge logs <id>`

Captured output for a run.

| Flag | |
|---|---|
| `-j, --job NAME` | only this job |
| `-s, --stream S` | `stdout`, `stderr` or `both` |
| `-n, --tail N` | only the last N lines of each stream |

Secrets are masked at capture time, so what is stored is already redacted.

```bash
forge logs 7 --job test
forge logs 7 --stream stderr
forge logs 7 -n 50
```

## `forge artifacts <id>`

Lists a run's collected files with sizes and checksums.

| Flag | |
|---|---|
| `-o, --output DIR` | extract them into DIR instead of listing |

```bash
forge artifacts 7
forge artifacts 7 --output ./out
```

Extraction validates both ends of the copy, so a stored path cannot write outside
the directory you chose.

## `forge approve <id> <job>` / `forge cancel <id>`

Both act on a run that is **currently executing**, so they reach the process
running it over the local API. That means a forge process must be running the
pipeline — either `forge serve`, or `forge run --wait-manual` in another
terminal. If neither is, the error says so and tells you what to start.

```bash
forge approve 7 deploy
forge cancel 7
```

## `forge serve`

Starts the API and the embedded dashboard.

| Flag | |
|---|---|
| `--host ADDR` | bind address (default `127.0.0.1`) |
| `-p, --port N` | port (default `7777`) |
| `--open` | open a browser |

```
forge serving on http://127.0.0.1:7777
  dashboard http://127.0.0.1:7777
  api       http://127.0.0.1:7777/api/v1
  metrics   http://127.0.0.1:7777/metrics
```

At startup, runs left mid-flight by a previous process are reconciled to
`cancelled`, so the database never claims something is running when it is not.

**The API can start pipelines, which means it can run shell commands.** Binding
anywhere other than loopback requires `server.token` (or `FORGE_TOKEN`) to be
set; forge refuses to start otherwise. See [security.md](security.md).

## `forge prune`

Applies retention: expired artifacts, and runs beyond the keep window along with
their workspaces. Active runs are never pruned.

| Flag | |
|---|---|
| `--keep N` | runs to keep per pipeline (default: `workspace.keep_runs`) |
| `--dry-run` | report what would be removed |

---

## Configuration

`forge.yaml` at the project root. Every setting has a working default; the file
is optional and any key may be omitted.

```yaml
runner:
  concurrency: 4              # jobs at once; default NumCPU
  default_executor: local     # local | docker
  default_timeout: 30m
  default_retries: 0
  retry_backoff: 2s
  docker_image: alpine:3.20   # for docker jobs that name no image
  env_passthrough: [PATH, HOME, LANG, LC_ALL, TZ, TERM, USER, SHELL, TMPDIR]
  manual_timeout: 0s          # 0s waits indefinitely

workspace:
  mode: isolated              # isolated | shared
  ignore: [.git, .forge, node_modules, .venv, __pycache__]
  keep_runs: 50               # 0 keeps everything

artifacts:
  dir: artifacts              # relative to the forge home
  retention: 720h             # 0 keeps forever
  max_file_size: 268435456
  max_total_size: 1073741824

logs:
  dir: logs
  max_size: 16777216          # per stream, per attempt
  retention: 720h

server:
  host: 127.0.0.1
  port: 7777
  # token: ""

secrets:
  file: secrets.env           # relative to the forge home
  env: []                     # host variables to treat as secrets
```

Durations must be strings with a unit (`30m`, `1h30m`, `500ms`, `0s`). A bare `0`
is a number, not a duration, and is rejected with an explanation.

### Environment overrides

These win over the file:

| Variable | Sets |
|---|---|
| `FORGE_HOME` | the state directory |
| `FORGE_DB` | the database path |
| `FORGE_CONCURRENCY` | `runner.concurrency` |
| `FORGE_HOST` | `server.host` |
| `FORGE_PORT` | `server.port` |
| `FORGE_TOKEN` | `server.token` |
| `NO_COLOR` | disables colour |

### On-disk layout

```
your-project/
├── forge.yaml
├── pipeline.yml
└── .forge/
    ├── forge.db              runs, jobs, artifact and log metadata
    ├── secrets.env           mode 0600, never committed
    ├── artifacts/<run>/<job>/…
    ├── logs/<run>/<job>/<attempt>.<stream>.log
    └── runs/<run>/jobs/<job>/    job workspaces
```

`forge init` writes a `.gitignore` into `.forge/` that excludes all of it.
