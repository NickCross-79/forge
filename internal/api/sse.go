package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
)

// heartbeatInterval keeps idle SSE connections alive through proxies and lets
// the handler notice a disconnected client that never sent anything.
const heartbeatInterval = 25 * time.Second

// sseWriter formats Server-Sent Events.
//
// SSE is used rather than WebSockets because the traffic is one-directional and
// EventSource gives browsers automatic reconnection with Last-Event-ID for free —
// which, combined with the byte offsets and event IDs below, means a reconnecting
// client resumes exactly where it stopped without any server-side session state.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer, which would defeat streaming.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, true
}

// send writes one event. Multi-line payloads are split across `data:` lines as
// the SSE format requires.
func (s *sseWriter) send(event, id string, payload any) error {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")

	if _, err := s.w.Write([]byte(b.String())); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// comment sends a no-op line, used as a heartbeat.
func (s *sseWriter) comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// handleStreamEvents streams a run's state transitions.
//
// The broker only signals that something changed; the authoritative data is read
// from the events table each time. That means a dropped notification is harmless
// and a slow client can never block a running job.
func (s *Server) handleStreamEvents(w http.ResponseWriter, r *http.Request) {
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

	sse, ok := newSSEWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming is not supported by this server")
		return
	}

	// Subscribe before the first read so nothing that happens in between is lost.
	notify, unsubscribe := s.sched.Broker().Subscribe(logs.RunTopic(id))
	defer unsubscribe()

	after := resumeFrom(r)
	ctx := r.Context()

	drain := func() (finished bool, err error) {
		for {
			events, err := s.db.ListEvents(ctx, id, after, 500)
			if err != nil {
				return false, err
			}
			if len(events) == 0 {
				return false, nil
			}
			for _, ev := range events {
				if err := sse.send("event", strconv.FormatInt(ev.ID, 10), ev); err != nil {
					return false, err
				}
				after = ev.ID
				if ev.Type == model.EventRunStatus && ev.To.Terminal() {
					finished = true
				}
			}
			if finished {
				return true, nil
			}
		}
	}

	// A run that already finished should still deliver its full timeline and then
	// close, rather than hanging open forever.
	if done, err := drain(); err != nil || done || run.Status.Terminal() {
		if err != nil {
			s.logger.Debug("event stream ended with an error", "run", id, "error", err)
		}
		_ = sse.send("done", "", map[string]any{"run_id": id})
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := sse.comment("keep-alive"); err != nil {
				return
			}
		case _, open := <-notify:
			done, err := drain()
			if err != nil {
				s.logger.Debug("event stream ended with an error", "run", id, "error", err)
				return
			}
			if done || !open {
				// `!open` means the run's topic closed: one final drain above has
				// already flushed whatever arrived alongside it.
				_ = sse.send("done", "", map[string]any{"run_id": id})
				return
			}
		}
	}
}

// handleStreamLogs tails one stream of a job's output.
//
// The client's position is a byte offset, carried in Last-Event-ID across
// reconnects, so resuming never duplicates or drops output.
func (s *Server) handleStreamLogs(w http.ResponseWriter, r *http.Request) {
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

	stream := model.Stream(r.URL.Query().Get("stream"))
	if stream != model.StreamStderr {
		stream = model.StreamStdout
	}

	records, err := s.db.ListLogs(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Tail the newest attempt of the requested stream.
	var path string
	for _, rec := range records {
		if rec.Stream == stream {
			path = rec.Path
		}
	}

	sse, ok := newSSEWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming is not supported by this server")
		return
	}
	if path == "" {
		// The job has not produced this stream yet; say so and let the client
		// reconnect rather than holding a connection open on nothing.
		_ = sse.send("done", "", map[string]any{"job_id": id, "reason": "no output yet"})
		return
	}

	notify, unsubscribe := s.sched.Broker().Subscribe(logTopicFor(id, stream))
	defer unsubscribe()

	offset := resumeFrom(r)
	ctx := r.Context()

	pump := func() error {
		for {
			data, next, err := logs.ReadFrom(path, offset, logs.DefaultChunkSize)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return nil
			}
			offset = next
			if err := sse.send("log", strconv.FormatInt(offset, 10), map[string]any{
				"stream":  stream,
				"offset":  offset,
				"content": string(data),
			}); err != nil {
				return err
			}
		}
	}

	if err := pump(); err != nil {
		s.logger.Debug("log stream ended with an error", "job", id, "error", err)
		return
	}
	// A finished job has nothing more to send.
	if job.Status.Terminal() {
		_ = sse.send("done", "", map[string]any{"job_id": id, "offset": offset})
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := sse.comment("keep-alive"); err != nil {
				return
			}
		case _, open := <-notify:
			if err := pump(); err != nil {
				s.logger.Debug("log stream ended with an error", "job", id, "error", err)
				return
			}
			if !open {
				// The writer closed the stream; everything has been flushed.
				_ = sse.send("done", "", map[string]any{"job_id": id, "offset": offset})
				return
			}
		}
	}
}

// resumeFrom reads the client's position from Last-Event-ID, falling back to the
// `after` or `offset` query parameters for clients that are not EventSource.
func resumeFrom(r *http.Request) int64 {
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	for _, key := range []string{"after", "offset"} {
		if raw := r.URL.Query().Get(key); raw != "" {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
				return n
			}
		}
	}
	return 0
}
