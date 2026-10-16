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

// config show points at "unset --overrides-shared", which removes exactly the
// keys it flagged and leaves the rest of the home configuration alone.
func TestRunConfigUnset_OverridesShared(t *testing.T) {
	path := setConfigFile(t)
	shared := filepath.Join(t.TempDir(), "shared.yaml")
	if err := os.WriteFile(shared, []byte("store:\n  mode: remote\n  url: \"https://shared.supabase.co\"\n  anon_key: k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", shared)
	if err := os.WriteFile(path, []byte("store:\n  mode: local\n  url: \"https://shared.supabase.co\"\n  refresh_interval: 30s\nusername: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { configUnsetOverridesShared = false })

	show := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})
	if !strings.Contains(show, "benchctl config unset --overrides-shared") {
		t.Errorf("config show does not suggest --overrides-shared:\n%s", show)
	}

	configUnsetOverridesShared = true
	out := captureStdout(t, func() {
		if err := runConfigUnset(nil, nil); err != nil {
			t.Fatalf("runConfigUnset: %v", err)
		}
	})
	if out != "Unset store.mode.\nUnset store.url.\n" {
		t.Errorf("output = %q", out)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if keys := cfg.KeysOverridingShared(); len(keys) != 0 {
		t.Errorf("still overriding: %v", keys)
	}
	if cfg.Username != "alice" || cfg.Source("store.refresh_interval") != config.SourceHome {
		t.Errorf("keys without a shared value were touched: username=%q refresh_interval from %v", cfg.Username, cfg.Source("store.refresh_interval"))
	}

	out = captureStdout(t, func() {
		if err := runConfigUnset(nil, nil); err != nil {
			t.Fatalf("runConfigUnset again: %v", err)
		}
	})
	if !strings.Contains(out, "No keys") {
		t.Errorf("second run output = %q", out)
	}
}

// unset takes either one key or --overrides-shared, never both or neither.
func TestConfigUnset_Args(t *testing.T) {
	t.Cleanup(func() { configUnsetOverridesShared = false })
	for _, tc := range []struct {
		flag    bool
		args    []string
		wantErr bool
	}{
		{false, []string{"store.mode"}, false},
		{false, nil, true},
		{true, nil, false},
		{true, []string{"store.mode"}, true},
	} {
		configUnsetOverridesShared = tc.flag
		if err := configUnsetCmd.Args(configUnsetCmd, tc.args); (err != nil) != tc.wantErr {
			t.Errorf("flag=%v args=%v: err = %v, wantErr %v", tc.flag, tc.args, err, tc.wantErr)
		}
	}
}
