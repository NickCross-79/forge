// Package logs captures, stores and serves the output of pipeline jobs.
//
// It owns four concerns that belong together: writing a job's stdout and stderr
// to disk as separate streams, masking secrets before those bytes are written,
// reading stored output back at an offset so a client can resume where it left
// off, and notifying subscribers that new output exists.
//
// These are a job's logs, not forge's own; application logging lives in the
// observability package.
package logs

import (
	"fmt"
	"sync"

	"github.com/nickcross-79/forge/internal/model"
)

// Broker is a coalescing notification hub used to drive live streaming.
//
// It deliberately carries no payload. Subscribers are told only that a topic has
// new data and then read the authoritative source themselves — the log file at
// their current offset, or the events table after their last seen ID. That keeps
// a slow HTTP client from ever blocking a running job, and makes reconnection
// trivial: a client that missed notifications catches up on its next read.
type Broker struct {
	mu     sync.Mutex
	topics map[string]*topic
	// closed remembers recently finished topics so that a subscriber arriving
	// just after a job ended is handed an already-closed channel instead of a
	// fresh topic that nothing will ever notify. Without this there is a race
	// between "is the job still running?" and Subscribe, and losing it leaves an
	// SSE stream open on a source that has already stopped producing.
	closed map[string]struct{}
	// closedOrder bounds that memory: topic names are evicted oldest-first, so a
	// long-lived server does not accumulate one entry per stream forever.
	closedOrder []string
}

// maxRememberedClosed caps the closed-topic set. A run contributes roughly two
// topics per job plus one for itself, so this covers the recent past by a wide
// margin at a cost of a few tens of kilobytes.
const maxRememberedClosed = 4096

type topic struct {
	subs   map[int64]chan struct{}
	nextID int64
	closed bool
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{
		topics: make(map[string]*topic),
		closed: make(map[string]struct{}),
	}
}

// Subscribe registers interest in a topic. The returned channel receives a value
// whenever the topic is notified and is closed when the topic is. The cancel
// function must be called to release the subscription.
func (b *Broker) Subscribe(name string) (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, done := b.closed[name]; done {
		// The producer already finished. Hand back a closed channel so the caller
		// takes its normal "stream ended" path after one final read.
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}

	t, ok := b.topics[name]
	if !ok {
		t = &topic{subs: make(map[int64]chan struct{})}
		b.topics[name] = t
	}

	id := t.nextID
	t.nextID++
	// Buffer of one: notifications coalesce, since the subscriber re-reads
	// everything available when it wakes.
	ch := make(chan struct{}, 1)
	t.subs[id] = ch

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		tt, ok := b.topics[name]
		if !ok {
			return
		}
		if sub, ok := tt.subs[id]; ok {
			delete(tt.subs, id)
			close(sub)
		}
		if len(tt.subs) == 0 && tt.closed {
			delete(b.topics, name)
		}
	}
}

// Notify wakes every subscriber to a topic. It never blocks: a subscriber that
// has not drained its previous notification already knows there is work to do.
func (b *Broker) Notify(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[name]
	if !ok || t.closed {
		return
	}
	for _, ch := range t.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Close marks a topic finished and closes every subscriber channel, which tells
// streaming handlers to send a final read and hang up.
func (b *Broker) Close(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[name]
	if !ok || t.closed {
		// A topic nothing ever subscribed to still counts as finished, so a
		// late subscriber is not left waiting on it.
		b.rememberClosedLocked(name)
		return
	}
	t.closed = true
	for id, ch := range t.subs {
		delete(t.subs, id)
		close(ch)
	}
	delete(b.topics, name)
	b.rememberClosedLocked(name)
}

// rememberClosedLocked records a finished topic, evicting the oldest entry once
// the set is full. The caller must hold b.mu.
func (b *Broker) rememberClosedLocked(name string) {
	if _, exists := b.closed[name]; exists {
		return
	}
	b.closed[name] = struct{}{}
	b.closedOrder = append(b.closedOrder, name)
	if len(b.closedOrder) > maxRememberedClosed {
		oldest := b.closedOrder[0]
		b.closedOrder = b.closedOrder[1:]
		delete(b.closed, oldest)
	}
}

// Closed reports whether a topic has been closed. A topic that was never opened
// counts as closed, which is the right answer for a job that already finished.
func (b *Broker) Closed(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[name]
	return !ok || t.closed
}

// LogTopic is the broker topic for one stream of one job.
func LogTopic(jobID int64, stream model.Stream) string {
	return fmt.Sprintf("log:%d:%s", jobID, stream)
}

// RunTopic is the broker topic for a run's status and job transitions.
func RunTopic(runID int64) string {
	return fmt.Sprintf("run:%d", runID)
}

// GlobalTopic carries every run transition, used by the dashboard overview.
const GlobalTopic = "runs"
