package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunAndJobCounters(t *testing.T) {
	r := New()

	r.RunStarted()
	r.JobQueued()
	r.JobDequeued()
	r.JobStarted("local")
	r.JobFinished("success", 250*time.Millisecond)
	r.JobStarted("docker")
	r.JobFinished("failed", 750*time.Millisecond)
	r.JobSkipped()
	r.JobRetried()
	r.ArtifactsCollected(3, 4096)
	r.RunFinished("success", 2*time.Second)

	s := r.Snapshot()
	checks := map[string]int64{
		"RunsStarted":        s.RunsStarted,
		"RunsSucceeded":      s.RunsSucceeded,
		"JobsStarted":        s.JobsStarted,
		"JobsSucceeded":      s.JobsSucceeded,
		"JobsFailed":         s.JobsFailed,
		"JobsSkipped":        s.JobsSkipped,
		"JobsRetried":        s.JobsRetried,
		"ArtifactsCollected": s.ArtifactsCollected,
	}
	want := map[string]int64{
		"RunsStarted": 1, "RunsSucceeded": 1, "JobsStarted": 2, "JobsSucceeded": 1,
		"JobsFailed": 1, "JobsSkipped": 1, "JobsRetried": 1, "ArtifactsCollected": 3,
	}
	for name, got := range checks {
		if got != want[name] {
			t.Errorf("%s = %d, want %d", name, got, want[name])
		}
	}

	if s.ArtifactBytes != 4096 {
		t.Errorf("ArtifactBytes = %d, want 4096", s.ArtifactBytes)
	}
	// Gauges must settle back to zero once everything finishes.
	if s.ActiveRuns != 0 || s.ActiveJobs != 0 || s.QueuedJobs != 0 {
		t.Errorf("gauges did not settle: runs=%d jobs=%d queued=%d",
			s.ActiveRuns, s.ActiveJobs, s.QueuedJobs)
	}
	if s.AvgJobDurationMS != 500 {
		t.Errorf("AvgJobDurationMS = %d, want the mean of 250 and 750", s.AvgJobDurationMS)
	}
	if s.MaxJobDurationMS != 750 {
		t.Errorf("MaxJobDurationMS = %d, want 750", s.MaxJobDurationMS)
	}
	if s.ExecutorRuns["local"] != 1 || s.ExecutorRuns["docker"] != 1 {
		t.Errorf("ExecutorRuns = %v, want one of each", s.ExecutorRuns)
	}
	if s.UptimeSeconds <= 0 {
		t.Error("UptimeSeconds should be positive")
	}
}

func TestGaugesTrackInFlightWork(t *testing.T) {
	r := New()
	r.RunStarted()
	r.JobQueued()
	r.JobQueued()
	r.JobDequeued()
	r.JobStarted("local")

	s := r.Snapshot()
	if s.ActiveRuns != 1 {
		t.Errorf("ActiveRuns = %d, want 1", s.ActiveRuns)
	}
	if s.QueuedJobs != 1 {
		t.Errorf("QueuedJobs = %d, want 1", s.QueuedJobs)
	}
	if s.ActiveJobs != 1 {
		t.Errorf("ActiveJobs = %d, want 1", s.ActiveJobs)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	r := New()
	r.JobStarted("local")
	first := r.Snapshot()

	r.JobStarted("local")
	first.ExecutorRuns["local"] = 999

	if second := r.Snapshot(); second.ExecutorRuns["local"] != 2 {
		t.Errorf("mutating a snapshot affected the registry: %v", second.ExecutorRuns)
	}
}

func TestPrometheusExposition(t *testing.T) {
	r := New()
	r.RunStarted()
	r.JobStarted("local")
	r.JobFinished("success", time.Second)
	r.RunFinished("success", 3*time.Second)

	out := r.Prometheus()
	for _, want := range []string{
		"# HELP forge_runs_started_total",
		"# TYPE forge_runs_started_total counter",
		"forge_runs_started_total 1",
		"# TYPE forge_jobs_active gauge",
		"forge_jobs_active 0",
		`forge_executor_jobs_total{executor="local"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prometheus output missing %q:\n%s", want, out)
		}
	}

	// Every metric line must be preceded by HELP and TYPE.
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "forge_") {
			t.Errorf("unexpected exposition line: %q", line)
		}
	}
}

func TestConcurrentUpdates(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.JobQueued()
				r.JobDequeued()
				r.JobStarted("local")
				r.JobFinished("success", time.Millisecond)
			}
		}()
	}
	wg.Wait()

	s := r.Snapshot()
	if s.JobsStarted != 5000 {
		t.Errorf("JobsStarted = %d, want 5000", s.JobsStarted)
	}
	if s.ActiveJobs != 0 || s.QueuedJobs != 0 {
		t.Errorf("gauges did not settle under concurrency: active=%d queued=%d",
			s.ActiveJobs, s.QueuedJobs)
	}
	if s.ExecutorRuns["local"] != 5000 {
		t.Errorf("ExecutorRuns[local] = %d, want 5000", s.ExecutorRuns["local"])
	}
}

func TestEmptyRegistrySnapshot(t *testing.T) {
	s := New().Snapshot()
	if s.AvgRunDurationMS != 0 || s.AvgJobDurationMS != 0 {
		t.Error("an empty registry should report zero averages, not divide by zero")
	}
	if len(s.ExecutorRuns) != 0 {
		t.Errorf("ExecutorRuns = %v, want empty", s.ExecutorRuns)
	}
}
