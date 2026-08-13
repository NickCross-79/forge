-- Initial schema for forge.
--
-- Timestamps are stored as Unix milliseconds in INTEGER columns rather than as
-- text or a driver-specific DATETIME type. That keeps ordering, comparison and
-- arithmetic exact, and avoids depending on how a particular SQLite driver
-- chooses to parse date strings.
--
-- Log bodies and artifact blobs live on disk; these tables hold only the
-- metadata and the path, so a chatty job cannot bloat the database.

CREATE TABLE pipelines (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    source_path TEXT    NOT NULL DEFAULT '',
    spec_yaml   TEXT    NOT NULL DEFAULT '',
    checksum    TEXT    NOT NULL DEFAULT '',
    description TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    -- Monotonic per pipeline, so the UI can show "run #7 of example".
    number      INTEGER NOT NULL,
    status      TEXT    NOT NULL,
    trigger     TEXT    NOT NULL DEFAULT 'cli',
    -- Snapshot of the pipeline as it was when this run started, so history stays
    -- explainable after the file on disk changes.
    spec_yaml   TEXT    NOT NULL DEFAULT '',
    work_dir    TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    UNIQUE (pipeline_id, number)
);

CREATE INDEX idx_runs_pipeline ON runs (pipeline_id, id DESC);
CREATE INDEX idx_runs_status   ON runs (status);
CREATE INDEX idx_runs_created  ON runs (created_at DESC);

CREATE TABLE jobs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    stage         TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL,
    -- JSON array of job names this job depends on.
    needs         TEXT    NOT NULL DEFAULT '[]',
    executor      TEXT    NOT NULL DEFAULT 'local',
    image         TEXT    NOT NULL DEFAULT '',
    manual        INTEGER NOT NULL DEFAULT 0,
    allow_failure INTEGER NOT NULL DEFAULT 0,
    attempts      INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 1,
    exit_code     INTEGER,
    error         TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    started_at    INTEGER,
    finished_at   INTEGER,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (run_id, name)
);

CREATE INDEX idx_jobs_run    ON jobs (run_id);
CREATE INDEX idx_jobs_status ON jobs (status);

CREATE TABLE job_attempts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    number      INTEGER NOT NULL,
    status      TEXT    NOT NULL,
    exit_code   INTEGER,
    error       TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER,
    finished_at INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    UNIQUE (job_id, number)
);

CREATE INDEX idx_attempts_job ON job_attempts (job_id);

CREATE TABLE logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id INTEGER NOT NULL REFERENCES job_attempts(id) ON DELETE CASCADE,
    job_id     INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    run_id     INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    -- 'stdout' or 'stderr'; the two streams are captured independently.
    stream     TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    bytes      INTEGER NOT NULL DEFAULT 0,
    truncated  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (attempt_id, stream)
);

CREATE INDEX idx_logs_job ON logs (job_id);
CREATE INDEX idx_logs_run ON logs (run_id);

CREATE TABLE artifacts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    job_id     INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    job_name   TEXT    NOT NULL DEFAULT '',
    -- Path relative to the job workspace, as the pipeline author would name it.
    path       TEXT    NOT NULL,
    -- Path relative to the artifact store root, where the bytes actually live.
    store_path TEXT    NOT NULL,
    size       INTEGER NOT NULL DEFAULT 0,
    sha256     TEXT    NOT NULL DEFAULT '',
    mode       INTEGER NOT NULL DEFAULT 420, -- 0644
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    UNIQUE (job_id, path)
);

CREATE INDEX idx_artifacts_run     ON artifacts (run_id);
CREATE INDEX idx_artifacts_expires ON artifacts (expires_at);

CREATE TABLE run_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    job_id     INTEGER REFERENCES jobs(id) ON DELETE CASCADE,
    job_name   TEXT    NOT NULL DEFAULT '',
    type       TEXT    NOT NULL,
    from_status TEXT   NOT NULL DEFAULT '',
    to_status   TEXT   NOT NULL DEFAULT '',
    message    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_events_run ON run_events (run_id, id);
