package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/config"
)

// setConfigFile points BENCHCTL_CONFIG_FILE at a fresh file in a temp dir and
// neutralizes every other BENCHCTL_* environment variable so these tests are
// hermetic regardless of what's set in the developer's own shell.
func setConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("BENCHCTL_CONFIG_FILE", path)
	for _, name := range []string{
		"BENCHCTL_STORE_MODE",
		"BENCHCTL_STORE_URL",
		"BENCHCTL_STORE_ANON_KEY",
		"BENCHCTL_STORE_SERVICE_ROLE_KEY",
		"BENCHCTL_STORE_AUTH_TOKEN",
		"BENCHCTL_METRICS_ENDPOINT",
		"BENCHCTL_METRICS_USERNAME",
		"BENCHCTL_METRICS_TOKEN",
		"BENCHCTL_METRICS_PASSWORD",
		"BENCHCTL_SUPABASE_ACCESS_TOKEN",
		"BENCHCTL_USERNAME",
	} {
		t.Setenv(name, "")
	}
	return path
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

func TestRunConfigSet_ValidKeys(t *testing.T) {
	cases := []struct {
		key   string
		value string
	}{
		{"store.url", "https://example.supabase.co"},
		{"store.anon_key", "anon-123"},
		{"tags.org", "engops"},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			setConfigFile(t)
			if err := runConfigSet(nil, []string{tc.key, tc.value}); err != nil {
				t.Fatalf("runConfigSet(%q, %q): %v", tc.key, tc.value, err)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			switch tc.key {
			case "store.url":
				if cfg.Store.URL != tc.value {
					t.Errorf("Store.URL = %q, want %q", cfg.Store.URL, tc.value)
				}
			case "store.anon_key":
				if cfg.Store.AnonKey != tc.value {
					t.Errorf("Store.AnonKey = %q, want %q", cfg.Store.AnonKey, tc.value)
				}
			case "tags.org":
				if cfg.Tags["org"] != tc.value {
					t.Errorf("Tags[org] = %q, want %q", cfg.Tags["org"], tc.value)
				}
			}
		})
	}
}

func TestRunConfigSet_InvalidKeyRejected(t *testing.T) {
	setConfigFile(t)
	err := runConfigSet(nil, []string{"bogus.key", "value"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error = %v, want mention of unknown key", err)
	}
}

func TestRunConfigShow_LabelsFromEnvWhenDisagreeing(t *testing.T) {
	setConfigFile(t)
	if err := config.Set("store.url", "https://config.supabase.co"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Setenv("BENCHCTL_STORE_URL", "https://env.supabase.co")

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "https://env.supabase.co (env)") {
		t.Errorf("output missing env-sourced value with label:\n%s", out)
	}
	if strings.Contains(out, "https://config.supabase.co") {
		t.Errorf("output should not show the shadowed config value:\n%s", out)
	}
}

func TestRunConfigShow_RedactsSensitiveEnvVars(t *testing.T) {
	setConfigFile(t)

	sensitive := map[string]string{
		"BENCHCTL_STORE_SERVICE_ROLE_KEY": "super-secret-role-key",
		"BENCHCTL_STORE_AUTH_TOKEN":       "super-secret-auth-token",
		"BENCHCTL_METRICS_TOKEN":          "super-secret-metrics-token",
		"BENCHCTL_METRICS_PASSWORD":       "super-secret-metrics-password",
		"BENCHCTL_SUPABASE_ACCESS_TOKEN":  "super-secret-access-token",
	}
	for k, v := range sensitive {
		t.Setenv(k, v)
	}

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	for name, value := range sensitive {
		if !strings.Contains(out, name+": [redacted]") {
			t.Errorf("expected %s to be shown as [redacted]:\n%s", name, out)
		}
		if strings.Contains(out, value) {
			t.Errorf("output leaked value of %s:\n%s", name, out)
		}
	}
}
