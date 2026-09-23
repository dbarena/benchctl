package sshdriver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/engine"
)

// writeTestTarGz builds a minimal .tar.gz containing files with the given
// name -> content mapping and returns its path.
func writeTestTarGz(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write content %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	p := filepath.Join(dir, "archive.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return p
}

func TestExtractTarGz_WritesFiles(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()
	archive := writeTestTarGz(t, src, map[string]string{
		"resume.log":              "hello driver log\n",
		"results_1.json":          `{"tpm":100}`,
		"results_2.json":          `{"tpm":110}`,
		"raw_samples_bench_1.csv": "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n",
	})

	if err := extractTarGz(archive, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	for name, want := range map[string]string{
		"resume.log":              "hello driver log\n",
		"results_1.json":          `{"tpm":100}`,
		"results_2.json":          `{"tpm":110}`,
		"raw_samples_bench_1.csv": "t_seconds,transaction,status,tpm\n1.0,NEW_ORDER,ok,600\n",
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
}

func TestExtractTarGz_EmptyArchiveSucceeds(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()
	archive := writeTestTarGz(t, src, map[string]string{}) // no files, e.g. fetch called before any artifacts exist

	if err := extractTarGz(archive, dest); err != nil {
		t.Fatalf("extractTarGz on empty archive: %v", err)
	}
}

func TestExtractTarGz_MissingArchiveErrors(t *testing.T) {
	dest := t.TempDir()
	if err := extractTarGz(filepath.Join(dest, "does-not-exist.tar.gz"), dest); err == nil {
		t.Fatal("expected error for missing archive, got nil")
	}
}

func TestFetchArtifacts_RequiresPublicIP(t *testing.T) {
	p := New(Config{ProviderName: "ec2", DefaultSSHUser: "ubuntu", Benchctl: &config.Config{}})

	err := p.FetchArtifacts(context.Background(), engine.Outputs{}, t.TempDir())
	if err == nil {
		t.Fatal("expected error when public_ip is missing, got nil")
	}
	if !strings.Contains(err.Error(), "public_ip") {
		t.Errorf("error %q should mention public_ip", err)
	}
}
