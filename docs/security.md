# Security

This document describes what forge defends against, and — just as important —
what it does not.

## The one thing to understand

**forge runs your pipeline's commands through a shell, with the full
operating-system permissions of the user who invoked it.**

There is no sandbox around local execution. A pipeline file is executable
content. Treat it exactly as you would treat a shell script sitting in the same
repository:

- Read a pipeline before running it, the same way you would read a `Makefile` or
  an install script from someone else.
- A pipeline can read your SSH keys, delete your files, and make network calls.
  So can any script in that repository. forge does not widen that; it also does
  not narrow it.
- If you need a boundary, use the Docker executor.

This is a deliberate design decision, not an oversight. A local CI runner that
could not run your build tools would be useless, and a convincing sandbox is not
something a tool like this can offer honestly. Being clear about the boundary is
worth more than a security theatre that fails quietly.

### Why commands go through a shell

The pipeline format promises redirection, pipes and globbing:

```yaml
commands:
  - echo "hello" > dist/output.txt
  - cat a.txt b.txt | sort | uniq > merged.txt
```

Those are shell features. Running commands through `sh -c` is what makes them
work, and it means command strings are shell syntax, with all that implies.

What forge does do carefully: the script is passed on **stdin**, never as a
command-line argument. Command lines are visible to every user on the host via
`ps`, and a job's commands may embed values that should not be.

---

## What is defended

### Path traversal in artifacts

Artifact patterns come from a pipeline file, and matched filenames come from the
filesystem — where the job's own commands may have created symlinks. Both are
untrusted.

- Absolute paths and anything containing `..` are rejected at **validation**
  time, before the run starts.
- Glob matching is rooted at the job workspace through an `fs.FS`, so a pattern
  cannot address anything outside it however it is written.
- Every match is then resolved through the filesystem and re-checked. A symlink
  that resolves outside the workspace is **skipped**, not followed — the rest of
  the artifacts are still collected.
- Restoring an artifact validates both ends of the copy, so neither a hostile
  stored path nor a hostile destination can escape.
- The download endpoint re-validates the stored path against the artifact store
  root, so even a tampered database row cannot become an arbitrary file read.

```yaml
artifacts:
  paths:
    - ../../etc/passwd     # ✘ rejected at validation
    - /etc/passwd          # ✘ rejected at validation
```

```bash
ln -s /etc/passwd out/leak.txt    # ✘ skipped at collection; other files still collected
```

### `working_dir` confinement

A job's `working_dir` is resolved and checked to be inside the workspace,
**after symlink resolution** — a link inside the workspace pointing outside it
would otherwise be a way around the restriction.

### Secrets

Secrets come from `.forge/secrets.env` (a `KEY=VALUE` file) and from host
variables named in `secrets.env` config.

- **Never written to the database.** They live in the process for the lifetime of
  a run and nowhere else.
- **Masked before bytes reach disk.** Redaction happens on the log write path, so
  a secret is never persisted even briefly and cannot be recovered by reading the
  log file directly.
- **Masked across write boundaries.** Output is buffered per line, so a value
  split across two `write` calls is still caught. When a line is implausibly long
  the buffer flushes early but holds back a tail as long as the longest secret.
- **Never sent to the browser.** `GET /api/v1/settings` returns secret *names*
  only.
- **Scopeable per job.** `secrets: [DEPLOY_KEY]` gives a job that one value and
  nothing else. An absent list means all of them.
- **Permission-checked.** forge refuses to load a secrets file that is group- or
  world-readable, and tells you how to fix it.

```bash
chmod 600 .forge/secrets.env
```

Values shorter than four characters are **not** masked — replacing every `a` and
`1` in your logs would make them useless. Do not use trivially short secrets.

The redactor masks longest-first, so a secret containing another is replaced
whole rather than being left as `***-extended`.

### Environment isolation

Jobs do **not** inherit your shell environment. Only variables listed in
`runner.env_passthrough` come through — by default `PATH`, `HOME`, `LANG`,
`LC_ALL`, `TZ`, `TERM`, `USER`, `SHELL`, `TMPDIR`.

A job cannot read your `AWS_SECRET_ACCESS_KEY` or `GITHUB_TOKEN` just because
they happen to be exported in the terminal you ran forge from.

### Runaway jobs

- Every job has a timeout (default 30 minutes; configurable per job).
- On timeout or cancellation the **entire process group** is killed, not just the
  shell — so a dev server or test runner the job backgrounded dies with it
  instead of leaking. This is why the executor calls `Setpgid` and signals the
  negated PID.
- `SIGTERM` first, then `SIGKILL` after five seconds for anything that ignores it.
- Log output is capped per stream per attempt (16 MiB by default), with a visible
  truncation notice, so a job printing in a loop cannot fill the disk.
- Artifact collection is capped per file and per job.

### Network exposure

The API can start pipelines, which means it can run shell commands. So:

- It binds to `127.0.0.1` by default.
- **forge refuses to start** on any other address unless `server.token` (or
  `FORGE_TOKEN`) is set. This is enforced in config validation, not just
  documented.
- Token comparison is constant-time.
- CORS allows only the configured origins (the Vite dev server, by default).
- A panic in one handler becomes a 500 for that request, not a dead server.

Even with a token, think carefully before exposing this. A shared secret is
protection against strangers, not a permission system: anyone holding it can run
commands as you.

### Docker isolation

When a job names an `image`:

- It runs in an ephemeral container (`--rm`), removed when the job finishes.
- The workspace is bind-mounted at `/forge/workspace`; the rest of your
  filesystem is not visible.
- The container runs as **your uid:gid**, so files written into the mount are not
  left root-owned.
- Environment values are forwarded **by name** (`--env KEY`) with the value in
  the docker CLI's own environment, never as `KEY=VALUE` in argv where `ps` would
  show them.
- Containers are named `forge-<job>-<random>`, so a stray one can be traced back.
- Cancellation stops the container explicitly — killing the docker CLI does not
  stop what it started.

Add `--network=none` or a memory cap through `DockerExecutor.ExtraArgs` if you
want more.

**Mounting the Docker socket into a container is equivalent to giving it root on
the host.** The commented-out mount in `docker-compose.yml` says so. On a
personal machine running your own pipelines that may be a fine trade; on a shared
host it is not.

---

## What is *not* defended

Being explicit, because the gaps matter more than the defences:

- **Local execution is not sandboxed.** A pipeline can do anything you can do.
- **There are no users, roles or permissions.** Anyone who can reach the API — or
  run the CLI — can do everything.
- **The token is a shared secret, not an identity.** There is no audit trail of
  *who* did something, only of what happened.
- **`forge.yaml` is trusted.** It can point the artifact store and database
  anywhere you can write.
- **Resource limits are coarse.** Timeouts and output caps exist; CPU and memory
  limits do not, except through Docker.
- **Process groups are POSIX.** On Windows, killing the shell is the best
  available approximation, and a job that backgrounds work may leave children
  behind.
- **Nothing is encrypted at rest.** The database, logs and artifacts are ordinary
  files with ordinary permissions.
- **Artifacts are not signed.** The stored SHA-256 detects corruption, not
  tampering by someone who can write to the store.

---

## Running someone else's pipeline

If you must:

1. **Read it first.** `forge validate` prints the plan, but read the `commands`.
2. **Use Docker execution.** Give every job an `image`.
3. **Add `--network=none`** for jobs that should not reach the network.
4. **Load no secrets.** Empty `secrets.env`, empty `secrets.env` config.
5. **Run in a throwaway VM or container** if the pipeline is genuinely untrusted.

forge is built for running *your* pipelines on *your* machine. It is not a
multi-tenant CI system and should not be used as one.

---

## Reporting a problem

Open an issue. Since forge runs entirely locally with no service behind it, there
is no infrastructure to compromise and no coordinated disclosure process — but a
traversal escape, a redaction bypass, or a way to reach the API without the
configured token are all real bugs worth reporting.
