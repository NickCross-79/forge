package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nickcross-79/forge/internal/model"
)

// UpsertPipeline records a pipeline by name, creating it or refreshing its
// stored source. Pipelines are keyed by the `name` in the YAML rather than by
// file path, so moving the file keeps the run history attached to the pipeline.
func (db *DB) UpsertPipeline(ctx context.Context, p *model.Pipeline) (*model.Pipeline, error) {
	if p.Name == "" {
		return nil, errors.New("store: pipeline name is required")
	}
	now := nowMillis()
	res, err := db.sql.ExecContext(ctx, `
		INSERT INTO pipelines (name, source_path, spec_yaml, checksum, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			source_path = excluded.source_path,
			spec_yaml   = excluded.spec_yaml,
			checksum    = excluded.checksum,
			description = excluded.description,
			updated_at  = excluded.updated_at`,
		p.Name, p.SourcePath, p.SpecYAML, p.Checksum, p.Description, now, now)
	if err != nil {
		return nil, fmt.Errorf("store: upsert pipeline: %w", err)
	}
	_ = res

	return db.PipelineByName(ctx, p.Name)
}

const pipelineColumns = `id, name, source_path, spec_yaml, checksum, description, created_at, updated_at`

func scanPipeline(row interface{ Scan(...any) error }) (*model.Pipeline, error) {
	var (
		p                    model.Pipeline
		createdMS, updatedMS int64
	)
	if err := row.Scan(&p.ID, &p.Name, &p.SourcePath, &p.SpecYAML, &p.Checksum,
		&p.Description, &createdMS, &updatedMS); err != nil {
		return nil, err
	}
	p.CreatedAt = fromMillis(createdMS)
	p.UpdatedAt = fromMillis(updatedMS)
	return &p, nil
}

// PipelineByID looks up a pipeline by its numeric ID.
func (db *DB) PipelineByID(ctx context.Context, id int64) (*model.Pipeline, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+pipelineColumns+` FROM pipelines WHERE id = ?`, id)
	p, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("pipeline %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get pipeline: %w", err)
	}
	return p, nil
}

// PipelineByName looks up a pipeline by the name declared in its YAML.
func (db *DB) PipelineByName(ctx context.Context, name string) (*model.Pipeline, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+pipelineColumns+` FROM pipelines WHERE name = ?`, name)
	p, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("pipeline %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get pipeline: %w", err)
	}
	return p, nil
}

// PipelineStats summarises a pipeline's run history for list views.
type PipelineStats struct {
	Runs        int64        `json:"runs"`
	Successes   int64        `json:"successes"`
	Failures    int64        `json:"failures"`
	LastStatus  model.Status `json:"last_status,omitempty"`
	LastRunID   int64        `json:"last_run_id,omitempty"`
	LastRunAt   *int64       `json:"last_run_at,omitempty"`
	AvgDuration int64        `json:"avg_duration_ms"`
}

// PipelineWithStats pairs a pipeline with its aggregate history.
type PipelineWithStats struct {
	*model.Pipeline
	Stats PipelineStats `json:"stats"`
}

// ListPipelines returns every known pipeline with its run statistics, most
// recently active first. The aggregates are computed in SQL so listing does not
// need to load run rows into memory.
func (db *DB) ListPipelines(ctx context.Context) ([]*PipelineWithStats, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT p.id, p.name, p.source_path, p.spec_yaml, p.checksum, p.description,
		       p.created_at, p.updated_at,
		       COUNT(r.id),
		       COALESCE(SUM(CASE WHEN r.status = 'success' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN r.status = 'failed'  THEN 1 ELSE 0 END), 0),
		       COALESCE(CAST(AVG(CASE WHEN r.duration_ms > 0 THEN r.duration_ms END) AS INTEGER), 0),
		       MAX(r.id)
		FROM pipelines p
		LEFT JOIN runs r ON r.pipeline_id = p.id
		GROUP BY p.id
		ORDER BY MAX(COALESCE(r.created_at, p.updated_at)) DESC, p.name`)
	if err != nil {
		return nil, fmt.Errorf("store: list pipelines: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*PipelineWithStats
	for rows.Next() {
		var (
			p                    model.Pipeline
			createdMS, updatedMS int64
			stats                PipelineStats
			lastRunID            sql.NullInt64
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.SourcePath, &p.SpecYAML, &p.Checksum, &p.Description,
			&createdMS, &updatedMS,
			&stats.Runs, &stats.Successes, &stats.Failures, &stats.AvgDuration, &lastRunID); err != nil {
			return nil, fmt.Errorf("store: scan pipeline: %w", err)
		}
		p.CreatedAt = fromMillis(createdMS)
		p.UpdatedAt = fromMillis(updatedMS)
		if lastRunID.Valid {
			stats.LastRunID = lastRunID.Int64
		}
		out = append(out, &PipelineWithStats{Pipeline: &p, Stats: stats})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pipelines: %w", err)
	}

	// Fill in the status of each pipeline's most recent run. The run count is
	// small (one query per pipeline, and a project has a handful), and doing it
	// here keeps the aggregate query above readable.
	for _, p := range out {
		if p.Stats.LastRunID == 0 {
			continue
		}
		var (
			status    string
			createdMS int64
		)
		err := db.sql.QueryRowContext(ctx,
			`SELECT status, created_at FROM runs WHERE id = ?`, p.Stats.LastRunID).Scan(&status, &createdMS)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: last run status: %w", err)
		}
		if err == nil {
			p.Stats.LastStatus = model.Status(status)
			p.Stats.LastRunAt = &createdMS
		}
	}
	return out, nil
}

// DeletePipeline removes a pipeline and, by cascade, all of its runs.
func (db *DB) DeletePipeline(ctx context.Context, id int64) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM pipelines WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete pipeline: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pipeline %d: %w", id, ErrNotFound)
	}
	return nil
}
