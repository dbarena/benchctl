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

// apiBase returns the Management API root for project p: the API provisioning
// used, or for runs that didn't record it, the provisioning profile's API or
// one derived from the database hostname, so a non-production project is not
// silently queried against production. The mapping lives in the Supabase
// provider, which calls the same API to resize disks.
func (c *Collector) apiBase(p supabaseprovider.TargetProject) string {
	switch {
	case c.hostOverride != "":
		return c.hostOverride + "/v1/projects/" + p.Ref
	case c.apiURL != "":
		return c.apiURL + "/v1/projects/" + p.Ref
	}
	return supabaseprovider.ManagementAPIBase(c.profile, p.DBHost) + "/v1/projects/" + p.Ref
}

// get issues an authenticated GET against a Management API path relative to
// project p, returning the body and the URL that produced it.
func (c *Collector) get(ctx context.Context, p supabaseprovider.TargetProject, path string, params url.Values) (body []byte, requestURL string, err error) {
	requestURL = c.apiBase(p) + path
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
		return nil, requestURL, fmt.Errorf("HTTP %d: %s", resp.StatusCode, supabaseprovider.APIErrorMessage(body, resp.Status))
	}
	return body, requestURL, nil
}
