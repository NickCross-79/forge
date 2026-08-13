package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

// RecordArtifact stores metadata for one collected file. Re-collecting the same
// path for the same job replaces the previous record, which is what a retried
// job should do.
func (db *DB) RecordArtifact(ctx context.Context, a *model.Artifact) error {
	res, err := db.sql.ExecContext(ctx, `
		INSERT INTO artifacts (run_id, job_id, job_name, path, store_path, size, sha256, mode, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (job_id, path) DO UPDATE SET
			store_path = excluded.store_path,
			size       = excluded.size,
			sha256     = excluded.sha256,
			mode       = excluded.mode,
			created_at = excluded.created_at,
			expires_at = excluded.expires_at`,
		a.RunID, a.JobID, a.JobName, a.Path, a.StorePath, a.Size, a.SHA256, a.Mode,
		nowMillis(), toNullMillis(a.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: record artifact: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		a.ID = id
	}
	return nil
}

const artifactColumns = `id, run_id, job_id, job_name, path, store_path, size, sha256, mode, created_at, expires_at`

func scanArtifact(row interface{ Scan(...any) error }) (*model.Artifact, error) {
	var (
		a         model.Artifact
		createdMS int64
		expiresMS sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.RunID, &a.JobID, &a.JobName, &a.Path, &a.StorePath,
		&a.Size, &a.SHA256, &a.Mode, &createdMS, &expiresMS); err != nil {
		return nil, err
	}
	a.CreatedAt = fromMillis(createdMS)
	a.ExpiresAt = fromNullMillis(expiresMS)
	return &a, nil
}

// ArtifactByID loads one artifact record, used by the download endpoint.
func (db *DB) ArtifactByID(ctx context.Context, id int64) (*model.Artifact, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE id = ?`, id)
	a, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("artifact %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get artifact: %w", err)
	}
	return a, nil
}

// ListArtifacts returns every artifact of a run.
func (db *DB) ListArtifacts(ctx context.Context, runID int64) ([]*model.Artifact, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE run_id = ? ORDER BY job_name, path`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectArtifacts(rows)
}

// ListJobArtifacts returns the artifacts produced by a single job. The scheduler
// uses this to seed a dependent job's workspace.
func (db *DB) ListJobArtifacts(ctx context.Context, jobID int64) ([]*model.Artifact, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE job_id = ? ORDER BY path`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list job artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectArtifacts(rows)
}

// ExpiredArtifacts returns artifacts whose retention window has passed.
func (db *DB) ExpiredArtifacts(ctx context.Context, now time.Time) ([]*model.Artifact, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		toMillis(now))
	if err != nil {
		return nil, fmt.Errorf("store: list expired artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectArtifacts(rows)
}

// DeleteArtifact removes an artifact record. The bytes are removed separately by
// the artifact store, which owns the filesystem.
func (db *DB) DeleteArtifact(ctx context.Context, id int64) error {
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete artifact: %w", err)
	}
	return nil
}

func collectArtifacts(rows *sql.Rows) ([]*model.Artifact, error) {
	out := []*model.Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
