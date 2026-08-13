# Pipeline reference

Every key forge understands, with defaults and examples.

Validate before you run — it reports every problem at once and prints the
execution plan:

```bash
forge validate pipeline.yml
```

---

## Before anything else: YAML quoting

Three shapes bite everyone at least once. forge's parser is strict and will tell
you something is wrong, but the message comes from YAML and can be cryptic.

**A colon followed by a space turns a list item into a mapping.**

```yaml
commands:
  - echo "lint: clean" > report.txt      # ✘ YAML reads this as a key/value pair
  - 'echo "lint: clean" > report.txt'    # ✔ quote the whole command
```

The error looks like `cannot unmarshal !!map into string`.

**A space followed by `#` starts a comment.**

```yaml
commands:
  - echo "run #${FORGE_RUN_NUMBER}"      # ✘ truncated at the #, quote unbalanced
  - 'echo "run #${FORGE_RUN_NUMBER}"'    # ✔
```

The shell then gets a malformed command, so the job fails for a reason that has
nothing to do with your pipeline.

**Durations are strings with a unit.**

```yaml
timeout: 0     # ✘ a bare number is not a duration
timeout: 0s    # ✔
timeout: 30m   # ✔  also: 1h30m, 500ms, 2h
```

When in doubt, single-quote the command. Or use a block scalar, which needs no
escaping at all:

```yaml
commands:
  - |
    if [ "${FORGE_ATTEMPT}" -lt 3 ]; then
      echo "attempt ${FORGE_ATTEMPT}: retrying"
      exit 1
    fi
```

---

## Top level

```yaml
name: example              # required
description: Optional prose shown in the CLI and dashboard
variables:                 # environment variables for every job
  NODE_ENV: test
stages: [build, test]      # execution order for jobs without `needs`
defaults:                  # per-job settings each job may override
  timeout: 10m
jobs:                      # required, at least one
  build: { ... }
```

### `name`

**Required.** Identifies the pipeline. Runs are grouped and numbered per name, so
renaming starts a fresh history — and moving the file does not, which is usually
what you want.

### `variables`

Environment variables available to every job. Override per run with
`forge run -e KEY=VALUE`; those overrides apply to the run only and are never
written back to the stored pipeline.

### `stages`

Declares execution order. A job with no `needs` depends on every job in the
nearest preceding stage that has any:

```yaml
stages: [build, test, deploy]

jobs:
  compile: { stage: build, commands: [make] }
  unit:    { stage: test,  commands: [make test] }   # waits for compile
  lint:    { stage: test,  commands: [make lint] }   # runs beside unit
  ship:    { stage: deploy, commands: [make deploy] } # waits for both
```

Omit `stages` entirely and every job sits in one default stage, ordered purely by
`needs`. If you declare `stages`, every job must name one.

An empty stage is skipped rather than severing the chain, so an unused stage in
the list does not silently break ordering between the stages around it.

### `defaults`

Fills gaps in every job. Anything the job sets wins.

```yaml
defaults:
  image: node:22-alpine
  executor: docker
  timeout: 15m
  retry: { max: 1, backoff: 5s }
  working_dir: app
  env:
    LOG_LEVEL: debug
  secrets: [NPM_TOKEN]
```

---

## Jobs

```yaml
jobs:
  build:
    stage: build
    needs: [prepare]
    commands:
      - make build
    env:
      TARGET: release
    variables: {}            # merged after `env`; both spellings accepted
    secrets: [DEPLOY_KEY]
    working_dir: subdir
    timeout: 10m
    retry: { max: 2, backoff: 5s }
    image: golang:1.24
    executor: docker
    when: on_success
    if: $BRANCH == "main"
    allow_failure: false
    artifacts:
      paths: [dist/]
```

### `commands`

**Required** (unless `when: never`). Run in order, in **one shell session**, so
`cd` and shell variables persist between them:

```yaml
commands:
  - mkdir -p build
  - cd build
  - cmake ..          # runs inside build/
```

`set -e` is applied, so the job stops at the first non-zero exit. Each command is
echoed to stdout as `$ <command>` before it runs, the way CI logs do.

### `needs`

Jobs that must reach a terminal state before this one is considered. When set, it
replaces any ordering that stages would impose.

A dependency that **failed** blocks this job (it is skipped) unless that
dependency set `allow_failure: true`. A dependency that was **skipped** also
blocks, which is how a skip propagates down the graph.

Artifacts from every job in `needs` are restored into this job's workspace before
it runs. That is the mechanism behind the whole thing:

```yaml
build:
  commands: [make]
  artifacts:
    paths: [dist/]

test:
  needs: [build]
  commands: [ls dist/]     # dist/ is here because test needs build
```

### `env` and `variables`

Both merge into the job environment. Precedence, lowest first: pipeline
`variables` → pipeline `defaults.env` → job `env` → job `variables` → secrets.

Both spellings exist because pipelines converted from other CI systems use one or
the other.

### `secrets`

Names the secrets this job receives. **An empty or absent list means all of
them**; naming them narrows what the job can read, which is the better habit.

```yaml
deploy:
  secrets: [DEPLOY_KEY]     # this job cannot read any other secret
  commands: [./deploy.sh]
```

See [security.md](security.md) for where secrets come from and how they are
masked.

### `working_dir`

A path **relative to the job workspace**, created if missing. Absolute paths and
anything escaping the workspace are rejected at validation time — including via a
symlink, which is checked after resolution.

### `timeout`

Wall-clock limit for the whole job, across all its commands and including retries
of a single attempt. Default: `runner.default_timeout` (30m).

On expiry the whole process group is killed, so anything the job backgrounded
dies with it.

### `retry`

Either a count or a mapping:

```yaml
retry: 2                      # two extra attempts, three in total
retry:
  max: 2
  backoff: 5s                 # before the first retry; doubles each time, capped at 5m
```

Each attempt gets a fresh workspace, so a failed attempt cannot leave debris. The
job's `FORGE_ATTEMPT` variable tells the commands which try they are on.

Infrastructure failures — a workspace that cannot be created, a log that cannot
be opened — are **not** retried, because they will fail the same way again.

### `image` and `executor`

```yaml
python:
  image: python:3.12-alpine   # implies executor: docker
  commands: [python --version]

host:
  executor: local             # explicit, overrides the implication
  commands: [uname -a]
```

A job that names an image uses Docker. Everything else uses
`runner.default_executor` (`local`). Setting `executor: local` alongside an
`image` is rejected, because the image would be silently ignored.

The container gets the workspace bind-mounted at `/forge/workspace` and runs as
your uid:gid, so files it writes are owned by you.

### `when`

| Value | Runs when |
|---|---|
| `on_success` | *(default)* dependencies succeeded and nothing has failed |
| `on_failure` | something in the run has failed — for notifications and triage |
| `always` | regardless of upstream outcome — for cleanup |
| `manual` | only after approval |
| `never` | never; keeps the job in the file without deleting it |

```yaml
cleanup:
  needs: [build]
  when: always
  commands: [rm -rf /tmp/scratch]

notify:
  needs: [build]
  when: on_failure
  commands: [./notify-the-team.sh]
```

**Manual jobs** are skipped by `forge run` by default, because a terminal
invocation has nobody to approve them. Release one with `--approve`, or block on
it with `--wait-manual` and approve from another terminal or the dashboard. Runs
started from the dashboard wait for a click.

### `if`

Evaluated against the job environment before the job runs. Both `when` and `if`
must pass.

```yaml
if: $DEPLOY_ENV == "production"
if: $CI != "" && $SKIP_TESTS != "1"
if: !($DRAFT == "true")
if: $TAG =~ "^v[0-9]+\\.[0-9]+"
if: $BRANCH                          # truthy: non-empty and not 0/false/no/off
```

Grammar:

| | |
|---|---|
| Values | `$VAR`, `${VAR}`, `"quoted"`, `'quoted'`, `bare` |
| Comparison | `==`, `!=` |
| Regex | `=~`, `!~` — right side must be a literal pattern |
| Boolean | `&&`, `\|\|`, `!`, parentheses |
| Truthiness | empty, `0`, `false`, `no`, `off`, `null`, `nil` are false |

An unset variable is the empty string, so `$MISSING == ""` is the idiomatic "is
this set" check. A condition that fails to evaluate skips the job and logs why.

If you need more than this, use a command that exits non-zero instead.

### `allow_failure`

The job may fail without failing the run, and its dependents still proceed.

```yaml
flaky-integration:
  allow_failure: true
  commands: [./sometimes-works.sh]
```

The job is still recorded as `failed`; the run is not.

---

## Artifacts

```yaml
artifacts:
  name: build-output          # label in the UI; defaults to the job name
  paths:
    - dist/                   # a directory, recursively
    - "**/*.log"              # a glob
    - coverage.xml            # one file
  exclude:
    - "**/*.tmp"
  when: on_success            # on_success (default) | on_failure | always
  expire_in: 168h             # overrides artifacts.retention
```

### `paths`

Globs relative to the job workspace:

| Pattern | Matches |
|---|---|
| `dist/` or `dist` | everything under `dist/`, recursively |
| `*.txt` | `.txt` files at the top level |
| `**/*.txt` | `.txt` files at any depth |
| `build/*.{js,css}` | brace alternatives |
| `reports/coverage.xml` | one file |

Absolute paths and anything containing `..` are rejected at validation. Matching
is rooted at the workspace, so a pattern cannot address anything outside it
however it is written, and a symlink that resolves outside is skipped rather than
followed.

### `when`

`on_failure` and `always` are how you capture evidence from a job that broke:

```yaml
integration:
  commands: [./run-tests.sh]
  artifacts:
    paths: [logs/, screenshots/]
    when: on_failure
```

---

## Variables forge sets

Available in every job:

| Variable | Value |
|---|---|
| `CI` | `true` |
| `FORGE` | `true` |
| `FORGE_PIPELINE` | pipeline name |
| `FORGE_RUN_ID` | database run ID |
| `FORGE_RUN_NUMBER` | per-pipeline run number |
| `FORGE_JOB` | job name |
| `FORGE_STAGE` | stage name |
| `FORGE_ATTEMPT` | attempt number, from 1 |
| `FORGE_WORKSPACE` | absolute path to this job's workspace |
| `FORGE_PROJECT_DIR` | absolute path to the project root |

The host environment is **not** inherited. Only variables listed in
`runner.env_passthrough` (by default `PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`,
`TERM`, `USER`, `SHELL`, `TMPDIR`) come through.

---

## Validation

forge rejects, before running anything:

- a missing `name`, or no jobs
- a job with no `commands` (unless `when: never`)
- `needs` naming a job that does not exist, or itself, or a duplicate
- **dependency cycles**, reported as a concrete path: `a -> b -> c -> a`
- a stage not listed in `stages`, or a duplicate stage
- an invalid `when`, `executor`, or `artifacts.when`
- `executor: local` together with an `image`
- negative `timeout`, `retry.max` or `retry.backoff`
- a `working_dir` or artifact path that is absolute or escapes the workspace
- an `if` expression that does not parse

All problems are reported together.

---

## A complete example

```yaml
name: web-app
description: Build, test and deploy the web application

variables:
  NODE_ENV: test
  DEPLOY_ENV: staging

stages: [install, verify, build, deploy]

defaults:
  timeout: 10m
  retry: { max: 1, backoff: 3s }

jobs:
  install:
    stage: install
    commands:
      - npm ci
    artifacts:
      paths: [node_modules/]
      expire_in: 24h

  lint:
    stage: verify
    needs: [install]
    commands: [npm run lint]

  test:
    stage: verify
    needs: [install]
    commands:
      - npm test -- --coverage
    artifacts:
      paths: [coverage/]
      when: always

  typecheck:
    stage: verify
    needs: [install]
    commands: [npm run typecheck]
    allow_failure: true

  build:
    stage: build
    needs: [lint, test]
    commands:
      - npm run build
    artifacts:
      paths: [dist/]

  deploy-staging:
    stage: deploy
    needs: [build]
    if: $DEPLOY_ENV == "staging"
    secrets: [STAGING_TOKEN]
    commands: [./scripts/deploy.sh staging]

  deploy-production:
    stage: deploy
    needs: [build]
    if: $DEPLOY_ENV == "production"
    when: manual
    secrets: [PRODUCTION_TOKEN]
    commands: [./scripts/deploy.sh production]

  notify:
    stage: deploy
    needs: [build]
    when: on_failure
    commands: ['echo "build failed for run #${FORGE_RUN_NUMBER}"']
```

`lint`, `test` and `typecheck` run in parallel. `build` waits for `lint` and
`test` but not `typecheck`, which is allowed to fail. `deploy-production` needs
both a matching condition and a human.
