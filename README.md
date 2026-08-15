<h1 align="center">forge</h1>

<p align="center">
  <strong>A CI/CD runner that lives on your machine.</strong><br>
  Define pipelines in YAML, run them with dependency-aware concurrency, and inspect
  logs, artifacts and history from a CLI or a built-in web dashboard.
</p>

<p align="center">
  <em>No cloud. No account. No server to deploy. $0/month, indefinitely.</em>
</p>

---

## Why

CI is usually the last thing you can run locally. You push, wait, read a log in a
browser tab, and push again. forge exists for the loop before that one: the same
pipeline, the same dependency graph, the same artifacts — on your laptop, in a
second, with no network involved.

It is deliberately a **subset** of GitHub Actions or GitLab CI, not a replacement.
It does the part that is useful locally: run a graph of jobs, in the right order,
in parallel where it can, and show you exactly what happened.

## What you get

- **A dependency-aware scheduler.** Jobs with no unmet dependencies run
  concurrently up to a limit you set. Cycles are rejected before anything runs.
- **Two execution backends.** Commands run as local child processes, or inside
  ephemeral Docker containers when a job names an `image`. Docker is never
  required unless you ask for it.
- **Artifacts that flow.** Files a job declares are collected, stored, and
  restored into the workspace of every job that `needs` it.
- **Isolated workspaces.** Each job gets its own copy of the project, so parallel
  jobs cannot corrupt each other.
- **Logs, kept properly.** stdout and stderr captured separately, streamed live
  to the browser, with secrets masked before bytes reach disk.
- **Retries, timeouts, conditions, manual gates.** The things that make a
  pipeline survive contact with reality.
- **History that persists.** SQLite, with migrations. Restarting forge — or
  crashing it — does not corrupt or lose state.
- **One binary.** The dashboard is compiled into it. `forge serve` is a single
  process.

## Install

Requires [Go 1.24+](https://go.dev/dl/) and, to build the dashboard,
[Node 20+](https://nodejs.org).

```bash
git clone https://github.com/nickcross-79/forge.git
cd forge
make            # builds the dashboard, then the binary, into bin/forge
```

Or without the dashboard, if you only want the CLI:

```bash
go build -o bin/forge ./cmd/forge
```

There is no CGO and no C toolchain involved: the SQLite driver is pure Go, so the
result is a single static binary.

## Sixty-second tour

```bash
cd ~/your-project
forge init                     # writes forge.yaml, pipeline.yml and .forge/
forge validate pipeline.yml    # check it, and see the execution plan
forge run pipeline.yml         # run it
```

```
example run #1
3 job(s), concurrency 4, isolated workspaces

build         │ $ echo "Building"
build         │ Building
build         │ $ mkdir -p dist
test          │ $ cat dist/output.txt
test          │ hello
package       │ $ echo "Packaging"

   JOB      STAGE    STATUS   TIME  DETAIL
✔  build    build    success  5ms
✔  test     test     success  3ms
✔  package  package  success  2ms

✔ run #1 success in 13ms
```

Then look around:

```bash
forge runs               # history
forge run-status 1       # one run in detail
forge logs 1             # captured output
forge artifacts 1        # collected files
forge serve              # the dashboard, on http://127.0.0.1:7777
```

## A pipeline

```yaml
name: example

variables:
  NODE_ENV: test

stages: [build, test, package]

jobs:
  build:
    stage: build
    commands:
      - echo "Building"
      - mkdir -p dist
      - echo "hello" > dist/output.txt
    artifacts:
      paths:
        - dist/

  test:
    stage: test
    needs: [build]
    commands:
      - echo "Running tests"
      - cat dist/output.txt      # restored from `build` because `test` needs it

  package:
    stage: package
    needs: [test]
    commands:
      - echo "Packaging"
```

Also supported: `retry`, `timeout`, `when` (`on_success` / `on_failure` /
`always` / `manual` / `never`), `if` conditions, `allow_failure`, `working_dir`,
`image`, `executor`, per-job `secrets`, and artifact `exclude` / `when` /
`expire_in`. The full grammar is in
**[docs/pipeline-reference.md](docs/pipeline-reference.md)**.

More examples in **[`examples/`](examples/)**: [parallel fan-out](examples/parallel.yml),
[failure handling](examples/failure-handling.yml), [conditions and manual gates](examples/conditional.yml),
[containers](examples/docker.yml), [secrets](examples/secrets.yml).

## The dashboard

`forge serve` opens a local dashboard on `127.0.0.1:7777`: an overview with
success rate and duration trend, the job graph drawn as a DAG, live log
streaming over SSE, artifact downloads, run history, and manual-job approval.

It is served from the binary itself — there is nothing else to start.

## Security, briefly

**forge runs your pipeline's commands through a shell, with your operating-system
permissions.** A pipeline file is executable content: treat it exactly as you
would a shell script in the same repository. There is no sandbox around local
execution, by design — Docker execution is the isolation boundary when you need
one.

Because the API can start pipelines, it binds to `127.0.0.1` and **refuses to
bind anywhere else without a token**.

What forge does defend: path traversal in artifacts (including via symlinks),
secrets in logs (masked before they are written), runaway jobs (timeouts, and
the whole process group is killed, not just the shell), and the host environment
(jobs see only an allow-list, not your whole shell).

The full picture, including what is *not* defended, is in
**[docs/security.md](docs/security.md)**. It is worth reading before you point
forge at something you did not write.

## Documentation

| | |
|---|---|
| [Architecture](docs/architecture.md) | How the pieces fit, and why |
| [Pipeline reference](docs/pipeline-reference.md) | Every YAML key |
| [CLI reference](docs/cli.md) | Every command and flag |
| [API reference](docs/api.md) | REST endpoints and SSE streams |
| [Security](docs/security.md) | Trust boundaries and defences |
| [Troubleshooting](docs/troubleshooting.md) | When something goes wrong |

## Development

```bash
make dev          # API on :7777 and the Vite dev server on :5173, together
make test         # Go suite with the race detector
make ci           # lint + tests + typecheck + frontend tests + build
make e2e          # drive the built binary end to end
```

Layout:

```
cmd/forge          entrypoint
internal/
  pipeline         YAML parsing, validation, the job graph
  scheduler        dependency resolution, concurrency, retries, cancellation
  executor         local and Docker backends
  artifacts        collection, storage, traversal defences
  logs             capture, secret redaction, live-tail notifications
  store            SQLite with embedded migrations
  api              REST + SSE + the embedded dashboard
  cli              commands
web/               React + TypeScript dashboard
```

## Zero cost, for real

forge uses local CPU, local disk, and open-source dependencies. It makes no
network calls of its own. Nothing expires, nothing phones home, and there is no
tier to run out of.

## Licence

MIT. See [LICENSE](LICENSE).
