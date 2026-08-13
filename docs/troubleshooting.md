# Troubleshooting

Problems you are likely to hit, and what they mean.

---

## Pipeline files

### `cannot unmarshal !!map into string`

A command contains a colon followed by a space, which YAML reads as a key/value
pair:

```yaml
commands:
  - echo "lint: clean" > report.txt      # ✘
  - 'echo "lint: clean" > report.txt'    # ✔
```

### `did not find expected ',' or ']'`

Usually the same cause, plus a `[` in the value that YAML then treats as a flow
sequence:

```yaml
- echo "empty: [${VAR:-}]"      # ✘
- 'echo "empty: [${VAR:-}]"'    # ✔
```

### A command runs but is mysteriously truncated

A space followed by `#` starts a YAML comment, cutting the scalar short and
leaving an unbalanced quote:

```yaml
- echo "run #${FORGE_RUN_NUMBER}"      # ✘ truncated at the #
- 'echo "run #${FORGE_RUN_NUMBER}"'    # ✔
```

**When in doubt, single-quote the whole command**, or use a block scalar:

```yaml
commands:
  - |
    if [ -f config.json ]; then
      echo "found: config"
    fi
```

### `field X not found in type …`

Strict parsing: an unrecognised key is an error rather than being silently
ignored, because a typo'd key usually means the pipeline is not doing what you
think. Check spelling and indentation — a key at the wrong level is a different
key.

### `dependency cycle detected: a -> b -> a`

Two or more jobs depend on each other. The message names the exact path. Nothing
ran and nothing was written.

### `job "x" uses stage "y", which is not listed in stages`

Every stage a job names must appear in the top-level `stages` list. Either add it
there or remove `stage:` from the job — but if you declare `stages` at all, every
job needs one.

---

## Configuration

### `cannot unmarshal !!int 0 into time.Duration`

Durations are strings with a unit:

```yaml
manual_timeout: 0      # ✘
manual_timeout: 0s     # ✔
default_timeout: 30m   # ✔  also 1h30m, 500ms, 2h
```

forge appends a hint to this error explaining exactly that.

### `server.host "0.0.0.0" is not loopback: set server.token`

Deliberate. The API can start pipelines, which means it can run shell commands,
so forge refuses to bind beyond loopback without a shared secret:

```bash
FORGE_TOKEN=$(openssl rand -hex 32) forge serve --host 0.0.0.0
```

Read [security.md](security.md) before doing this. A token keeps out strangers;
it is not a permission system.

### forge is using the wrong directory

It walks up from the current directory looking for `forge.yaml` or `.forge/`, the
way git does. Check with:

```bash
forge serve            # prints the resolved paths at startup
curl -s localhost:7777/api/v1/settings | jq '.project_root, .home'
```

Override with `-C /path/to/project` or `-c /path/to/forge.yaml`.

---

## Running

### A job fails immediately with a shell syntax error

Look at the echoed command in the log — the line beginning `$ `. If it is
truncated or has an unbalanced quote, it is a YAML quoting problem; see above.

### `working_dir "x" escapes the workspace`

`working_dir` must be relative and stay inside the job workspace. The check is
done after resolving symlinks, so a link inside the workspace pointing outside it
is also rejected.

### A job cannot see a file another job created

Jobs get **isolated workspaces** by default, which is what makes parallel jobs
safe. Files move between jobs through artifacts:

```yaml
build:
  commands: [make]
  artifacts:
    paths: [dist/]     # declare what to keep

test:
  needs: [build]       # and depend on the job that produced it
  commands: [ls dist/]
```

Without both halves, `test` sees a clean copy of the project and nothing else.

If you genuinely want a shared directory, set `workspace.mode: shared` — and
accept that concurrent jobs can then clobber each other.

### A job cannot see an environment variable

The host environment is not inherited. Add the variable to
`runner.env_passthrough`, or set it in the pipeline:

```yaml
variables:
  MY_VAR: value
```

### A job was skipped and I do not know why

The reason is recorded. `forge run-status <id>` shows it in the detail column:

- `dependency "build" failed`
- `dependency "test" was skipped`
- `condition was false: $DEPLOY_ENV == "production"`
- `when: on_failure, but nothing failed`
- `manual job skipped: approval was not requested`

### A manual job is always skipped

`forge run` skips manual jobs by default — a terminal invocation has nobody to
approve them. Either pre-approve, or wait:

```bash
forge run pipeline.yml --approve deploy
forge run pipeline.yml --wait-manual      # then approve elsewhere
```

Runs started from the dashboard wait for a click.

### `forge approve` says it cannot reach a server

Approval and cancellation act on **in-memory run state**, so they only mean
something inside the process actually running the pipeline. Start one:

```bash
forge serve                        # then approve from the CLI or the dashboard
forge run pipeline.yml --wait-manual   # or approve from another terminal
```

### A job timed out unexpectedly

Default is 30 minutes. Raise it per job:

```yaml
slow:
  timeout: 2h
  commands: [./long-thing.sh]
```

Note the whole process group is killed on timeout, so background children stop
too.

### Output stops with "output truncated by forge"

The per-stream size cap (16 MiB by default). Raise `logs.max_size`, or make the
job less chatty.

---

## Docker execution

### `cannot reach the Docker daemon`

Only jobs that name an `image` need Docker. Check the daemon:

```bash
docker info
```

On Linux, either add yourself to the `docker` group or run forge with the
privileges to reach `/var/run/docker.sock`. On macOS or Windows, start Docker
Desktop.

To run everything on the host instead, remove `image:` or set
`executor: local`.

### Files created in a container are owned by root

They should not be — forge passes `--user <uid>:<gid>`. If you see this, the
image probably has an `ENTRYPOINT` that switches user, or you overrode `User` on
the executor.

### The container cannot find a file

The workspace is mounted at `/forge/workspace` and that is the working directory.
Paths in your commands should be relative to it, exactly as for local execution.

---

## Server and dashboard

### `The dashboard has not been built`

The binary was built without a dashboard bundle:

```bash
make web && make build
```

Or `make`, which does both. The API works either way; only the UI is missing.

### `address already in use`

Another process — often a previous `forge serve` — holds the port:

```bash
forge serve --port 7788
# or find the holder
lsof -i :7777
```

### The dashboard shows stale data

Detail pages use SSE and converge on their own; lists poll every few seconds. If
something looks stuck, check the connection dot in the sidebar. A hard refresh
clears a cached `index.html` (assets are content-hashed, so they cannot go
stale).

### Logs are not streaming

The stream only opens while a job is active — a finished job's output is fetched
once instead. If a running job shows nothing, confirm it is producing output at
all (`forge logs <id> --job <name>`) and check for a proxy buffering the
response; forge sets `X-Accel-Buffering: no`, which nginx honours.

### `401 unauthorized`

A token is configured. Set it in the dashboard's Settings page, or send it:

```bash
curl -H "Authorization: Bearer $FORGE_TOKEN" http://127.0.0.1:7777/api/v1/runs
```

---

## State and storage

### Runs are stuck in `running` after a crash

`forge serve` reconciles these to `cancelled` at startup, with an explanation —
forge holds run state in memory, so a run left `running` in the database can only
be residue. Just start the server.

### `database is locked`

Should not happen: the pool is limited to one connection precisely to avoid it.
If it does, something else has the file open — another forge process against the
same `.forge`, or a SQLite browser. Close it.

### Disk usage is growing

Runs keep a workspace, logs and artifacts:

```bash
du -sh .forge/*
forge prune --dry-run     # see what would go
forge prune               # apply retention
```

Tune `workspace.keep_runs`, `artifacts.retention` and `logs.retention`.

### Can I delete `.forge/`?

Yes — it is all local state. You lose run history, logs and artifacts; nothing
else. forge recreates it on the next command.

---

## Building

### `no required module provides package …`

```bash
go mod download
```

### The frontend build fails

```bash
rm -rf web/node_modules && make web-deps
```

Node 20 or newer is required.

### Do I need a C compiler?

No. The SQLite driver is pure Go and `CGO_ENABLED=0` is set throughout, so the
result is a single static binary and cross-compilation works normally.

---

## Getting more detail

```bash
forge run pipeline.yml -v        # debug logging
forge run-status 7 --json | jq   # everything forge knows about a run
forge validate pipeline.yml      # the resolved plan, without running
curl -s localhost:7777/api/v1/settings | jq   # effective configuration
```
