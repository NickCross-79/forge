// Package metrics holds the process-lifetime counters and gauges that describe
// what forge is doing right now.
//
// Historical aggregates come from SQL (see store.Stats); this package covers the
// things the database cannot answer — how many jobs are executing at this
// instant, how deep the queue is — and the counters that reset when forge
// restarts.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Registry is a small, dependency-free metrics collector.
//
// A full Prometheus client would be a heavier dependency than the handful of
// numbers forge exposes justifies, and this keeps the zero-cost promise intact:
// counters are served as JSON for the dashboard and in Prometheus text format for
// anyone who wants to scrape them locally.
type Registry struct {
	startedAt time.Time

	runsStarted   atomic.Int64
	runsSucceeded atomic.Int64
	runsFailed    atomic.Int64
	runsCancelled atomic.Int64

	jobsStarted   atomic.Int64
	jobsSucceeded atomic.Int64
	jobsFailed    atomic.Int64
	jobsSkipped   atomic.Int64
	jobsRetried   atomic.Int64
	jobsCancelled atomic.Int64

	activeJobs atomic.Int64
	queuedJobs atomic.Int64
	activeRuns atomic.Int64

	artifactsCollected atomic.Int64
	artifactBytes      atomic.Int64

	mu             sync.Mutex
	runDurations   durationStats
	jobDurations   durationStats
	executorCounts map[string]int64
}

type durationStats struct {
	count int64
	total time.Duration
	max   time.Duration
}

func (d *durationStats) observe(v time.Duration) {
	d.count++
	d.total += v
	if v > d.max {
		d.max = v
	}
}

func (d durationStats) mean() time.Duration {
	if d.count == 0 {
		return 0
	}
	return d.total / time.Duration(d.count)
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		startedAt:      time.Now(),
		executorCounts: make(map[string]int64),
	}
}

// RunStarted records a run entering execution.
func (r *Registry) RunStarted() {
	r.runsStarted.Add(1)
	r.activeRuns.Add(1)
}

// RunFinished records a run reaching a terminal state.
func (r *Registry) RunFinished(status string, d time.Duration) {
	r.activeRuns.Add(-1)
	switch status {
	case "success":
		r.runsSucceeded.Add(1)
	case "failed":
		r.runsFailed.Add(1)
	case "cancelled":
		r.runsCancelled.Add(1)
	}
	r.mu.Lock()
	r.runDurations.observe(d)
	r.mu.Unlock()
}

// JobQueued records a job entering the queue.
func (r *Registry) JobQueued() { r.queuedJobs.Add(1) }

// JobDequeued records a job leaving the queue, whether it ran or was skipped.
func (r *Registry) JobDequeued() { r.queuedJobs.Add(-1) }

// JobStarted records an attempt beginning on a given executor.
func (r *Registry) JobStarted(executor string) {
	r.jobsStarted.Add(1)
	r.activeJobs.Add(1)
	r.mu.Lock()
	r.executorCounts[executor]++
	r.mu.Unlock()
}

// JobFinished records an attempt ending.
func (r *Registry) JobFinished(status string, d time.Duration) {
	r.activeJobs.Add(-1)
	switch status {
	case "success":
		r.jobsSucceeded.Add(1)
	case "failed":
		r.jobsFailed.Add(1)
	case "cancelled":
		r.jobsCancelled.Add(1)
	}
	r.mu.Lock()
	r.jobDurations.observe(d)
	r.mu.Unlock()
}

// JobSkipped records a job that never ran.
func (r *Registry) JobSkipped() { r.jobsSkipped.Add(1) }

// JobRetried records a failed attempt that will be tried again.
func (r *Registry) JobRetried() { r.jobsRetried.Add(1) }

// ArtifactsCollected records artifacts stored by a job.
func (r *Registry) ArtifactsCollected(count int, bytes int64) {
	r.artifactsCollected.Add(int64(count))
	r.artifactBytes.Add(bytes)
}

// Snapshot is a consistent-enough read of every counter, for JSON output.
type Snapshot struct {
	UptimeSeconds float64 `json:"uptime_seconds"`

	RunsStarted   int64 `json:"runs_started"`
	RunsSucceeded int64 `json:"runs_succeeded"`
	RunsFailed    int64 `json:"runs_failed"`
	RunsCancelled int64 `json:"runs_cancelled"`
	ActiveRuns    int64 `json:"active_runs"`

	JobsStarted   int64 `json:"jobs_started"`
	JobsSucceeded int64 `json:"jobs_succeeded"`
	JobsFailed    int64 `json:"jobs_failed"`
	JobsSkipped   int64 `json:"jobs_skipped"`
	JobsRetried   int64 `json:"jobs_retried"`
	JobsCancelled int64 `json:"jobs_cancelled"`
	ActiveJobs    int64 `json:"active_jobs"`
	QueuedJobs    int64 `json:"queued_jobs"`

	AvgRunDurationMS int64 `json:"avg_run_duration_ms"`
	MaxRunDurationMS int64 `json:"max_run_duration_ms"`
	AvgJobDurationMS int64 `json:"avg_job_duration_ms"`
	MaxJobDurationMS int64 `json:"max_job_duration_ms"`

	ArtifactsCollected int64 `json:"artifacts_collected"`
	ArtifactBytes      int64 `json:"artifact_bytes"`

	ExecutorRuns map[string]int64 `json:"executor_runs"`
}

// Snapshot reads the current values.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	runD, jobD := r.runDurations, r.jobDurations
	execCounts := make(map[string]int64, len(r.executorCounts))
	for k, v := range r.executorCounts {
		execCounts[k] = v
	}
	r.mu.Unlock()

	return Snapshot{
		UptimeSeconds: time.Since(r.startedAt).Seconds(),

		RunsStarted:   r.runsStarted.Load(),
		RunsSucceeded: r.runsSucceeded.Load(),
		RunsFailed:    r.runsFailed.Load(),
		RunsCancelled: r.runsCancelled.Load(),
		ActiveRuns:    r.activeRuns.Load(),

		JobsStarted:   r.jobsStarted.Load(),
		JobsSucceeded: r.jobsSucceeded.Load(),
		JobsFailed:    r.jobsFailed.Load(),
		JobsSkipped:   r.jobsSkipped.Load(),
		JobsRetried:   r.jobsRetried.Load(),
		JobsCancelled: r.jobsCancelled.Load(),
		ActiveJobs:    r.activeJobs.Load(),
		QueuedJobs:    r.queuedJobs.Load(),

		AvgRunDurationMS: runD.mean().Milliseconds(),
		MaxRunDurationMS: runD.max.Milliseconds(),
		AvgJobDurationMS: jobD.mean().Milliseconds(),
		MaxJobDurationMS: jobD.max.Milliseconds(),

		ArtifactsCollected: r.artifactsCollected.Load(),
		ArtifactBytes:      r.artifactBytes.Load(),

		ExecutorRuns: execCounts,
	}
}

// Prometheus renders the snapshot in the Prometheus text exposition format, so a
// local Prometheus or a `curl | grep` can read it without any extra dependency.
func (r *Registry) Prometheus() string {
	s := r.Snapshot()
	var b strings.Builder

	write := func(name, help, typ string, value any) {
		fmt.Fprintf(&b, "# HELP forge_%s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE forge_%s %s\n", name, typ)
		fmt.Fprintf(&b, "forge_%s %v\n", name, value)
	}

	write("uptime_seconds", "Seconds since this forge process started.", "gauge", s.UptimeSeconds)
	write("runs_started_total", "Runs started since process start.", "counter", s.RunsStarted)
	write("runs_succeeded_total", "Runs that finished successfully.", "counter", s.RunsSucceeded)
	write("runs_failed_total", "Runs that finished with a failure.", "counter", s.RunsFailed)
	write("runs_cancelled_total", "Runs that were cancelled.", "counter", s.RunsCancelled)
	write("runs_active", "Runs currently executing.", "gauge", s.ActiveRuns)
	write("jobs_started_total", "Job attempts started.", "counter", s.JobsStarted)
	write("jobs_succeeded_total", "Job attempts that succeeded.", "counter", s.JobsSucceeded)
	write("jobs_failed_total", "Job attempts that failed.", "counter", s.JobsFailed)
	write("jobs_skipped_total", "Jobs skipped by condition or upstream failure.", "counter", s.JobsSkipped)
	write("jobs_retried_total", "Failed attempts that were retried.", "counter", s.JobsRetried)
	write("jobs_cancelled_total", "Job attempts that were cancelled.", "counter", s.JobsCancelled)
	write("jobs_active", "Job attempts currently executing.", "gauge", s.ActiveJobs)
	write("jobs_queued", "Jobs waiting to execute.", "gauge", s.QueuedJobs)
	write("run_duration_avg_ms", "Mean run duration in milliseconds.", "gauge", s.AvgRunDurationMS)
	write("run_duration_max_ms", "Longest run duration in milliseconds.", "gauge", s.MaxRunDurationMS)
	write("job_duration_avg_ms", "Mean job attempt duration in milliseconds.", "gauge", s.AvgJobDurationMS)
	write("job_duration_max_ms", "Longest job attempt duration in milliseconds.", "gauge", s.MaxJobDurationMS)
	write("artifacts_collected_total", "Artifacts stored.", "counter", s.ArtifactsCollected)
	write("artifact_bytes_total", "Bytes of artifacts stored.", "counter", s.ArtifactBytes)

	if len(s.ExecutorRuns) > 0 {
		b.WriteString("# HELP forge_executor_jobs_total Job attempts by executor backend.\n")
		b.WriteString("# TYPE forge_executor_jobs_total counter\n")
		names := make([]string, 0, len(s.ExecutorRuns))
		for name := range s.ExecutorRuns {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "forge_executor_jobs_total{executor=%q} %d\n", name, s.ExecutorRuns[name])
		}
	}
	return b.String()
}
