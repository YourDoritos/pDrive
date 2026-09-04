package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The failure this guards against: one 429 became permanent, and the lockout
// was account-wide — Proton limits on IP and account, and Mail, Pass and
// Drive all sit behind the same gateway. Requests arriving during a cooldown
// extend it, so a client that keeps trying sustains its own ban.
func TestCooldownStopsTrafficLocally(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"Code":2028,"Error":"Too many requests"}`))
	}))
	defer srv.Close()

	limiter := &Limiter{}
	client := &http.Client{Transport: limiter.Transport(nil)}

	// First request reaches the server and arms the cooldown.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("first request hit the server %d times, want 1", hits.Load())
	}
	if !limiter.Limited() {
		t.Fatal("a 429 did not arm the cooldown")
	}

	// Everything after it is answered locally.
	for i := 0; i < 20; i++ {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("local answer was %d, want 429", resp.StatusCode)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Error("the local answer carries no Retry-After")
		}
		resp.Body.Close()
	}
	if hits.Load() != 1 {
		t.Errorf("%d requests reached the server during the cooldown; "+
			"each one extends the limit", hits.Load()-1)
	}

	// Clearing it lets traffic through again.
	limiter.Clear()
	resp, err = client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 2 {
		t.Errorf("after clearing, the request did not reach the server")
	}
}

func TestCooldownHonoursRetryAfter(t *testing.T) {
	l := &Limiter{}
	l.enterCooldown(45 * time.Second)

	got := l.LimitedFor()
	if got < 40*time.Second || got > 45*time.Second {
		t.Errorf("cooldown = %v, want about 45s", got)
	}

	// A shorter one must not shorten a longer cooldown already running.
	l.enterCooldown(time.Second)
	if l.LimitedFor() < 40*time.Second {
		t.Error("a short cooldown cut short a longer one")
	}
}

func TestCooldownIsClamped(t *testing.T) {
	l := &Limiter{}
	// A hostile or broken header must not take the client out for hours.
	l.enterCooldown(72 * time.Hour)
	if l.LimitedFor() > maxCooldown {
		t.Errorf("cooldown = %v, want at most %v", l.LimitedFor(), maxCooldown)
	}

	// No hint at all still parks us.
	l2 := &Limiter{}
	l2.enterCooldown(0)
	if l2.LimitedFor() <= 0 {
		t.Error("a 429 without Retry-After did not arm a cooldown")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"30", 30 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"", 0},
		{"not a number", 0},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := ParseRetryAfter(h); got != c.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", c.header, got, c.want)
		}
	}

	// HTTP-date form.
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
	if got := ParseRetryAfter(h); got < 80*time.Second || got > 90*time.Second {
		t.Errorf("HTTP-date Retry-After = %v, want about 90s", got)
	}
}

// A 429 must not be retried: one call used to become four requests in six
// seconds, each of them extending the limit.
func TestClientDoesNotRetryRateLimits(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"Code":2028,"Error":"Too many requests"}`))
	}))
	defer srv.Close()

	limiter := &Limiter{}
	c := &Client{
		baseURL:    srv.URL,
		httpClient: &http.Client{Transport: limiter.Transport(nil)},
	}

	err := c.doRequest(t.Context(), http.MethodGet, "/anything", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsRateLimit(err) {
		t.Errorf("error = %v, want a rate-limit error", err)
	}
	if hits.Load() != 1 {
		t.Errorf("a single call made %d requests; 429 must never be retried", hits.Load())
	}
}
