package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dbarena/benchctl/internal/consolelog"
)

const (
	// defaultRateLimitWait is the pause after a 429 that carries no
	// X-RateLimit-Reset: the API blocks a caller for 60 s.
	defaultRateLimitWait = 61 * time.Second

	// skipThrottlingHeader carries a token the Management API is configured
	// (THROTTLING_SKIP_TOKENS) to exempt from throttling.
	skipThrottlingHeader = "x-supabase-skip-throttling"

	// requestTimeout bounds one request, so a stalled connection fails its
	// project instead of hanging the run. GET /v1/projects isn't paginated, so
	// it allows for a large list.
	requestTimeout = 2 * time.Minute
)

// errNotSent marks a throttled call that ended before its request was sent.
var errNotSent = errors.New("request not sent")

// mgmtClient calls the Management API with a Bearer token.
type mgmtClient struct {
	out       io.Writer
	doer      httpDoer
	base      string
	token     string
	skipToken string
	// block holds throttled calls back after a 429. Copies of a client share it.
	block *block
}

// newClient returns a client for the Management API at base.
func (p *Provider) newClient(base, skipToken string) (*mgmtClient, error) {
	var token string
	if p.cfg != nil {
		token = p.cfg.Supabase.AccessToken
	}
	if token == "" {
		return nil, errors.New("no Supabase access token; set BENCHCTL_SUPABASE_ACCESS_TOKEN or SUPABASE_ACCESS_TOKEN")
	}
	return &mgmtClient{
		out: p.out, doer: p.doer, base: base, token: token, skipToken: skipToken,
		block: &block{},
	}, nil
}

// APIError is a Management API response with a non-2xx status.
type APIError struct {
	Status  int
	Message string
	// retryAfter is how long a 429 says the block lasts.
	retryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	if e.Status == http.StatusUnauthorized {
		msg += "\nTip: set BENCHCTL_SUPABASE_ACCESS_TOKEN"
	}
	return msg
}

// hasStatus reports whether err is an APIError with the given status.
func hasStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// APIErrorMessage pulls the readable part out of a Management API error body,
// falling back to fallback.
func APIErrorMessage(body []byte, fallback string) string {
	var parsed struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Message != "" {
			return parsed.Message
		}
		if parsed.Error != "" {
			return parsed.Error
		}
	}
	return fallback
}

// do sends body, if not nil, as JSON to path and returns the response body. A
// non-2xx response is an *APIError.
func (c *mgmtClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.skipToken != "" {
		req.Header.Set(skipThrottlingHeader, c.skipToken)
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		fallback := strings.TrimSpace(truncate(data, 500))
		if fallback == "" {
			fallback = http.StatusText(resp.StatusCode)
		}
		return data, &APIError{
			Status:     resp.StatusCode,
			Message:    APIErrorMessage(data, fallback),
			retryAfter: rateLimitReset(resp.Header),
		}
	}
	return data, nil
}

// doThrottled is do for a call the API throttles. There is no pacing: calls
// go out as fast as the caller makes them, and on a 429 every throttled call
// is held back until the block lifts, then retried, for as long as ctx lasts.
// The throttler rejects a request before its handler runs, so a retried create
// can't make a second project. If ctx ends while the call waits for a block to
// lift, the error wraps errNotSent: no attempt reached the API.
func (c *mgmtClient) doThrottled(ctx context.Context, method, path string, body any) ([]byte, error) {
	for {
		if err := c.block.wait(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", errNotSent, err)
		}
		out, err := c.do(ctx, method, path, body)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			return out, err
		}
		if c.block.hold(apiErr.retryAfter) {
			consolelog.Println(c.out, fmt.Sprintf("supabase: rate limited by the Management API, pausing for %s", apiErr.retryAfter))
		}
	}
}

// rateLimitReset reads X-RateLimit-Reset, the seconds until a throttling block
// lifts.
func rateLimitReset(h http.Header) time.Duration {
	secs, err := strconv.ParseFloat(h.Get("X-RateLimit-Reset"), 64)
	if err != nil || secs < 0 {
		return defaultRateLimitWait
	}
	return time.Duration(secs * float64(time.Second))
}

// block holds throttled calls back until a time set by hold.
type block struct {
	mu    sync.Mutex
	until time.Time
}

// wait returns once no hold is in force, or when ctx ends.
func (b *block) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		d := time.Until(b.until)
		b.mu.Unlock()
		if d <= 0 {
			return nil
		}
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
}

// hold keeps throttled calls back for d, and reports whether that extended an
// existing hold.
func (b *block) hold(d time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until := time.Now().Add(d)
	if !until.After(b.until) {
		return false
	}
	b.until = until
	return true
}

// sleep waits for d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
