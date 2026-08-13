package store

import (
	"context"
	"fmt"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

// Stats is the aggregate view of everything forge has executed, served at
// /api/v1/metrics and shown on the dashboard overview.
type Stats struct {
	Pipelines int64 `json:"pipelines"`

	RunsTotal     int64 `json:"runs_total"`
	RunsSucceeded int64 `json:"runs_succeeded"`
	RunsFailed    int64 `json:"runs_failed"`
	RunsCancelled int64 `json:"runs_cancelled"`
	RunsActive    int64 `json:"runs_active"`

	JobsTotal     int64 `json:"jobs_total"`
	JobsSucceeded int64 `json:"jobs_succeeded"`
	JobsFailed    int64 `json:"jobs_failed"`
	JobsSkipped   int64 `json:"jobs_skipped"`
	JobsRunning   int64 `json:"jobs_running"`
	JobsQueued    int64 `json:"jobs_queued"`

	AvgRunDurationMS int64 `json:"avg_run_duration_ms"`
	MaxRunDurationMS int64 `json:"max_run_duration_ms"`
	AvgJobDurationMS int64 `json:"avg_job_duration_ms"`

	ArtifactCount int64 `json:"artifact_count"`
	ArtifactBytes int64 `json:"artifact_bytes"`
	LogBytes      int64 `json:"log_bytes"`

	// SuccessRate is the share of finished runs that succeeded, 0..1.
	SuccessRate float64 `json:"success_rate"`
}

// Stats computes the aggregate counters. Everything is done in SQL so the
// endpoint stays cheap no matter how much history has accumulated.
func (db *DB) Stats(ctx context.Context) (*Stats, error) {
	var s Stats

	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipelines`).Scan(&s.Pipelines); err != nil {
		return nil, fmt.Errorf("store: count pipelines: %w", err)
	}

	err := db.sql.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(status = 'success'), 0),
			COALESCE(SUM(status = 'failed'), 0),
			COALESCE(SUM(status = 'cancelled'), 0),
			COALESCE(SUM(status IN ('queued', 'running', 'retrying', 'awaiting_manual')), 0),
			COALESCE(CAST(AVG(CASE WHEN duration_ms > 0 THEN duration_ms END) AS INTEGER), 0),
			COALESCE(MAX(duration_ms), 0)
		FROM runs`).Scan(
		&s.RunsTotal, &s.RunsSucceeded, &s.RunsFailed, &s.RunsCancelled, &s.RunsActive,
		&s.AvgRunDurationMS, &s.MaxRunDurationMS)
	if err != nil {
		return nil, fmt.Errorf("store: run stats: %w", err)
	}

	err = db.sql.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(status = 'success'), 0),
			COALESCE(SUM(status = 'failed'), 0),
			COALESCE(SUM(status = 'skipped'), 0),
			COALESCE(SUM(status = 'running'), 0),
			COALESCE(SUM(status IN ('queued', 'retrying', 'awaiting_manual')), 0),
			COALESCE(CAST(AVG(CASE WHEN duration_ms > 0 THEN duration_ms END) AS INTEGER), 0)
		FROM jobs`).Scan(
		&s.JobsTotal, &s.JobsSucceeded, &s.JobsFailed, &s.JobsSkipped,
		&s.JobsRunning, &s.JobsQueued, &s.AvgJobDurationMS)
	if err != nil {
		return nil, fmt.Errorf("store: job stats: %w", err)
	}

	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM artifacts`).Scan(&s.ArtifactCount, &s.ArtifactBytes); err != nil {
		return nil, fmt.Errorf("store: artifact stats: %w", err)
	}
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM logs`).Scan(&s.LogBytes); err != nil {
		return nil, fmt.Errorf("store: log stats: %w", err)
	}

	if finished := s.RunsSucceeded + s.RunsFailed; finished > 0 {
		s.SuccessRate = float64(s.RunsSucceeded) / float64(finished)
	}
	return &s, nil
}

// DurationPoint is one finished run, for the dashboard's duration trend.
type DurationPoint struct {
	RunID      int64        `json:"run_id"`
	Number     int64        `json:"number"`
	Pipeline   string       `json:"pipeline"`
	Status     model.Status `json:"status"`
	DurationMS int64        `json:"duration_ms"`
	FinishedAt int64        `json:"finished_at"`
}

// RecentDurations returns the most recent finished runs oldest-first, which is
// the order a trend chart wants to plot.
func (db *DB) RecentDurations(ctx context.Context, limit int) ([]DurationPoint, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT r.id, r.number, p.name, r.status, r.duration_ms, COALESCE(r.finished_at, r.created_at)
		FROM runs r JOIN pipelines p ON p.id = r.pipeline_id
		WHERE r.finished_at IS NOT NULL
		ORDER BY r.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent durations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DurationPoint
	for rows.Next() {
		var (
			p      DurationPoint
			status string
		)
		if err := rows.Scan(&p.RunID, &p.Number, &p.Pipeline, &status, &p.DurationMS, &p.FinishedAt); err != nil {
			return nil, fmt.Errorf("store: scan duration point: %w", err)
		}
		p.Status = model.Status(status)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse into chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// PrunableRuns returns the IDs of runs that fall outside the keep window,
// newest-first per pipeline, without deleting anything. Active runs are never
// included: a run still executing must not have its history pulled out from
// under it.
func (db *DB) PrunableRuns(ctx context.Context, keep int) ([]int64, error) {
	if keep <= 0 {
		return nil, nil
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id FROM (
			SELECT id, ROW_NUMBER() OVER (PARTITION BY pipeline_id ORDER BY id DESC) AS rn
			FROM runs
			WHERE status NOT IN ('queued', 'running', 'retrying', 'awaiting_manual')
		) WHERE rn > ?
		ORDER BY id`, keep)
	if err != nil {
		return nil, fmt.Errorf("store: find prunable runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PruneRuns deletes all but the newest keep runs of every pipeline and returns
// the IDs it removed, so the caller can clean up the matching directories.
func (db *DB) PruneRuns(ctx context.Context, keep int) ([]int64, error) {
	ids, err := db.PrunableRuns(ctx, keep)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := db.sql.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("store: prune run %d: %w", id, err)
		}
	}
	return ids, nil
}

// ExpiredLogs returns log records older than the retention window.
func (db *DB) ExpiredLogs(ctx context.Context, before time.Time) ([]*model.Log, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, attempt_id, job_id, run_id, stream, path, bytes, truncated, created_at
		FROM logs WHERE created_at <= ?`, toMillis(before))
	if err != nil {
		return nil, fmt.Errorf("store: list expired logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLogs(rows)
}
