// Package supabase collects provider-side diagnostics for Supabase targets:
// the Postgres log, the project's configuration, and the `supabase inspect
// report` catalogue snapshot.
//
// Supabase offers markedly less than AWS or GCP, and the gap is structural
// rather than an omission here. There is no historical metric store: the
// project's Prometheus endpoint is an instantaneous scrape with no retention,
// so a time series cannot be recovered after a run the way Performance
// Insights or Cloud Monitoring allow. There is no wait-event sampler either,
// and none can be installed, because shared_preload_libraries is not settable
// on a hosted project. What is left is the log, the configuration, and
// whatever pg_stat_statements accumulated, which is what this collects.
package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	supabaseprovider "github.com/dbarena/benchctl/internal/providers/supabase"
)

// httpDoer is satisfied by *http.Client; injected so tests can serve captured
// API responses.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// apiBase returns the Management API root for this project, deriving the host
// from the database hostname so a staging project is not silently queried
// against production. The mapping lives in the Supabase provider, which
// derives the pooler host the same way.
func (c *Collector) apiBase() string {
	if c.hostOverride != "" {
		return c.hostOverride + "/v1/projects/" + c.projectRef
	}
	return "https://" + supabaseprovider.ManagementAPIHost(c.dbHost) + "/v1/projects/" + c.projectRef
}

// get issues an authenticated GET against a Management API path relative to
// the project, returning the body and the URL that produced it.
func (c *Collector) get(ctx context.Context, path string, params url.Values) (body []byte, requestURL string, err error) {
	requestURL = c.apiBase() + path
	if len(params) > 0 {
		requestURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, requestURL, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, requestURL, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, requestURL, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, requestURL, fmt.Errorf("HTTP %d: %s", resp.StatusCode, apiErrorMessage(body, resp.Status))
	}
	return body, requestURL, nil
}

// apiErrorMessage pulls the readable part out of a Management API error,
// falling back to the status line.
func apiErrorMessage(body []byte, fallback string) string {
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
