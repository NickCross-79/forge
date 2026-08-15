// Package integration exercises forge end to end: the real parser, the real
// scheduler, the real local executor, real SQLite, and the real artifact store.
//
// Everything else in the tree is tested with a fake executor so that the logic
// under test is isolated. These tests do the opposite on purpose — they run
// actual shell commands against the shipped example pipelines, which is the only
// way to catch problems that live in the seams between packages.
package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickcross-79/forge/internal/artifacts"
	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/nickcross-79/forge/internal/scheduler"
	"github.com/nickcross-79/forge/internal/secrets"
	"github.com/nickcross-79/forge/internal/store"
)

// project is a throwaway forge project on disk.
type project struct {
	t     *testing.T
	root  string
	cfg   *config.Config
	db    *store.DB
	sched *scheduler.Scheduler
	sec   *secrets.Store
}

func newProject(t *testing.T, tune ...func(*config.Config)) *project {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the example pipelines assume a POSIX shell")
	}

	root := t.TempDir()
	cfg := config.Default(root)
	cfg.Runner.Concurrency = 4
	cfg.Runner.RetryBackoff = 10 * time.Millisecond
	cfg.Runner.DefaultTimeout = 60 * time.Second
	for _, fn := range tune {
		fn(cfg)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(context.Background(), store.Options{
		Path: cfg.Database.Path, BusyTimeout: cfg.Database.BusyTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	art, err := artifacts.NewStore(cfg.Artifacts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	art.MaxFileSize = cfg.Artifacts.MaxFileSize
	art.MaxTotalSize = cfg.Artifacts.MaxTotalSize

	sec := secrets.New()

	sched, err := scheduler.New(scheduler.Options{
		Config: cfg, DB: db, Artifacts: art, Secrets: sec,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	return &project{t: t, root: root, cfg: cfg, db: db, sched: sched, sec: sec}
}

// writePipeline drops a pipeline file into the project.
func (p *project) writePipeline(name, body string) string {
	p.t.Helper()
	path := filepath.Join(p.root, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return path
}

// copyExample copies one of the shipped examples into the project, so the tests
// exercise exactly the files a user would run.
func (p *project) copyExample(name string) string {
	p.t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
	if err != nil {
		p.t.Fatalf("read example %s: %v", name, err)
	}
	return p.writePipeline(name, string(body))
}

func (p *project) run(path string, opts ...func(*scheduler.RunOptions)) *model.Run {
	p.t.Helper()
	spec, graph, err := pipeline.Load(path)
	if err != nil {
		p.t.Fatalf("load %s: %v", path, err)
	}
	o := scheduler.RunOptions{Trigger: model.TriggerCLI, Manual: scheduler.ManualSkip, SourcePath: path}
	for _, fn := range opts {
		fn(&o)
	}
	run, err := p.sched.Run(context.Background(), spec, graph, o)
	if err != nil {
		p.t.Fatalf("run %s: %v", path, err)
	}
	return run
}

func (p *project) jobs(runID int64) map[string]*model.Job {
	p.t.Helper()
	list, err := p.db.ListJobs(context.Background(), runID)
	if err != nil {
		p.t.Fatal(err)
	}
	out := make(map[string]*model.Job, len(list))
	for _, j := range list {
		out[j.Name] = j
	}
	return out
}

// stdout returns the concatenated stdout captured for a job.
func (p *project) stdout(jobID int64) string {
	p.t.Helper()
	records, err := p.db.ListLogs(context.Background(), jobID)
	if err != nil {
		p.t.Fatal(err)
	}
	var b strings.Builder
	for _, rec := range records {
		if rec.Stream != model.StreamStdout {
			continue
		}
		body, err := logs.ReadAll(rec.Path)
		if err != nil {
			p.t.Fatal(err)
		}
		b.Write(body)
	}
	return b.String()
}

func (p *project) assertStatuses(runID int64, want map[string]model.Status) {
	p.t.Helper()
	jobs := p.jobs(runID)
	for name, wantStatus := range want {
		job, ok := jobs[name]
		if !ok {
			p.t.Errorf("job %q missing from run", name)
			continue
		}
		if job.Status != wantStatus {
			p.t.Errorf("job %q = %q, want %q (error: %s)", name, job.Status, wantStatus, job.Error)
		}
	}
}

// --- the shipped examples ---------------------------------------------------

func TestExampleBasic(t *testing.T) {
	p := newProject(t)
	run := p.run(p.copyExample("basic.yml"))

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success (error: %s)", run.Status, run.Error)
	}
	p.assertStatuses(run.ID, map[string]model.Status{
		"build": model.StatusSuccess, "test": model.StatusSuccess, "package": model.StatusSuccess,
	})

	// The artifact was collected, with the right bytes and checksum.
	arts, err := p.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("len(artifacts) = %d, want 1", len(arts))
	}
	if arts[0].Path != "dist/output.txt" {
		t.Errorf("artifact path = %q", arts[0].Path)
	}
	// sha256 of "hello\n"
	const wantSum = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if arts[0].SHA256 != wantSum {
		t.Errorf("artifact sha256 = %q, want %q", arts[0].SHA256, wantSum)
	}

	body, err := p.sched.Artifacts().Open(arts[0].StorePath)
	if err != nil {
		t.Fatalf("open stored artifact: %v", err)
	}
	defer func() { _ = body.Close() }()
	content, _ := io.ReadAll(body)
	if string(content) != "hello\n" {
		t.Errorf("stored artifact = %q, want %q", content, "hello\n")
	}

	// `test` consumed the artifact `build` produced, in a separate workspace.
	jobs := p.jobs(run.ID)
	if out := p.stdout(jobs["test"].ID); !strings.Contains(out, "hello") {
		t.Errorf("test job did not see the restored artifact; stdout = %q", out)
	}
}

func TestExampleParallel(t *testing.T) {
	p := newProject(t, func(c *config.Config) { c.Runner.Concurrency = 3 })

	start := time.Now()
	run := p.run(p.copyExample("parallel.yml"))
	elapsed := time.Since(start)

	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success (error: %s)", run.Status, run.Error)
	}

	// Three jobs sleep one second each. Run serially that is at least 3s; with
	// concurrency 3 it should be close to 1s. A 2.5s ceiling proves they
	// overlapped without being so tight that a slow machine fails the test.
	if elapsed > 2500*time.Millisecond {
		t.Errorf("run took %v; the three 1s jobs do not appear to have run in parallel", elapsed)
	}

	jobs := p.jobs(run.ID)
	out := p.stdout(jobs["report"].ID)
	for _, want := range []string{"lint: clean", "unit: 42 passed", "integration: 7 passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report job did not receive %q from its dependencies; stdout = %q", want, out)
		}
	}
}

func TestExampleFailureHandling(t *testing.T) {
	p := newProject(t)
	run := p.run(p.copyExample("failure-handling.yml"))

	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}

	p.assertStatuses(run.ID, map[string]model.Status{
		"eventually-succeeds": model.StatusSuccess,
		"tolerated":           model.StatusFailed,
		"broken":              model.StatusFailed,
		"downstream":          model.StatusSkipped,
		"always-cleanup":      model.StatusSuccess,
		"notify-failure":      model.StatusSuccess,
	})

	jobs := p.jobs(run.ID)

	// The flaky job really did retry.
	if got := jobs["eventually-succeeds"].Attempts; got != 3 {
		t.Errorf("eventually-succeeds attempts = %d, want 3", got)
	}
	// FORGE_ATTEMPT was visible to the job and incremented.
	if out := p.stdout(jobs["eventually-succeeds"].ID); !strings.Contains(out, "attempt 3: succeeded") {
		t.Errorf("retry did not expose FORGE_ATTEMPT correctly; stdout = %q", out)
	}
	// A skipped job explains itself.
	if !strings.Contains(jobs["downstream"].Error, "broken") {
		t.Errorf("downstream skip reason = %q, should name the failed dependency", jobs["downstream"].Error)
	}
	// artifacts.when: on_failure captured the crash log.
	arts, err := p.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Path != "crash.log" {
		t.Errorf("artifacts = %+v, want crash.log from the failed job", arts)
	}
}

func TestExampleConditional(t *testing.T) {
	p := newProject(t)
	path := p.copyExample("conditional.yml")

	// Default variables: staging deploys, production is skipped.
	staging := p.run(path)
	if staging.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success (error: %s)", staging.Status, staging.Error)
	}
	p.assertStatuses(staging.ID, map[string]model.Status{
		"deploy-staging": model.StatusSuccess,
		"deploy":         model.StatusSkipped, // manual and condition false
		"in-ci":          model.StatusSuccess,
		"disabled":       model.StatusSkipped,
	})

	// Override the variable: now the production condition matches, but the
	// manual gate still stops it under the skip policy.
	prod := p.run(path, func(o *scheduler.RunOptions) {
		o.Variables = map[string]string{"DEPLOY_ENV": "production"}
	})
	p.assertStatuses(prod.ID, map[string]model.Status{
		"deploy-staging": model.StatusSkipped,
		"deploy":         model.StatusSkipped,
	})

	// Approve it up front and it runs.
	approved := p.run(path, func(o *scheduler.RunOptions) {
		o.Variables = map[string]string{"DEPLOY_ENV": "production"}
		o.Approved = []string{"deploy"}
	})
	p.assertStatuses(approved.ID, map[string]model.Status{"deploy": model.StatusSuccess})

	jobs := p.jobs(approved.ID)
	if out := p.stdout(jobs["deploy"].ID); !strings.Contains(out, "Deploying to production") {
		t.Errorf("approved deploy did not run its commands; stdout = %q", out)
	}

	// The override must not have leaked into the stored pipeline definition.
	// "production" appears legitimately throughout this pipeline (in the `if`
	// expressions and the deploy command), so the check has to look at the
	// variable's actual value rather than at the text of the document.
	stored, err := p.db.PipelineByName(context.Background(), "conditional")
	if err != nil {
		t.Fatal(err)
	}
	storedSpec, err := pipeline.Parse([]byte(stored.SpecYAML))
	if err != nil {
		t.Fatalf("stored pipeline does not parse: %v", err)
	}
	if got := storedSpec.Variables["DEPLOY_ENV"]; got != "staging" {
		t.Errorf("stored pipeline DEPLOY_ENV = %q, want the original %q: a per-run "+
			"override was written back to the pipeline definition", got, "staging")
	}
}

func TestExampleSecretsAreRedactedEverywhere(t *testing.T) {
	p := newProject(t)
	// A fixture literal that appears nowhere else in the tree, so a hit is
	// unambiguous. It must not collide with the placeholder in the example's own
	// header comment, because that file is copied into every job workspace.
	const secretValue = "example-value-that-must-not-reach-disk"
	p.sec.Set("API_TOKEN", secretValue)

	run := p.run(p.copyExample("secrets.yml"))
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q, want success (error: %s)", run.Status, run.Error)
	}

	jobs := p.jobs(run.ID)

	// The job could read the secret...
	out := p.stdout(jobs["uses-all-secrets"].ID)
	if !strings.Contains(out, logs.Mask) {
		t.Errorf("expected the secret to be masked in stdout, got %q", out)
	}
	// ...but the value never reached anything forge wrote: not the captured logs,
	// and not the database.
	for _, dir := range []string{p.cfg.Logs.Dir, filepath.Dir(p.cfg.Database.Path)} {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil //nolint:nilerr // keep walking
			}
			body, readErr := os.ReadFile(path) //nolint:gosec // test-controlled tree
			if readErr != nil {
				return nil //nolint:nilerr // a locked database is not a leak
			}
			if strings.Contains(string(body), secretValue) {
				t.Errorf("secret value reached disk in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A job that names no secrets in a scoped list still gets them; a scoped job
	// sees only what it asked for. Both ran, which is what matters here.
	p.assertStatuses(run.ID, map[string]model.Status{
		"uses-all-secrets": model.StatusSuccess,
		"scoped":           model.StatusSuccess,
		"isolated-env":     model.StatusSuccess,
	})
}

func TestAllShippedExamplesValidate(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no examples found")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			path := filepath.Join("..", "..", "examples", entry.Name())
			spec, graph, err := pipeline.Load(path)
			if err != nil {
				t.Fatalf("example does not validate: %v", err)
			}
			if spec.Name == "" || len(graph.Order) == 0 {
				t.Errorf("example produced an empty pipeline: %q, %v", spec.Name, graph.Order)
			}
		})
	}
}

// --- cross-cutting behaviour ------------------------------------------------

func TestWorkspaceIsolationIsReal(t *testing.T) {
	p := newProject(t)
	// Two concurrent jobs write the same filename. Under isolated workspaces
	// neither can see the other's file, so both read back their own value.
	path := p.writePipeline("iso.yml", `
name: isolation
jobs:
  a:
    commands:
      - echo "from-a" > shared.txt
      - sleep 0.3
      - cat shared.txt
  b:
    commands:
      - echo "from-b" > shared.txt
      - sleep 0.3
      - cat shared.txt
`)
	run := p.run(path)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q (error: %s)", run.Status, run.Error)
	}

	jobs := p.jobs(run.ID)
	if out := p.stdout(jobs["a"].ID); !strings.Contains(out, "from-a") || strings.Contains(out, "from-b") {
		t.Errorf("job a saw the wrong file: %q", out)
	}
	if out := p.stdout(jobs["b"].ID); !strings.Contains(out, "from-b") || strings.Contains(out, "from-a") {
		t.Errorf("job b saw the wrong file: %q", out)
	}
}

func TestProjectFilesAreAvailableToJobs(t *testing.T) {
	p := newProject(t)
	if err := os.WriteFile(filepath.Join(p.root, "source.txt"), []byte("project content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := p.writePipeline("seed.yml", `
name: seed
jobs:
  read:
    commands:
      - cat source.txt
      - test ! -d .forge
`)
	run := p.run(path)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q (error: %s)", run.Status, run.Error)
	}
	jobs := p.jobs(run.ID)
	if out := p.stdout(jobs["read"].ID); !strings.Contains(out, "project content") {
		t.Errorf("project file was not seeded into the workspace; stdout = %q", out)
	}
}

func TestTimeoutKillsAJob(t *testing.T) {
	p := newProject(t)
	path := p.writePipeline("timeout.yml", `
name: timeout
jobs:
  slow:
    timeout: 300ms
    commands:
      - sleep 30
`)
	start := time.Now()
	run := p.run(path)
	elapsed := time.Since(start)

	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	if elapsed > 20*time.Second {
		t.Errorf("timeout took %v to take effect", elapsed)
	}
	jobs := p.jobs(run.ID)
	if !strings.Contains(jobs["slow"].Error, "timed out") {
		t.Errorf("job error = %q, want a timeout message", jobs["slow"].Error)
	}
}

func TestCancellationStopsRealProcesses(t *testing.T) {
	p := newProject(t)
	path := p.writePipeline("cancel.yml", `
name: cancel
jobs:
  slow:
    commands:
      - sleep 30
  after:
    needs: [slow]
    commands:
      - echo "should not run"
`)
	spec, graph, err := pipeline.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := scheduler.RunOptions{Manual: scheduler.ManualSkip}
	run, err := p.sched.Prepare(context.Background(), spec, graph, opts)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan *model.Run, 1)
	go func() {
		final, err := p.sched.Execute(context.Background(), run, spec, graph, opts)
		if err != nil {
			t.Errorf("Execute() error = %v", err)
		}
		done <- final
	}()

	// Wait until the job is genuinely running before cancelling.
	deadline := time.After(10 * time.Second)
	for {
		job, err := p.db.JobByName(context.Background(), run.ID, "slow")
		if err == nil && job.Status == model.StatusRunning {
			break
		}
		select {
		case <-deadline:
			t.Fatal("slow job never started")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if err := p.sched.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}

	select {
	case final := <-done:
		if final.Status != model.StatusCancelled {
			t.Errorf("run status = %q, want cancelled", final.Status)
		}
		p.assertStatuses(run.ID, map[string]model.Status{
			"slow":  model.StatusCancelled,
			"after": model.StatusCancelled,
		})
	case <-time.After(20 * time.Second):
		t.Fatal("cancellation did not finish the run")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	p := newProject(t)
	run := p.run(p.copyExample("basic.yml"))
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q", run.Status)
	}
	if err := p.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen exactly as a new forge process would.
	db2, err := store.Open(context.Background(), store.Options{Path: p.cfg.Database.Path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()

	reloaded, err := db2.RunByID(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("run did not survive restart: %v", err)
	}
	if reloaded.Status != model.StatusSuccess {
		t.Errorf("status after restart = %q", reloaded.Status)
	}
	jobs, err := db2.ListJobs(context.Background(), run.ID)
	if err != nil || len(jobs) != 3 {
		t.Errorf("jobs after restart: %d, %v", len(jobs), err)
	}
	arts, err := db2.ListArtifacts(context.Background(), run.ID)
	if err != nil || len(arts) != 1 {
		t.Errorf("artifacts after restart: %d, %v", len(arts), err)
	}
	// The artifact bytes are still readable from the store.
	art, err := artifacts.NewStore(p.cfg.Artifacts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := art.Open(arts[0].StorePath)
	if err != nil {
		t.Fatalf("artifact bytes did not survive restart: %v", err)
	}
	_ = rc.Close()
}

func TestPipelineWithACycleNeverRuns(t *testing.T) {
	p := newProject(t)
	path := p.writePipeline("cycle.yml", `
name: cyclic
jobs:
  a:
    needs: [b]
    commands: [echo a]
  b:
    needs: [a]
    commands: [echo b]
`)
	_, _, err := pipeline.Load(path)
	if err == nil {
		t.Fatal("a cyclic pipeline loaded successfully")
	}
	var cycleErr *pipeline.CycleError
	if !errors.As(err, &cycleErr) && !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want a cycle error", err)
	}
	// Nothing was written: validation happens before any state is created.
	runs, err := p.db.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("a rejected pipeline created %d run(s)", len(runs))
	}
}

func TestArtifactTraversalIsRefusedEndToEnd(t *testing.T) {
	p := newProject(t)
	// A job that tries to exfiltrate a file from outside its workspace via a
	// symlink. Collection must refuse to follow it.
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("classified\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := p.writePipeline("escape.yml", fmt.Sprintf(`
name: escape
jobs:
  sneaky:
    commands:
      - mkdir -p out
      - ln -s %s out/leak.txt
      - echo "legitimate" > out/fine.txt
    artifacts:
      paths: [out/]
`, secretFile))

	run := p.run(path)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q (error: %s)", run.Status, run.Error)
	}

	arts, err := p.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range arts {
		if strings.Contains(a.Path, "leak") {
			t.Errorf("collected %q, which points outside the workspace", a.Path)
		}
	}
	// The legitimate file was still collected: one bad match does not poison the rest.
	if len(arts) != 1 || arts[0].Path != "out/fine.txt" {
		t.Errorf("artifacts = %+v, want only out/fine.txt", arts)
	}
	// And nothing in the store contains the secret.
	err = filepath.Walk(p.cfg.Artifacts.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // keep walking
		}
		body, _ := os.ReadFile(path) //nolint:gosec // test-controlled tree
		if strings.Contains(string(body), "classified") {
			t.Errorf("a file from outside the workspace reached the artifact store: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStdoutAndStderrStayApart(t *testing.T) {
	p := newProject(t)
	path := p.writePipeline("streams.yml", `
name: streams
jobs:
  noisy:
    commands:
      - echo "this is stdout"
      - echo "this is stderr" >&2
`)
	run := p.run(path)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %q", run.Status)
	}

	jobs := p.jobs(run.ID)
	records, err := p.db.ListLogs(context.Background(), jobs["noisy"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("len(log records) = %d, want 2", len(records))
	}

	// The executor echoes each command to stdout before running it, the way CI
	// logs do, so `$ echo "this is stderr" >&2` legitimately appears on stdout.
	// Comparing whole lines rather than substrings distinguishes that echo from
	// the job's actual output, which is what this test is really about.
	hasLine := func(text, want string) bool {
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == want {
				return true
			}
		}
		return false
	}

	for _, rec := range records {
		body, err := logs.ReadAll(rec.Path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		switch rec.Stream {
		case model.StreamStdout:
			if !hasLine(text, "this is stdout") {
				t.Errorf("stdout log missing its line: %q", text)
			}
			if hasLine(text, "this is stderr") {
				t.Errorf("stderr output leaked into the stdout log: %q", text)
			}
		case model.StreamStderr:
			if !hasLine(text, "this is stderr") {
				t.Errorf("stderr log missing its line: %q", text)
			}
			if hasLine(text, "this is stdout") {
				t.Errorf("stdout output leaked into the stderr log: %q", text)
			}
			// The command echo belongs to stdout only; stderr carries output alone.
			if strings.Contains(text, "$ echo") {
				t.Errorf("command echoes should not appear on stderr: %q", text)
			}
		}
	}
}

func TestRunEventTimelineIsComplete(t *testing.T) {
	p := newProject(t)
	run := p.run(p.copyExample("basic.yml"))

	events, err := p.db.ListEvents(context.Background(), run.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Every job should have at least a running and a terminal transition.
	seen := map[string]int{}
	for _, e := range events {
		if e.Type == model.EventJobStatus {
			seen[e.JobName]++
		}
	}
	for _, name := range []string{"build", "test", "package"} {
		if seen[name] < 2 {
			t.Errorf("job %q produced %d transitions, want at least 2", name, seen[name])
		}
	}

	// IDs must increase monotonically so SSE resume works.
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("event IDs are not increasing: %d then %d", events[i-1].ID, events[i].ID)
		}
	}
}

func TestConcurrentRunsOfTheSamePipeline(t *testing.T) {
	p := newProject(t)
	path := p.copyExample("basic.yml")
	spec, graph, err := pipeline.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	const parallel = 4
	results := make(chan *model.Run, parallel)
	for i := 0; i < parallel; i++ {
		go func() {
			run, err := p.sched.Run(context.Background(), spec, graph,
				scheduler.RunOptions{Manual: scheduler.ManualSkip, SourcePath: path})
			if err != nil {
				t.Errorf("Run() error = %v", err)
				results <- nil
				return
			}
			results <- run
		}()
	}

	numbers := map[int64]bool{}
	for i := 0; i < parallel; i++ {
		select {
		case run := <-results:
			if run == nil {
				t.Fatal("a concurrent run failed to start")
			}
			if run.Status != model.StatusSuccess {
				t.Errorf("run %d status = %q, want success (error: %s)", run.ID, run.Status, run.Error)
			}
			if numbers[run.Number] {
				t.Errorf("run number %d was allocated twice", run.Number)
			}
			numbers[run.Number] = true
		case <-time.After(60 * time.Second):
			t.Fatal("concurrent runs did not finish")
		}
	}
	if len(numbers) != parallel {
		t.Errorf("got %d distinct run numbers, want %d", len(numbers), parallel)
	}
}
