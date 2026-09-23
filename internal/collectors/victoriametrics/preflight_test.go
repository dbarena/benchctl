package victoriametrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/config"
)

// newTestServer returns an httptest.Server that records the Authorization
// header of the last request it received and always responds with status.
func newTestServer(status int, gotAuth *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
}

func TestPreflight_StatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantErr   bool
		errSubstr string
	}{
		{name: "200 OK passes", status: http.StatusOK, wantErr: false},
		{name: "204 No Content passes", status: http.StatusNoContent, wantErr: false},
		{name: "400 Bad Request passes (proves auth+parser reached)", status: http.StatusBadRequest, wantErr: false},
		{name: "401 Unauthorized rejected", status: http.StatusUnauthorized, wantErr: true, errSubstr: "credentials rejected"},
		{name: "403 Forbidden rejected", status: http.StatusForbidden, wantErr: true, errSubstr: "credentials rejected"},
		{name: "404 Not Found", status: http.StatusNotFound, wantErr: true, errSubstr: "import endpoint not found"},
		{name: "500 unhealthy", status: http.StatusInternalServerError, wantErr: true, errSubstr: "unhealthy"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := newTestServer(tc.status, &gotAuth)
			defer srv.Close()

			cfg := &config.Config{Metrics: config.MetricsConfig{Endpoint: srv.URL, Token: "test-token"}}
			c := New(cfg)
			err := c.Preflight(nil)

			if tc.wantErr && err == nil {
				t.Fatalf("Preflight() = nil, want error containing %q", tc.errSubstr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Preflight() = %v, want nil", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("Preflight() error = %q, want substring %q", err.Error(), tc.errSubstr)
			}
			if gotAuth != "Bearer test-token" {
				t.Errorf("Authorization header = %q, want %q; the probe must carry the same auth as a real push", gotAuth, "Bearer test-token")
			}
		})
	}
}

func TestPreflight_BasicAuthCarriedOnProbe(t *testing.T) {
	var gotAuth string
	srv := newTestServer(http.StatusOK, &gotAuth)
	defer srv.Close()

	cfg := &config.Config{Metrics: config.MetricsConfig{Endpoint: srv.URL, Username: "user", Password: "pass"}}
	c := New(cfg)
	if err := c.Preflight(nil); err != nil {
		t.Fatalf("Preflight() = %v, want nil", err)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("Authorization header = %q, want Basic auth", gotAuth)
	}
}

func TestPreflight_EmptyEndpoint(t *testing.T) {
	cfg := &config.Config{}
	c := New(cfg)
	err := c.Preflight(nil)
	if err == nil {
		t.Fatal("Preflight() = nil, want error")
	}
	if !strings.Contains(err.Error(), "metrics.endpoint") || !strings.Contains(err.Error(), "BENCHCTL_METRICS_ENDPOINT") {
		t.Errorf("Preflight() error = %q, want it to name metrics.endpoint and BENCHCTL_METRICS_ENDPOINT", err.Error())
	}
}

func TestPreflight_SchemelessEndpoint(t *testing.T) {
	cfg := &config.Config{Metrics: config.MetricsConfig{Endpoint: "example.com:8428"}}
	c := New(cfg)
	err := c.Preflight(nil)
	if err == nil {
		t.Fatal("Preflight() = nil, want error")
	}
	if !strings.Contains(err.Error(), "example.com:8428") {
		t.Errorf("Preflight() error = %q, want it to quote the invalid endpoint value", err.Error())
	}
}

func TestPreflight_PasswordWithoutUsername(t *testing.T) {
	cfg := &config.Config{Metrics: config.MetricsConfig{Endpoint: "http://localhost:8428", Password: "pass"}}
	c := New(cfg)
	err := c.Preflight(nil)
	if err == nil {
		t.Fatal("Preflight() = nil, want error")
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("Preflight() error = %q, want it to mention the missing username", err.Error())
	}
}
