// Package scheduler turns a validated pipeline graph into a running pipeline.
//
// It owns dependency resolution, concurrency, retries, failure propagation,
// cancellation and manual approval, and it is the only package that writes run
// state to the database while a run is in flight.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nickcross-79/forge/internal/artifacts"
	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/executor"
	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/metrics"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/nickcross-79/forge/internal/secrets"
	"github.com/nickcross-79/forge/internal/store"
)

// ErrRunNotActive is returned when cancelling or approving a run that is not
// currently executing in this process.
var ErrRunNotActive = errors.New("run is not active")

// ErrJobNotAwaitingApproval is returned when approving a job that is not blocked
// on a manual gate.
var ErrJobNotAwaitingApproval = errors.New("job is not awaiting approval")

// ManualPolicy decides what happens to `when: manual` jobs.
type ManualPolicy string

const (
	// ManualWait blocks the job until it is approved, cancelled, or the manual
	// timeout expires. This is the default for runs started from the API or the
	// dashboard, where there is a UI to approve from.
	ManualWait ManualPolicy = "wait"
	// ManualSkip skips manual jobs immediately. This is the default for `forge
	// run`, so a terminal invocation finishes instead of hanging on a gate the
	// user cannot see.
	ManualSkip ManualPolicy = "skip"
)

// Scheduler executes pipelines. A single instance is safe for concurrent use and
// can drive several runs at once.
type Scheduler struct {
	cfg       *config.Config
	db        *store.DB
	artifacts *artifacts.Store
	broker    *logs.Broker
	secrets   *secrets.Store
	metrics   *metrics.Registry
	logger    *slog.Logger

	local  executor.Executor
	docker executor.Executor

	mu     sync.Mutex
	active map[int64]*runState
}

// Options configures a Scheduler. Everything except Config and DB has a default.
type Options struct {
	Config    *config.Config
	DB        *store.DB
	Artifacts *artifacts.Store
	Broker    *logs.Broker
	Secrets   *secrets.Store
	Metrics   *metrics.Registry
	Logger    *slog.Logger
	// Local and Docker override the executors, which is how tests substitute a
	// fake backend without touching the rest of the engine.
	Local  executor.Executor
	Docker executor.Executor
}

// New builds a Scheduler.
func New(opts Options) (*Scheduler, error) {
	if opts.Config == nil {
		return nil, errors.New("scheduler: config is required")
	}
	if opts.DB == nil {
		return nil, errors.New("scheduler: database is required")
	}

	s := &Scheduler{
		cfg:       opts.Config,
		db:        opts.DB,
		artifacts: opts.Artifacts,
		broker:    opts.Broker,
		secrets:   opts.Secrets,
		metrics:   opts.Metrics,
		logger:    opts.Logger,
		local:     opts.Local,
		docker:    opts.Docker,
		active:    make(map[int64]*runState),
	}

	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.metrics == nil {
		s.metrics = metrics.New()
	}
	if s.broker == nil {
		s.broker = logs.NewBroker()
	}
	if s.secrets == nil {
		s.secrets = secrets.New()
	}
	if s.artifacts == nil {
		art, err := artifacts.NewStore(opts.Config.Artifacts.Dir)
		if err != nil {
			return nil, err
		}
		art.MaxFileSize = opts.Config.Artifacts.MaxFileSize
		art.MaxTotalSize = opts.Config.Artifacts.MaxTotalSize
		s.artifacts = art
	}
	if s.local == nil {
		s.local = executor.NewLocal(opts.Config.Runner.Shell)
	}
	if s.docker == nil {
		s.docker = executor.NewDocker("", opts.Config.Runner.DockerImage)
	}
	return s, nil
}

// Broker exposes the notification hub so the API can subscribe to live updates.
func (s *Scheduler) Broker() *logs.Broker { return s.broker }

// Metrics exposes the counter registry.
func (s *Scheduler) Metrics() *metrics.Registry { return s.metrics }

// Artifacts exposes the artifact store, which the API needs for downloads.
func (s *Scheduler) Artifacts() *artifacts.Store { return s.artifacts }

// RunOptions configures a single execution.
type RunOptions struct {
	// Trigger records where the run came from.
	Trigger model.Trigger
	// Concurrency overrides the configured job concurrency for this run.
	Concurrency int
	// Manual decides how `when: manual` jobs are treated.
	Manual ManualPolicy
	// Approved pre-approves the named manual jobs, so `forge run --approve deploy`
	// runs a gated job without a second command.
	Approved []string
	// Variables override pipeline variables for this run only. They are applied
	// to the run's snapshot, never to the stored pipeline definition, so a
	// one-off override cannot silently become the pipeline's new default.
	Variables map[string]string
	// SourcePath records where the pipeline was loaded from.
	SourcePath string
}

// Prepare registers the pipeline and creates the run and job rows, without
// starting execution. Splitting this from execution lets the API return a run ID
// immediately and lets `forge run` print the run number before work begins.
func (s *Scheduler) Prepare(ctx context.Context, spec *pipeline.Spec, graph *pipeline.Graph, opts RunOptions) (*model.Run, error) {
	if spec == nil || graph == nil {
		return nil, errors.New("scheduler: spec and graph are required")
	}
	if opts.Trigger == "" {
		opts.Trigger = model.TriggerCLI
	}

	// The pipeline row records the definition as written. The run row records the
	// definition as executed, which differs when this run carries overrides.
	definitionYAML, err := spec.Marshal()
	if err != nil {
		return nil, fmt.Errorf("scheduler: encode pipeline definition: %w", err)
	}
	runSpecYAML, err := effectiveSpec(spec, opts).Marshal()
	if err != nil {
		return nil, fmt.Errorf("scheduler: encode run snapshot: %w", err)
	}

	p, err := s.db.UpsertPipeline(ctx, &model.Pipeline{
		Name:        spec.Name,
		Description: spec.Description,
		SourcePath:  opts.SourcePath,
		SpecYAML:    string(definitionYAML),
		Checksum:    pipeline.Checksum(definitionYAML),
	})
	if err != nil {
		return nil, err
	}

	run, err := s.db.CreateRun(ctx, &model.Run{
		PipelineID: p.ID,
		Status:     model.StatusQueued,
		Trigger:    opts.Trigger,
		SpecYAML:   string(runSpecYAML),
	})
	if err != nil {
		return nil, err
	}
	run.PipelineName = p.Name
	run.WorkDir = s.cfg.RunDir(run.ID)

	if _, err := s.db.SQL().ExecContext(ctx,
		`UPDATE runs SET work_dir = ? WHERE id = ?`, run.WorkDir, run.ID); err != nil {
		return nil, fmt.Errorf("scheduler: record work dir: %w", err)
	}

	jobs := make([]*model.Job, 0, len(graph.Nodes))
	for _, name := range spec.JobNames() {
		node := graph.Nodes[name]
		job := node.Job
		jobs = append(jobs, &model.Job{
			Name:         name,
			Stage:        job.Stage,
			Status:       model.StatusQueued,
			Needs:        node.Needs,
			Executor:     s.executorKind(job),
			Image:        job.Image,
			Manual:       job.When == pipeline.WhenManual,
			AllowFailure: job.AllowFailure,
			MaxAttempts:  s.maxAttempts(job),
		})
	}
	if err := s.db.CreateJobs(ctx, run.ID, jobs); err != nil {
		return nil, err
	}
	return run, nil
}

// effectiveSpec applies this run's variable overrides to a copy of the spec.
//
// It is deterministic and side-effect free, so Prepare and Execute can each call
// it and be certain they are describing the same execution.
func effectiveSpec(spec *pipeline.Spec, opts RunOptions) *pipeline.Spec {
	if len(opts.Variables) == 0 {
		return spec
	}
	out := spec.Clone()
	if out.Variables == nil {
		out.Variables = make(map[string]string, len(opts.Variables))
	}
	for k, v := range opts.Variables {
		out.Variables[k] = v
	}
	return out
}

// Execute runs a prepared pipeline to completion and returns the final run state.
func (s *Scheduler) Execute(ctx context.Context, run *model.Run, spec *pipeline.Spec, graph *pipeline.Graph, opts RunOptions) (*model.Run, error) {
	rs, err := s.newRunState(ctx, run, effectiveSpec(spec, opts), graph, opts)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.active[run.ID] = rs
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, run.ID)
		s.mu.Unlock()
		rs.cancel()
	}()

	s.execute(rs)
	return s.db.RunByID(context.WithoutCancel(ctx), run.ID)
}

// Start prepares and executes a run in the background, returning as soon as the
// run row exists. The API uses this so an HTTP request does not have to stay open
// for the length of a pipeline.
func (s *Scheduler) Start(ctx context.Context, spec *pipeline.Spec, graph *pipeline.Graph, opts RunOptions) (*model.Run, error) {
	run, err := s.Prepare(ctx, spec, graph, opts)
	if err != nil {
		return nil, err
	}

	// The run outlives the request that started it, so it gets a background
	// context rather than the request's.
	go func() {
		bg := context.WithoutCancel(ctx)
		if _, err := s.Execute(bg, run, spec, graph, opts); err != nil {
			s.logger.Error("run failed to execute", "run", run.ID, "error", err)
		}
	}()
	return run, nil
}

// Run is the one-shot path used by `forge run`: prepare, then execute, blocking
// until the pipeline finishes.
func (s *Scheduler) Run(ctx context.Context, spec *pipeline.Spec, graph *pipeline.Graph, opts RunOptions) (*model.Run, error) {
	run, err := s.Prepare(ctx, spec, graph, opts)
	if err != nil {
		return nil, err
	}
	return s.Execute(ctx, run, spec, graph, opts)
}

// Cancel stops an active run. Jobs in flight are killed and everything still
// queued is marked cancelled.
func (s *Scheduler) Cancel(runID int64) error {
	s.mu.Lock()
	rs, ok := s.active[runID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("run %d: %w", runID, ErrRunNotActive)
	}
	rs.cancel()
	return nil
}

// Approve releases a manual job that is waiting for a decision.
func (s *Scheduler) Approve(runID int64, jobName string) error {
	s.mu.Lock()
	rs, ok := s.active[runID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("run %d: %w", runID, ErrRunNotActive)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	js, ok := rs.jobs[jobName]
	if !ok {
		return fmt.Errorf("job %q in run %d: %w", jobName, runID, store.ErrNotFound)
	}
	if !js.manual || js.status != model.StatusAwaitingManual {
		return fmt.Errorf("job %q: %w", jobName, ErrJobNotAwaitingApproval)
	}
	js.approveOnce.Do(func() { close(js.approved) })
	return nil
}

// ActiveRuns lists the run IDs this scheduler is currently executing.
func (s *Scheduler) ActiveRuns() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.active))
	for id := range s.active {
		out = append(out, id)
	}
	return out
}

// IsActive reports whether a run is executing in this process.
func (s *Scheduler) IsActive(runID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[runID]
	return ok
}

// executorKind decides which backend a job will use.
func (s *Scheduler) executorKind(job *pipeline.Job) model.ExecutorKind {
	switch {
	case job.Executor == "docker":
		return model.ExecutorDocker
	case job.Executor == "local":
		return model.ExecutorLocal
	case job.Image != "":
		return model.ExecutorDocker
	case s.cfg.Runner.DefaultExecutor == "docker":
		return model.ExecutorDocker
	default:
		return model.ExecutorLocal
	}
}

// maxAttempts is the total number of tries a job gets, including the first.
func (s *Scheduler) maxAttempts(job *pipeline.Job) int {
	retries := job.Retry.Max
	if retries == 0 {
		retries = s.cfg.Runner.DefaultRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries + 1
}

// retryBackoff is the delay before a given attempt, doubling each time so a
// flaky dependency is not hammered.
func (s *Scheduler) retryBackoff(job *pipeline.Job, attempt int) time.Duration {
	base := job.Retry.Backoff
	if base <= 0 {
		base = s.cfg.Runner.RetryBackoff
	}
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > 5*time.Minute {
			return 5 * time.Minute
		}
	}
	return d
}

// timeout is the wall-clock limit for one attempt at a job.
func (s *Scheduler) timeout(job *pipeline.Job) time.Duration {
	if job.Timeout > 0 {
		return job.Timeout
	}
	return s.cfg.Runner.DefaultTimeout
}
