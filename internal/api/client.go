package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the Proton Drive API host, matching
	// rclone/go-proton-api's DefaultHostURL.
	DefaultBaseURL = "https://drive-api.proton.me"

	// AppVersion identifies pDrive to the Proton API.
	//
	// This value is mandatory and must be honest. From the Proton Drive SDK
	// integration rules: third-party clients must set x-pm-appversion in the
	// documented shape and must not masquerade as a first-party client.
	// Format: external-drive-{name}@{semver}[-{channel}][+{build}]
	//
	// Overridable via PDRIVE_APPVERSION for diagnosis: Proton gates some
	// endpoints on a minimum client version, and the rejection message
	// ("You are using an outdated version of the app") does not say which
	// part of the string it objects to.
	DefaultAppVersion = "external-drive-pdrive@0.1.0-alpha"

	// UserAgent deliberately identifies pDrive rather than imitating an
	// official Proton client.
	UserAgent = "pdrive/0.1.0 (Linux)"

	DefaultTimeout = 30 * time.Second
	MaxRetries     = 3
)

// AppVersion returns the value sent in x-pm-appversion.
func AppVersion() string {
	if v := os.Getenv("PDRIVE_APPVERSION"); v != "" {
		return v
	}
	return DefaultAppVersion
}

// Client is the Proton API client: authenticated requests, automatic token
// refresh, bounded retry.
type Client struct {
	httpClient *http.Client
	baseURL    string

	// refreshMu single-flights /auth/v4/refresh. Refresh tokens are
	// single-use and rotate, so two concurrent refreshes race and the loser
	// replays an already-spent token — which Proton reads as token reuse, a
	// session-compromise signal, and counts hard against the auth limit.
	refreshMu   sync.Mutex
	lastRefresh time.Time

	mu           sync.RWMutex
	uid          string
	accessToken  string
	refreshToken string
	loginEmail   string

	// OnTokenRefresh is called when tokens rotate, so the session can be
	// re-persisted. Proton rotates the refresh token on every use: losing a
	// rotation means losing the session.
	OnTokenRefresh func(uid, accessToken, refreshToken string)
}

// NewClient creates a client, optionally seeded with an existing session.
func NewClient(session *Session) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
			// Through the shared limiter: a cooldown that only some of the
			// traffic respects does not let the window close.
			Transport: SharedLimiter.Transport(&http.Transport{
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ForceAttemptHTTP2:     true,
			}),
		},
	}
	if session != nil {
		c.uid = session.UID
		c.accessToken = session.AccessToken
		c.refreshToken = session.RefreshToken
		c.loginEmail = session.LoginEmail
	}
	return c
}

// SetSession replaces the client's auth tokens.
func (c *Client) SetSession(uid, accessToken, refreshToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uid = uid
	c.accessToken = accessToken
	c.refreshToken = refreshToken
}

// GetSession snapshots the current tokens.
func (c *Client) GetSession() Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Session{
		UID:          c.uid,
		AccessToken:  c.accessToken,
		RefreshToken: c.refreshToken,
		LoginEmail:   c.loginEmail,
	}
}

// LoginEmail returns the email used to log in.
func (c *Client) LoginEmail() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loginEmail
}

// BaseURL returns the API base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// IsAuthenticated reports whether the client holds an access token.
func (c *Client) IsAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken != ""
}

// RequestError is an API-level error response.
type RequestError struct {
	HTTPStatus int
	Code       int
	Message    string
	// RetryAfter is how long Proton asked us to wait, when it said so.
	RetryAfter time.Duration
}

// IsRateLimit reports whether err is a rate-limit refusal, from Proton or
// from our own local cooldown.
func IsRateLimit(err error) bool {
	reqErr, ok := err.(*RequestError)
	if !ok {
		return false
	}
	return reqErr.HTTPStatus == 429 || reqErr.Code == 2028
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("API error %d (HTTP %d): %s", e.Code, e.HTTPStatus, e.Message)
}

// IsAuthError reports whether the session is permanently dead and the user
// must log in again. Transient failures (network, timeout, 5xx) return false.
func (e *RequestError) IsAuthError() bool {
	switch e.Code {
	case 10013: // refresh token invalid
		return true
	case 10002: // account deleted
		return true
	case 10003: // account disabled
		return true
	}
	return e.HTTPStatus == 401
}

// IsAuthError reports whether err is a permanent auth failure.
func IsAuthError(err error) bool {
	var reqErr *RequestError
	if ok := asRequestError(err, &reqErr); !ok {
		return false
	}
	return reqErr.IsAuthError()
}

func asRequestError(err error, target **RequestError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*RequestError); ok {
		*target = e
		return true
	}
	return false
}

// doRequest issues a request with auth headers, refreshing tokens on 401 and
// retrying transient failures.
func (c *Client) doRequest(ctx context.Context, method, path string, body, result interface{}) error {
	var lastErr error

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if attempt > 0 {
			// Jittered: components that failed together must not come back
			// together, or the retry itself arrives as a burst.
			wait := time.Duration(attempt) * time.Second
			wait += time.Duration(rand.Int63n(int64(wait / 2)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		err := c.doSingleRequest(ctx, method, path, body, result)
		if err == nil {
			return nil
		}
		lastErr = err

		reqErr, ok := err.(*RequestError)
		if !ok {
			continue // network error: retry
		}

		switch reqErr.HTTPStatus {
		case 401:
			if refreshErr := c.refreshTokens(ctx); refreshErr != nil {
				if IsAuthError(refreshErr) {
					return refreshErr
				}
				return fmt.Errorf("token refresh failed: %w (original: %w)", refreshErr, err)
			}
			continue
		case 429:
			// Never retried. A 429 means Proton has already told us to stop,
			// and each further request extends the limit rather than
			// shortening it — one call used to become four in six seconds.
			// The shared limiter has armed a cooldown; the caller decides
			// when to come back.
			return err

		case 503:
			continue
		default:
			return err
		}
	}

	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *Client) doSingleRequest(ctx context.Context, method, path string, body, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-pm-appversion", AppVersion())
	req.Header.Set("User-Agent", UserAgent)

	c.mu.RLock()
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.uid != "" {
		req.Header.Set("x-pm-uid", c.uid)
	}
	c.mu.RUnlock()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var retryAfter time.Duration
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter = ParseRetryAfter(resp.Header)
			if retryAfter == 0 {
				retryAfter = defaultCooldown
			}
		}
		var apiErr APIError
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Code != 0 {
			return &RequestError{
				HTTPStatus: resp.StatusCode, Code: apiErr.Code,
				Message: apiErr.Error, RetryAfter: retryAfter,
			}
		}
		return &RequestError{
			HTTPStatus: resp.StatusCode, Message: string(respBody),
			RetryAfter: retryAfter,
		}
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

// minRefreshInterval is the shortest gap between two refreshes. If a request
// gets a 401 within this window of a successful refresh, the token is as
// fresh as it can be and the session is genuinely dead — refreshing again
// cannot help and only spends another single-use token.
const minRefreshInterval = 10 * time.Second

// refreshTokens exchanges the refresh token for a new token pair.
func (c *Client) refreshTokens(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Another caller may have refreshed while we waited for the lock; if so
	// our 401 is already stale and there is nothing to do.
	c.mu.RLock()
	since := time.Since(c.lastRefresh)
	c.mu.RUnlock()
	if !c.lastRefresh.IsZero() && since < minRefreshInterval {
		return nil
	}

	c.mu.RLock()
	refreshToken := c.refreshToken
	uid := c.uid
	c.mu.RUnlock()

	if refreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	reqBody := RefreshRequest{
		ResponseType: "token",
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
		RedirectURI:  "http://protonmail.ch",
	}

	var result RefreshResponse
	if err := c.doSingleRequest(ctx, http.MethodPost, "/auth/v4/refresh", reqBody, &result); err != nil {
		return err
	}

	c.mu.Lock()
	c.accessToken = result.AccessToken
	c.refreshToken = result.RefreshToken
	c.lastRefresh = time.Now()
	c.mu.Unlock()

	if c.OnTokenRefresh != nil {
		c.OnTokenRefresh(uid, result.AccessToken, result.RefreshToken)
	}
	return nil
}

// GetUser returns the account, including quota and the key list.
func (c *Client) GetUser(ctx context.Context) (*User, error) {
	var res UserResponse
	if err := c.doRequest(ctx, http.MethodGet, "/core/v4/users", nil, &res); err != nil {
		return nil, err
	}
	return &res.User, nil
}

// GetSalts returns the per-key salts used to derive the key passphrase.
func (c *Client) GetSalts(ctx context.Context) (*SaltsResponse, error) {
	var res SaltsResponse
	if err := c.doRequest(ctx, http.MethodGet, "/core/v4/keys/salts", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
