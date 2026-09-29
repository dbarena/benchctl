// Package gcp collects provider-side diagnostics for Google Cloud targets:
// Cloud Monitoring time series, Cloud Logging Postgres logs, and Cloud SQL
// configuration.
//
// Unlike the AWS collector this talks REST rather than shelling out to
// gcloud, for two reasons. Cloud Monitoring has no gcloud command to read
// time series at all. And `gcloud sql` and `gcloud logging` authenticate with
// the gcloud account credential, while benchctl documents only application
// default credentials (README, "GCP async workflow"); going through REST with
// ADC keeps one credential for provisioning and collection alike.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// API hosts, overridable in tests.
const (
	defaultMonitoringHost = "https://monitoring.googleapis.com"
	defaultLoggingHost    = "https://logging.googleapis.com"
	defaultSQLAdminHost   = "https://sqladmin.googleapis.com"
)

// httpDoer is satisfied by *http.Client; injected so tests can serve captured
// API responses. Mirrors the seam in internal/providers/supabase.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// tokenSource returns an OAuth bearer token. Injected so tests need no
// credentials.
type tokenSource func(ctx context.Context) (string, error)

// cachedToken wraps a tokenSource so the credential is minted at most once.
//
// adcToken shells out to gcloud, which costs about 0.6s. A fetch makes one
// request per metric per window, 96 for a three-window run, so minting per
// request spent a minute of a 99-second fetch re-reading the same
// credential. Access tokens last an hour; a fetch takes under two minutes.
func cachedToken(src tokenSource) tokenSource {
	var (
		mu    sync.Mutex
		token string
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if token != "" {
			return token, nil
		}
		fresh, err := src(ctx)
		if err != nil {
			return "", err
		}
		token = fresh
		return token, nil
	}
}

// adcQuotaProject reports the quota project recorded in the application
// default credentials, or "" when there is none.
//
// This has to be sent explicitly. The quota_project_id field in the ADC file
// is honoured by Google's client libraries, which translate it into an
// x-goog-user-project header; a bearer token on a plain net/http request, as
// here, carries no such signal, and the API then bills the OAuth client's own
// project. For gcloud-issued credentials that is a Google-owned project
// shared by every gcloud user, where someone else's traffic counts against
// the same per-minute allowance.
func adcQuotaProject() string {
	path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if path == "" {
		dir := os.Getenv("CLOUDSDK_CONFIG")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			dir = filepath.Join(home, ".config", "gcloud")
		}
		path = filepath.Join(dir, "application_default_credentials.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var adc struct {
		QuotaProjectID string `json:"quota_project_id"`
	}
	if err := json.Unmarshal(data, &adc); err != nil {
		return ""
	}
	return adc.QuotaProjectID
}

// adcToken reads an application default credentials token via gcloud.
//
// The `application-default` form is deliberate: the plain `gcloud auth
// print-access-token` reads the account credential, which a machine set up
// per the README may not have.
func adcToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// get issues an authenticated GET and returns the body along with the URL, so
// callers can record what produced an artifact.
func (c *Collector) get(ctx context.Context, endpoint string, params url.Values) (body []byte, requestURL string, err error) {
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, endpoint, err
	}
	return c.do(req, endpoint)
}

// post issues an authenticated POST with a JSON body. Cloud Logging's
// entries.list is a POST because its filter and page token do not fit a query
// string comfortably.
func (c *Collector) post(ctx context.Context, endpoint string, payload any) (body []byte, requestURL string, err error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, endpoint, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return nil, endpoint, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, endpoint)
}

func (c *Collector) do(req *http.Request, requestURL string) ([]byte, string, error) {
	token, err := c.token(req.Context())
	if err != nil {
		return nil, requestURL, fmt.Errorf("application default credentials: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if project := c.quotaProject(); project != "" {
		req.Header.Set("x-goog-user-project", project)
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, requestURL, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, requestURL, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, requestURL, fmt.Errorf("HTTP %d: %s", resp.StatusCode, apiErrorMessage(body, resp.Status))
	}
	return body, requestURL, nil
}

// apiErrorMessage pulls the human-readable part out of a Google API error,
// falling back to the status line. The raw body is a wall of JSON that buries
// the one sentence worth reading.
func apiErrorMessage(body []byte, fallback string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error.Message == "" {
		return fallback
	}
	return parsed.Error.Message + quotaHint(parsed.Error.Message)
}

// quotaHint explains a read-quota rejection.
//
// One fetch is nowhere near any limit: 372 Cloud Monitoring queries at the
// widest (12 windows) against 6000/min, and a single Cloud Logging read
// against 60/min. But application default credentials with no quota project
// attribute reads to a shared Google-owned project, where everyone's
// traffic counts against the same allowance.
func quotaHint(message string) string {
	if !strings.Contains(message, "Quota exceeded") || !strings.Contains(message, "consumer") {
		return ""
	}
	return " (application default credentials may have no quota project; set one with `gcloud auth application-default set-quota-project <project>`)"
}
