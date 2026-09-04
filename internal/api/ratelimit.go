package api

import (
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Rate-limit cooldown bounds.
const (
	// defaultCooldown applies when a 429 carries no usable Retry-After.
	defaultCooldown = 2 * time.Minute
	// maxCooldown caps how long a single response can park us, so a bad
	// header cannot take the client out for hours.
	maxCooldown = 30 * time.Minute
)

// Limiter parks every outgoing request after Proton answers 429.
//
// Proton's gateway limits on source IP and account, and vpn-api, account,
// Pass and Drive all sit behind it — so one component's retry loop can lock
// the user out of everything, including their password manager in the
// browser. Worse, requests received *while* limited extend the window, so a
// client that keeps trying sustains its own ban indefinitely.
//
// The cooldown is therefore enforced locally: while it is in effect nothing
// leaves the machine at all. That is the only way the window can close.
//
// One Limiter is shared by every path that talks to Proton — our own client
// and the vendored bridge both route through its Transport — because a
// cooldown that only half the traffic respects is not a cooldown.
type Limiter struct {
	mu    sync.RWMutex
	until time.Time
}

// SharedLimiter is the process-wide limiter. One process serves one account,
// and Proton limits per account, so a single instance is the correct scope.
var SharedLimiter = &Limiter{}

// LimitedFor reports how long requests will be refused, or 0 if not limited.
func (l *Limiter) LimitedFor() time.Duration {
	l.mu.RLock()
	until := l.until
	l.mu.RUnlock()
	if d := time.Until(until); d > 0 {
		return d
	}
	return 0
}

// Limited reports whether a cooldown is in effect.
func (l *Limiter) Limited() bool { return l.LimitedFor() > 0 }

// enterCooldown parks requests for d, clamped, unless a longer cooldown is
// already running.
func (l *Limiter) enterCooldown(d time.Duration) {
	if d <= 0 {
		d = defaultCooldown
	}
	if d > maxCooldown {
		d = maxCooldown
	}
	until := time.Now().Add(d)

	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.until) {
		l.until = until
	}
}

// Clear ends the cooldown. For tests, and for an explicit user retry.
func (l *Limiter) Clear() {
	l.mu.Lock()
	l.until = time.Time{}
	l.mu.Unlock()
}

// Transport wraps base so every request through it observes the cooldown.
func (l *Limiter) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &limitedTransport{base: base, limiter: l}
}

type limitedTransport struct {
	base    http.RoundTripper
	limiter *Limiter
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if remaining := t.limiter.LimitedFor(); remaining > 0 {
		// Answered locally: no packet leaves the machine. Shaped as a real
		// 429 with Retry-After so every caller, including the vendored
		// bridge's own retry logic, backs off exactly as it would for a
		// genuine one.
		return &http.Response{
			Status:     "429 Too Many Requests",
			StatusCode: http.StatusTooManyRequests,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{
				"Retry-After":  []string{strconv.Itoa(int(remaining.Seconds()) + 1)},
				"Content-Type": []string{"application/json"},
			},
			Body:          newErrorBody(remaining),
			ContentLength: -1,
			Request:       req,
		}, nil
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		// Arm before returning, so every other caller stops immediately
		// rather than each discovering the limit for itself.
		t.limiter.enterCooldown(ParseRetryAfter(resp.Header))
	}
	return resp, nil
}

// ParseRetryAfter reads a Retry-After header in either supported form,
// returning 0 when absent or unparseable.
func ParseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// newErrorBody builds the JSON body for a locally-generated 429, in Proton's
// error shape so callers parse it the same way.
func newErrorBody(remaining time.Duration) *bodyReader {
	msg := `{"Code":2028,"Error":"Rate limited locally; retry in ` +
		remaining.Round(time.Second).String() + `"}`
	return &bodyReader{s: msg}
}

type bodyReader struct {
	s string
	i int
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.i >= len(b.s) {
		return 0, io.EOF
	}
	n := copy(p, b.s[b.i:])
	b.i += n
	return n, nil
}

func (b *bodyReader) Close() error { return nil }
