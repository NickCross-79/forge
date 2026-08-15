// Package api serves forge's REST API and the embedded dashboard.
//
// The API can start pipelines, which means it can run arbitrary shell commands.
// It therefore binds to loopback by default, and config.Validate refuses a
// non-loopback bind without a shared token.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/scheduler"
	"github.com/nickcross-79/forge/internal/secrets"
	"github.com/nickcross-79/forge/internal/store"
)

// Server holds the dependencies the handlers need.
type Server struct {
	cfg     *config.Config
	db      *store.DB
	sched   *scheduler.Scheduler
	secrets *secrets.Store
	logger  *slog.Logger

	handler http.Handler
}

// Options configures a Server.
type Options struct {
	Config    *config.Config
	DB        *store.DB
	Scheduler *scheduler.Scheduler
	Secrets   *secrets.Store
	Logger    *slog.Logger
}

// New builds the HTTP handler tree.
func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.DB == nil || opts.Scheduler == nil {
		return nil, errors.New("api: config, database and scheduler are required")
	}
	s := &Server{
		cfg:     opts.Config,
		db:      opts.DB,
		sched:   opts.Scheduler,
		secrets: opts.Secrets,
		logger:  opts.Logger,
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.secrets == nil {
		s.secrets = secrets.New()
	}
	s.handler = s.routes()
	return s, nil
}

// Handler is the fully-wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP lets the Server be used directly as an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// routes builds the mux. Go 1.22's method-and-wildcard patterns cover everything
// forge needs, so there is no router dependency.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	const v1 = "/api/v1"
	mux.HandleFunc("GET "+v1+"/health", s.handleHealth)
	mux.HandleFunc("GET "+v1+"/settings", s.handleSettings)
	mux.HandleFunc("GET "+v1+"/metrics", s.handleMetrics)

	mux.HandleFunc("GET "+v1+"/pipelines", s.handleListPipelines)
	mux.HandleFunc("GET "+v1+"/pipelines/{id}", s.handleGetPipeline)
	mux.HandleFunc("POST "+v1+"/pipelines/{id}/runs", s.handleStartRun)

	mux.HandleFunc("GET "+v1+"/runs", s.handleListRuns)
	mux.HandleFunc("GET "+v1+"/runs/{id}", s.handleGetRun)
	mux.HandleFunc("POST "+v1+"/runs/{id}/cancel", s.handleCancelRun)
	mux.HandleFunc("DELETE "+v1+"/runs/{id}", s.handleDeleteRun)
	mux.HandleFunc("GET "+v1+"/runs/{id}/jobs", s.handleListJobs)
	mux.HandleFunc("POST "+v1+"/runs/{id}/jobs/{name}/approve", s.handleApproveJob)
	mux.HandleFunc("GET "+v1+"/runs/{id}/artifacts", s.handleListArtifacts)
	mux.HandleFunc("GET "+v1+"/runs/{id}/events", s.handleListEvents)
	mux.HandleFunc("GET "+v1+"/runs/{id}/events/stream", s.handleStreamEvents)

	mux.HandleFunc("GET "+v1+"/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("GET "+v1+"/jobs/{id}/logs", s.handleJobLogs)
	mux.HandleFunc("GET "+v1+"/jobs/{id}/logs/stream", s.handleStreamLogs)

	mux.HandleFunc("GET "+v1+"/artifacts/{id}", s.handleGetArtifact)
	mux.HandleFunc("GET "+v1+"/artifacts/{id}/download", s.handleDownloadArtifact)

	// Prometheus scrape endpoint, outside the versioned API by convention.
	mux.HandleFunc("GET /metrics", s.handlePrometheus)

	// Everything else is the dashboard.
	mux.Handle("/", s.dashboardHandler())

	return s.withMiddleware(mux)
}

// withMiddleware wraps the mux with panic recovery, logging, CORS and auth, in
// that order from the outside in.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.recoverPanics(s.logRequests(s.cors(s.authenticate(next))))
}

// recoverPanics keeps one bad request from taking down the server. A panic in a
// handler becomes a 500 for that request and a stack trace in the log.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path,
					"panic", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE keeps working through the
// logging wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Streaming endpoints stay open for minutes; logging them at debug keeps
		// the request log readable.
		level := slog.LevelInfo
		if strings.HasSuffix(r.URL.Path, "/stream") {
			level = slog.LevelDebug
		}
		s.logger.Log(r.Context(), level, "http request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond))
	})
}

// cors allows the Vite dev server to reach the API during development. Only the
// configured origins are allowed, and credentials are never echoed back.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(s.cfg.Server.CORSOrigins))
	for _, o := range s.cfg.Server.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate enforces the bearer token when one is configured. Comparison is
// constant-time so the token cannot be recovered by timing the response.
func (s *Server) authenticate(next http.Handler) http.Handler {
	token := s.cfg.Server.Token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		// The dashboard's own assets are public; without them a browser could
		// never present a token in the first place.
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if presented == "" {
			presented = r.URL.Query().Get("token") // EventSource cannot set headers
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe runs the HTTP server until the context is cancelled, then
// shuts down gracefully so in-flight requests can finish.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr())
	if err != nil {
		return fmt.Errorf("api: listen on %s: %w", s.cfg.Addr(), err)
	}

	srv := &http.Server{
		Handler: s.handler,
		// No WriteTimeout: SSE responses stay open for the length of a run.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("api: shutdown: %w", err)
		}
		return nil
	}
}

// Addr reports the address the server is configured to bind.
func (s *Server) Addr() string { return s.cfg.Addr() }

// --- helpers ----------------------------------------------------------------

// errorBody is the shape of every error response.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		// The status line is already sent, so there is nothing to do but note it.
		slog.Default().Debug("failed to encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

// writeStoreError maps a store error onto the right HTTP status.
func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// pathInt reads a numeric path parameter.
func pathInt(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a valid %s", raw, name)
	}
	return id, nil
}

// queryInt reads an optional integer query parameter.
func queryInt(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// queryStatuses parses repeated or comma-separated `status` filters, ignoring
// values that are not real statuses.
func queryStatuses(r *http.Request) []model.Status {
	valid := make(map[string]bool)
	for _, s := range model.ValidStatuses() {
		valid[string(s)] = true
	}
	var out []model.Status
	for _, raw := range r.URL.Query()["status"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if valid[part] {
				out = append(out, model.Status(part))
			}
		}
	}
	return out
}

// decodeJSON reads a JSON request body, rejecting unknown fields so a typo in a
// client request is reported rather than silently ignored.
func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, http.ErrBodyReadAfterClose) {
			return nil
		}
		return err
	}
	return nil
}

// logTopicFor is a small indirection so handlers and tests agree on topic names.
func logTopicFor(jobID int64, stream model.Stream) string { return logs.LogTopic(jobID, stream) }
