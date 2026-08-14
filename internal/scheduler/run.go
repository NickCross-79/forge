package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/nickcross-79/forge/internal/artifacts"
	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/executor"
	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
)

// runState is everything the engine needs to drive one run.
type runState struct {
	// ctx controls execution: cancelling it kills running jobs and stops
	// dispatch.
	ctx    context.Context
	cancel context.CancelFunc
	// dbCtx is never cancelled. Recording why a run stopped has to succeed even
	// when the run stopped because it was cancelled.
	dbCtx context.Context

	run   *model.Run
	spec  *pipeline.Spec
	graph *pipeline.Graph
	opts  RunOptions

	redactor *logs.Redactor

	// mu guards jobs' mutable fields and the failure flag.
	mu     sync.Mutex
	jobs   map[string]*jobState
	failed bool
}

// jobState tracks one job through the run.
type jobState struct {
	node   *pipeline.Node
	spec   *pipeline.Job
	record *model.Job

	status     model.Status
	dispatched bool
	manual     bool

	approved    chan struct{}
	approveOnce sync.Once
}

// outcome is what a worker reports back to the engine loop.
type outcome struct {
	name   string
	status model.Status
	err    error
}

func (s *Scheduler) newRunState(ctx context.Context, run *model.Run, spec *pipeline.Spec, graph *pipeline.Graph, opts RunOptions) (*runState, error) {
	records, err := s.db.ListJobs(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*model.Job, len(records))
	for _, r := range records {
		byName[r.Name] = r
	}

	dbCtx := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancel(dbCtx)

	rs := &runState{
		ctx:      runCtx,
		cancel:   cancel,
		dbCtx:    dbCtx,
		run:      run,
		spec:     spec,
		graph:    graph,
		opts:     opts,
		redactor: logs.NewRedactor(s.secrets.Values()),
		jobs:     make(map[string]*jobState, len(graph.Nodes)),
	}

	// The caller's context still cancels the run; it is decoupled above only so
	// that database writes during shutdown are not cancelled mid-flight.
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()

	preApproved := make(map[string]bool, len(opts.Approved))
	for _, name := range opts.Approved {
		preApproved[name] = true
	}

	for name, node := range graph.Nodes {
		record, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("scheduler: job %q has no database record", name)
		}
		js := &jobState{
			node:     node,
			spec:     node.Job,
			record:   record,
			status:   model.StatusQueued,
			manual:   node.Job.When == pipeline.WhenManual,
			approved: make(chan struct{}),
		}
		if js.manual && preApproved[name] {
			js.approveOnce.Do(func() { close(js.approved) })
		}
		rs.jobs[name] = js
	}
	return rs, nil
}

// execute is the engine loop.
//
// Ready jobs are pushed onto a buffered queue drained by a fixed pool of workers,
// which is what enforces the concurrency limit and keeps queueing fair: jobs are
// enqueued in the pipeline's deterministic order and workers take them in turn.
// The loop itself never blocks on anything but results, so a long-running job
// cannot stall the release of unrelated work.
func (s *Scheduler) execute(rs *runState) {
	total := len(rs.graph.Nodes)
	if total == 0 {
		s.finishRun(rs, model.StatusSuccess, "")
		return
	}

	started := time.Now()
	if err := s.db.StartRun(rs.dbCtx, rs.run.ID, started); err != nil {
		s.logger.Error("failed to mark run started", "run", rs.run.ID, "error", err)
	}
	s.emit(rs, nil, "", model.EventRunStatus, model.StatusQueued, model.StatusRunning, "run started")
	s.metrics.RunStarted()
	s.logger.Info("run started",
		"run", rs.run.ID, "pipeline", rs.spec.Name, "jobs", total,
		"concurrency", s.concurrency(rs))

	queue := make(chan string, total)
	results := make(chan outcome, total)

	var workers sync.WaitGroup
	for i := 0; i < s.concurrency(rs); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for name := range queue {
				s.metrics.JobDequeued()
				results <- s.runJob(rs, name)
			}
		}()
	}

	inflight := 0
	for {
		// Once the run is cancelled nothing new is dispatched; whatever is still
		// in flight is drained below and the rest is marked cancelled in finalize.
		if rs.ctx.Err() == nil {
			// Resolve everything decidable without running anything. This runs to a
			// fixed point because skipping one job can make its dependents
			// decidable in the same pass.
			for {
				progressed := false
				for _, name := range rs.graph.Order {
					act, ok := s.decide(rs, name)
					if !ok {
						continue
					}
					switch act {
					case actionSkip:
						s.markSkipped(rs, name)
						progressed = true
					case actionRun:
						rs.markDispatched(name)
						inflight++
						s.metrics.JobQueued()
						queue <- name
					case actionManual:
						rs.markDispatched(name)
						inflight++
						// The gate is awaited off the worker pool so it cannot
						// occupy a worker that other jobs need. Exactly one result
						// is produced per dispatched job: either the gate reports a
						// skip or cancellation itself, or it hands the job to a
						// worker which reports the real outcome.
						go func(name string) {
							out, enqueue := s.awaitApproval(rs, name)
							if enqueue {
								s.metrics.JobQueued()
								queue <- name
								return
							}
							results <- out
						}(name)
					}
				}
				if !progressed {
					break
				}
			}
		}

		if inflight == 0 {
			break
		}
		out := <-results
		inflight--
		rs.applyOutcome(out)
	}

	close(queue)
	workers.Wait()

	s.finalize(rs, started)
}

type action int

const (
	actionRun action = iota
	actionSkip
	actionManual
)

// decide reports what to do with a job, or ok=false if it is not yet decidable.
func (s *Scheduler) decide(rs *runState, name string) (action, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	js := rs.jobs[name]
	if js.dispatched || js.status != model.StatusQueued {
		return 0, false
	}
	// Every dependency must have reached a terminal state before this job can be
	// judged, whatever that state turned out to be.
	for _, need := range js.node.Needs {
		if !rs.jobs[need].status.Terminal() {
			return 0, false
		}
	}

	// `when` decides against the outcome so far; `if` decides against the
	// environment. Both must pass.
	switch js.spec.When {
	case pipeline.WhenNever:
		return actionSkip, true
	case pipeline.WhenAlways:
		// Runs regardless of upstream outcome.
	case pipeline.WhenOnFailure:
		if !rs.failed {
			return actionSkip, true
		}
	default: // on_success and manual
		if !rs.dependenciesSatisfiedLocked(js) || rs.failed {
			return actionSkip, true
		}
	}

	if js.spec.If != "" {
		ok, err := pipeline.EvalCondition(js.spec.If, s.conditionEnvironment(rs, js.spec))
		if err != nil {
			s.logger.Warn("condition failed to evaluate; skipping job",
				"run", rs.run.ID, "job", name, "if", js.spec.If, "error", err)
			return actionSkip, true
		}
		if !ok {
			return actionSkip, true
		}
	}

	if js.manual {
		return actionManual, true
	}
	return actionRun, true
}

// dependenciesSatisfiedLocked reports whether every dependency finished in a way
// that allows this job to proceed. A failed dependency marked allow_failure does
// not block; a skipped one does, which is what propagates a skip down the graph.
func (rs *runState) dependenciesSatisfiedLocked(js *jobState) bool {
	for _, need := range js.node.Needs {
		dep := rs.jobs[need]
		switch dep.status {
		case model.StatusSuccess:
		case model.StatusFailed:
			if !dep.spec.AllowFailure {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (rs *runState) markDispatched(name string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.jobs[name].dispatched = true
}

func (rs *runState) applyOutcome(out outcome) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	js := rs.jobs[out.name]
	js.status = out.status
	if out.status == model.StatusFailed && !js.spec.AllowFailure {
		rs.failed = true
	}
}

// concurrency is the effective worker count for this run.
func (s *Scheduler) concurrency(rs *runState) int {
	n := rs.opts.Concurrency
	if n <= 0 {
		n = s.cfg.Runner.Concurrency
	}
	if n <= 0 {
		n = 1
	}
	if total := len(rs.graph.Nodes); n > total {
		n = total
	}
	return n
}

// awaitApproval blocks a manual job until it is approved, times out, or the run
// is cancelled.
//
// It returns enqueue=true when the job should be handed to a worker, in which
// case the worker reports the outcome; otherwise the returned outcome is final.
// This split is what keeps exactly one result flowing back per dispatched job.
func (s *Scheduler) awaitApproval(rs *runState, name string) (outcome, bool) {
	rs.mu.Lock()
	approved := rs.jobs[name].approved
	rs.mu.Unlock()

	if rs.opts.Manual == ManualSkip {
		select {
		case <-approved: // pre-approved with --approve
			return outcome{}, true
		default:
			s.metrics.JobSkipped()
			s.finishJob(rs, name, model.StatusSkipped,
				nil, "manual job skipped: approval was not requested")
			return outcome{name: name, status: model.StatusSkipped}, false
		}
	}

	s.setJobStatus(rs, name, model.StatusAwaitingManual, "waiting for manual approval")
	if err := s.db.SetRunStatus(rs.dbCtx, rs.run.ID, model.StatusAwaitingManual); err != nil {
		s.logger.Warn("failed to mark run awaiting approval", "run", rs.run.ID, "error", err)
	}

	var timeout <-chan time.Time
	if d := s.cfg.Runner.ManualTimeout; d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case <-approved:
		// Other jobs may still be in flight, so the run goes back to running.
		if err := s.db.SetRunStatus(rs.dbCtx, rs.run.ID, model.StatusRunning); err != nil {
			s.logger.Warn("failed to restore run status", "run", rs.run.ID, "error", err)
		}
		s.setJobStatus(rs, name, model.StatusQueued, "manual job approved")
		return outcome{}, true
	case <-timeout:
		s.metrics.JobSkipped()
		s.finishJob(rs, name, model.StatusSkipped, nil,
			fmt.Sprintf("manual approval not received within %s", s.cfg.Runner.ManualTimeout))
		return outcome{name: name, status: model.StatusSkipped}, false
	case <-rs.ctx.Done():
		s.finishJob(rs, name, model.StatusCancelled, nil, "run cancelled while awaiting approval")
		return outcome{name: name, status: model.StatusCancelled}, false
	}
}

// runJob executes one job, including its retries and artifact collection.
func (s *Scheduler) runJob(rs *runState, name string) outcome {
	rs.mu.Lock()
	js := rs.jobs[name]
	rs.mu.Unlock()

	if rs.ctx.Err() != nil {
		s.setJobStatus(rs, name, model.StatusCancelled, "run cancelled before the job started")
		return outcome{name: name, status: model.StatusCancelled}
	}

	maxAttempts := js.record.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var last executor.Result
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := s.retryBackoff(js.spec, attempt-1)
			s.setJobStatus(rs, name, model.StatusRetrying,
				fmt.Sprintf("attempt %d of %d failed; retrying in %s", attempt-1, maxAttempts, backoff))
			s.metrics.JobRetried()
			select {
			case <-time.After(backoff):
			case <-rs.ctx.Done():
				s.finishJob(rs, name, model.StatusCancelled, nil, "run cancelled during retry backoff")
				return outcome{name: name, status: model.StatusCancelled}
			}
		}

		result, err := s.attempt(rs, js, attempt)
		if err != nil {
			// An infrastructure problem (workspace, logs) is not retried: it will
			// fail again the same way.
			s.finishJob(rs, name, model.StatusFailed, nil, err.Error())
			return outcome{name: name, status: model.StatusFailed, err: err}
		}
		last = result

		if result.Success() {
			s.collectArtifacts(rs, js, true)
			s.finishJob(rs, name, model.StatusSuccess, &result.ExitCode, "")
			return outcome{name: name, status: model.StatusSuccess}
		}
		if result.Cancelled {
			s.collectArtifacts(rs, js, false)
			s.finishJob(rs, name, model.StatusCancelled, exitPtr(result.ExitCode), "cancelled")
			return outcome{name: name, status: model.StatusCancelled}
		}
	}

	s.collectArtifacts(rs, js, false)
	msg := "job failed"
	if last.Err != nil {
		msg = rs.redactor.RedactString(last.Err.Error())
	}
	s.finishJob(rs, name, model.StatusFailed, exitPtr(last.ExitCode), msg)
	return outcome{name: name, status: model.StatusFailed, err: last.Err}
}

// attempt performs a single try: provision the workspace, restore dependency
// artifacts, open log files, and hand the job to an executor.
func (s *Scheduler) attempt(rs *runState, js *jobState, attemptNo int) (executor.Result, error) {
	name := js.spec.Name

	workspace, err := s.prepareWorkspace(rs.run.ID, name)
	if err != nil {
		return executor.Result{}, fmt.Errorf("prepare workspace: %w", err)
	}
	if err := s.restoreDependencyArtifacts(rs, name, workspace); err != nil {
		return executor.Result{}, err
	}

	now := time.Now()
	att, err := s.db.CreateAttempt(rs.dbCtx, js.record.ID, attemptNo, now)
	if err != nil {
		return executor.Result{}, fmt.Errorf("record attempt: %w", err)
	}
	if err := s.db.IncrementJobAttempts(rs.dbCtx, js.record.ID); err != nil {
		s.logger.Warn("failed to increment attempt counter", "job", name, "error", err)
	}

	stdout, stderr, closeLogs, err := s.openLogs(rs, js, att.ID, attemptNo)
	if err != nil {
		return executor.Result{}, err
	}
	defer closeLogs()

	s.setJobStatus(rs, name, model.StatusRunning,
		fmt.Sprintf("attempt %d of %d", attemptNo, js.record.MaxAttempts))

	env := s.jobEnvironment(rs, js.spec, workspace, attemptNo)
	execJob := executor.Job{
		Name:         name,
		Commands:     js.spec.Commands,
		Env:          env,
		Workspace:    workspace,
		WorkingDir:   js.spec.WorkingDir,
		Timeout:      s.timeout(js.spec),
		Image:        js.spec.Image,
		Stdout:       stdout,
		Stderr:       stderr,
		EchoCommands: true,
	}

	backend := s.local
	kind := string(model.ExecutorLocal)
	if js.record.Executor == model.ExecutorDocker {
		backend = s.docker
		kind = string(model.ExecutorDocker)
	}

	s.metrics.JobStarted(kind)
	result := backend.Run(rs.ctx, execJob)
	s.metrics.JobFinished(outcomeLabel(result), result.Duration())

	status := model.StatusSuccess
	errText := ""
	switch {
	case result.Cancelled:
		status = model.StatusCancelled
		errText = "cancelled"
	case !result.Success():
		status = model.StatusFailed
		if result.Err != nil {
			errText = rs.redactor.RedactString(result.Err.Error())
		}
	}
	if err := s.db.FinishAttempt(rs.dbCtx, att.ID, status, exitPtr(result.ExitCode), errText, time.Now()); err != nil {
		s.logger.Warn("failed to record attempt outcome", "job", name, "error", err)
	}
	return result, nil
}

// openLogs creates the stdout and stderr files for an attempt and registers them
// so they can be streamed while the job is still running.
func (s *Scheduler) openLogs(rs *runState, js *jobState, attemptID int64, attemptNo int) (stdout, stderr *logs.Writer, closeFn func(), err error) {
	dir := filepath.Join(s.cfg.Logs.Dir,
		strconv.FormatInt(rs.run.ID, 10),
		sanitizeDirName(js.spec.Name))

	open := func(stream model.Stream) (*logs.Writer, error) {
		path := filepath.Join(dir, fmt.Sprintf("%d.%s.log", attemptNo, stream))
		w, err := logs.NewWriter(logs.WriterOptions{
			Path:     path,
			Redactor: rs.redactor,
			Broker:   s.broker,
			Topic:    logs.LogTopic(js.record.ID, stream),
			MaxBytes: s.cfg.Logs.MaxSize,
		})
		if err != nil {
			return nil, err
		}
		// Register the row up front so a client can stream the file while the job
		// is still writing to it.
		if err := s.db.RecordLog(rs.dbCtx, &model.Log{
			AttemptID: attemptID, JobID: js.record.ID, RunID: rs.run.ID,
			Stream: stream, Path: path,
		}); err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("record log: %w", err)
		}
		return w, nil
	}

	stdout, err = open(model.StreamStdout)
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err = open(model.StreamStderr)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, err
	}

	closeFn = func() {
		for _, pair := range []struct {
			w      *logs.Writer
			stream model.Stream
		}{{stdout, model.StreamStdout}, {stderr, model.StreamStderr}} {
			if err := pair.w.Close(); err != nil {
				s.logger.Warn("failed to close log", "job", js.spec.Name, "stream", pair.stream, "error", err)
			}
			if err := s.db.RecordLog(rs.dbCtx, &model.Log{
				AttemptID: attemptID, JobID: js.record.ID, RunID: rs.run.ID,
				Stream:    pair.stream,
				Path:      filepath.Join(dir, fmt.Sprintf("%d.%s.log", attemptNo, pair.stream)),
				Bytes:     pair.w.Bytes(),
				Truncated: pair.w.Truncated(),
			}); err != nil {
				s.logger.Warn("failed to finalise log record", "job", js.spec.Name, "error", err)
			}
		}
	}
	return stdout, stderr, closeFn, nil
}

// collectArtifacts stores the files a job declared, if its artifacts.when clause
// matches the outcome.
func (s *Scheduler) collectArtifacts(rs *runState, js *jobState, succeeded bool) {
	spec := js.spec.Artifacts
	if spec == nil || len(spec.Paths) == 0 {
		return
	}
	switch spec.When {
	case pipeline.ArtifactOnSuccess:
		if !succeeded {
			return
		}
	case pipeline.ArtifactOnFailure:
		if succeeded {
			return
		}
	}

	workspace := s.workspaceDir(rs.run.ID, js.spec.Name)
	// Collection must survive run cancellation: capturing the output of a job
	// that was killed is often the whole point of artifacts.when: always.
	ctx, cancel := context.WithTimeout(rs.dbCtx, 2*time.Minute)
	defer cancel()

	files, err := s.artifacts.Collect(ctx, artifacts.CollectRequest{
		RunID:     rs.run.ID,
		JobName:   js.spec.Name,
		Workspace: workspace,
		Paths:     spec.Paths,
		Exclude:   spec.Exclude,
	})
	if err != nil {
		s.logger.Error("artifact collection failed",
			"run", rs.run.ID, "job", js.spec.Name, "error", err)
		s.emit(rs, &js.record.ID, js.spec.Name, model.EventArtifact, "", "",
			"artifact collection failed: "+err.Error())
		return
	}
	if len(files) == 0 {
		return
	}

	expires := s.artifactExpiry(spec)
	var bytes int64
	for _, f := range files {
		bytes += f.Size
		if err := s.db.RecordArtifact(ctx, &model.Artifact{
			RunID: rs.run.ID, JobID: js.record.ID, JobName: js.spec.Name,
			Path: f.Path, StorePath: f.StorePath, Size: f.Size,
			SHA256: f.SHA256, Mode: uint32(f.Mode.Perm()), ExpiresAt: expires,
		}); err != nil {
			s.logger.Error("failed to record artifact",
				"run", rs.run.ID, "job", js.spec.Name, "path", f.Path, "error", err)
		}
	}

	s.metrics.ArtifactsCollected(len(files), bytes)
	s.emit(rs, &js.record.ID, js.spec.Name, model.EventArtifact, "", "",
		fmt.Sprintf("collected %d artifact(s), %d bytes", len(files), bytes))
	s.logger.Info("collected artifacts",
		"run", rs.run.ID, "job", js.spec.Name, "files", len(files), "bytes", bytes)
}

// artifactExpiry resolves the retention deadline for a set of artifacts.
func (s *Scheduler) artifactExpiry(spec *pipeline.Artifacts) *time.Time {
	d := spec.ExpireIn
	if d == 0 {
		d = s.cfg.Artifacts.Retention
	}
	if d <= 0 {
		return nil // keep forever
	}
	t := time.Now().Add(d)
	return &t
}

// workspaceDir returns the directory a job ran in, without creating it.
func (s *Scheduler) workspaceDir(runID int64, jobName string) string {
	if s.cfg.Workspace.Mode == config.WorkspaceShared {
		return filepath.Join(s.cfg.RunDir(runID), "workspace")
	}
	return filepath.Join(s.cfg.RunDir(runID), "jobs", sanitizeDirName(jobName))
}

// --- state transitions ------------------------------------------------------

// setJobStatus records a non-terminal transition and publishes it.
func (s *Scheduler) setJobStatus(rs *runState, name string, status model.Status, message string) {
	rs.mu.Lock()
	js := rs.jobs[name]
	from := js.status
	js.status = status
	id := js.record.ID
	rs.mu.Unlock()

	if err := s.db.SetJobStatus(rs.dbCtx, id, status, time.Now()); err != nil {
		s.logger.Error("failed to persist job status", "job", name, "status", status, "error", err)
	}
	s.emit(rs, &id, name, model.EventJobStatus, from, status, message)
	s.logger.Info("job "+string(status), "run", rs.run.ID, "job", name, "message", message)
}

// finishJob records a terminal transition with its exit code and error.
func (s *Scheduler) finishJob(rs *runState, name string, status model.Status, exitCode *int, message string) {
	rs.mu.Lock()
	js := rs.jobs[name]
	from := js.status
	js.status = status
	id := js.record.ID
	rs.mu.Unlock()

	if err := s.db.FinishJob(rs.dbCtx, id, status, exitCode, message, time.Now()); err != nil {
		s.logger.Error("failed to persist job outcome", "job", name, "status", status, "error", err)
	}
	s.emit(rs, &id, name, model.EventJobStatus, from, status, message)

	level := s.logger.Info
	if status == model.StatusFailed {
		level = s.logger.Error
	}
	level("job "+string(status), "run", rs.run.ID, "job", name, "message", message)
}

// markSkipped records a job that will never run.
func (s *Scheduler) markSkipped(rs *runState, name string) {
	reason := skipReason(rs, name)
	rs.mu.Lock()
	rs.jobs[name].dispatched = true
	rs.mu.Unlock()

	s.metrics.JobSkipped()
	s.finishJob(rs, name, model.StatusSkipped, nil, reason)
}

// skipReason explains why a job was skipped, which is the first thing anyone
// looking at a skipped job wants to know.
func skipReason(rs *runState, name string) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	js := rs.jobs[name]

	if rs.ctx.Err() != nil {
		return "run was cancelled"
	}
	if js.spec.When == pipeline.WhenNever {
		return "when: never"
	}
	if js.spec.When == pipeline.WhenOnFailure && !rs.failed {
		return "when: on_failure, but nothing failed"
	}
	for _, need := range js.node.Needs {
		dep := rs.jobs[need]
		switch dep.status {
		case model.StatusFailed:
			if !dep.spec.AllowFailure {
				return fmt.Sprintf("dependency %q failed", need)
			}
		case model.StatusSkipped:
			return fmt.Sprintf("dependency %q was skipped", need)
		case model.StatusCancelled:
			return fmt.Sprintf("dependency %q was cancelled", need)
		}
	}
	if rs.failed {
		return "an earlier job in the run failed"
	}
	if js.spec.If != "" {
		return fmt.Sprintf("condition was false: %s", js.spec.If)
	}
	return "skipped"
}

// finalize computes the run's terminal status and records it.
func (s *Scheduler) finalize(rs *runState, started time.Time) {
	rs.mu.Lock()
	var (
		failedJobs    []string
		cancelled     bool
		anyUnfinished bool
	)
	for _, name := range rs.graph.Order {
		js := rs.jobs[name]
		switch js.status {
		case model.StatusFailed:
			if !js.spec.AllowFailure {
				failedJobs = append(failedJobs, name)
			}
		case model.StatusCancelled:
			cancelled = true
		default:
			if !js.status.Terminal() {
				anyUnfinished = true
			}
		}
	}
	rs.mu.Unlock()

	// Anything left non-terminal can only be a job the loop never reached, which
	// happens when the run was cancelled.
	if anyUnfinished {
		s.cancelRemaining(rs)
		cancelled = true
	}

	status := model.StatusSuccess
	message := ""
	switch {
	case len(failedJobs) > 0:
		status = model.StatusFailed
		if len(failedJobs) == 1 {
			message = fmt.Sprintf("job %q failed", failedJobs[0])
		} else {
			message = fmt.Sprintf("%d jobs failed: %v", len(failedJobs), failedJobs)
		}
	case cancelled || rs.ctx.Err() != nil:
		status = model.StatusCancelled
		message = "run was cancelled"
	}

	s.finishRun(rs, status, message)
	s.metrics.RunFinished(string(status), time.Since(started))
	s.logger.Info("run "+string(status),
		"run", rs.run.ID, "pipeline", rs.spec.Name,
		"duration", time.Since(started).Round(time.Millisecond), "message", message)
}

// cancelRemaining marks jobs that never got to run.
func (s *Scheduler) cancelRemaining(rs *runState) {
	rs.mu.Lock()
	var pending []string
	for name, js := range rs.jobs {
		if !js.status.Terminal() {
			pending = append(pending, name)
		}
	}
	rs.mu.Unlock()

	for _, name := range pending {
		s.finishJob(rs, name, model.StatusCancelled, nil, "run was cancelled before this job ran")
	}
}

func (s *Scheduler) finishRun(rs *runState, status model.Status, message string) {
	if err := s.db.FinishRun(rs.dbCtx, rs.run.ID, status, time.Now(), message); err != nil {
		s.logger.Error("failed to record run outcome", "run", rs.run.ID, "error", err)
	}
	s.emit(rs, nil, "", model.EventRunStatus, model.StatusRunning, status, message)
	// Closing the run topic tells streaming clients the run is over.
	s.broker.Close(logs.RunTopic(rs.run.ID))
	s.broker.Notify(logs.GlobalTopic)
}

// emit appends an event to the run timeline and wakes streaming subscribers.
func (s *Scheduler) emit(rs *runState, jobID *int64, jobName string, typ model.EventType, from, to model.Status, message string) {
	ev := &model.Event{
		RunID: rs.run.ID, JobID: jobID, JobName: jobName,
		Type: typ, From: from, To: to,
		Message: rs.redactor.RedactString(message),
	}
	if err := s.db.AppendEvent(rs.dbCtx, ev); err != nil {
		s.logger.Warn("failed to append run event", "run", rs.run.ID, "error", err)
	}
	s.broker.Notify(logs.RunTopic(rs.run.ID))
	s.broker.Notify(logs.GlobalTopic)
}

func exitPtr(code int) *int {
	if code < 0 {
		return nil
	}
	return &code
}

func outcomeLabel(r executor.Result) string {
	switch {
	case r.Cancelled:
		return "cancelled"
	case r.Success():
		return "success"
	default:
		return "failed"
	}
}
