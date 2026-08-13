package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/nickcross-79/forge/internal/artifacts"
	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/nickcross-79/forge/internal/scheduler"
	"github.com/nickcross-79/forge/internal/store"
)

// handleHealth reports liveness and the schema version, which is enough for a
// script to know forge is up and its database is migrated.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	versions, err := s.db.AppliedVersions(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy", err.Error())
		return
	}
	schema := ""
	if len(versions) > 0 {
		schema = versions[len(versions)-1]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"schema_version": schema,
		"active_runs":    s.sched.ActiveRuns(),
		"time":           time.Now().UTC(),
	})
}

// handleSettings returns the effective configuration. Secret values are never
// included — only their names, so the UI can show what is available.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"project_root":  s.cfg.Root,
		"home":          s.cfg.Home,
		"database":      s.cfg.Database.Path,
		"artifacts_dir": s.cfg.Artifacts.Dir,
		"logs_dir":      s.cfg.Logs.Dir,
		"config_file":   s.cfg.Path,
		"loopback_only": s.cfg.LoopbackHost(),
		"auth_required": s.cfg.Server.Token != "",
		"secret_names":  s.secrets.Names(),
		"runner": map[string]any{
			"concurrency":      s.cfg.Runner.Concurrency,
			"default_executor": s.cfg.Runner.DefaultExecutor,
			"default_timeout":  s.cfg.Runner.DefaultTimeout.String(),
			"default_retries":  s.cfg.Runner.DefaultRetries,
			"manual_timeout":   s.cfg.Runner.ManualTimeout.String(),
			"env_passthrough":  s.cfg.Runner.EnvPassthrough,
		},
		"workspace": map[string]any{
			"mode":      string(s.cfg.Workspace.Mode),
			"ignore":    s.cfg.Workspace.Ignore,
			"keep_runs": s.cfg.Workspace.KeepRuns,
		},
		"retention": map[string]any{
			"artifacts": s.cfg.Artifacts.Retention.String(),
			"logs":      s.cfg.Logs.Retention.String(),
		},
	})
}

// handleMetrics returns process counters merged with historical aggregates.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	durations, err := s.db.RecentDurations(r.Context(), queryInt(r, "trend", 30))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"process":   s.sched.Metrics().Snapshot(),
		"totals":    stats,
		"durations": durations,
	})
}

// handlePrometheus serves the text exposition format.
func (s *Server) handlePrometheus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := io.WriteString(w, s.sched.Metrics().Prometheus()); err != nil {
		s.logger.Debug("failed to write metrics", "error", err)
	}
}

// --- pipelines --------------------------------------------------------------

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.ListPipelines(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if list == nil {
		list = []*store.PipelineWithStats{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pipelines": list})
}

func (s *Server) handleGetPipeline(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	p, err := s.db.PipelineByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	body := map[string]any{"pipeline": p}

	// Parsing the stored spec gives the dashboard the job graph without needing
	// the pipeline file to still exist on disk.
	if spec, graph, err := parseStored(p.SpecYAML); err == nil {
		body["jobs"] = describeJobs(spec, graph)
		body["stages"] = graph.Stages
		body["layers"] = graph.Layers()
	} else {
		body["spec_error"] = err.Error()
	}

	runs, err := s.db.ListRuns(r.Context(), store.RunFilter{PipelineID: id, Limit: queryInt(r, "runs", 20)})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	body["runs"] = runs
	writeJSON(w, http.StatusOK, body)
}

// startRunRequest is the body of POST /pipelines/{id}/runs.
type startRunRequest struct {
	Concurrency int               `json:"concurrency,omitempty"`
	Variables   map[string]string `json:"variables,omitempty"`
	Approved    []string          `json:"approved,omitempty"`
	// Manual selects "wait" (default from the API, since the dashboard can
	// approve) or "skip".
	Manual string `json:"manual,omitempty"`
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var req startRunRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return
	}

	p, err := s.db.PipelineByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	spec, graph, err := parseStored(p.SpecYAML)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_pipeline",
			fmt.Sprintf("stored pipeline %q is not valid: %v", p.Name, err))
		return
	}

	manual := scheduler.ManualWait
	if req.Manual == string(scheduler.ManualSkip) {
		manual = scheduler.ManualSkip
	}

	run, err := s.sched.Start(r.Context(), spec, graph, scheduler.RunOptions{
		Trigger:     model.TriggerAPI,
		Concurrency: req.Concurrency,
		Manual:      manual,
		Approved:    req.Approved,
		Variables:   req.Variables,
		SourcePath:  p.SourcePath,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/api/v1/runs/%d", run.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

// --- runs -------------------------------------------------------------------

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	filter := store.RunFilter{
		PipelineID: int64(queryInt(r, "pipeline_id", 0)),
		Status:     queryStatuses(r),
		Limit:      queryInt(r, "limit", 50),
		Offset:     queryInt(r, "offset", 0),
	}
	runs, err := s.db.ListRuns(r.Context(), filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	total, err := s.db.CountRuns(r.Context(), filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":   runs,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	run, err := s.db.RunByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	jobs, err := s.db.ListJobs(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	arts, err := s.db.ListArtifacts(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	body := map[string]any{
		"run":       run,
		"jobs":      jobs,
		"artifacts": arts,
		"active":    s.sched.IsActive(id),
	}

	// The graph comes from the snapshot taken when the run started, so an edited
	// pipeline file does not rewrite the history of an old run.
	if _, graph, err := parseStored(run.SpecYAML); err == nil {
		body["layers"] = graph.Layers()
		body["edges"] = describeEdges(graph)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if _, err := s.db.RunByID(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.sched.Cancel(id); err != nil {
		if errors.Is(err, scheduler.ErrRunNotActive) {
			writeError(w, http.StatusConflict, "not_active",
				"this run is not executing in this forge process, so there is nothing to cancel")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": id})
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.sched.IsActive(id) {
		writeError(w, http.StatusConflict, "run_active", "cancel the run before deleting it")
		return
	}
	if err := s.db.DeleteRun(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.sched.Artifacts().RemoveRun(id); err != nil {
		s.logger.Warn("failed to remove artifacts for deleted run", "run", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleApproveJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "a job name is required")
		return
	}

	if err := s.sched.Approve(id, name); err != nil {
		switch {
		case errors.Is(err, scheduler.ErrRunNotActive):
			writeError(w, http.StatusConflict, "not_active", "this run is not currently executing")
		case errors.Is(err, scheduler.ErrJobNotAwaitingApproval):
			writeError(w, http.StatusConflict, "not_awaiting", err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"approved": name, "run_id": id})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	jobs, err := s.db.ListJobs(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	after := int64(queryInt(r, "after", 0))
	events, err := s.db.ListEvents(r.Context(), id, after, queryInt(r, "limit", 500))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// --- jobs -------------------------------------------------------------------

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	job, err := s.db.JobByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	attempts, err := s.db.ListAttempts(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	logRecords, err := s.db.ListLogs(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	arts, err := s.db.ListJobArtifacts(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":       job,
		"attempts":  attempts,
		"logs":      logRecords,
		"artifacts": arts,
	})
}

// handleJobLogs returns captured output. `stream` selects stdout, stderr or both;
// `offset` supports incremental reads, which is what the SSE client falls back to.
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	records, err := s.db.ListLogs(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	wanted := r.URL.Query().Get("stream")
	type payload struct {
		Stream    model.Stream `json:"stream"`
		Attempt   int64        `json:"attempt_id"`
		Content   string       `json:"content"`
		Bytes     int64        `json:"bytes"`
		Offset    int64        `json:"offset"`
		Truncated bool         `json:"truncated"`
	}

	out := []payload{}
	for _, rec := range records {
		if wanted != "" && wanted != "both" && string(rec.Stream) != wanted {
			continue
		}
		offset := int64(queryInt(r, "offset", 0))
		data, next, err := logs.ReadFrom(rec.Path, offset, int64(queryInt(r, "limit", 1<<20)))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out = append(out, payload{
			Stream:    rec.Stream,
			Attempt:   rec.AttemptID,
			Content:   string(data),
			Bytes:     rec.Bytes,
			Offset:    next,
			Truncated: rec.Truncated,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

// --- artifacts --------------------------------------------------------------

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	list, err := s.db.ListArtifacts(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": list})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a, err := s.db.ArtifactByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": a})
}

// handleDownloadArtifact streams an artifact's bytes.
//
// The store path is re-validated against the store root on the way out, so even
// a tampered database row cannot turn this endpoint into an arbitrary file read.
func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a, err := s.db.ArtifactByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	reader, err := s.sched.Artifacts().Open(a.StorePath)
	if err != nil {
		if errors.Is(err, artifacts.ErrPathEscape) {
			s.logger.Error("refused to serve an artifact outside the store",
				"artifact", id, "store_path", a.StorePath)
			writeError(w, http.StatusForbidden, "forbidden", "artifact path is outside the artifact store")
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "artifact bytes are no longer on disk")
		return
	}
	defer func() { _ = reader.Close() }()

	filename := path.Base(a.Path)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	// The filename is quoted and the header is a fixed shape, so a hostile
	// artifact name cannot inject extra headers.
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(filename)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if a.SHA256 != "" {
		w.Header().Set("X-Artifact-SHA256", a.SHA256)
	}

	if _, err := io.Copy(w, reader); err != nil {
		s.logger.Debug("artifact download interrupted", "artifact", id, "error", err)
	}
}

// sanitizeFilename strips characters that have meaning inside a header value.
func sanitizeFilename(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 32 || r == '"' || r == '\\' || r == '\n' || r == '\r' {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "artifact"
	}
	return string(out)
}

// --- shared helpers ---------------------------------------------------------

// parseStored rebuilds a spec and graph from a stored YAML snapshot.
func parseStored(specYAML string) (*pipeline.Spec, *pipeline.Graph, error) {
	if specYAML == "" {
		return nil, nil, errors.New("no stored pipeline definition")
	}
	spec, err := pipeline.Parse([]byte(specYAML))
	if err != nil {
		return nil, nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, nil, err
	}
	graph, err := spec.Graph()
	if err != nil {
		return nil, nil, err
	}
	return spec, graph, nil
}

// jobSummary is the static description of a job, as opposed to its run state.
type jobSummary struct {
	Name         string   `json:"name"`
	Stage        string   `json:"stage"`
	Needs        []string `json:"needs"`
	Commands     []string `json:"commands"`
	Image        string   `json:"image,omitempty"`
	Executor     string   `json:"executor"`
	When         string   `json:"when"`
	If           string   `json:"if,omitempty"`
	AllowFailure bool     `json:"allow_failure"`
	Timeout      string   `json:"timeout,omitempty"`
	Retries      int      `json:"retries"`
	Artifacts    []string `json:"artifacts,omitempty"`
	Implicit     bool     `json:"implicit_needs"`
}

func describeJobs(spec *pipeline.Spec, graph *pipeline.Graph) []jobSummary {
	out := make([]jobSummary, 0, len(spec.Jobs))
	for _, name := range spec.JobNames() {
		job := spec.Jobs[name]
		node := graph.Nodes[name]
		needs := node.Needs
		if needs == nil {
			// A root job has no dependencies. Emitting null here would make every
			// client guard the field before iterating it.
			needs = []string{}
		}
		summary := jobSummary{
			Name:         name,
			Stage:        job.Stage,
			Needs:        needs,
			Commands:     job.Commands,
			Image:        job.Image,
			Executor:     job.Executor,
			When:         string(job.When),
			If:           job.If,
			AllowFailure: job.AllowFailure,
			Retries:      job.Retry.Max,
			Implicit:     node.Implicit,
		}
		if summary.Executor == "" {
			summary.Executor = "local"
		}
		if summary.Commands == nil {
			summary.Commands = []string{}
		}
		if job.Timeout > 0 {
			summary.Timeout = job.Timeout.String()
		}
		if job.Artifacts != nil {
			summary.Artifacts = job.Artifacts.Paths
		}
		out = append(out, summary)
	}
	return out
}

// edge is a dependency link, for drawing the graph.
type edge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Implicit bool   `json:"implicit"`
}

func describeEdges(graph *pipeline.Graph) []edge {
	edges := []edge{}
	for _, name := range graph.Order {
		node := graph.Nodes[name]
		for _, need := range node.Needs {
			edges = append(edges, edge{From: need, To: name, Implicit: node.Implicit})
		}
	}
	return edges
}
