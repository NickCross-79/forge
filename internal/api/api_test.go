package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/executor"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/nickcross-79/forge/internal/scheduler"
	"github.com/nickcross-79/forge/internal/secrets"
	"github.com/nickcross-79/forge/internal/store"
)

// Fixture values standing in for a secret and an API token. Neither is a real
// credential; they exist so the tests can assert that a value registered as
// secret is masked, and that the token check accepts and rejects correctly.
const (
	fixtureSecretValue = "example-not-a-real-secret"
	fixtureAPIToken    = "example-not-a-real-token"
)

// stubExecutor produces deterministic output without touching a shell.
type stubExecutor struct {
	hold chan struct{}
	fail bool
}

func (e *stubExecutor) Run(ctx context.Context, job executor.Job) executor.Result {
	started := time.Now()
	if job.Stdout != nil {
		fmt.Fprintf(job.Stdout, "hello from %s\n", job.Name)
	}
	if job.Stderr != nil {
		fmt.Fprintf(job.Stderr, "diagnostics from %s\n", job.Name)
	}
	if e.hold != nil {
		select {
		case <-e.hold:
		case <-ctx.Done():
			return executor.Result{ExitCode: -1, Cancelled: true, StartedAt: started, EndedAt: time.Now()}
		}
	}
	if e.fail {
		return executor.Result{ExitCode: 1, Err: fmt.Errorf("job %q failed", job.Name),
			StartedAt: started, EndedAt: time.Now()}
	}
	return executor.Result{ExitCode: 0, StartedAt: started, EndedAt: time.Now()}
}

type testEnv struct {
	t      *testing.T
	server *Server
	db     *store.DB
	sched  *scheduler.Scheduler
	cfg    *config.Config
	stub   *stubExecutor
	ts     *httptest.Server
}

func newTestEnv(t *testing.T, tune ...func(*config.Config)) *testEnv {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default(root)
	cfg.Runner.Concurrency = 4
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

	stub := &stubExecutor{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sched, err := scheduler.New(scheduler.Options{
		Config: cfg, DB: db, Local: stub, Docker: stub, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	sec := secrets.New()
	sec.Set("API_TOKEN", fixtureSecretValue)

	srv, err := New(Options{Config: cfg, DB: db, Scheduler: sched, Secrets: sec, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &testEnv{t: t, server: srv, db: db, sched: sched, cfg: cfg, stub: stub, ts: ts}
}

// seedRun executes a pipeline synchronously so the API has something to serve.
func (e *testEnv) seedRun(doc string) *model.Run {
	e.t.Helper()
	spec, err := pipeline.Parse([]byte(doc))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := spec.Validate(); err != nil {
		e.t.Fatal(err)
	}
	graph, err := spec.Graph()
	if err != nil {
		e.t.Fatal(err)
	}
	run, err := e.sched.Run(context.Background(), spec, graph,
		scheduler.RunOptions{Manual: scheduler.ManualSkip, SourcePath: "pipeline.yml"})
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

func (e *testEnv) get(path string) *http.Response {
	e.t.Helper()
	resp, err := http.Get(e.ts.URL + path) //nolint:noctx // test client
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *testEnv) post(path, body string) *http.Response {
	e.t.Helper()
	resp, err := http.Post(e.ts.URL+path, "application/json", strings.NewReader(body)) //nolint:noctx // test client
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

// decode reads a JSON response body into a map.
func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

const simplePipeline = `
name: api-test
jobs:
  build:
    commands: [echo build]
    artifacts:
      paths: [dist/]
  test:
    commands: [echo test]
    needs: [build]
`

// --- basics -----------------------------------------------------------------

func TestHealth(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get("/api/v1/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if body["schema_version"] == "" {
		t.Error("schema_version is empty; migrations should have run")
	}
}

func TestSettingsNeverLeaksSecretValues(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get("/api/v1/settings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), fixtureSecretValue) {
		t.Errorf("settings response leaked a secret value:\n%s", raw)
	}
	// The name is fine to expose, and the UI needs it.
	if !strings.Contains(string(raw), "API_TOKEN") {
		t.Errorf("settings should list secret names, got:\n%s", raw)
	}
}

func TestMetricsEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun(simplePipeline)

	resp := e.get("/api/v1/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decode(t, resp)
	for _, key := range []string{"process", "totals", "durations"} {
		if _, ok := body[key]; !ok {
			t.Errorf("metrics response is missing %q", key)
		}
	}
	totals, _ := body["totals"].(map[string]any)
	if totals["runs_total"].(float64) != 1 {
		t.Errorf("runs_total = %v, want 1", totals["runs_total"])
	}

	prom := e.get("/metrics")
	defer func() { _ = prom.Body.Close() }()
	if ct := prom.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	raw, _ := io.ReadAll(prom.Body)
	for _, want := range []string{"forge_runs_started_total", "# TYPE forge_jobs_active gauge"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("prometheus output missing %q", want)
		}
	}
}

// --- pipelines and runs -----------------------------------------------------

func TestListAndGetPipeline(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun(simplePipeline)

	body := decode(t, e.get("/api/v1/pipelines"))
	list, _ := body["pipelines"].([]any)
	if len(list) != 1 {
		t.Fatalf("len(pipelines) = %d, want 1", len(list))
	}
	first, _ := list[0].(map[string]any)
	if first["name"] != "api-test" {
		t.Errorf("name = %v, want api-test", first["name"])
	}

	detail := decode(t, e.get("/api/v1/pipelines/1"))
	if _, ok := detail["jobs"]; !ok {
		t.Error("pipeline detail should describe its jobs")
	}
	if _, ok := detail["layers"]; !ok {
		t.Error("pipeline detail should include graph layers for the dashboard")
	}
	jobs, _ := detail["jobs"].([]any)
	if len(jobs) != 2 {
		t.Errorf("len(jobs) = %d, want 2", len(jobs))
	}

	missing := e.get("/api/v1/pipelines/999")
	defer func() { _ = missing.Body.Close() }()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing pipeline status = %d, want 404", missing.StatusCode)
	}
}

func TestListRunsFilterAndPaginate(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < 3; i++ {
		e.seedRun(simplePipeline)
	}

	body := decode(t, e.get("/api/v1/runs"))
	runs, _ := body["runs"].([]any)
	if len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3", len(runs))
	}
	if body["total"].(float64) != 3 {
		t.Errorf("total = %v, want 3", body["total"])
	}

	paged := decode(t, e.get("/api/v1/runs?limit=1&offset=1"))
	pagedRuns, _ := paged["runs"].([]any)
	if len(pagedRuns) != 1 {
		t.Errorf("len(paged runs) = %d, want 1", len(pagedRuns))
	}

	filtered := decode(t, e.get("/api/v1/runs?status=success"))
	successRuns, _ := filtered["runs"].([]any)
	if len(successRuns) != 3 {
		t.Errorf("len(success runs) = %d, want 3", len(successRuns))
	}

	none := decode(t, e.get("/api/v1/runs?status=failed"))
	noneRuns, _ := none["runs"].([]any)
	if len(noneRuns) != 0 {
		t.Errorf("len(failed runs) = %d, want 0", len(noneRuns))
	}
	// An unrecognised status is ignored rather than erroring, so a stale
	// bookmark still returns something sensible.
	bogus := decode(t, e.get("/api/v1/runs?status=nonsense"))
	if _, ok := bogus["runs"]; !ok {
		t.Error("an unknown status filter should not break the endpoint")
	}
}

func TestGetRunIncludesGraph(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)

	body := decode(t, e.get(fmt.Sprintf("/api/v1/runs/%d", run.ID)))
	if _, ok := body["run"]; !ok {
		t.Fatal("response is missing the run")
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 2 {
		t.Errorf("len(jobs) = %d, want 2", len(jobs))
	}
	edges, _ := body["edges"].([]any)
	if len(edges) != 1 {
		t.Errorf("len(edges) = %d, want 1 (build -> test)", len(edges))
	}
	if active, _ := body["active"].(bool); active {
		t.Error("a finished run should not be reported as active")
	}
}

func TestStartRunViaAPI(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun(simplePipeline) // registers the pipeline

	resp := e.post("/api/v1/pipelines/1/runs", `{"manual":"skip"}`)
	if resp.StatusCode != http.StatusAccepted {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, raw)
	}
	if loc := resp.Header.Get("Location"); loc == "" {
		t.Error("a started run should return a Location header")
	}
	body := decode(t, resp)
	runInfo, _ := body["run"].(map[string]any)
	runID := int64(runInfo["id"].(float64))

	// The run executes in the background; wait for it to settle.
	waitForStatus(t, e, runID, model.StatusSuccess)

	final, err := e.db.RunByID(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Trigger != model.TriggerAPI {
		t.Errorf("trigger = %q, want api", final.Trigger)
	}
}

func TestStartRunWithVariableOverride(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun("name: vars\njobs:\n  a:\n    commands: [echo hi]\n")

	resp := e.post("/api/v1/pipelines/1/runs", `{"manual":"skip","variables":{"EXTRA":"value"}}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	body := decode(t, resp)
	runInfo, _ := body["run"].(map[string]any)
	waitForStatus(t, e, int64(runInfo["id"].(float64)), model.StatusSuccess)

	// The stored pipeline must be unchanged by a per-run override.
	p, err := e.db.PipelineByID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.SpecYAML, "EXTRA") {
		t.Error("a per-run variable override was written back to the stored pipeline")
	}
}

func TestStartRunRejectsUnknownField(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun(simplePipeline)

	resp := e.post("/api/v1/pipelines/1/runs", `{"concurency":2}`) // typo
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field", resp.StatusCode)
	}
}

func TestCancelRun(t *testing.T) {
	e := newTestEnv(t)
	e.stub.hold = make(chan struct{})
	defer close(e.stub.hold)

	spec, _ := pipeline.Parse([]byte("name: slow\njobs:\n  a:\n    commands: [x]\n"))
	graph, _ := spec.Graph()
	run, err := e.sched.Start(context.Background(), spec, graph, scheduler.RunOptions{Manual: scheduler.ManualSkip})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the scheduler owns the run.
	deadline := time.After(5 * time.Second)
	for !e.sched.IsActive(run.ID) {
		select {
		case <-deadline:
			t.Fatal("run never became active")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	resp := e.post(fmt.Sprintf("/api/v1/runs/%d/cancel", run.ID), "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, raw)
	}
	waitForStatus(t, e, run.ID, model.StatusCancelled)
}

func TestCancelInactiveRunConflicts(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)

	resp := e.post(fmt.Sprintf("/api/v1/runs/%d/cancel", run.ID), "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a finished run", resp.StatusCode)
	}

	missing := e.post("/api/v1/runs/9999/cancel", "")
	defer func() { _ = missing.Body.Close() }()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown run", missing.StatusCode)
	}
}

func TestApproveManualJob(t *testing.T) {
	e := newTestEnv(t)
	doc := "name: gated\njobs:\n  deploy:\n    commands: [x]\n    when: manual\n"
	spec, _ := pipeline.Parse([]byte(doc))
	graph, _ := spec.Graph()
	run, err := e.sched.Start(context.Background(), spec, graph, scheduler.RunOptions{Manual: scheduler.ManualWait})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the gate.
	deadline := time.After(10 * time.Second)
	for {
		job, err := e.db.JobByName(context.Background(), run.ID, "deploy")
		if err == nil && job.Status == model.StatusAwaitingManual {
			break
		}
		select {
		case <-deadline:
			t.Fatal("deploy never reached awaiting_manual")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	resp := e.post(fmt.Sprintf("/api/v1/runs/%d/jobs/deploy/approve", run.ID), "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, raw)
	}
	waitForStatus(t, e, run.ID, model.StatusSuccess)
}

func TestApproveErrors(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)

	resp := e.post(fmt.Sprintf("/api/v1/runs/%d/jobs/build/approve", run.ID), "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 approving a job in a finished run", resp.StatusCode)
	}
}

func TestDeleteRun(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete,
		fmt.Sprintf("%s/api/v1/runs/%d", e.ts.URL, run.ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, err := e.db.RunByID(context.Background(), run.ID); err == nil {
		t.Error("run still exists after delete")
	}
}

// --- jobs, logs and artifacts -----------------------------------------------

func TestGetJobAndLogs(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)
	jobs, err := e.db.ListJobs(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	jobID := jobs[0].ID

	detail := decode(t, e.get(fmt.Sprintf("/api/v1/jobs/%d", jobID)))
	for _, key := range []string{"job", "attempts", "logs", "artifacts"} {
		if _, ok := detail[key]; !ok {
			t.Errorf("job detail is missing %q", key)
		}
	}

	all := decode(t, e.get(fmt.Sprintf("/api/v1/jobs/%d/logs", jobID)))
	entries, _ := all["logs"].([]any)
	if len(entries) != 2 {
		t.Fatalf("len(log entries) = %d, want 2 (stdout and stderr)", len(entries))
	}

	stdoutOnly := decode(t, e.get(fmt.Sprintf("/api/v1/jobs/%d/logs?stream=stdout", jobID)))
	only, _ := stdoutOnly["logs"].([]any)
	if len(only) != 1 {
		t.Fatalf("len(stdout entries) = %d, want 1", len(only))
	}
	entry, _ := only[0].(map[string]any)
	if !strings.Contains(entry["content"].(string), "hello from") {
		t.Errorf("stdout content = %q", entry["content"])
	}
	if entry["stream"] != "stdout" {
		t.Errorf("stream = %v, want stdout", entry["stream"])
	}
}

func TestArtifactListingAndDownload(t *testing.T) {
	e := newTestEnv(t)
	// Write an artifact from inside the job so there is something to collect.
	e.stub = &stubExecutor{}
	run := e.seedRunWithArtifact()

	list := decode(t, e.get(fmt.Sprintf("/api/v1/runs/%d/artifacts", run.ID)))
	arts, _ := list["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("len(artifacts) = %d, want 1", len(arts))
	}
	art, _ := arts[0].(map[string]any)
	artID := int64(art["id"].(float64))

	meta := decode(t, e.get(fmt.Sprintf("/api/v1/artifacts/%d", artID)))
	if _, ok := meta["artifact"]; !ok {
		t.Error("artifact metadata response is malformed")
	}

	resp := e.get(fmt.Sprintf("/api/v1/artifacts/%d/download", artID))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="output.txt"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if resp.Header.Get("X-Artifact-SHA256") == "" {
		t.Error("download should report the artifact checksum")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "artifact contents\n" {
		t.Errorf("downloaded body = %q", body)
	}
}

// seedRunWithArtifact runs a pipeline whose job writes a file to collect.
func (e *testEnv) seedRunWithArtifact() *model.Run {
	e.t.Helper()
	writer := &writingExecutor{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sched, err := scheduler.New(scheduler.Options{
		Config: e.cfg, DB: e.db, Local: writer, Docker: writer, Logger: logger,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	// The API server holds the original scheduler, so point it at this one for
	// artifact lookups by swapping the field the handlers read.
	e.server.sched = sched

	doc := "name: with-artifacts\njobs:\n  build:\n    commands: [x]\n    artifacts:\n      paths: [dist/]\n"
	spec, _ := pipeline.Parse([]byte(doc))
	graph, _ := spec.Graph()
	run, err := sched.Run(context.Background(), spec, graph, scheduler.RunOptions{Manual: scheduler.ManualSkip})
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

type writingExecutor struct{}

func (writingExecutor) Run(_ context.Context, job executor.Job) executor.Result {
	started := time.Now()
	dir := filepath.Join(job.Workspace, "dist")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return executor.Result{ExitCode: 1, Err: err, StartedAt: started, EndedAt: time.Now()}
	}
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte("artifact contents\n"), 0o600); err != nil {
		return executor.Result{ExitCode: 1, Err: err, StartedAt: started, EndedAt: time.Now()}
	}
	return executor.Result{ExitCode: 0, StartedAt: started, EndedAt: time.Now()}
}

func TestDownloadRejectsMissingArtifact(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get("/api/v1/artifacts/9999/download")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDownloadRefusesEscapingStorePath is the security regression test for the
// download endpoint: even a database row whose store_path points outside the
// artifact store must not turn into an arbitrary file read.
func TestDownloadRefusesEscapingStorePath(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)
	jobs, _ := e.db.ListJobs(context.Background(), run.ID)

	secretPath := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(secretPath, []byte("root:x:0:0"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a tampered or corrupted row.
	if _, err := e.db.SQL().ExecContext(context.Background(), `
		INSERT INTO artifacts (run_id, job_id, job_name, path, store_path, size, sha256, mode, created_at)
		VALUES (?, ?, 'evil', 'passwd', ?, 10, '', 420, 0)`,
		run.ID, jobs[0].ID, "../../../../../../"+strings.TrimPrefix(secretPath, "/")); err != nil {
		t.Fatal(err)
	}

	arts, err := e.db.ListArtifacts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected the planted artifact row, got %d", len(arts))
	}

	resp := e.get(fmt.Sprintf("/api/v1/artifacts/%d/download", arts[0].ID))
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("an escaping store path was served with 200: %q", body)
	}
	if strings.Contains(string(body), "root:x:0:0") {
		t.Fatalf("the download endpoint served a file outside the artifact store: %q", body)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestSecretsAreRedactedInServedLogs(t *testing.T) {
	e := newTestEnv(t)
	// The scheduler built in newTestEnv has no secrets, so build one that does.
	sec := secrets.New()
	sec.Set("API_TOKEN", fixtureSecretValue)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	leaky := &leakyExecutor{}
	sched, err := scheduler.New(scheduler.Options{
		Config: e.cfg, DB: e.db, Local: leaky, Docker: leaky, Secrets: sec, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.server.sched = sched

	spec, _ := pipeline.Parse([]byte("name: leaky\njobs:\n  a:\n    commands: [x]\n"))
	graph, _ := spec.Graph()
	run, err := sched.Run(context.Background(), spec, graph, scheduler.RunOptions{Manual: scheduler.ManualSkip})
	if err != nil {
		t.Fatal(err)
	}

	jobs, _ := e.db.ListJobs(context.Background(), run.ID)
	resp := e.get(fmt.Sprintf("/api/v1/jobs/%d/logs", jobs[0].ID))
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(raw), fixtureSecretValue) {
		t.Errorf("the logs endpoint served an unredacted secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "***") {
		t.Errorf("expected a redaction mask in the served logs:\n%s", raw)
	}
}

type leakyExecutor struct{}

func (leakyExecutor) Run(_ context.Context, job executor.Job) executor.Result {
	started := time.Now()
	fmt.Fprintf(job.Stdout, "token=%s\n", job.Env["API_TOKEN"])
	return executor.Result{ExitCode: 0, StartedAt: started, EndedAt: time.Now()}
}

// --- SSE --------------------------------------------------------------------

func TestStreamEventsForFinishedRun(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/runs/%d/events/stream", e.ts.URL, run.ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events, done := readSSE(t, resp.Body, 10*time.Second)
	if !done {
		t.Error("a finished run's stream should end with a done event rather than hanging")
	}
	if len(events) == 0 {
		t.Fatal("no events were streamed")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "run_status") || !strings.Contains(joined, "job_status") {
		t.Errorf("stream is missing transitions:\n%s", joined)
	}
}

func TestStreamEventsResumesFromLastEventID(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)

	all, err := e.db.ListEvents(context.Background(), run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("need several events to test resume, got %d", len(all))
	}
	resumeAfter := all[len(all)-2].ID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/runs/%d/events/stream", e.ts.URL, run.ID), nil)
	req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", resumeAfter))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	events, _ := readSSE(t, resp.Body, 10*time.Second)
	// Only the events after the resume point should be replayed.
	if len(events) > 2 {
		t.Errorf("resume replayed %d events, want at most 2", len(events))
	}
	for _, ev := range events {
		if strings.Contains(ev, fmt.Sprintf(`"id":%d,`, all[0].ID)) {
			t.Error("resume replayed events the client had already seen")
		}
	}
}

func TestStreamLogsForFinishedJob(t *testing.T) {
	e := newTestEnv(t)
	run := e.seedRun(simplePipeline)
	jobs, _ := e.db.ListJobs(context.Background(), run.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/jobs/%d/logs/stream?stream=stdout", e.ts.URL, jobs[0].ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	events, done := readSSE(t, resp.Body, 10*time.Second)
	if !done {
		t.Error("a finished job's log stream should terminate")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "hello from build") {
		t.Errorf("log stream did not deliver the output:\n%s", joined)
	}
}

// readSSE reads events until a `done` event arrives or the deadline passes.
func readSSE(t *testing.T, body io.Reader, timeout time.Duration) (events []string, done bool) {
	t.Helper()
	type result struct {
		events []string
		done   bool
	}
	ch := make(chan result, 1)

	go func() {
		var collected []string
		sawDone := false
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var current strings.Builder
		isDone := false

		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: done"):
				isDone = true
			case strings.HasPrefix(line, "data: "):
				current.WriteString(strings.TrimPrefix(line, "data: "))
			case line == "":
				if current.Len() > 0 && !isDone {
					collected = append(collected, current.String())
				}
				current.Reset()
				if isDone {
					sawDone = true
					ch <- result{collected, sawDone}
					return
				}
			}
		}
		ch <- result{collected, sawDone}
	}()

	select {
	case res := <-ch:
		return res.events, res.done
	case <-time.After(timeout):
		return nil, false
	}
}

// --- middleware -------------------------------------------------------------

func TestAuthTokenRequired(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config) { c.Server.Token = fixtureAPIToken })

	// No token: rejected.
	resp := e.get("/api/v1/health")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status without a token = %d, want 401", resp.StatusCode)
	}

	// Wrong token: rejected.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.ts.URL+"/api/v1/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	wrong, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wrong.Body.Close() }()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("status with a wrong token = %d, want 401", wrong.StatusCode)
	}

	// Correct token in the header: accepted.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.ts.URL+"/api/v1/health", nil)
	req2.Header.Set("Authorization", "Bearer "+fixtureAPIToken)
	ok, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ok.Body.Close() }()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("status with the right token = %d, want 200", ok.StatusCode)
	}

	// Query parameter, which is how EventSource authenticates.
	viaQuery := e.get("/api/v1/health?token=" + fixtureAPIToken)
	defer func() { _ = viaQuery.Body.Close() }()
	if viaQuery.StatusCode != http.StatusOK {
		t.Errorf("status with ?token = %d, want 200", viaQuery.StatusCode)
	}

	// Dashboard assets stay reachable so a browser can load the page that will
	// then present the token.
	page := e.get("/")
	defer func() { _ = page.Body.Close() }()
	if page.StatusCode == http.StatusUnauthorized {
		t.Error("the dashboard page should not require a token")
	}
}

func TestUnknownAPIPathIs404JSON(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get("/api/v1/nope")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON for an API path", ct)
	}
}

func TestBadPathParameter(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/api/v1/runs/abc", "/api/v1/jobs/-1", "/api/v1/pipelines/0"} {
		resp := e.get(path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestCORSForDevServer(t *testing.T) {
	e := newTestEnv(t)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.ts.URL+"/api/v1/health", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the dev server origin", got)
	}

	// An origin that is not configured gets no CORS headers at all.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.ts.URL+"/api/v1/health", nil)
	req2.Header.Set("Origin", "https://evil.example")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was allowed: %q", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"output.txt", "output.txt"},
		{`evil"; drop`, "evil; drop"},
		{"with\nnewline", "withnewline"},
		{"back\\slash", "backslash"},
		{"", "artifact"},
	}
	for _, tc := range tests {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// waitForStatus polls until a run reaches the expected terminal state.
func waitForStatus(t *testing.T, e *testEnv, runID int64, want model.Status) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		run, err := e.db.RunByID(context.Background(), runID)
		if err == nil && run.Status == want {
			return
		}
		select {
		case <-deadline:
			current := "unknown"
			if run, err := e.db.RunByID(context.Background(), runID); err == nil {
				current = string(run.Status)
			}
			t.Fatalf("run %d did not reach %q (currently %q)", runID, want, current)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestPipelineJobsNeverSerialiseNullNeeds is a regression test.
//
// A job with no dependencies has a nil Needs slice in Go, which marshals to JSON
// `null`. The dashboard iterates that field, so a null blanked the whole page.
// Every list the API emits must be an empty array rather than null.
func TestPipelineJobsNeverSerialiseNullNeeds(t *testing.T) {
	e := newTestEnv(t)
	// `root` has no dependencies at all; `leaf` depends on it.
	e.seedRun("name: roots\njobs:\n  root:\n    commands: [echo hi]\n  leaf:\n    needs: [root]\n    commands: [echo hi]\n")

	resp := e.get("/api/v1/pipelines/1")
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"needs":null`) {
		t.Errorf("a root job serialised needs as null:\n%s", raw)
	}
	if strings.Contains(string(raw), `"commands":null`) {
		t.Errorf("a job serialised commands as null:\n%s", raw)
	}

	var body struct {
		Jobs []struct {
			Name     string   `json:"name"`
			Needs    []string `json:"needs"`
			Commands []string `json:"commands"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(body.Jobs))
	}
	for _, job := range body.Jobs {
		if job.Needs == nil {
			t.Errorf("job %q has a nil needs slice", job.Name)
		}
		if job.Commands == nil {
			t.Errorf("job %q has a nil commands slice", job.Name)
		}
	}
}
