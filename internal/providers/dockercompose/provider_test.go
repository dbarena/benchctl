package dockercompose

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// composeFile writes a minimal Docker Compose YAML with a single service and
// port mapping to a temp directory and returns the file path.
func composeFile(t *testing.T, serviceName, port string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	content := "services:\n  " + serviceName + ":\n    ports:\n      - \"" + port + ":" + port + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("composeFile: %v", err)
	}
	return path
}

// ---- findService tests ----

func TestFindService_Found(t *testing.T) {
	cfg := map[string]any{
		"services": []any{
			map[string]any{"name": "db", "definition": "./docker-compose.yml"},
			map[string]any{"name": "other", "definition": "./other.yml"},
		},
	}
	svc, err := findService(cfg, "db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc.Name != "db" {
		t.Errorf("svc.Name = %q, want db", svc.Name)
	}
}

func TestFindService_NotFound(t *testing.T) {
	cfg := map[string]any{
		"services": []any{
			map[string]any{"name": "db", "definition": "./docker-compose.yml"},
		},
	}
	_, err := findService(cfg, "missing")
	if err == nil {
		t.Fatal("expected error for missing service, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q should mention the missing service name", err)
	}
}

// ---- containerPort tests ----

func TestContainerPort_Single(t *testing.T) {
	path := composeFile(t, "db", "5432")
	port, err := containerPort(path, "db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port != "5432" {
		t.Errorf("port = %q, want 5432", port)
	}
}

func TestContainerPort_IPPrefixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	content := "services:\n  db:\n    ports:\n      - \"127.0.0.1:5432:5433\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	port, err := containerPort(path, "db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port != "5433" {
		t.Errorf("port = %q, want 5433", port)
	}
}

func TestContainerPort_NoService(t *testing.T) {
	path := composeFile(t, "db", "5432")
	_, err := containerPort(path, "missing")
	if err == nil {
		t.Fatal("expected error for missing service")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q should mention the service name", err)
	}
}

func TestContainerPort_NoPorts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	content := "services:\n  db:\n    image: postgres\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := containerPort(path, "db")
	if err == nil {
		t.Fatal("expected error for service with no ports")
	}
	if !strings.Contains(err.Error(), "no ports") {
		t.Errorf("error %q should mention no ports", err)
	}
}

// ---- parseHostPort tests ----

func TestParseHostPort_AllInterfaces(t *testing.T) {
	h, p, err := parseHostPort("0.0.0.0:54321")
	if err != nil {
		t.Fatal(err)
	}
	if h != "localhost" {
		t.Errorf("host = %q, want localhost", h)
	}
	if p != "54321" {
		t.Errorf("port = %q, want 54321", p)
	}
}

func TestParseHostPort_ExplicitHost(t *testing.T) {
	h, p, err := parseHostPort("127.0.0.1:5432")
	if err != nil {
		t.Fatal(err)
	}
	if h != "127.0.0.1" {
		t.Errorf("host = %q", h)
	}
	if p != "5432" {
		t.Errorf("port = %q", p)
	}
}

func TestParseHostPort_Empty(t *testing.T) {
	_, _, err := parseHostPort("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseHostPort_Malformed(t *testing.T) {
	_, _, err := parseHostPort("not-a-host-port")
	if err == nil {
		t.Fatal("expected error for malformed input")
	}
}

// ---- Provision tests ----

func TestProvision_ValidConfig(t *testing.T) {
	p := newWithRunner(func(_ context.Context, _ []string, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return nil, nil
	})
	outputs, err := p.Provision(context.Background(), "", map[string]any{
		"services": []any{map[string]any{
			"name":       "db",
			"definition": "./docker-compose.yml",
		}},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(outputs) != 0 {
		t.Errorf("Provision should return empty outputs, got %v", outputs)
	}
}

func TestProvision_InvalidServices(t *testing.T) {
	p := newWithRunner(func(_ context.Context, _ []string, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return nil, nil
	})
	_, err := p.Provision(context.Background(), "", map[string]any{
		"services": []any{map[string]any{"name": "db"}}, // missing definition
	})
	if err == nil {
		t.Fatal("expected error for invalid service definition, got nil")
	}
}

// ---- StartBenchmark tests ----

func cfg1Service(t *testing.T) map[string]any {
	t.Helper()
	composePath := composeFile(t, "db", "5432")
	return map[string]any{
		"services": []any{map[string]any{
			"name":       "db",
			"definition": composePath,
			"vars": map[string]any{
				"user":     "bench",
				"password": "bench",
				"db":       "tpcc",
			},
		}},
	}
}

func TestStartBenchmark_Success(t *testing.T) {
	var calls []string
	mock := func(_ context.Context, _ []string, _ io.Writer, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if strings.Contains(call, "port") {
			return []byte("0.0.0.0:54321\n"), nil
		}
		return []byte{}, nil
	}

	p := newWithRunner(mock)
	outputs, err := p.StartBenchmark(context.Background(), cfg1Service(t), "db")
	if err != nil {
		t.Fatalf("StartBenchmark: %v", err)
	}
	if outputs["service.db.host"] != "localhost" {
		t.Errorf("host = %q, want localhost", outputs["service.db.host"])
	}
	if outputs["service.db.port"] != "54321" {
		t.Errorf("port = %q, want 54321", outputs["service.db.port"])
	}
	if outputs["service.db.user"] != "bench" {
		t.Errorf("user = %q, want bench", outputs["service.db.user"])
	}

	if len(calls) != 4 {
		t.Fatalf("expected 4 docker commands, got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "pull") {
		t.Errorf("first call should be pull, got %q", calls[0])
	}
	if !strings.Contains(calls[1], "config --images") {
		t.Errorf("second call should resolve the image for digest capture, got %q", calls[1])
	}
	if !strings.Contains(calls[2], "up -d") {
		t.Errorf("third call should be up -d, got %q", calls[2])
	}
	if !strings.Contains(calls[3], "port") {
		t.Errorf("fourth call should be port, got %q", calls[3])
	}
}

func TestStartBenchmark_CapturesImageDigest(t *testing.T) {
	mock := func(_ context.Context, _ []string, _ io.Writer, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.Contains(call, "port"):
			return []byte("0.0.0.0:54321\n"), nil
		case strings.Contains(call, "config --images"):
			return []byte("example.org/db:nightly\n"), nil
		case strings.Contains(call, "inspect"):
			return []byte("example.org/db@sha256:deadbeef\n"), nil
		default:
			return []byte{}, nil
		}
	}

	p := newWithRunner(mock)
	outputs, err := p.StartBenchmark(context.Background(), cfg1Service(t), "db")
	if err != nil {
		t.Fatalf("StartBenchmark: %v", err)
	}
	if got, want := outputs["service.db.image_digest"], "example.org/db@sha256:deadbeef"; got != want {
		t.Errorf("image_digest = %q, want %q", got, want)
	}
}

func TestStartBenchmark_ImageDigestUnresolvableIsNonFatal(t *testing.T) {
	mock := func(_ context.Context, _ []string, _ io.Writer, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		if strings.Contains(call, "port") {
			return []byte("0.0.0.0:54321\n"), nil
		}
		// config --images and inspect both return empty output, as they would
		// for a locally-built image with no registry digest.
		return []byte{}, nil
	}

	p := newWithRunner(mock)
	outputs, err := p.StartBenchmark(context.Background(), cfg1Service(t), "db")
	if err != nil {
		t.Fatalf("StartBenchmark: %v", err)
	}
	if _, ok := outputs["service.db.image_digest"]; ok {
		t.Errorf("image_digest should be absent when it cannot be resolved, got %q", outputs["service.db.image_digest"])
	}
}

func TestStartBenchmark_UnknownService(t *testing.T) {
	p := newWithRunner(func(_ context.Context, _ []string, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return nil, nil
	})
	_, err := p.StartBenchmark(context.Background(), cfg1Service(t), "missing")
	if err == nil {
		t.Fatal("expected error for unknown service, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q should mention the service name", err)
	}
}

func TestStartBenchmark_PullFails(t *testing.T) {
	mock := func(_ context.Context, _ []string, _ io.Writer, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "pull") {
			return []byte("pull error"), errors.New("exit 1")
		}
		return []byte{}, nil
	}
	p := newWithRunner(mock)
	_, err := p.StartBenchmark(context.Background(), cfg1Service(t), "db")
	if err == nil || !strings.Contains(err.Error(), "docker compose pull") {
		t.Errorf("expected 'docker compose pull' error, got %v", err)
	}
}

func TestStartBenchmark_UpFails(t *testing.T) {
	var calls []string
	mock := func(_ context.Context, _ []string, _ io.Writer, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if strings.Contains(call, "up") {
			return []byte("up error"), errors.New("exit 1")
		}
		return []byte{}, nil
	}
	p := newWithRunner(mock)
	_, err := p.StartBenchmark(context.Background(), cfg1Service(t), "db")
	if err == nil || !strings.Contains(err.Error(), "docker compose up") {
		t.Errorf("expected 'docker compose up' error, got %v", err)
	}
	// "up --wait" can start containers before failing its own health check;
	// StartBenchmark returns no outputs on failure, so nothing else would
	// ever stop them; verify the inline cleanup ran.
	if !containsCall(calls, "down --volumes") {
		t.Errorf("expected a 'down --volumes' cleanup call after up failure, got calls: %v", calls)
	}
}

func TestStartBenchmark_PortFails(t *testing.T) {
	var calls []string
	mock := func(_ context.Context, _ []string, _ io.Writer, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if strings.Contains(call, "port") {
			return nil, errors.New("no port mapping")
		}
		return []byte{}, nil
	}
	p := newWithRunner(mock)
	_, err := p.StartBenchmark(context.Background(), cfg1Service(t), "db")
	if err == nil || !strings.Contains(err.Error(), "docker compose port") {
		t.Errorf("expected 'docker compose port' error, got %v", err)
	}
	// "up --wait" already succeeded by the time "port" fails, so the
	// container is confirmed running; verify the inline cleanup ran.
	if !containsCall(calls, "down --volumes") {
		t.Errorf("expected a 'down --volumes' cleanup call after port failure, got calls: %v", calls)
	}
}

// containsCall reports whether any recorded call contains substr.
func containsCall(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// ---- StopBenchmark tests ----

func cfg1ServiceStop() map[string]any {
	return map[string]any{
		"services": []any{map[string]any{
			"name":       "db",
			"definition": "./docker-compose.yml",
		}},
	}
}

func TestStopBenchmark_Success(t *testing.T) {
	var downCalled bool
	mock := func(_ context.Context, _ []string, _ io.Writer, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "down") {
			downCalled = true
		}
		return []byte{}, nil
	}
	p := newWithRunner(mock)
	if err := p.StopBenchmark(context.Background(), cfg1ServiceStop(), "db"); err != nil {
		t.Fatalf("StopBenchmark: %v", err)
	}
	if !downCalled {
		t.Error("docker compose down was not called")
	}
}

func TestStopBenchmark_UnknownService(t *testing.T) {
	p := newWithRunner(func(_ context.Context, _ []string, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return nil, nil
	})
	err := p.StopBenchmark(context.Background(), cfg1ServiceStop(), "missing")
	if err == nil {
		t.Fatal("expected error for unknown service, got nil")
	}
}

// ---- Teardown tests ----

func TestTeardown_IsNoop(t *testing.T) {
	var called bool
	p := newWithRunner(func(_ context.Context, _ []string, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := p.Teardown(context.Background(), nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if called {
		t.Error("Teardown should be a no-op and not invoke any docker commands")
	}
}
