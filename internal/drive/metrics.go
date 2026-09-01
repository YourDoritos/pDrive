package drive

import (
	"sync"
	"time"
)

// Metrics counts API work and the wall time spent in it.
//
// This exists because the Phase 3 gate has a hard budget: a directory listing
// may be held for at most `max_block_ms` (400 by default). Knowing which calls
// cost what — and how many of them a single change triggers — is the
// difference between designing that budget and guessing at it.
type Metrics struct {
	mu sync.Mutex

	OpenCalls int
	OpenTime  time.Duration

	EventPolls int
	EventTime  time.Duration

	ListCalls int
	ListTime  time.Duration

	AttrCalls int
	AttrTime  time.Duration

	DownloadCalls int
	DownloadTime  time.Duration
	DownloadBytes int64
}

func (m *Metrics) add(calls *int, total *time.Duration, started time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	*calls++
	*total += time.Since(started)
}

// Snapshot returns a copy safe to read without the lock.
func (m *Metrics) Snapshot() Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Metrics{
		OpenCalls: m.OpenCalls, OpenTime: m.OpenTime,
		EventPolls: m.EventPolls, EventTime: m.EventTime,
		ListCalls: m.ListCalls, ListTime: m.ListTime,
		AttrCalls: m.AttrCalls, AttrTime: m.AttrTime,
		DownloadCalls: m.DownloadCalls, DownloadTime: m.DownloadTime,
		DownloadBytes: m.DownloadBytes,
	}
}

// Metrics returns this Drive's API metrics.
func (d *Drive) Metrics() *Metrics { return d.metrics }
