// Package model holds the runtime entities forge persists and exchanges between
// components. It deliberately depends on nothing else in the tree so that the
// store, scheduler, API and CLI can all speak the same vocabulary without
// creating an import cycle.
package model

import (
	"time"
)

// Status is the state of a run, a job, or a single job attempt.
//
// The lifecycle for a job is:
//
//	queued ──► running ──► success
//	   │          │
//	   │          ├──────► failed
//	   │          ├──────► cancelled
//	   │          └──────► retrying ──► running (next attempt)
//	   ├──────────────────► skipped         (condition false, or upstream failed)
//	   └──────────────────► awaiting_manual (when: manual, until approved)
type Status string

const (
	// StatusQueued means the job is known to the scheduler but not yet started,
	// either because its dependencies are unfinished or because the concurrency
	// limit is saturated.
	StatusQueued Status = "queued"
	// StatusRunning means an attempt is currently executing.
	StatusRunning Status = "running"
	// StatusSuccess means the job (or run) completed without error.
	StatusSuccess Status = "success"
	// StatusFailed means the job exhausted its retries and did not succeed.
	StatusFailed Status = "failed"
	// StatusCancelled means execution was interrupted by the user or a shutdown.
	StatusCancelled Status = "cancelled"
	// StatusSkipped means the job never ran: its `if` condition was false, its
	// `when` clause did not match the run outcome, or an upstream job failed.
	StatusSkipped Status = "skipped"
	// StatusRetrying is a transient state between a failed attempt and the next
	// one. It is observable so that dashboards can distinguish "failing" from
	// "definitively failed".
	StatusRetrying Status = "retrying"
	// StatusAwaitingManual means a `when: manual` job is blocked pending approval.
	StatusAwaitingManual Status = "awaiting_manual"
)

// Terminal reports whether the status is final and will not change again.
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusCancelled, StatusSkipped:
		return true
	default:
		return false
	}
}

// Active reports whether the status represents work in flight or pending.
func (s Status) Active() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusRetrying, StatusAwaitingManual:
		return true
	default:
		return false
	}
}

// Successful reports whether downstream jobs should be allowed to proceed.
// Skipped counts as non-blocking: a skipped upstream does not fail its dependents
// outright, it causes them to be skipped too (handled by the scheduler).
func (s Status) Successful() bool { return s == StatusSuccess }

// ValidStatuses lists every status forge can persist. Used for API validation.
func ValidStatuses() []Status {
	return []Status{
		StatusQueued, StatusRunning, StatusSuccess, StatusFailed,
		StatusCancelled, StatusSkipped, StatusRetrying, StatusAwaitingManual,
	}
}

// Trigger records what caused a run to start.
type Trigger string

const (
	// TriggerCLI is a run started by `forge run`.
	TriggerCLI Trigger = "cli"
	// TriggerAPI is a run started through the HTTP API or dashboard.
	TriggerAPI Trigger = "api"
)

// ExecutorKind selects the backend used to run a job's commands.
type ExecutorKind string

const (
	// ExecutorLocal runs commands as child processes of forge.
	ExecutorLocal ExecutorKind = "local"
	// ExecutorDocker runs commands inside an ephemeral container.
	ExecutorDocker ExecutorKind = "docker"
)

// Pipeline is a YAML pipeline definition that forge has seen at least once.
// The spec text is stored verbatim so historical runs remain explainable even
// after the file on disk changes.
type Pipeline struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	SourcePath  string    `json:"source_path"`
	SpecYAML    string    `json:"spec_yaml"`
	Checksum    string    `json:"checksum"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Run is one execution of a pipeline.
type Run struct {
	ID         int64      `json:"id"`
	PipelineID int64      `json:"pipeline_id"`
	Number     int64      `json:"number"`
	Status     Status     `json:"status"`
	Trigger    Trigger    `json:"trigger"`
	SpecYAML   string     `json:"spec_yaml,omitempty"`
	WorkDir    string     `json:"work_dir"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`

	// PipelineName is denormalised on read for convenience in list views.
	PipelineName string `json:"pipeline_name,omitempty"`
}

// Job is one node of a run's DAG.
type Job struct {
	ID           int64        `json:"id"`
	RunID        int64        `json:"run_id"`
	Name         string       `json:"name"`
	Stage        string       `json:"stage"`
	Status       Status       `json:"status"`
	Needs        []string     `json:"needs"`
	Executor     ExecutorKind `json:"executor"`
	Image        string       `json:"image,omitempty"`
	Manual       bool         `json:"manual"`
	AllowFailure bool         `json:"allow_failure"`
	Attempts     int          `json:"attempts"`
	MaxAttempts  int          `json:"max_attempts"`
	ExitCode     *int         `json:"exit_code,omitempty"`
	Error        string       `json:"error,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	StartedAt    *time.Time   `json:"started_at,omitempty"`
	FinishedAt   *time.Time   `json:"finished_at,omitempty"`
	DurationMS   int64        `json:"duration_ms"`
}

// Attempt is a single try at running a job. A job with retries has several.
type Attempt struct {
	ID         int64      `json:"id"`
	JobID      int64      `json:"job_id"`
	Number     int        `json:"number"`
	Status     Status     `json:"status"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
}

// Stream identifies which output channel a log belongs to. stdout and stderr are
// captured independently so a job's diagnostics can be read apart from its output.
type Stream string

const (
	// StreamStdout is the job's standard output.
	StreamStdout Stream = "stdout"
	// StreamStderr is the job's standard error.
	StreamStderr Stream = "stderr"
)

// Log points at the on-disk file holding one stream of one attempt. Log bodies are
// kept out of SQLite so that a chatty job cannot bloat the database.
type Log struct {
	ID        int64     `json:"id"`
	AttemptID int64     `json:"attempt_id"`
	JobID     int64     `json:"job_id"`
	RunID     int64     `json:"run_id"`
	Stream    Stream    `json:"stream"`
	Path      string    `json:"path"`
	Bytes     int64     `json:"bytes"`
	Truncated bool      `json:"truncated"`
	CreatedAt time.Time `json:"created_at"`
}

// Artifact is a file collected from a job's workspace after it ran.
type Artifact struct {
	ID        int64      `json:"id"`
	RunID     int64      `json:"run_id"`
	JobID     int64      `json:"job_id"`
	JobName   string     `json:"job_name"`
	Path      string     `json:"path"`
	StorePath string     `json:"-"`
	Size      int64      `json:"size"`
	SHA256    string     `json:"sha256"`
	Mode      uint32     `json:"mode"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// EventType classifies entries in a run's timeline.
type EventType string

const (
	// EventRunStatus is a run-level state transition.
	EventRunStatus EventType = "run_status"
	// EventJobStatus is a job-level state transition.
	EventJobStatus EventType = "job_status"
	// EventLog signals that new log bytes are available for a job.
	EventLog EventType = "log"
	// EventArtifact signals that an artifact was collected.
	EventArtifact EventType = "artifact"
)

// Event is an entry in a run's timeline. Every state transition produces one, which
// gives the dashboard a live feed and gives operators an audit trail after the fact.
type Event struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	JobID     *int64    `json:"job_id,omitempty"`
	JobName   string    `json:"job_name,omitempty"`
	Type      EventType `json:"type"`
	From      Status    `json:"from,omitempty"`
	To        Status    `json:"to,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
