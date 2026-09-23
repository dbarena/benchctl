package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/dbarena/benchctl/internal/auth"
	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/schema"
)

// ---- parseSetFlags ----

func TestParseSetFlags_Valid(t *testing.T) {
	m, err := parseSetFlags([]string{"warehouses=50", "threads=8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["warehouses"] != "50" {
		t.Errorf("warehouses = %q, want 50", m["warehouses"])
	}
	if m["threads"] != "8" {
		t.Errorf("threads = %q, want 8", m["threads"])
	}
}

func TestParseSetFlags_Empty(t *testing.T) {
	m, err := parseSetFlags(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestParseSetFlags_ValueContainsEquals(t *testing.T) {
	// Values may contain '=' (e.g. a URL or base64 string).
	m, err := parseSetFlags([]string{"endpoint=http://host:8080/path=foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["endpoint"] != "http://host:8080/path=foo" {
		t.Errorf("endpoint = %q", m["endpoint"])
	}
}

func TestParseSetFlags_MissingEquals(t *testing.T) {
	_, err := parseSetFlags([]string{"noequals"})
	if err == nil {
		t.Fatal("expected error for missing '='")
	}
}

// ---- resolveCreatedByFrom ----

// fakeJWT builds a "header.payload.signature" string with the given claims
// JSON-encoded into the payload segment. jwtSub/jwtEmail don't verify the
// signature, so the header and signature segments are dummy placeholders.
func fakeJWT(t *testing.T, sub, email string) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		Sub   string `json:"sub,omitempty"`
		Email string `json:"email,omitempty"`
	}{Sub: sub, Email: email})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestResolveCreatedByFrom_ValidJWTWinsOverUsername(t *testing.T) {
	creds := &auth.Credentials{
		AccessToken: fakeJWT(t, "11111111-1111-1111-1111-111111111111", "dev@example.com"),
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	createdBy, createdByEmail := resolveCreatedByFrom("ci/nightly", creds)
	if createdBy != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("createdBy = %q, want JWT sub", createdBy)
	}
	if createdByEmail != "dev@example.com" {
		t.Errorf("createdByEmail = %q, want JWT email", createdByEmail)
	}
}

func TestResolveCreatedByFrom_NoCredsFallsBackToUsername(t *testing.T) {
	createdBy, createdByEmail := resolveCreatedByFrom("ci/nightly", nil)
	if createdBy != "ci/nightly" {
		t.Errorf("createdBy = %q, want %q", createdBy, "ci/nightly")
	}
	if createdByEmail != "" {
		t.Errorf("createdByEmail = %q, want empty", createdByEmail)
	}
}

func TestResolveCreatedByFrom_ExpiredCredsFallsBackToUsername(t *testing.T) {
	creds := &auth.Credentials{
		AccessToken: fakeJWT(t, "11111111-1111-1111-1111-111111111111", "dev@example.com"),
		ExpiresAt:   time.Now().Add(-time.Hour),
	}
	createdBy, createdByEmail := resolveCreatedByFrom("ci/nightly", creds)
	if createdBy != "ci/nightly" {
		t.Errorf("createdBy = %q, want %q", createdBy, "ci/nightly")
	}
	if createdByEmail != "" {
		t.Errorf("createdByEmail = %q, want empty", createdByEmail)
	}
}

func TestResolveCreatedByFrom_NoCredsNoUsername(t *testing.T) {
	createdBy, createdByEmail := resolveCreatedByFrom("", nil)
	if createdBy != "" || createdByEmail != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\")", createdBy, createdByEmail)
	}
}

func TestResolveCreatedByFrom_ValidCredsNoSubFallsBackToUsername(t *testing.T) {
	creds := &auth.Credentials{
		AccessToken: fakeJWT(t, "", "dev@example.com"),
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	createdBy, createdByEmail := resolveCreatedByFrom("ci/nightly", creds)
	if createdBy != "ci/nightly" {
		t.Errorf("createdBy = %q, want %q", createdBy, "ci/nightly")
	}
	if createdByEmail != "" {
		t.Errorf("createdByEmail = %q, want empty", createdByEmail)
	}
}

// ---- buildRunner ----

func TestBuildRunner_LocalScenario(t *testing.T) {
	s, err := schema.Load("../../scenarios/postgres-tpcc-local.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inputs, err := s.ResolveInputs(nil)
	if err != nil {
		t.Fatalf("ResolveInputs: %v", err)
	}
	r, err := buildRunner(&config.Config{}, s, inputs)
	if err != nil {
		t.Fatalf("buildRunner: %v", err)
	}
	if r.Target == nil {
		t.Error("Target is nil")
	}
	if r.Driver == nil {
		t.Error("Driver is nil")
	}
	if len(r.Workloads) == 0 {
		t.Error("Workloads is empty")
	}
	if r.Collector == nil {
		t.Error("Collector is nil")
	}
}

func TestBuildRunner_UnsupportedTargetProvider(t *testing.T) {
	s := &schema.Scenario{
		Target:    schema.Target{Provider: "kubernetes"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	_, err := buildRunner(&config.Config{}, s, schema.ResolvedInputs{})
	if err == nil {
		t.Fatal("expected error for unsupported target provider")
	}
}

func TestBuildRunner_UnsupportedDriverProvider(t *testing.T) {
	s := &schema.Scenario{
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "unknown-driver"},
		Collector: schema.Collector{Provider: "stdout"},
	}
	_, err := buildRunner(&config.Config{}, s, schema.ResolvedInputs{})
	if err == nil {
		t.Fatal("expected error for unsupported driver provider")
	}
}

func TestBuildRunner_UnsupportedCollector(t *testing.T) {
	s := &schema.Scenario{
		Target:    schema.Target{Provider: "docker-compose"},
		Driver:    schema.Driver{Provider: "local"},
		Collector: schema.Collector{Provider: "unsupported-collector"},
	}
	_, err := buildRunner(&config.Config{}, s, schema.ResolvedInputs{})
	if err == nil {
		t.Fatal("expected error for unsupported collector")
	}
}

// ---- validate command ----

func TestValidateScenario_Valid(t *testing.T) {
	for _, f := range []string{
		"../../scenarios/postgres-tpcc-local.yaml",
		"../../scenarios/postgres-tpcc-ec2.yaml",
	} {
		if err := validateScenario(nil, []string{f}); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestValidateScenario_NonexistentFile(t *testing.T) {
	err := validateScenario(nil, []string{"nonexistent.yaml"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
