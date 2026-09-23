package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderComposeTemplate(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "docker-compose.tmpl.yml")
	content := "services:\n  ${SERVICE_NAME}:\n    image: ${IMAGE}\n"
	if err := os.WriteFile(tmpl, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	outPath, err := RenderComposeTemplate(tmpl, "acme-jemalloc")
	if err != nil {
		t.Fatalf("RenderComposeTemplate: %v", err)
	}
	// Per-service filename avoids collisions when multiple services share a template.
	want := filepath.Join(dir, "docker-compose.acme-jemalloc.yml")
	if outPath != want {
		t.Errorf("outPath = %q, want %q", outPath, want)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "${SERVICE_NAME}") {
		t.Error("rendered file still contains ${SERVICE_NAME}")
	}
	if !strings.Contains(got, "acme-jemalloc:") {
		t.Errorf("rendered file missing service name; got:\n%s", got)
	}
	// Non-matching vars (like ${IMAGE}) are left untouched for Docker Compose.
	if !strings.Contains(got, "${IMAGE}") {
		t.Error("rendered file should leave ${IMAGE} untouched")
	}
}

func TestRenderComposeTemplate_NoCollision(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "docker-compose.tmpl.yml")
	if err := os.WriteFile(tmpl, []byte("services:\n  ${SERVICE_NAME}:\n    image: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	services := []string{"acme", "acme-jemalloc", "acme-ubuntu", "acme-ubuntu-jemalloc"}
	seen := map[string]bool{}
	for _, svc := range services {
		path, err := RenderComposeTemplate(tmpl, svc)
		if err != nil {
			t.Fatalf("RenderComposeTemplate(%q): %v", svc, err)
		}
		if seen[path] {
			t.Errorf("path collision: %q produced by multiple services", path)
		}
		seen[path] = true
	}
}

func TestResolveComposePath_Template(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "docker-compose.tmpl.yml")
	if err := os.WriteFile(tmpl, []byte("services:\n  ${SERVICE_NAME}:\n    image: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveComposePath(tmpl, "myservice")
	if err != nil {
		t.Fatalf("ResolveComposePath: %v", err)
	}
	want := filepath.Join(dir, "docker-compose.myservice.yml")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveComposePath_NonTemplate(t *testing.T) {
	path := "./services/acme/docker-compose.yml"
	got, err := ResolveComposePath(path, "acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != path {
		t.Errorf("non-template path should pass through unchanged; got %q", got)
	}
}
