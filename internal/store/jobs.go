package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

// CreateJobs inserts every job of a run in one transaction, so a run is never
// observed with a partially-populated job list.
func (db *DB) CreateJobs(ctx context.Context, runID int64, jobs []*model.Job) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin create jobs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO jobs (run_id, name, stage, status, needs, executor, image, manual,
		                  allow_failure, attempts, max_attempts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare insert job: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := nowMillis()
	for _, job := range jobs {
		needs, err := json.Marshal(nonNilStrings(job.Needs))
		if err != nil {
			return fmt.Errorf("store: encode needs for %q: %w", job.Name, err)
		}
		if job.Status == "" {
			job.Status = model.StatusQueued
		}
		if job.MaxAttempts < 1 {
			job.MaxAttempts = 1
		}
		res, err := stmt.ExecContext(ctx, runID, job.Name, job.Stage, string(job.Status), string(needs),
			string(job.Executor), job.Image, job.Manual, job.AllowFailure, job.MaxAttempts, now)
		if err != nil {
			return fmt.Errorf("store: insert job %q: %w", job.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: job id for %q: %w", job.Name, err)
		}
		job.ID = id
		job.RunID = runID
		job.CreatedAt = fromMillis(now)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit create jobs: %w", err)
	}
	return nil
}

const jobColumns = `id, run_id, name, stage, status, needs, executor, image, manual, allow_failure,
	attempts, max_attempts, exit_code, error, created_at, started_at, finished_at, duration_ms`

func scanJob(row interface{ Scan(...any) error }) (*model.Job, error) {
	var (
		j          model.Job
		status     string
		executor   string
		needsJSON  string
		exitCode   sql.NullInt64
		createdMS  int64
		startedMS  sql.NullInt64
		finishedMS sql.NullInt64
	)
	if err := row.Scan(&j.ID, &j.RunID, &j.Name, &j.Stage, &status, &needsJSON, &executor, &j.Image,
		&j.Manual, &j.AllowFailure, &j.Attempts, &j.MaxAttempts, &exitCode, &j.Error,
		&createdMS, &startedMS, &finishedMS, &j.DurationMS); err != nil {
		return nil, err
	}
	j.Status = model.Status(status)
	j.Executor = model.ExecutorKind(executor)
	j.ExitCode = fromNullInt(exitCode)
	j.CreatedAt = fromMillis(createdMS)
	j.StartedAt = fromNullMillis(startedMS)
	j.FinishedAt = fromNullMillis(finishedMS)
	if err := json.Unmarshal([]byte(needsJSON), &j.Needs); err != nil {
		return nil, fmt.Errorf("decode needs: %w", err)
	}
	if j.Needs == nil {
		j.Needs = []string{}
	}
	return &j, nil
}

// JobByID loads one job.
func (db *DB) JobByID(ctx context.Context, id int64) (*model.Job, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("job %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job: %w", err)
	}
	return job, nil
}

// JobByName loads a job by run and name, which is how the CLI and API address
// individual jobs (`forge approve 7 deploy`).
func (db *DB) JobByName(ctx context.Context, runID int64, name string) (*model.Job, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE run_id = ? AND name = ?`, runID, name)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("job %q in run %d: %w", name, runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job: %w", err)
	}
	return job, nil
}

// ListJobs returns a run's jobs in creation order, which matches the pipeline's
// stable stage ordering.
func (db *DB) ListJobs(ctx context.Context, runID int64) ([]*model.Job, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// SetJobStatus updates a job's state, stamping start and finish times as the
// state machine reaches them.
func (db *DB) SetJobStatus(ctx context.Context, id int64, status model.Status, at time.Time) error {
	ms := toMillis(at)
	switch {
	case status == model.StatusRunning:
		// started_at is only set on the first attempt so that a retried job still
		// reports the wall-clock time from its first start.
		_, err := db.sql.ExecContext(ctx,
			`UPDATE jobs SET status = ?, started_at = COALESCE(started_at, ?) WHERE id = ?`,
			string(status), ms, id)
		if err != nil {
			return fmt.Errorf("store: set job running: %w", err)
		}
	case status.Terminal():
		_, err := db.sql.ExecContext(ctx, `
			UPDATE jobs SET status = ?, finished_at = ?,
			    duration_ms = CASE WHEN started_at IS NOT NULL THEN ? - started_at ELSE 0 END
			WHERE id = ?`,
			string(status), ms, ms, id)
		if err != nil {
			return fmt.Errorf("store: finish job: %w", err)
		}
	default:
		_, err := db.sql.ExecContext(ctx, `UPDATE jobs SET status = ? WHERE id = ?`, string(status), id)
		if err != nil {
			return fmt.Errorf("store: set job status: %w", err)
		}
	}
	return nil
}

// FinishJob records a job's terminal state together with its exit code and error.
func (db *DB) FinishJob(ctx context.Context, id int64, status model.Status, exitCode *int, jobErr string, at time.Time) error {
	ms := toMillis(at)
	_, err := db.sql.ExecContext(ctx, `
		UPDATE jobs SET status = ?, exit_code = ?, error = ?, finished_at = ?,
		    duration_ms = CASE WHEN started_at IS NOT NULL THEN ? - started_at ELSE 0 END
		WHERE id = ?`,
		string(status), toNullInt(exitCode), jobErr, ms, ms, id)
	if err != nil {
		return fmt.Errorf("store: finish job: %w", err)
	}
	return nil
}

// IncrementJobAttempts bumps the attempt counter shown in listings.
func (db *DB) IncrementJobAttempts(ctx context.Context, id int64) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE jobs SET attempts = attempts + 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: increment attempts: %w", err)
	}
	return nil
}

// --- attempts ---------------------------------------------------------------

// CreateAttempt records the start of one try at a job.
func (db *DB) CreateAttempt(ctx context.Context, jobID int64, number int, at time.Time) (*model.Attempt, error) {
	ms := toMillis(at)
	res, err := db.sql.ExecContext(ctx, `
		INSERT INTO job_attempts (job_id, number, status, started_at)
		VALUES (?, ?, ?, ?)`,
		jobID, number, string(model.StatusRunning), ms)
	if err != nil {
		return nil, fmt.Errorf("store: create attempt: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: attempt id: %w", err)
	}
	started := fromMillis(ms)
	return &model.Attempt{
		ID: id, JobID: jobID, Number: number,
		Status: model.StatusRunning, StartedAt: &started,
	}, nil
}

// FinishAttempt records the outcome of one try.
func (db *DB) FinishAttempt(ctx context.Context, id int64, status model.Status, exitCode *int, attemptErr string, at time.Time) error {
	ms := toMillis(at)
	_, err := db.sql.ExecContext(ctx, `
		UPDATE job_attempts SET status = ?, exit_code = ?, error = ?, finished_at = ?,
		    duration_ms = CASE WHEN started_at IS NOT NULL THEN ? - started_at ELSE 0 END
		WHERE id = ?`,
		string(status), toNullInt(exitCode), attemptErr, ms, ms, id)
	if err != nil {
		return fmt.Errorf("store: finish attempt: %w", err)
	}
	return nil
}

// ListAttempts returns a job's attempts oldest first.
func (db *DB) ListAttempts(ctx context.Context, jobID int64) ([]*model.Attempt, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, job_id, number, status, exit_code, error, started_at, finished_at, duration_ms
		FROM job_attempts WHERE job_id = ? ORDER BY number`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.Attempt{}
	for rows.Next() {
		var (
			a          model.Attempt
			status     string
			exitCode   sql.NullInt64
			startedMS  sql.NullInt64
			finishedMS sql.NullInt64
		)
		if err := rows.Scan(&a.ID, &a.JobID, &a.Number, &status, &exitCode, &a.Error,
			&startedMS, &finishedMS, &a.DurationMS); err != nil {
			return nil, fmt.Errorf("store: scan attempt: %w", err)
		}
		a.Status = model.Status(status)
		a.ExitCode = fromNullInt(exitCode)
		a.StartedAt = fromNullMillis(startedMS)
		a.FinishedAt = fromNullMillis(finishedMS)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// --- logs -------------------------------------------------------------------

// RecordLog registers the on-disk location of one captured stream. It is called
// when the stream is opened and again when it closes, so the row exists (and is
// streamable) while the job is still running.
func (db *DB) RecordLog(ctx context.Context, l *model.Log) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO logs (attempt_id, job_id, run_id, stream, path, bytes, truncated, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (attempt_id, stream) DO UPDATE SET
			bytes     = excluded.bytes,
			truncated = excluded.truncated`,
		l.AttemptID, l.JobID, l.RunID, string(l.Stream), l.Path, l.Bytes, l.Truncated, nowMillis())
	if err != nil {
		return fmt.Errorf("store: record log: %w", err)
	}
	return nil
}

// ListLogs returns the log records for a job, newest attempt last.
func (db *DB) ListLogs(ctx context.Context, jobID int64) ([]*model.Log, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT l.id, l.attempt_id, l.job_id, l.run_id, l.stream, l.path, l.bytes, l.truncated, l.created_at
		FROM logs l
		JOIN job_attempts a ON a.id = l.attempt_id
		WHERE l.job_id = ?
		ORDER BY a.number, l.stream`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLogs(rows)
}

// LogsForRun returns every log record in a run, used by `forge logs <run>`.
func (db *DB) LogsForRun(ctx context.Context, runID int64) ([]*model.Log, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT l.id, l.attempt_id, l.job_id, l.run_id, l.stream, l.path, l.bytes, l.truncated, l.created_at
		FROM logs l
		JOIN job_attempts a ON a.id = l.attempt_id
		WHERE l.run_id = ?
		ORDER BY l.job_id, a.number, l.stream`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list run logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLogs(rows)
}

// LogForAttempt returns one stream of one attempt.
func (db *DB) LogForAttempt(ctx context.Context, attemptID int64, stream model.Stream) (*model.Log, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, attempt_id, job_id, run_id, stream, path, bytes, truncated, created_at
		FROM logs WHERE attempt_id = ? AND stream = ?`, attemptID, string(stream))
	var (
		l         model.Log
		stream1   string
		createdMS int64
	)
	err := row.Scan(&l.ID, &l.AttemptID, &l.JobID, &l.RunID, &stream1, &l.Path, &l.Bytes, &l.Truncated, &createdMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("log for attempt %d stream %s: %w", attemptID, stream, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get log: %w", err)
	}
	l.Stream = model.Stream(stream1)
	l.CreatedAt = fromMillis(createdMS)
	return &l, nil
}

func scanLogs(rows *sql.Rows) ([]*model.Log, error) {
	out := []*model.Log{}
	for rows.Next() {
		var (
			l         model.Log
			stream    string
			createdMS int64
		)
		if err := rows.Scan(&l.ID, &l.AttemptID, &l.JobID, &l.RunID, &stream, &l.Path,
			&l.Bytes, &l.Truncated, &createdMS); err != nil {
			return nil, fmt.Errorf("store: scan log: %w", err)
		}
		l.Stream = model.Stream(stream)
		l.CreatedAt = fromMillis(createdMS)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
