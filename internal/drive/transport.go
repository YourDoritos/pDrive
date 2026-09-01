package drive

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// tracingTransport logs every HTTP request pDrive makes.
//
// Enabled with PDRIVE_TRACE=1. The bridge's bootstrap is opaque from the
// outside — this is the only way to see exactly which calls it makes and what
// each one costs, which is what sizing the daemon's startup depends on.
type tracingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	n    int
	t0   time.Time
}

func newTracingTransport(base http.RoundTripper) http.RoundTripper {
	if os.Getenv("PDRIVE_TRACE") == "" {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &tracingTransport{base: base, t0: time.Now()}
}

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(started)

	t.mu.Lock()
	t.n++
	n := t.n
	sinceStart := time.Since(t.t0)
	t.mu.Unlock()

	status := "ERR"
	if resp != nil {
		status = fmt.Sprintf("%d", resp.StatusCode)
	}
	path := req.URL.Path
	if len(path) > 46 {
		path = "…" + path[len(path)-45:]
	}
	fmt.Fprintf(os.Stderr, "  [trace] #%-2d %+7s  %-3s %-4s %-46s  (t+%s)\n",
		n, elapsed.Round(time.Millisecond), status, req.Method, path,
		sinceStart.Round(time.Millisecond))
	_ = strings.TrimSpace
	return resp, err
}
