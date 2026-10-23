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
		"BENCHCTL_STORE_REFRESH_INTERVAL",
		"BENCHCTL_STORE_SERVICE_ROLE_KEY",
		"BENCHCTL_STORE_AUTH_TOKEN",
		"BENCHCTL_METRICS_ENDPOINT",
		"BENCHCTL_METRICS_USERNAME",
		"BENCHCTL_METRICS_TOKEN",
		"BENCHCTL_METRICS_PASSWORD",
		"BENCHCTL_SUPABASE_ACCESS_TOKEN",
		"BENCHCTL_USERNAME",
		"BENCHCTL_SHARED_CONFIG_FILE",
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
		{"tags.org", "example-org"},
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
		t.Errorf("output should not show the overridden config value:\n%s", out)
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

// An environment variable that wins over a user or shared value is listed,
// whether or not the user configuration sets the key.
func TestRunConfigShow_ListsEnvOverrides(t *testing.T) {
	setUseSharedFixture(t)
	t.Setenv("BENCHCTL_STORE_MODE", "local")     // user and shared both set it
	t.Setenv("BENCHCTL_STORE_ANON_KEY", "other") // shared only
	t.Setenv("BENCHCTL_USERNAME", "bob")         // user only
	t.Setenv("BENCHCTL_METRICS_ENDPOINT", "x")   // no file sets it

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})
	want := "Your environment overrides the configuration files for these keys:\n" +
		"  store.mode (BENCHCTL_STORE_MODE)\n" +
		"  store.anon_key (BENCHCTL_STORE_ANON_KEY)\n" +
		"  username (BENCHCTL_USERNAME)\n\n"
	if !strings.Contains(out, want) {
		t.Errorf("output missing env override block (want %q):\n%s", want, out)
	}
	if strings.Contains(out, "metrics.endpoint (") {
		t.Errorf("BENCHCTL_METRICS_ENDPOINT overrides no file value but was listed:\n%s", out)
	}
}

func TestRunConfigShow_NoEnvOverrideBlockWithoutOverrides(t *testing.T) {
	setUseSharedFixture(t)
	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})
	if strings.Contains(out, "Your environment overrides") {
		t.Errorf("unexpected env override block:\n%s", out)
	}
}

func setUseSharedFixture(t *testing.T) {
	t.Helper()
	path := setConfigFile(t)
	shared := filepath.Join(t.TempDir(), "shared.yaml")
	if err := os.WriteFile(shared, []byte("store:\n  mode: remote\n  url: \"https://shared.supabase.co\"\n  anon_key: k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", shared)
	if err := os.WriteFile(path, []byte("store:\n  mode: local\n  url: \"https://mine.supabase.co\"\n  refresh_interval: 30s\nusername: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With no arguments, use-shared removes every user value that overrides a
// shared one
func TestRunConfigUseShared_All(t *testing.T) {
	setUseSharedFixture(t)

	show := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})
	if !strings.Contains(show, "benchctl config use-shared") {
		t.Errorf("config show does not suggest use-shared:\n%s", show)
	}
	if !strings.Contains(show, "store.url: https://mine.supabase.co (user configuration)") || !strings.Contains(show, "  store.url\n") {
		t.Errorf("config show does not list store.url as overriding the shared configuration:\n%s", show)
	}

	out := captureStdout(t, func() {
		if err := runConfigUseShared(nil, nil); err != nil {
			t.Fatalf("runConfigUseShared: %v", err)
		}
	})
	if out != "Unset store.mode.\nUnset store.url.\n" {
		t.Errorf("output = %q", out)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if keys := cfg.UserOverrideKeys(); len(keys) != 0 {
		t.Errorf("still overriding: %v", keys)
	}
	if cfg.Username != "alice" || cfg.Source("store.refresh_interval") != config.SourceUser {
		t.Errorf("keys without a shared value were touched: username=%q refresh_interval from %v", cfg.Username, cfg.Source("store.refresh_interval"))
	}

	out = captureStdout(t, func() {
		if err := runConfigUseShared(nil, nil); err != nil {
			t.Fatalf("runConfigUseShared again: %v", err)
		}
	})
	if !strings.Contains(out, "No keys") {
		t.Errorf("second run output = %q", out)
	}
}

// With keys, use-shared removes only those user values.
func TestRunConfigUseShared_Keys(t *testing.T) {
	setUseSharedFixture(t)

	out := captureStdout(t, func() {
		if err := runConfigUseShared(nil, []string{"store.url", "username"}); err != nil {
			t.Fatalf("runConfigUseShared: %v", err)
		}
	})
	if !strings.Contains(out, "Unset store.url.") || !strings.Contains(out, "username does not override") {
		t.Errorf("output = %q", out)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source("store.url") != config.SourceShared {
		t.Errorf("store.url from %v, want shared", cfg.Source("store.url"))
	}
	if cfg.Source("store.mode") != config.SourceUser || cfg.Username != "alice" {
		t.Errorf("other keys were touched: store.mode from %v, username=%q", cfg.Source("store.mode"), cfg.Username)
	}
}

// An environment variable cannot be unset
func TestRunConfigUseShared_EnvKeepsWinning(t *testing.T) {
	setUseSharedFixture(t)
	t.Setenv("BENCHCTL_STORE_MODE", "local")

	out := captureStdout(t, func() {
		if err := runConfigUseShared(nil, nil); err != nil {
			t.Fatalf("runConfigUseShared: %v", err)
		}
	})
	if !strings.Contains(out, "BENCHCTL_STORE_MODE is set, so store.mode keeps its environment value") {
		t.Errorf("output does not mention the environment variable:\n%s", out)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Mode != "local" || cfg.Source("store.mode") != config.SourceEnv {
		t.Errorf("store.mode = %q from %v, want local from env", cfg.Store.Mode, cfg.Source("store.mode"))
	}
	if cfg.UserOverridesShared("store.mode") {
		t.Error("the user store.mode should have been removed")
	}
}
