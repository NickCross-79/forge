package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickcross-79/forge/internal/model"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "forge.db")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedRun(t *testing.T, db *DB) (*model.Pipeline, *model.Run) {
	t.Helper()
	ctx := context.Background()
	p, err := db.UpsertPipeline(ctx, &model.Pipeline{Name: "example", SpecYAML: "name: example", Checksum: "abc"})
	if err != nil {
		t.Fatalf("UpsertPipeline() error = %v", err)
	}
	run, err := db.CreateRun(ctx, &model.Run{PipelineID: p.ID, Trigger: model.TriggerCLI})
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	return p, run
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "forge.db")

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	versions, err := db.AppliedVersions(ctx)
	if err != nil {
		t.Fatalf("AppliedVersions() error = %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("no migrations were applied")
	}

	// Running migrations again must change nothing.
	for i := 0; i < 3; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("re-running Migrate() failed: %v", err)
		}
	}
	after, err := db.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(versions) {
		t.Errorf("migration count changed on re-run: %d then %d", len(versions), len(after))
	}
	_ = db.Close()

	// Re-opening an existing database must not corrupt or re-apply anything.
	db2, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("re-Open() error = %v", err)
	}
	defer func() { _ = db2.Close() }()
	reopened, err := db2.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened) != len(versions) {
		t.Errorf("migrations re-applied on reopen: %d then %d", len(versions), len(reopened))
	}
}

func TestPipelineUpsert(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	first, err := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p", SpecYAML: "v1", Checksum: "c1"})
	if err != nil {
		t.Fatalf("UpsertPipeline() error = %v", err)
	}
	second, err := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p", SpecYAML: "v2", Checksum: "c2"})
	if err != nil {
		t.Fatalf("UpsertPipeline() error = %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("upsert created a second row: %d then %d", first.ID, second.ID)
	}
	if second.SpecYAML != "v2" || second.Checksum != "c2" {
		t.Errorf("upsert did not refresh the spec: %+v", second)
	}

	if _, err := db.PipelineByID(ctx, first.ID); err != nil {
		t.Errorf("PipelineByID() error = %v", err)
	}
	if _, err := db.PipelineByName(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("PipelineByName(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := db.PipelineByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("PipelineByID(9999) error = %v, want ErrNotFound", err)
	}
}

func TestRunNumbersAreMonotonicPerPipeline(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	a, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "a"})
	b, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "b"})

	for i := int64(1); i <= 3; i++ {
		run, err := db.CreateRun(ctx, &model.Run{PipelineID: a.ID})
		if err != nil {
			t.Fatalf("CreateRun() error = %v", err)
		}
		if run.Number != i {
			t.Errorf("run number = %d, want %d", run.Number, i)
		}
	}
	// A different pipeline starts its own numbering.
	run, err := db.CreateRun(ctx, &model.Run{PipelineID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Number != 1 {
		t.Errorf("second pipeline's first run number = %d, want 1", run.Number)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, run := seedRun(t, db)

	start := time.Now()
	if err := db.StartRun(ctx, run.ID, start); err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	loaded, err := db.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != model.StatusRunning {
		t.Errorf("status = %q, want running", loaded.Status)
	}
	if loaded.StartedAt == nil {
		t.Fatal("StartedAt was not stamped")
	}
	if loaded.PipelineName != "example" {
		t.Errorf("PipelineName = %q, want example", loaded.PipelineName)
	}

	finish := start.Add(1500 * time.Millisecond)
	if err := db.FinishRun(ctx, run.ID, model.StatusSuccess, finish, ""); err != nil {
		t.Fatalf("FinishRun() error = %v", err)
	}
	loaded, err = db.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != model.StatusSuccess {
		t.Errorf("status = %q, want success", loaded.Status)
	}
	if loaded.DurationMS != 1500 {
		t.Errorf("DurationMS = %d, want 1500", loaded.DurationMS)
	}
}

func TestListRunsFilters(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p"})
	other, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "other"})

	for i := 0; i < 5; i++ {
		run, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
		status := model.StatusSuccess
		if i%2 == 0 {
			status = model.StatusFailed
		}
		if err := db.FinishRun(ctx, run.ID, status, time.Now(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateRun(ctx, &model.Run{PipelineID: other.ID}); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListRuns(ctx, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Errorf("len(all runs) = %d, want 6", len(all))
	}
	// Newest first.
	if all[0].ID < all[len(all)-1].ID {
		t.Error("runs are not ordered newest first")
	}

	byPipeline, err := db.ListRuns(ctx, RunFilter{PipelineID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byPipeline) != 5 {
		t.Errorf("len(pipeline runs) = %d, want 5", len(byPipeline))
	}

	failed, err := db.ListRuns(ctx, RunFilter{Status: []model.Status{model.StatusFailed}})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 3 {
		t.Errorf("len(failed runs) = %d, want 3", len(failed))
	}

	paged, err := db.ListRuns(ctx, RunFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(paged) != 2 {
		t.Errorf("len(paged) = %d, want 2", len(paged))
	}
	if paged[0].ID != all[2].ID {
		t.Errorf("offset paging returned the wrong page: got run %d, want %d", paged[0].ID, all[2].ID)
	}

	count, err := db.CountRuns(ctx, RunFilter{PipelineID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Errorf("CountRuns() = %d, want 5", count)
	}
}

func TestJobsAndAttempts(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, run := seedRun(t, db)

	jobs := []*model.Job{
		{Name: "build", Stage: "build", Needs: nil, Executor: model.ExecutorLocal, MaxAttempts: 3},
		{Name: "test", Stage: "test", Needs: []string{"build"}, Executor: model.ExecutorLocal, MaxAttempts: 1},
	}
	if err := db.CreateJobs(ctx, run.ID, jobs); err != nil {
		t.Fatalf("CreateJobs() error = %v", err)
	}
	for _, j := range jobs {
		if j.ID == 0 {
			t.Fatalf("job %q was not assigned an ID", j.Name)
		}
	}

	loaded, err := db.ListJobs(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(loaded))
	}
	// A nil needs slice must round-trip as an empty JSON array, not null.
	if loaded[0].Needs == nil || len(loaded[0].Needs) != 0 {
		t.Errorf("build.Needs = %#v, want empty slice", loaded[0].Needs)
	}
	if len(loaded[1].Needs) != 1 || loaded[1].Needs[0] != "build" {
		t.Errorf("test.Needs = %#v, want [build]", loaded[1].Needs)
	}

	byName, err := db.JobByName(ctx, run.ID, "test")
	if err != nil {
		t.Fatalf("JobByName() error = %v", err)
	}
	if byName.ID != jobs[1].ID {
		t.Errorf("JobByName returned job %d, want %d", byName.ID, jobs[1].ID)
	}
	if _, err := db.JobByName(ctx, run.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("JobByName(nope) error = %v, want ErrNotFound", err)
	}

	// A retried job keeps its original start time so wall-clock duration is right.
	first := time.Now()
	if err := db.SetJobStatus(ctx, jobs[0].ID, model.StatusRunning, first); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobStatus(ctx, jobs[0].ID, model.StatusRunning, first.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := db.JobByID(ctx, jobs[0].ID)
	if reloaded.StartedAt == nil || reloaded.StartedAt.UnixMilli() != first.UnixMilli() {
		t.Errorf("StartedAt was overwritten on the second attempt: %v", reloaded.StartedAt)
	}

	exit := 2
	if err := db.FinishJob(ctx, jobs[0].ID, model.StatusFailed, &exit, "boom", first.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	reloaded, _ = db.JobByID(ctx, jobs[0].ID)
	if reloaded.Status != model.StatusFailed {
		t.Errorf("status = %q, want failed", reloaded.Status)
	}
	if reloaded.ExitCode == nil || *reloaded.ExitCode != 2 {
		t.Errorf("ExitCode = %v, want 2", reloaded.ExitCode)
	}
	if reloaded.Error != "boom" {
		t.Errorf("Error = %q, want boom", reloaded.Error)
	}
	if reloaded.DurationMS != 2000 {
		t.Errorf("DurationMS = %d, want 2000", reloaded.DurationMS)
	}

	// Attempts.
	att, err := db.CreateAttempt(ctx, jobs[0].ID, 1, first)
	if err != nil {
		t.Fatalf("CreateAttempt() error = %v", err)
	}
	if err := db.FinishAttempt(ctx, att.ID, model.StatusFailed, &exit, "boom", first.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAttempt(ctx, jobs[0].ID, 2, first.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListAttempts(ctx, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("len(attempts) = %d, want 2", len(attempts))
	}
	if attempts[0].Number != 1 || attempts[1].Number != 2 {
		t.Errorf("attempts are out of order: %d, %d", attempts[0].Number, attempts[1].Number)
	}
	if attempts[0].DurationMS != 1000 {
		t.Errorf("attempt DurationMS = %d, want 1000", attempts[0].DurationMS)
	}

	if err := db.IncrementJobAttempts(ctx, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	reloaded, _ = db.JobByID(ctx, jobs[0].ID)
	if reloaded.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", reloaded.Attempts)
	}
}

func TestLogsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, run := seedRun(t, db)
	jobs := []*model.Job{{Name: "build", Executor: model.ExecutorLocal}}
	if err := db.CreateJobs(ctx, run.ID, jobs); err != nil {
		t.Fatal(err)
	}
	att, err := db.CreateAttempt(ctx, jobs[0].ID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	rec := &model.Log{
		AttemptID: att.ID, JobID: jobs[0].ID, RunID: run.ID,
		Stream: model.StreamStdout, Path: "/tmp/out.log",
	}
	if err := db.RecordLog(ctx, rec); err != nil {
		t.Fatalf("RecordLog() error = %v", err)
	}
	// Recording again must update rather than duplicate: the writer registers the
	// row when it opens the stream and again when it closes.
	rec.Bytes = 4096
	rec.Truncated = true
	if err := db.RecordLog(ctx, rec); err != nil {
		t.Fatalf("RecordLog() update error = %v", err)
	}

	got, err := db.LogForAttempt(ctx, att.ID, model.StreamStdout)
	if err != nil {
		t.Fatalf("LogForAttempt() error = %v", err)
	}
	if got.Bytes != 4096 || !got.Truncated {
		t.Errorf("log record was not updated: %+v", got)
	}

	all, err := db.ListLogs(ctx, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("RecordLog created a duplicate row: %d logs", len(all))
	}

	forRun, err := db.LogsForRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(forRun) != 1 {
		t.Errorf("LogsForRun() = %d rows, want 1", len(forRun))
	}
}

func TestArtifactsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, run := seedRun(t, db)
	jobs := []*model.Job{{Name: "build"}}
	if err := db.CreateJobs(ctx, run.ID, jobs); err != nil {
		t.Fatal(err)
	}

	expires := time.Now().Add(-time.Hour)
	a := &model.Artifact{
		RunID: run.ID, JobID: jobs[0].ID, JobName: "build",
		Path: "dist/output.txt", StorePath: "1/build/dist/output.txt",
		Size: 6, SHA256: "deadbeef", Mode: 0o644, ExpiresAt: &expires,
	}
	if err := db.RecordArtifact(ctx, a); err != nil {
		t.Fatalf("RecordArtifact() error = %v", err)
	}
	// Re-collecting the same path (as a retry would) replaces the record.
	a.Size = 12
	if err := db.RecordArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}

	list, err := db.ListArtifacts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("len(artifacts) = %d, want 1 (upsert should not duplicate)", len(list))
	}
	if list[0].Size != 12 {
		t.Errorf("Size = %d, want 12", list[0].Size)
	}

	byJob, err := db.ListJobArtifacts(ctx, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byJob) != 1 {
		t.Errorf("ListJobArtifacts() = %d, want 1", len(byJob))
	}

	expired, err := db.ExpiredArtifacts(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 {
		t.Errorf("ExpiredArtifacts() = %d, want 1", len(expired))
	}

	fetched, err := db.ArtifactByID(ctx, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.StorePath != a.StorePath {
		t.Errorf("StorePath = %q, want %q", fetched.StorePath, a.StorePath)
	}
	if err := db.DeleteArtifact(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArtifactByID(ctx, list[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete, error = %v, want ErrNotFound", err)
	}
}

func TestEventsResumeAfterID(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, run := seedRun(t, db)

	for i := 0; i < 5; i++ {
		if err := db.AppendEvent(ctx, &model.Event{
			RunID: run.ID, Type: model.EventRunStatus,
			From: model.StatusQueued, To: model.StatusRunning,
			Message: "transition",
		}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.ListEvents(ctx, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("len(events) = %d, want 5", len(all))
	}

	// A reconnecting client resumes from its last seen ID.
	rest, err := db.ListEvents(ctx, run.ID, all[2].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Errorf("len(events after id) = %d, want 2", len(rest))
	}
	if rest[0].ID != all[3].ID {
		t.Errorf("resume returned event %d, want %d", rest[0].ID, all[3].ID)
	}
}

func TestCascadeDeleteRemovesChildren(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, run := seedRun(t, db)
	jobs := []*model.Job{{Name: "build"}}
	if err := db.CreateJobs(ctx, run.ID, jobs); err != nil {
		t.Fatal(err)
	}
	att, _ := db.CreateAttempt(ctx, jobs[0].ID, 1, time.Now())
	if err := db.RecordLog(ctx, &model.Log{
		AttemptID: att.ID, JobID: jobs[0].ID, RunID: run.ID,
		Stream: model.StreamStdout, Path: "/tmp/x.log",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordArtifact(ctx, &model.Artifact{
		RunID: run.ID, JobID: jobs[0].ID, Path: "a.txt", StorePath: "a.txt",
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteRun(ctx, run.ID); err != nil {
		t.Fatalf("DeleteRun() error = %v", err)
	}

	// Foreign keys must be enabled for these cascades to happen.
	remaining, err := db.ListJobs(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("jobs survived the run delete: %d", len(remaining))
	}
	arts, err := db.ListArtifacts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 0 {
		t.Errorf("artifacts survived the run delete: %d", len(arts))
	}
	if err := db.DeleteRun(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteRun error = %v, want ErrNotFound", err)
	}
}

func TestReconcileInterruptedRuns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "forge.db")

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p"})
	run, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
	if err := db.StartRun(ctx, run.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	jobs := []*model.Job{{Name: "a", Status: model.StatusRunning}, {Name: "b", Status: model.StatusQueued}}
	if err := db.CreateJobs(ctx, run.ID, jobs); err != nil {
		t.Fatal(err)
	}
	done, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
	if err := db.FinishRun(ctx, done.ID, model.StatusSuccess, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: close without finishing the active run.
	_ = db.Close()

	db2, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer func() { _ = db2.Close() }()

	n, err := db2.ReconcileInterruptedRuns(ctx)
	if err != nil {
		t.Fatalf("ReconcileInterruptedRuns() error = %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled %d runs, want 1", n)
	}

	reloaded, _ := db2.RunByID(ctx, run.ID)
	if reloaded.Status != model.StatusCancelled {
		t.Errorf("interrupted run status = %q, want cancelled", reloaded.Status)
	}
	if reloaded.Error == "" {
		t.Error("interrupted run should carry an explanation")
	}
	finished, _ := db2.RunByID(ctx, done.ID)
	if finished.Status != model.StatusSuccess {
		t.Errorf("already-finished run was modified: %q", finished.Status)
	}
	reloadedJobs, _ := db2.ListJobs(ctx, run.ID)
	for _, j := range reloadedJobs {
		if j.Status != model.StatusCancelled {
			t.Errorf("job %q status = %q, want cancelled", j.Name, j.Status)
		}
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p"})

	mk := func(status model.Status, dur time.Duration) *model.Run {
		run, err := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := db.StartRun(ctx, run.ID, start); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishRun(ctx, run.ID, status, start.Add(dur), ""); err != nil {
			t.Fatal(err)
		}
		return run
	}
	r1 := mk(model.StatusSuccess, 1*time.Second)
	mk(model.StatusSuccess, 3*time.Second)
	mk(model.StatusFailed, 2*time.Second)

	if err := db.CreateJobs(ctx, r1.ID, []*model.Job{
		{Name: "ok", Status: model.StatusSuccess},
		{Name: "bad", Status: model.StatusFailed},
		{Name: "skip", Status: model.StatusSkipped},
		{Name: "wait", Status: model.StatusQueued},
	}); err != nil {
		t.Fatal(err)
	}

	s, err := db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if s.RunsTotal != 3 {
		t.Errorf("RunsTotal = %d, want 3", s.RunsTotal)
	}
	if s.RunsSucceeded != 2 || s.RunsFailed != 1 {
		t.Errorf("run outcomes = %d success / %d failed, want 2/1", s.RunsSucceeded, s.RunsFailed)
	}
	if s.AvgRunDurationMS != 2000 {
		t.Errorf("AvgRunDurationMS = %d, want 2000", s.AvgRunDurationMS)
	}
	if s.MaxRunDurationMS != 3000 {
		t.Errorf("MaxRunDurationMS = %d, want 3000", s.MaxRunDurationMS)
	}
	if s.JobsSucceeded != 1 || s.JobsFailed != 1 || s.JobsSkipped != 1 || s.JobsQueued != 1 {
		t.Errorf("job counters wrong: %+v", s)
	}
	want := 2.0 / 3.0
	if diff := s.SuccessRate - want; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("SuccessRate = %v, want %v", s.SuccessRate, want)
	}

	points, err := db.RecentDurations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("len(RecentDurations) = %d, want 3", len(points))
	}
	// Chronological order for plotting.
	if points[0].RunID > points[2].RunID {
		t.Error("RecentDurations should be oldest first")
	}
}

func TestPruneRuns(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p"})

	for i := 0; i < 5; i++ {
		run, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
		if err := db.FinishRun(ctx, run.ID, model.StatusSuccess, time.Now(), ""); err != nil {
			t.Fatal(err)
		}
	}
	// An active run must never be pruned.
	active, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})

	pruned, err := db.PruneRuns(ctx, 2)
	if err != nil {
		t.Fatalf("PruneRuns() error = %v", err)
	}
	if len(pruned) != 3 {
		t.Errorf("pruned %d runs, want 3", len(pruned))
	}
	for _, id := range pruned {
		if id == active.ID {
			t.Error("PruneRuns removed an active run")
		}
	}

	remaining, _ := db.ListRuns(ctx, RunFilter{})
	if len(remaining) != 3 { // 2 kept + 1 active
		t.Errorf("len(remaining) = %d, want 3", len(remaining))
	}
}

func TestListPipelinesIncludesStats(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "p"})
	if _, err := db.UpsertPipeline(ctx, &model.Pipeline{Name: "never-run"}); err != nil {
		t.Fatal(err)
	}

	run, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
	start := time.Now()
	_ = db.StartRun(ctx, run.ID, start)
	_ = db.FinishRun(ctx, run.ID, model.StatusFailed, start.Add(2*time.Second), "nope")

	list, err := db.ListPipelines(ctx)
	if err != nil {
		t.Fatalf("ListPipelines() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(pipelines) = %d, want 2", len(list))
	}

	byName := map[string]*PipelineWithStats{}
	for _, p := range list {
		byName[p.Name] = p
	}
	if got := byName["p"].Stats.Runs; got != 1 {
		t.Errorf("p.Stats.Runs = %d, want 1", got)
	}
	if got := byName["p"].Stats.LastStatus; got != model.StatusFailed {
		t.Errorf("p.Stats.LastStatus = %q, want failed", got)
	}
	if got := byName["p"].Stats.AvgDuration; got != 2000 {
		t.Errorf("p.Stats.AvgDuration = %d, want 2000", got)
	}
	if got := byName["never-run"].Stats.Runs; got != 0 {
		t.Errorf("never-run.Stats.Runs = %d, want 0", got)
	}
	if byName["never-run"].Stats.LastStatus != "" {
		t.Errorf("never-run should have no last status, got %q", byName["never-run"].Stats.LastStatus)
	}
}

func TestStatePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "forge.db")

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.UpsertPipeline(ctx, &model.Pipeline{Name: "durable", SpecYAML: "name: durable"})
	run, _ := db.CreateRun(ctx, &model.Run{PipelineID: p.ID})
	if err := db.CreateJobs(ctx, run.ID, []*model.Job{{Name: "a", Status: model.StatusSuccess}}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(ctx, run.ID, model.StatusSuccess, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db2, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = db2.Close() }()

	reloaded, err := db2.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("run did not survive restart: %v", err)
	}
	if reloaded.Status != model.StatusSuccess {
		t.Errorf("status after restart = %q, want success", reloaded.Status)
	}
	jobs, _ := db2.ListJobs(ctx, run.ID)
	if len(jobs) != 1 || jobs[0].Name != "a" {
		t.Errorf("jobs did not survive restart: %+v", jobs)
	}
}
