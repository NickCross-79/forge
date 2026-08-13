package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

// CreateRun inserts a run and assigns it the next number for its pipeline.
//
// The number is allocated inside the same transaction as the insert, so two runs
// started at the same moment cannot receive the same number.
func (db *DB) CreateRun(ctx context.Context, run *model.Run) (*model.Run, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin create run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(number), 0) + 1 FROM runs WHERE pipeline_id = ?`,
		run.PipelineID).Scan(&next); err != nil {
		return nil, fmt.Errorf("store: allocate run number: %w", err)
	}

	now := nowMillis()
	if run.Status == "" {
		run.Status = model.StatusQueued
	}
	if run.Trigger == "" {
		run.Trigger = model.TriggerCLI
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO runs (pipeline_id, number, status, trigger, spec_yaml, work_dir, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?)`,
		run.PipelineID, next, string(run.Status), string(run.Trigger), run.SpecYAML, run.WorkDir, now)
	if err != nil {
		return nil, fmt.Errorf("store: insert run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: run id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit create run: %w", err)
	}

	run.ID = id
	run.Number = next
	run.CreatedAt = fromMillis(now)
	return run, nil
}

const runColumns = `r.id, r.pipeline_id, r.number, r.status, r.trigger, r.spec_yaml, r.work_dir,
	r.error, r.created_at, r.started_at, r.finished_at, r.duration_ms`

func scanRun(row interface{ Scan(...any) error }) (*model.Run, error) {
	var (
		r          model.Run
		status     string
		trigger    string
		createdMS  int64
		startedMS  sql.NullInt64
		finishedMS sql.NullInt64
		name       sql.NullString
	)
	if err := row.Scan(&r.ID, &r.PipelineID, &r.Number, &status, &trigger, &r.SpecYAML, &r.WorkDir,
		&r.Error, &createdMS, &startedMS, &finishedMS, &r.DurationMS, &name); err != nil {
		return nil, err
	}
	r.Status = model.Status(status)
	r.Trigger = model.Trigger(trigger)
	r.CreatedAt = fromMillis(createdMS)
	r.StartedAt = fromNullMillis(startedMS)
	r.FinishedAt = fromNullMillis(finishedMS)
	r.PipelineName = name.String
	return &r, nil
}

// RunByID loads a single run.
func (db *DB) RunByID(ctx context.Context, id int64) (*model.Run, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT `+runColumns+`, p.name
		FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		WHERE r.id = ?`, id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run: %w", err)
	}
	return run, nil
}

// RunFilter narrows a run listing.
type RunFilter struct {
	PipelineID int64
	// Status, when non-empty, keeps only runs in one of these states.
	Status []model.Status
	Limit  int
	Offset int
}

// ListRuns returns runs newest first.
func (db *DB) ListRuns(ctx context.Context, f RunFilter) ([]*model.Run, error) {
	var (
		where []string
		args  []any
	)
	if f.PipelineID > 0 {
		where = append(where, "r.pipeline_id = ?")
		args = append(args, f.PipelineID)
	}
	if len(f.Status) > 0 {
		placeholders := make([]string, len(f.Status))
		for i, s := range f.Status {
			placeholders[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "r.status IN ("+strings.Join(placeholders, ", ")+")")
	}

	query := `SELECT ` + runColumns + `, p.name
		FROM runs r JOIN pipelines p ON p.id = r.pipeline_id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY r.id DESC"

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	query += " LIMIT ?"
	args = append(args, limit)
	if f.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, f.Offset)
	}

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// CountRuns returns the number of runs matching a filter, for pagination.
func (db *DB) CountRuns(ctx context.Context, f RunFilter) (int64, error) {
	var (
		where []string
		args  []any
	)
	if f.PipelineID > 0 {
		where = append(where, "pipeline_id = ?")
		args = append(args, f.PipelineID)
	}
	if len(f.Status) > 0 {
		placeholders := make([]string, len(f.Status))
		for i, s := range f.Status {
			placeholders[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "status IN ("+strings.Join(placeholders, ", ")+")")
	}
	query := "SELECT COUNT(*) FROM runs"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	var n int64
	if err := db.sql.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count runs: %w", err)
	}
	return n, nil
}

// StartRun marks a run as running and stamps its start time.
func (db *DB) StartRun(ctx context.Context, id int64, at time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`,
		string(model.StatusRunning), toMillis(at), id)
	if err != nil {
		return fmt.Errorf("store: start run: %w", err)
	}
	return nil
}

// FinishRun records a run's terminal state, its finish time and its duration.
func (db *DB) FinishRun(ctx context.Context, id int64, status model.Status, at time.Time, runErr string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, finished_at = ?, error = ?,
		    duration_ms = CASE
		        WHEN started_at IS NOT NULL THEN ? - started_at
		        ELSE 0
		    END
		WHERE id = ?`,
		string(status), toMillis(at), runErr, toMillis(at), id)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	return nil
}

// SetRunStatus updates a run's status without touching its timestamps. Used for
// intermediate transitions such as a run entering the awaiting-approval state.
func (db *DB) SetRunStatus(ctx context.Context, id int64, status model.Status) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE runs SET status = ? WHERE id = ?`, string(status), id)
	if err != nil {
		return fmt.Errorf("store: set run status: %w", err)
	}
	return nil
}

// ReconcileInterruptedRuns marks runs and jobs that were in flight when the
// process died as cancelled.
//
// forge holds run state in memory while executing, so a run left as "running" in
// the database can only be the residue of a crash or a kill. Cleaning it up at
// startup is what keeps the database honest across restarts.
func (db *DB) ReconcileInterruptedRuns(ctx context.Context) (int64, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := nowMillis()
	active := []any{
		string(model.StatusQueued), string(model.StatusRunning),
		string(model.StatusRetrying), string(model.StatusAwaitingManual),
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET status = ?, error = ?, finished_at = ?
		WHERE status IN (?, ?, ?, ?)`,
		append([]any{string(model.StatusCancelled), "interrupted: forge exited while this job was active", now}, active...)...,
	); err != nil {
		return 0, fmt.Errorf("store: reconcile jobs: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, error = ?, finished_at = ?,
		    duration_ms = CASE WHEN started_at IS NOT NULL THEN ? - started_at ELSE 0 END
		WHERE status IN (?, ?, ?, ?)`,
		append([]any{string(model.StatusCancelled), "interrupted: forge exited while this run was active", now, now}, active...)...,
	)
	if err != nil {
		return 0, fmt.Errorf("store: reconcile runs: %w", err)
	}
	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit reconcile: %w", err)
	}
	return n, nil
}

// DeleteRun removes a run and everything attached to it.
func (db *DB) DeleteRun(ctx context.Context, id int64) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %d: %w", id, ErrNotFound)
	}
	return nil
}
