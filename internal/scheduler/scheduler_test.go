package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/executor"
	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/nickcross-79/forge/internal/secrets"
	"github.com/nickcross-79/forge/internal/store"
)

// fixtureSecretValue stands in for a secret in the redaction tests. It is not a
// real credential.
const fixtureSecretValue = "example-not-a-real-secret"

// fakeExecutor records what ran and lets a test dictate each job's outcome.
type fakeExecutor struct {
	mu sync.Mutex
	// behaviour maps a job name to the results it returns on successive attempts.
	// The last entry is reused once exhausted.
	behaviour map[string][]executor.Result
	// hold blocks the named job until its channel is closed.
	hold map[string]chan struct{}
	// delay makes a job take time, so overlap can be observed.
	delay map[string]time.Duration

	started  []string
	finished []string
	attempts map[string]int

	concurrent    atomic.Int64
	maxConcurrent atomic.Int64

	// onRun, if set, is invoked inside the job with its workspace directory.
	onRun func(job executor.Job)
}

func newFake() *fakeExecutor {
	return &fakeExecutor{
		behaviour: map[string][]executor.Result{},
		hold:      map[string]chan struct{}{},
		delay:     map[string]time.Duration{},
		attempts:  map[string]int{},
	}
}

func (f *fakeExecutor) Run(ctx context.Context, job executor.Job) executor.Result {
	started := time.Now()

	f.mu.Lock()
	f.started = append(f.started, job.Name)
	f.attempts[job.Name]++
	attempt := f.attempts[job.Name]
	results := f.behaviour[job.Name]
	hold := f.hold[job.Name]
	delay := f.delay[job.Name]
	onRun := f.onRun
	f.mu.Unlock()

	n := f.concurrent.Add(1)
	for {
		peak := f.maxConcurrent.Load()
		if n <= peak || f.maxConcurrent.CompareAndSwap(peak, n) {
			break
		}
	}
	defer f.concurrent.Add(-1)

	if onRun != nil {
		onRun(job)
	}
	if job.Stdout != nil {
		fmt.Fprintf(job.Stdout, "running %s (attempt %d)\n", job.Name, attempt)
	}

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return executor.Result{ExitCode: -1, Cancelled: true, Err: ctx.Err(),
				StartedAt: started, EndedAt: time.Now()}
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return executor.Result{ExitCode: -1, Cancelled: true, Err: ctx.Err(),
				StartedAt: started, EndedAt: time.Now()}
		}
	}
	if ctx.Err() != nil {
		return executor.Result{ExitCode: -1, Cancelled: true, Err: ctx.Err(),
			StartedAt: started, EndedAt: time.Now()}
	}

	f.mu.Lock()
	f.finished = append(f.finished, job.Name)
	f.mu.Unlock()

	res := executor.Result{ExitCode: 0, StartedAt: started, EndedAt: time.Now()}
	if len(results) > 0 {
		idx := attempt - 1
		if idx >= len(results) {
			idx = len(results) - 1
		}
		res = results[idx]
		res.StartedAt = started
		res.EndedAt = time.Now()
	}
	return res
}

func (f *fakeExecutor) fail(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behaviour[name] = []executor.Result{{ExitCode: code, Err: fmt.Errorf("job %q failed: exit status %d", name, code)}}
}

// failThenSucceed makes a job fail `times` times before succeeding.
func (f *fakeExecutor) failThenSucceed(name string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := make([]executor.Result, 0, times+1)
	for i := 0; i < times; i++ {
		seq = append(seq, executor.Result{ExitCode: 1, Err: errors.New("transient")})
	}
	seq = append(seq, executor.Result{ExitCode: 0})
	f.behaviour[name] = seq
}

func (f *fakeExecutor) startOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func (f *fakeExecutor) ran(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.started {
		if n == name {
			return true
		}
	}
	return false
}

func (f *fakeExecutor) attemptCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[name]
}

// harness wires a scheduler against temporary storage.
type harness struct {
	t    *testing.T
	sch  *Scheduler
	db   *store.DB
	cfg  *config.Config
	fake *fakeExecutor
}

func newHarness(t *testing.T, tune ...func(*config.Config)) *harness {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default(root)
	cfg.Home = filepath.Join(root, ".forge")
	cfg.Database.Path = filepath.Join(cfg.Home, "forge.db")
	cfg.Artifacts.Dir = filepath.Join(cfg.Home, "artifacts")
	cfg.Logs.Dir = filepath.Join(cfg.Home, "logs")
	cfg.Runner.Concurrency = 4
	cfg.Runner.RetryBackoff = time.Millisecond
	cfg.Runner.DefaultTimeout = 30 * time.Second
	for _, fn := range tune {
		fn(cfg)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(context.Background(), store.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fake := newFake()
	sch, err := New(Options{
		Config: cfg,
		DB:     db,
		Local:  fake,
		Docker: fake,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, sch: sch, db: db, cfg: cfg, fake: fake}
}

func (h *harness) load(doc string) (*pipeline.Spec, *pipeline.Graph) {
	h.t.Helper()
	spec, err := pipeline.Parse([]byte(doc))
	if err != nil {
		h.t.Fatalf("parse pipeline: %v", err)
	}
	if err := spec.Validate(); err != nil {
		h.t.Fatalf("validate pipeline: %v", err)
	}
	graph, err := spec.Graph()
	if err != nil {
		h.t.Fatalf("build graph: %v", err)
	}
	return spec, graph
}

func (h *harness) run(doc string, opts ...func(*RunOptions)) *model.Run {
	h.t.Helper()
	spec, graph := h.load(doc)
	o := RunOptions{Trigger: model.TriggerCLI, Manual: ManualSkip}
	for _, fn := range opts {
		fn(&o)
	}
	run, err := h.sch.Run(context.Background(), spec, graph, o)
	if err != nil {
		h.t.Fatalf("Run() error = %v", err)
	}
	return run
}

func (h *harness) jobs(runID int64) map[string]*model.Job {
	h.t.Helper()
	list, err := h.db.ListJobs(context.Background(), runID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make(map[string]*model.Job, len(list))
	for _, j := range list {
		out[j.Name] = j
	}
	return out
}

func (h *harness) assertStatuses(runID int64, want map[string]model.Status) {
	h.t.Helper()
	jobs := h.jobs(runID)
	for name, wantStatus := range want {
		job, ok := jobs[name]
		if !ok {
			h.t.Errorf("job %q is missing from the run", name)
			continue
		}
		if job.Status != wantStatus {
			h.t.Errorf("job %q status = %q, want %q (error: %s)", name, job.Status, wantStatus, job.Error)
		}
	}
}

// --- tests ------------------------------------------------------------------

func TestRunLinearPipeline(t *testing.T) {
	h := newHarness(t)
	run := h.run(`
name: linear
stages: [build, test, package]
jobs:
  build:
    stage: build
    commands: [echo build]
  test:
    stage: test
    needs: [build]
    commands: [echo test]
  package:
    stage: package
    needs: [test]
    commands: [echo package]
`)

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success (error: %s)", run.Status, run.Error)
	}
	if got := h.fake.startOrder(); strings.Join(got, ",") != "build,test,package" {
		t.Errorf("execution order = %v, want [build test package]", got)
	}
	h.assertStatuses(run.ID, map[string]model.Status{
		"build": model.StatusSuccess, "test": model.StatusSuccess, "package": model.StatusSuccess,
	})
	if run.DurationMS < 0 {
		t.Error("run duration is negative")
	}
	for name, job := range h.jobs(run.ID) {
		if job.StartedAt == nil || job.FinishedAt == nil {
			t.Errorf("job %q is missing start/finish timestamps", name)
		}
	}
}

func TestIndependentJobsRunConcurrently(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Runner.Concurrency = 4 })
	for _, name := range []string{"a", "b", "c", "d"} {
		h.fake.delay[name] = 120 * time.Millisecond
	}

	run := h.run(`
name: fanout
jobs:
  a: {commands: [echo a]}
  b: {commands: [echo b]}
  c: {commands: [echo c]}
  d: {commands: [echo d]}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success", run.Status)
	}
	if peak := h.fake.maxConcurrent.Load(); peak < 2 {
		t.Errorf("peak concurrency = %d; independent jobs did not overlap", peak)
	}
}

func TestConcurrencyLimitIsEnforced(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Runner.Concurrency = 2 })
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		h.fake.delay[name] = 60 * time.Millisecond
	}

	run := h.run(`
name: limited
jobs:
  a: {commands: [x]}
  b: {commands: [x]}
  c: {commands: [x]}
  d: {commands: [x]}
  e: {commands: [x]}
  f: {commands: [x]}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success", run.Status)
	}
	if peak := h.fake.maxConcurrent.Load(); peak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", peak)
	}
	if len(h.fake.startOrder()) != 6 {
		t.Errorf("started %d jobs, want 6", len(h.fake.startOrder()))
	}
}

func TestPerRunConcurrencyOverride(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Runner.Concurrency = 8 })
	for _, name := range []string{"a", "b", "c", "d"} {
		h.fake.delay[name] = 60 * time.Millisecond
	}
	run := h.run(`
name: override
jobs:
  a: {commands: [x]}
  b: {commands: [x]}
  c: {commands: [x]}
  d: {commands: [x]}
`, func(o *RunOptions) { o.Concurrency = 1 })

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q", run.Status)
	}
	if peak := h.fake.maxConcurrent.Load(); peak != 1 {
		t.Errorf("peak concurrency = %d, want 1", peak)
	}
}

func TestDependenciesWaitForUpstream(t *testing.T) {
	h := newHarness(t)
	h.fake.delay["slow"] = 100 * time.Millisecond

	run := h.run(`
name: diamond
jobs:
  root:  {commands: [x]}
  slow:  {commands: [x], needs: [root]}
  quick: {commands: [x], needs: [root]}
  join:  {commands: [x], needs: [slow, quick]}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q", run.Status)
	}

	order := h.fake.startOrder()
	pos := map[string]int{}
	for i, name := range order {
		pos[name] = i
	}
	if pos["root"] != 0 {
		t.Errorf("root should run first, order = %v", order)
	}
	if pos["join"] != len(order)-1 {
		t.Errorf("join should run last, order = %v", order)
	}
}

func TestFailurePropagatesToDependents(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("build", 2)

	run := h.run(`
name: failing
jobs:
  build:   {commands: [x]}
  test:    {commands: [x], needs: [build]}
  package: {commands: [x], needs: [test]}
`)

	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	if !strings.Contains(run.Error, "build") {
		t.Errorf("run error = %q, should name the failing job", run.Error)
	}
	h.assertStatuses(run.ID, map[string]model.Status{
		"build":   model.StatusFailed,
		"test":    model.StatusSkipped,
		"package": model.StatusSkipped,
	})
	if h.fake.ran("test") || h.fake.ran("package") {
		t.Error("downstream jobs executed despite an upstream failure")
	}

	jobs := h.jobs(run.ID)
	if jobs["build"].ExitCode == nil || *jobs["build"].ExitCode != 2 {
		t.Errorf("build exit code = %v, want 2", jobs["build"].ExitCode)
	}
	// A skipped job should explain itself.
	if !strings.Contains(jobs["test"].Error, "build") {
		t.Errorf("test skip reason = %q, should mention the failed dependency", jobs["test"].Error)
	}
}

func TestAllowFailureLetsDependentsProceed(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("flaky", 1)

	run := h.run(`
name: tolerant
jobs:
  flaky:
    commands: [x]
    allow_failure: true
  after:
    commands: [x]
    needs: [flaky]
`)

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success: allow_failure should not fail the run (error: %s)", run.Status, run.Error)
	}
	h.assertStatuses(run.ID, map[string]model.Status{
		"flaky": model.StatusFailed,
		"after": model.StatusSuccess,
	})
	if !h.fake.ran("after") {
		t.Error("dependent of an allow_failure job did not run")
	}
}

func TestRetriesUntilSuccess(t *testing.T) {
	h := newHarness(t)
	h.fake.failThenSucceed("flaky", 2)

	run := h.run(`
name: retry
jobs:
  flaky:
    commands: [x]
    retry:
      max: 3
      backoff: 1ms
`)

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success after retries (error: %s)", run.Status, run.Error)
	}
	if got := h.fake.attemptCount("flaky"); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}

	jobs := h.jobs(run.ID)
	if jobs["flaky"].Attempts != 3 {
		t.Errorf("recorded attempts = %d, want 3", jobs["flaky"].Attempts)
	}
	attempts, err := h.db.ListAttempts(context.Background(), jobs["flaky"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 {
		t.Fatalf("len(attempt rows) = %d, want 3", len(attempts))
	}
	if attempts[0].Status != model.StatusFailed || attempts[2].Status != model.StatusSuccess {
		t.Errorf("attempt statuses = %q, %q, %q; want failed, failed, success",
			attempts[0].Status, attempts[1].Status, attempts[2].Status)
	}
}

func TestRetriesExhausted(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("doomed", 1)

	run := h.run(`
name: retry-exhausted
jobs:
  doomed:
    commands: [x]
    retry: 2
`)
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	// retry: 2 means two extra attempts, three in total.
	if got := h.fake.attemptCount("doomed"); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestWhenAlwaysRunsAfterFailure(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("build", 1)

	run := h.run(`
name: cleanup
jobs:
  build:
    commands: [x]
  cleanup:
    commands: [x]
    needs: [build]
    when: always
`)

	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	h.assertStatuses(run.ID, map[string]model.Status{
		"build":   model.StatusFailed,
		"cleanup": model.StatusSuccess,
	})
	if !h.fake.ran("cleanup") {
		t.Error("when: always job did not run after a failure")
	}
}

func TestWhenOnFailure(t *testing.T) {
	tests := []struct {
		name       string
		failBuild  bool
		wantNotify model.Status
	}{
		{"runs when something failed", true, model.StatusSuccess},
		{"skipped when everything passed", false, model.StatusSkipped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.failBuild {
				h.fake.fail("build", 1)
			}
			run := h.run(`
name: notify
jobs:
  build:
    commands: [x]
  notify:
    commands: [x]
    needs: [build]
    when: on_failure
`)
			h.assertStatuses(run.ID, map[string]model.Status{"notify": tc.wantNotify})
		})
	}
}

func TestWhenNever(t *testing.T) {
	h := newHarness(t)
	run := h.run(`
name: disabled
jobs:
  active:   {commands: [x]}
  disabled: {commands: [x], when: never}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success", run.Status)
	}
	h.assertStatuses(run.ID, map[string]model.Status{"disabled": model.StatusSkipped})
	if h.fake.ran("disabled") {
		t.Error("a when: never job executed")
	}
}

func TestIfConditionControlsExecution(t *testing.T) {
	h := newHarness(t)
	run := h.run(`
name: conditional
variables:
  DEPLOY_ENV: staging
jobs:
  always-runs:
    commands: [x]
    if: $DEPLOY_ENV == "staging"
  never-runs:
    commands: [x]
    if: $DEPLOY_ENV == "production"
  uses-builtin:
    commands: [x]
    if: $CI
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success", run.Status)
	}
	h.assertStatuses(run.ID, map[string]model.Status{
		"always-runs":  model.StatusSuccess,
		"never-runs":   model.StatusSkipped,
		"uses-builtin": model.StatusSuccess,
	})

	jobs := h.jobs(run.ID)
	if !strings.Contains(jobs["never-runs"].Error, "condition") {
		t.Errorf("skip reason = %q, should mention the condition", jobs["never-runs"].Error)
	}
}

func TestCancellation(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.fake.hold["slow"] = gate

	spec, graph := h.load(`
name: cancellable
jobs:
  slow:  {commands: [x]}
  after: {commands: [x], needs: [slow]}
`)

	run, err := h.sch.Prepare(context.Background(), spec, graph, RunOptions{Manual: ManualSkip})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan *model.Run, 1)
	go func() {
		final, err := h.sch.Execute(context.Background(), run, spec, graph, RunOptions{Manual: ManualSkip})
		if err != nil {
			t.Errorf("Execute() error = %v", err)
		}
		done <- final
	}()

	// Wait for the job to be in flight, then cancel.
	deadline := time.After(5 * time.Second)
	for !h.sch.IsActive(run.ID) || !h.fake.ran("slow") {
		select {
		case <-deadline:
			t.Fatal("job never started")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := h.sch.Cancel(run.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	select {
	case final := <-done:
		if final.Status != model.StatusCancelled {
			t.Errorf("run status = %q, want cancelled", final.Status)
		}
		h.assertStatuses(run.ID, map[string]model.Status{
			"slow":  model.StatusCancelled,
			"after": model.StatusCancelled,
		})
	case <-time.After(15 * time.Second):
		close(gate)
		t.Fatal("cancellation did not finish the run")
	}
	close(gate)
}

func TestCancelUnknownRun(t *testing.T) {
	h := newHarness(t)
	if err := h.sch.Cancel(12345); !errors.Is(err, ErrRunNotActive) {
		t.Errorf("Cancel(unknown) error = %v, want ErrRunNotActive", err)
	}
}

func TestManualSkipPolicy(t *testing.T) {
	h := newHarness(t)
	run := h.run(`
name: gated
jobs:
  build:  {commands: [x]}
  deploy: {commands: [x], needs: [build], when: manual}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success", run.Status)
	}
	h.assertStatuses(run.ID, map[string]model.Status{"deploy": model.StatusSkipped})
	if h.fake.ran("deploy") {
		t.Error("a manual job ran without approval under the skip policy")
	}
}

func TestManualPreApproved(t *testing.T) {
	h := newHarness(t)
	run := h.run(`
name: gated
jobs:
  build:  {commands: [x]}
  deploy: {commands: [x], needs: [build], when: manual}
`, func(o *RunOptions) { o.Approved = []string{"deploy"} })

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success (error: %s)", run.Status, run.Error)
	}
	h.assertStatuses(run.ID, map[string]model.Status{"deploy": model.StatusSuccess})
	if !h.fake.ran("deploy") {
		t.Error("a pre-approved manual job did not run")
	}
}

func TestManualWaitThenApprove(t *testing.T) {
	h := newHarness(t)
	spec, graph := h.load(`
name: gated
jobs:
  build:  {commands: [x]}
  deploy: {commands: [x], needs: [build], when: manual}
`)
	opts := RunOptions{Manual: ManualWait}
	run, err := h.sch.Prepare(context.Background(), spec, graph, opts)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan *model.Run, 1)
	go func() {
		final, err := h.sch.Execute(context.Background(), run, spec, graph, opts)
		if err != nil {
			t.Errorf("Execute() error = %v", err)
		}
		done <- final
	}()

	// Wait for the gate to be reached.
	deadline := time.After(10 * time.Second)
	for {
		jobs := h.jobs(run.ID)
		if jobs["deploy"] != nil && jobs["deploy"].Status == model.StatusAwaitingManual {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("deploy never reached awaiting_manual (status %q)", jobs["deploy"].Status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if err := h.sch.Approve(run.ID, "deploy"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	select {
	case final := <-done:
		if final.Status != model.StatusSuccess {
			t.Errorf("run status = %q, want success (error: %s)", final.Status, final.Error)
		}
		h.assertStatuses(run.ID, map[string]model.Status{"deploy": model.StatusSuccess})
	case <-time.After(15 * time.Second):
		t.Fatal("run did not finish after approval")
	}
}

func TestManualTimeoutSkips(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Runner.ManualTimeout = 80 * time.Millisecond })
	spec, graph := h.load(`
name: gated
jobs:
  deploy: {commands: [x], when: manual}
`)
	opts := RunOptions{Manual: ManualWait}
	run, err := h.sch.Run(context.Background(), spec, graph, opts)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.StatusSuccess {
		t.Errorf("run status = %q, want success", run.Status)
	}
	h.assertStatuses(run.ID, map[string]model.Status{"deploy": model.StatusSkipped})

	jobs := h.jobs(run.ID)
	if !strings.Contains(jobs["deploy"].Error, "not received") {
		t.Errorf("skip reason = %q, should mention the approval timeout", jobs["deploy"].Error)
	}
}

func TestApproveErrors(t *testing.T) {
	h := newHarness(t)
	if err := h.sch.Approve(999, "job"); !errors.Is(err, ErrRunNotActive) {
		t.Errorf("Approve(unknown run) error = %v, want ErrRunNotActive", err)
	}
}

func TestManualGateDoesNotBlockOtherJobs(t *testing.T) {
	// A manual job waits off the worker pool, so with a single worker an
	// unrelated job must still be able to run while the gate is open.
	h := newHarness(t, func(c *config.Config) {
		c.Runner.Concurrency = 1
		c.Runner.ManualTimeout = 3 * time.Second
	})
	spec, graph := h.load(`
name: gated-parallel
jobs:
  gated:       {commands: [x], when: manual}
  independent: {commands: [x]}
`)
	opts := RunOptions{Manual: ManualWait}
	run, err := h.sch.Run(context.Background(), spec, graph, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !h.fake.ran("independent") {
		t.Error("an independent job was blocked by an open manual gate")
	}
	h.assertStatuses(run.ID, map[string]model.Status{
		"independent": model.StatusSuccess,
		"gated":       model.StatusSkipped,
	})
}

func TestArtifactsFlowAlongNeeds(t *testing.T) {
	h := newHarness(t)
	// The producer writes a file; the consumer asserts it is present in its own,
	// separate workspace.
	var consumerSaw string
	h.fake.onRun = func(job executor.Job) {
		switch job.Name {
		case "producer":
			dir := filepath.Join(job.Workspace, "dist")
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte("hello\n"), 0o600); err != nil {
				t.Error(err)
			}
		case "consumer":
			body, err := os.ReadFile(filepath.Join(job.Workspace, "dist", "output.txt"))
			if err == nil {
				consumerSaw = string(body)
			}
		}
	}

	run := h.run(`
name: artifacts
jobs:
  producer:
    commands: [x]
    artifacts:
      paths: [dist/]
  consumer:
    commands: [x]
    needs: [producer]
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q (error: %s)", run.Status, run.Error)
	}
	if consumerSaw != "hello\n" {
		t.Errorf("consumer saw %q, want the producer's artifact restored into its workspace", consumerSaw)
	}

	stored, err := h.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("len(artifacts) = %d, want 1", len(stored))
	}
	if stored[0].Path != "dist/output.txt" {
		t.Errorf("artifact path = %q, want dist/output.txt", stored[0].Path)
	}
	if stored[0].Size != 6 || stored[0].SHA256 == "" {
		t.Errorf("artifact metadata incomplete: %+v", stored[0])
	}
	if stored[0].ExpiresAt == nil {
		t.Error("artifact should inherit the configured retention deadline")
	}
}

func TestArtifactsOnFailure(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("crashing", 1)
	h.fake.onRun = func(job executor.Job) {
		_ = os.WriteFile(filepath.Join(job.Workspace, "crash.log"), []byte("stack trace"), 0o600)
	}

	run := h.run(`
name: crash-artifacts
jobs:
  crashing:
    commands: [x]
    artifacts:
      paths: [crash.log]
      when: on_failure
`)
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	stored, err := h.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Path != "crash.log" {
		t.Errorf("artifacts = %+v, want crash.log collected from the failed job", stored)
	}
}

func TestArtifactsNotCollectedOnFailureByDefault(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("build", 1)
	h.fake.onRun = func(job executor.Job) {
		_ = os.WriteFile(filepath.Join(job.Workspace, "partial.txt"), []byte("incomplete"), 0o600)
	}

	run := h.run(`
name: no-artifacts-on-failure
jobs:
  build:
    commands: [x]
    artifacts:
      paths: [partial.txt]
`)
	stored, err := h.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("artifacts = %+v, want none: the default is on_success", stored)
	}
	_ = run
}

func TestIsolatedWorkspacesDoNotShare(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	workspaces := map[string]string{}
	h.fake.onRun = func(job executor.Job) {
		mu.Lock()
		workspaces[job.Name] = job.Workspace
		mu.Unlock()
		_ = os.WriteFile(filepath.Join(job.Workspace, job.Name+".marker"), []byte("x"), 0o600)
	}

	run := h.run(`
name: isolated
jobs:
  a: {commands: [x]}
  b: {commands: [x]}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q", run.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if workspaces["a"] == workspaces["b"] {
		t.Fatalf("both jobs shared workspace %q; isolated mode should give each its own", workspaces["a"])
	}
	// Neither job should see the other's marker file.
	if _, err := os.Stat(filepath.Join(workspaces["a"], "b.marker")); err == nil {
		t.Error("job a's workspace contains job b's file")
	}
}

func TestSharedWorkspaceMode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Workspace.Mode = config.WorkspaceShared })
	var mu sync.Mutex
	workspaces := map[string]string{}
	h.fake.onRun = func(job executor.Job) {
		mu.Lock()
		workspaces[job.Name] = job.Workspace
		mu.Unlock()
	}

	run := h.run(`
name: shared
jobs:
  a: {commands: [x]}
  b: {commands: [x], needs: [a]}
`)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q", run.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if workspaces["a"] != workspaces["b"] {
		t.Errorf("shared mode gave different workspaces: %q and %q", workspaces["a"], workspaces["b"])
	}
}

func TestWorkspaceSeededFromProject(t *testing.T) {
	h := newHarness(t)
	// A file in the project root must appear in the job workspace; the forge
	// home must not.
	if err := os.WriteFile(filepath.Join(h.cfg.Root, "source.txt"), []byte("code"), 0o600); err != nil {
		t.Fatal(err)
	}

	var sawSource, sawForgeHome bool
	h.fake.onRun = func(job executor.Job) {
		if _, err := os.Stat(filepath.Join(job.Workspace, "source.txt")); err == nil {
			sawSource = true
		}
		if _, err := os.Stat(filepath.Join(job.Workspace, ".forge")); err == nil {
			sawForgeHome = true
		}
	}

	h.run("name: seed\njobs:\n  a: {commands: [x]}\n")
	if !sawSource {
		t.Error("project files were not copied into the job workspace")
	}
	if sawForgeHome {
		t.Error(".forge was copied into the job workspace; it should be ignored")
	}
}

func TestLogsAreCapturedAndPersisted(t *testing.T) {
	h := newHarness(t)
	run := h.run("name: logged\njobs:\n  a: {commands: [x]}\n")

	jobs := h.jobs(run.ID)
	records, err := h.db.ListLogs(context.Background(), jobs["a"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("len(log records) = %d, want 2 (stdout and stderr)", len(records))
	}

	var foundStdout bool
	for _, rec := range records {
		if rec.Stream != model.StreamStdout {
			continue
		}
		foundStdout = true
		body, err := logs.ReadAll(rec.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "running a") {
			t.Errorf("stdout log = %q, want the job's output", body)
		}
		if rec.Bytes == 0 {
			t.Error("log record has a zero byte count")
		}
	}
	if !foundStdout {
		t.Error("no stdout log record was written")
	}
}

func TestSecretsAreInjectedAndRedacted(t *testing.T) {
	h := newHarness(t)
	sec := secrets.New()
	sec.Set("API_TOKEN", fixtureSecretValue)
	h.sch.secrets = sec

	var sawToken string
	h.fake.onRun = func(job executor.Job) {
		sawToken = job.Env["API_TOKEN"]
		// A job that leaks its secret to stdout must still not leak it to disk.
		fmt.Fprintf(job.Stdout, "token is %s\n", job.Env["API_TOKEN"])
	}

	run := h.run("name: secrets\njobs:\n  a: {commands: [x]}\n")
	if sawToken != fixtureSecretValue {
		t.Errorf("secret was not injected into the job env, got %q", sawToken)
	}

	jobs := h.jobs(run.ID)
	records, err := h.db.ListLogs(context.Background(), jobs["a"].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		body, err := logs.ReadAll(rec.Path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), fixtureSecretValue) {
			t.Errorf("secret leaked into the %s log: %q", rec.Stream, body)
		}
		if rec.Stream == model.StreamStdout && !strings.Contains(string(body), logs.Mask) {
			t.Errorf("stdout should contain the mask, got %q", body)
		}
	}
}

func TestScopedSecrets(t *testing.T) {
	h := newHarness(t)
	sec := secrets.New()
	sec.Set("DEPLOY_KEY", "example-deploy-value")
	sec.Set("OTHER_KEY", "example-other-value")
	h.sch.secrets = sec

	var mu sync.Mutex
	seen := map[string]map[string]string{}
	h.fake.onRun = func(job executor.Job) {
		mu.Lock()
		defer mu.Unlock()
		seen[job.Name] = map[string]string{
			"DEPLOY_KEY": job.Env["DEPLOY_KEY"],
			"OTHER_KEY":  job.Env["OTHER_KEY"],
		}
	}

	h.run(`
name: scoped
jobs:
  scoped:
    commands: [x]
    secrets: [DEPLOY_KEY]
  unscoped:
    commands: [x]
`)

	mu.Lock()
	defer mu.Unlock()
	if seen["scoped"]["DEPLOY_KEY"] != "example-deploy-value" {
		t.Error("a named secret was not injected")
	}
	if seen["scoped"]["OTHER_KEY"] != "" {
		t.Error("a secret outside the job's `secrets` list was injected")
	}
	if seen["unscoped"]["OTHER_KEY"] != "example-other-value" {
		t.Error("a job with no `secrets` list should receive all secrets")
	}
}

func TestForgeEnvironmentVariables(t *testing.T) {
	h := newHarness(t)
	var env map[string]string
	h.fake.onRun = func(job executor.Job) { env = job.Env }

	run := h.run("name: envtest\njobs:\n  build: {commands: [x], stage: default}\n")

	checks := map[string]string{
		"CI":               "true",
		"FORGE":            "true",
		"FORGE_PIPELINE":   "envtest",
		"FORGE_JOB":        "build",
		"FORGE_RUN_NUMBER": fmt.Sprintf("%d", run.Number),
		"FORGE_ATTEMPT":    "1",
	}
	for key, want := range checks {
		if env[key] != want {
			t.Errorf("env[%s] = %q, want %q", key, env[key], want)
		}
	}
	if env["FORGE_WORKSPACE"] == "" {
		t.Error("FORGE_WORKSPACE is empty")
	}
}

func TestHostEnvironmentIsNotLeaked(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FORGE_TEST_LEAKY", "should-not-appear")

	var env map[string]string
	h.fake.onRun = func(job executor.Job) { env = job.Env }
	h.run("name: envleak\njobs:\n  a: {commands: [x]}\n")

	if _, ok := env["FORGE_TEST_LEAKY"]; ok {
		t.Error("a host variable outside env_passthrough reached the job")
	}
	// Allow-listed variables do come through.
	if env["PATH"] == "" {
		t.Error("PATH should be passed through to jobs")
	}
}

func TestEventTimelineRecordsTransitions(t *testing.T) {
	h := newHarness(t)
	run := h.run("name: events\njobs:\n  a: {commands: [x]}\n")

	events, err := h.db.ListEvents(context.Background(), run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 {
		t.Fatalf("len(events) = %d, want at least 3 transitions", len(events))
	}

	var sawRunStart, sawRunEnd, sawJob bool
	for _, e := range events {
		switch {
		case e.Type == model.EventRunStatus && e.To == model.StatusRunning:
			sawRunStart = true
		case e.Type == model.EventRunStatus && e.To == model.StatusSuccess:
			sawRunEnd = true
		case e.Type == model.EventJobStatus && e.JobName == "a":
			sawJob = true
		}
	}
	if !sawRunStart || !sawRunEnd || !sawJob {
		t.Errorf("timeline is missing transitions: start=%v end=%v job=%v", sawRunStart, sawRunEnd, sawJob)
	}
}

func TestMetricsAreRecorded(t *testing.T) {
	h := newHarness(t)
	h.fake.fail("bad", 1)
	h.run(`
name: metrics
jobs:
  good: {commands: [x]}
  bad:  {commands: [x]}
  never: {commands: [x], when: never}
`)

	snap := h.sch.Metrics().Snapshot()
	if snap.RunsStarted != 1 {
		t.Errorf("RunsStarted = %d, want 1", snap.RunsStarted)
	}
	if snap.RunsFailed != 1 {
		t.Errorf("RunsFailed = %d, want 1", snap.RunsFailed)
	}
	if snap.JobsSucceeded != 1 {
		t.Errorf("JobsSucceeded = %d, want 1", snap.JobsSucceeded)
	}
	if snap.JobsFailed != 1 {
		t.Errorf("JobsFailed = %d, want 1", snap.JobsFailed)
	}
	if snap.JobsSkipped != 1 {
		t.Errorf("JobsSkipped = %d, want 1", snap.JobsSkipped)
	}
	// Gauges must return to zero once the run is over.
	if snap.ActiveJobs != 0 || snap.ActiveRuns != 0 || snap.QueuedJobs != 0 {
		t.Errorf("gauges did not settle: active jobs=%d runs=%d queued=%d",
			snap.ActiveJobs, snap.ActiveRuns, snap.QueuedJobs)
	}
}

func TestStartIsAsynchronous(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.fake.hold["slow"] = gate

	spec, graph := h.load("name: async\njobs:\n  slow: {commands: [x]}\n")
	run, err := h.sch.Start(context.Background(), spec, graph, RunOptions{Trigger: model.TriggerAPI, Manual: ManualSkip})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if run.ID == 0 {
		t.Fatal("Start() returned a run without an ID")
	}

	close(gate)
	deadline := time.After(10 * time.Second)
	for {
		current, err := h.db.RunByID(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			if current.Status != model.StatusSuccess {
				t.Errorf("run status = %q, want success", current.Status)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("background run did not finish, status = %q", current.Status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestDeepDependencyChain(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Runner.Concurrency = 8 })
	var b strings.Builder
	b.WriteString("name: chain\njobs:\n")
	const depth = 25
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "  j%02d:\n    commands: [x]\n", i)
		if i > 0 {
			fmt.Fprintf(&b, "    needs: [j%02d]\n", i-1)
		}
	}

	run := h.run(b.String())
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q (error: %s)", run.Status, run.Error)
	}
	order := h.fake.startOrder()
	if len(order) != depth {
		t.Fatalf("ran %d jobs, want %d", len(order), depth)
	}
	for i, name := range order {
		want := fmt.Sprintf("j%02d", i)
		if name != want {
			t.Fatalf("order[%d] = %q, want %q (full order: %v)", i, name, want, order)
		}
	}
}

func TestWideFanOutFanIn(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Runner.Concurrency = 6 })
	var b strings.Builder
	b.WriteString("name: fan\njobs:\n  root:\n    commands: [x]\n")
	const width = 20
	needs := make([]string, 0, width)
	for i := 0; i < width; i++ {
		name := fmt.Sprintf("leaf%02d", i)
		fmt.Fprintf(&b, "  %s:\n    commands: [x]\n    needs: [root]\n", name)
		needs = append(needs, name)
	}
	fmt.Fprintf(&b, "  join:\n    commands: [x]\n    needs: [%s]\n", strings.Join(needs, ", "))

	run := h.run(b.String())
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q (error: %s)", run.Status, run.Error)
	}
	order := h.fake.startOrder()
	if len(order) != width+2 {
		t.Fatalf("ran %d jobs, want %d", len(order), width+2)
	}
	if order[0] != "root" || order[len(order)-1] != "join" {
		t.Errorf("fan-in ordering is wrong: first=%q last=%q", order[0], order[len(order)-1])
	}
}

func TestEmptyPipelineSucceeds(t *testing.T) {
	h := newHarness(t)
	spec := &pipeline.Spec{Name: "empty", Jobs: map[string]*pipeline.Job{}}
	graph, err := spec.Graph()
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.sch.Run(context.Background(), spec, graph, RunOptions{Manual: ManualSkip})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.Status != model.StatusSuccess {
		t.Errorf("empty pipeline status = %q, want success", run.Status)
	}
}

func TestRunNumbersIncrement(t *testing.T) {
	h := newHarness(t)
	doc := "name: numbered\njobs:\n  a: {commands: [x]}\n"
	first := h.run(doc)
	second := h.run(doc)
	if second.Number != first.Number+1 {
		t.Errorf("run numbers = %d then %d, want consecutive", first.Number, second.Number)
	}
	if second.PipelineID != first.PipelineID {
		t.Error("the same pipeline name produced two pipeline rows")
	}
}
