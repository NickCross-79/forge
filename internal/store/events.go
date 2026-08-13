package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nickcross-79/forge/internal/model"
)

// AppendEvent records a transition in a run's timeline. Every state change goes
// through here, which gives the dashboard a live feed and leaves an audit trail
// explaining after the fact why a run ended the way it did.
func (db *DB) AppendEvent(ctx context.Context, e *model.Event) error {
	var jobID sql.NullInt64
	if e.JobID != nil {
		jobID = sql.NullInt64{Int64: *e.JobID, Valid: true}
	}
	now := nowMillis()
	res, err := db.sql.ExecContext(ctx, `
		INSERT INTO run_events (run_id, job_id, job_name, type, from_status, to_status, message, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, jobID, e.JobName, string(e.Type), string(e.From), string(e.To), e.Message, now)
	if err != nil {
		return fmt.Errorf("store: append event: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		e.ID = id
	}
	e.CreatedAt = fromMillis(now)
	return nil
}

// ListEvents returns a run's timeline. afterID lets a reconnecting SSE client
// resume exactly where it left off instead of replaying the whole run.
func (db *DB) ListEvents(ctx context.Context, runID, afterID int64, limit int) ([]*model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, run_id, job_id, job_name, type, from_status, to_status, message, created_at
		FROM run_events
		WHERE run_id = ? AND id > ?
		ORDER BY id
		LIMIT ?`, runID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*model.Event{}
	for rows.Next() {
		var (
			e          model.Event
			jobID      sql.NullInt64
			eventType  string
			fromStatus string
			toStatus   string
			createdMS  int64
		)
		if err := rows.Scan(&e.ID, &e.RunID, &jobID, &e.JobName, &eventType,
			&fromStatus, &toStatus, &e.Message, &createdMS); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		if jobID.Valid {
			id := jobID.Int64
			e.JobID = &id
		}
		e.Type = model.EventType(eventType)
		e.From = model.Status(fromStatus)
		e.To = model.Status(toStatus)
		e.CreatedAt = fromMillis(createdMS)
		out = append(out, &e)
	}
	return out, rows.Err()
}
