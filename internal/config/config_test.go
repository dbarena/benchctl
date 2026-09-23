package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setConfigFile points BENCHCTL_CONFIG_FILE at a fresh temp file and clears
// every table-driven BENCHCTL_* variable, so tests are isolated from
// whatever a developer or CI happens to have set in the ambient shell.
func setConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("BENCHCTL_CONFIG_FILE", path)
	for _, e := range envTable {
		t.Setenv(e.env, "")
	}
	return path
}

func TestLoad_MissingFileCreatesTemplate(t *testing.T) {
	path := setConfigFile(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tags == nil {
		t.Fatal("Tags is nil, want non-nil empty map")
	}
	if len(cfg.Tags) != 0 {
		t.Fatalf("Tags = %v, want empty", cfg.Tags)
	}
	if cfg.Store.Mode != ModeLocal {
		t.Errorf("Store.Mode = %q, want %q (parsed from template)", cfg.Store.Mode, ModeLocal)
	}
	if cfg.Store.URL != "" || cfg.Store.AnonKey != "" {
		t.Fatalf("Store = %+v, want zero value", cfg.Store)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if !strings.Contains(string(data), "store:") || !strings.Contains(string(data), "tags: {}") {
		t.Fatalf("template missing expected sections:\n%s", data)
	}
}

// TestLoad_DefaultComesFromTemplateBytes proves that Load's fallback path
// for a missing file actually parses the template bytes it writes, rather
// than hand-building a Config: a template with a non-default mode must
// produce a Config with that mode.
func TestLoad_DefaultComesFromTemplateBytes(t *testing.T) {
	path := setConfigFile(t)
	variant := strings.Replace(defaultConfigTemplate, "mode: local", "mode: remote", 1)
	if variant == defaultConfigTemplate {
		t.Fatal("template does not contain \"mode: local\"; test fixture is stale")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(variant), 0o600); err != nil {
		t.Fatalf("write variant: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Mode != ModeRemote {
		t.Fatalf("Store.Mode = %q, want %q", cfg.Store.Mode, ModeRemote)
	}
}

func TestLoad_ParsesExistingFile(t *testing.T) {
	path := setConfigFile(t)
	fixture := `store:
  mode: remote
  url: "https://example.supabase.co"
  anon_key: "anon-123"

metrics:
  endpoint: "https://metrics.example.com"
  username: "metrics-user"

username: "ci-bot"

tags:
  org: engops
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Mode != ModeRemote {
		t.Errorf("Store.Mode = %q", cfg.Store.Mode)
	}
	if cfg.Store.URL != "https://example.supabase.co" {
		t.Errorf("Store.URL = %q", cfg.Store.URL)
	}
	if cfg.Store.AnonKey != "anon-123" {
		t.Errorf("Store.AnonKey = %q", cfg.Store.AnonKey)
	}
	if cfg.Metrics.Endpoint != "https://metrics.example.com" {
		t.Errorf("Metrics.Endpoint = %q", cfg.Metrics.Endpoint)
	}
	if cfg.Metrics.Username != "metrics-user" {
		t.Errorf("Metrics.Username = %q", cfg.Metrics.Username)
	}
	if cfg.Username != "ci-bot" {
		t.Errorf("Username = %q", cfg.Username)
	}
	if cfg.Tags["org"] != "engops" {
		t.Errorf("Tags[org] = %q", cfg.Tags["org"])
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	path := setConfigFile(t)
	fixture := `store:
  mode: local
  url: "https://file.example.com"
  anon_key: "file-anon"

metrics:
  endpoint: "https://file-metrics.example.com"
  username: "file-user"

username: "file-owner"

tags: {}
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for _, e := range envTable {
		envValue := "env-value-for-" + e.key
		if e.key == "store.mode" {
			envValue = string(ModeRemote)
		}
		t.Run(e.key, func(t *testing.T) {
			t.Setenv(e.env, envValue)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := e.get(cfg); got != envValue {
				t.Errorf("%s: got %q, want %q", e.key, got, envValue)
			}
			if src := cfg.Source(e.key); src != SourceEnv {
				t.Errorf("Source(%q) = %v, want SourceEnv", e.key, src)
			}
		})
	}
}

func TestLoad_EmptyEnvVarTreatedAsUnset(t *testing.T) {
	setConfigFile(t)
	fixture := `store:
  mode: local
  url: "https://file.example.com"
  anon_key: ""

tags: {}
`
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	t.Setenv("BENCHCTL_STORE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.URL != "https://file.example.com" {
		t.Fatalf("Store.URL = %q, want file value to win over empty env var", cfg.Store.URL)
	}
	if src := cfg.Source("store.url"); src != SourceFile {
		t.Errorf("Source(store.url) = %v, want SourceFile", src)
	}
}

func TestLoad_StrictDecodingRejectsUnknownKey(t *testing.T) {
	path := setConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fixture := `store:
  mode: local
bogus_top_level_key: true
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown top-level key")
	}
}

func TestLoad_StrictDecodingRejectsSecretInFile(t *testing.T) {
	path := setConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fixture := `store:
  service_role_key: x
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected error for secret key present in file")
	}
}

func TestLoad_InvalidModeRejected(t *testing.T) {
	path := setConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fixture := `store:
  mode: bogus
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid store.mode")
	}
}

func TestLoad_AbsentModeDefaultsToLocal(t *testing.T) {
	path := setConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fixture := `store:
  url: ""
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Mode != ModeLocal {
		t.Fatalf("Store.Mode = %q, want %q", cfg.Store.Mode, ModeLocal)
	}
}

func TestValidate(t *testing.T) {
	// Every case sets BENCHCTL_STORE_SERVICE_ROLE_KEY explicitly (to a
	// harmless placeholder unless the case is specifically about it), so the
	// table isn't at the mercy of whatever the developer's or CI's ambient
	// shell happens to have exported.
	tests := []struct {
		name    string
		cfg     Config
		envKey  string
		wantErr string // substring, "" means no error
	}{
		{
			name:   "local with nothing set passes",
			cfg:    Config{Store: StoreConfig{Mode: ModeLocal}},
			envKey: "placeholder",
		},
		{
			name:    "remote without url",
			cfg:     Config{Store: StoreConfig{Mode: ModeRemote, AnonKey: "k"}},
			envKey:  "placeholder",
			wantErr: "store.url",
		},
		{
			name:    "remote without anon_key",
			cfg:     Config{Store: StoreConfig{Mode: ModeRemote, URL: "https://example.com"}},
			envKey:  "placeholder",
			wantErr: "store.anon_key",
		},
		{
			name:   "remote with both passes",
			cfg:    Config{Store: StoreConfig{Mode: ModeRemote, URL: "https://example.com", AnonKey: "k"}},
			envKey: "placeholder",
		},
		{
			name:    "remote with service role key set but empty is rejected",
			cfg:     Config{Store: StoreConfig{Mode: ModeRemote, URL: "https://example.com", AnonKey: "k"}},
			envKey:  "",
			wantErr: "BENCHCTL_STORE_SERVICE_ROLE_KEY",
		},
		{
			name:   "local mode ignores service role key set but empty",
			cfg:    Config{Store: StoreConfig{Mode: ModeLocal}},
			envKey: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BENCHCTL_STORE_SERVICE_ROLE_KEY", tt.envKey)
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_ServiceRoleKeyAbsentPasses proves the empty-env-var check
// added above only fires when the var is present-but-empty, not merely
// unset. os.LookupEnv, not os.Getenv, is load-bearing here.
func TestValidate_ServiceRoleKeyAbsentPasses(t *testing.T) {
	if old, ok := os.LookupEnv("BENCHCTL_STORE_SERVICE_ROLE_KEY"); ok {
		os.Unsetenv("BENCHCTL_STORE_SERVICE_ROLE_KEY")
		t.Cleanup(func() { os.Setenv("BENCHCTL_STORE_SERVICE_ROLE_KEY", old) })
	}

	cfg := Config{Store: StoreConfig{Mode: ModeRemote, URL: "https://example.com", AnonKey: "k"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil when env var is simply unset", err)
	}
}

func TestSet_PreservesComments(t *testing.T) {
	path := setConfigFile(t)
	fixture := `# head comment for the whole file
store:
  mode: local
  url: "" # inline comment on url
  anon_key: ""

tags: {}
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := Set("store.url", "https://new.supabase.co"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "https://new.supabase.co") {
		t.Fatalf("new value missing:\n%s", content)
	}
	if !strings.Contains(content, "# head comment for the whole file") {
		t.Fatalf("head comment lost:\n%s", content)
	}
	if !strings.Contains(content, "# inline comment on url") {
		t.Fatalf("line comment lost:\n%s", content)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after Set: %v", err)
	}
	if cfg.Store.URL != "https://new.supabase.co" {
		t.Errorf("Store.URL = %q after Set", cfg.Store.URL)
	}
}

func TestSet_StoreModeRemote(t *testing.T) {
	setConfigFile(t)
	if err := Set("store.mode", "remote"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Mode != ModeRemote {
		t.Fatalf("Store.Mode = %q, want %q", cfg.Store.Mode, ModeRemote)
	}
}

func TestSet_StoreModeInvalidRejected(t *testing.T) {
	setConfigFile(t)
	if err := Set("store.mode", "bogus"); err == nil {
		t.Fatal("expected error for invalid store.mode value")
	}
}

func TestSet_NewTagOnEmptyMapProducesBlockStyle(t *testing.T) {
	path := setConfigFile(t)
	// Load() with no file present writes the default template, which has
	// tags: {} in flow style.
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := Set("tags.org", "engops"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "{org: engops}") || strings.Contains(content, "{ org: engops }") {
		t.Fatalf("expected block style, got flow style:\n%s", content)
	}
	if !strings.Contains(content, "org: engops") {
		t.Fatalf("tag missing:\n%s", content)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after Set: %v", err)
	}
	if cfg.Tags["org"] != "engops" {
		t.Fatalf("Tags[org] = %q, want engops", cfg.Tags["org"])
	}
}

func TestSet_SecretKeysRejected(t *testing.T) {
	setConfigFile(t)
	for _, e := range envTable {
		if !e.secret {
			continue
		}
		t.Run(e.key, func(t *testing.T) {
			err := Set(e.key, "value")
			if err == nil {
				t.Fatalf("expected error for secret key %q", e.key)
			}
			if !strings.Contains(err.Error(), e.env) {
				t.Fatalf("error %v does not name env var %s", err, e.env)
			}
		})
	}
}

func TestSet_UnknownKeyRejected(t *testing.T) {
	setConfigFile(t)
	if err := Set("bogus.key", "value"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestSetAndLoad_CreateParentDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sub", "config.yaml")
	t.Setenv("BENCHCTL_CONFIG_FILE", path)
	for _, e := range envTable {
		t.Setenv(e.env, "")
	}

	if err := Set("store.url", "https://example.supabase.co"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created in nested dir: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.URL != "https://example.supabase.co" {
		t.Errorf("Store.URL = %q", cfg.Store.URL)
	}
}

func TestRemoteEnv_OmitsEmptyAndConfigFile(t *testing.T) {
	setConfigFile(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Store.URL = "https://example.supabase.co"
	cfg.Store.AnonKey = "anon-123"

	env := cfg.RemoteEnv()
	if env["BENCHCTL_STORE_URL"] != "https://example.supabase.co" {
		t.Errorf("BENCHCTL_STORE_URL = %q", env["BENCHCTL_STORE_URL"])
	}
	if env["BENCHCTL_STORE_ANON_KEY"] != "anon-123" {
		t.Errorf("BENCHCTL_STORE_ANON_KEY = %q", env["BENCHCTL_STORE_ANON_KEY"])
	}
	if _, ok := env["BENCHCTL_CONFIG_FILE"]; ok {
		t.Error("RemoteEnv must never contain BENCHCTL_CONFIG_FILE")
	}
	if _, ok := env["BENCHCTL_METRICS_ENDPOINT"]; ok {
		t.Error("RemoteEnv must omit empty values")
	}
	// Store.Mode defaults to "local" which is non-empty, so it is expected.
	if env["BENCHCTL_STORE_MODE"] != string(ModeLocal) {
		t.Errorf("BENCHCTL_STORE_MODE = %q, want %q", env["BENCHCTL_STORE_MODE"], ModeLocal)
	}
}

func TestSource(t *testing.T) {
	setConfigFile(t)
	fixture := `store:
  mode: local
  url: "https://file.example.com"

tags: {}
`
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("BENCHCTL_STORE_ANON_KEY", "env-anon")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src := cfg.Source("store.url"); src != SourceFile {
		t.Errorf("Source(store.url) = %v, want SourceFile", src)
	}
	if src := cfg.Source("store.anon_key"); src != SourceEnv {
		t.Errorf("Source(store.anon_key) = %v, want SourceEnv", src)
	}
	if src := cfg.Source("metrics.endpoint"); src != SourceUnset {
		t.Errorf("Source(metrics.endpoint) = %v, want SourceUnset", src)
	}
}

func TestEnvStates(t *testing.T) {
	setConfigFile(t)
	t.Setenv("BENCHCTL_STORE_URL", "") // set but empty
	t.Setenv("BENCHCTL_STORE_ANON_KEY", "anon-123")
	t.Setenv("BENCHCTL_STORE_SERVICE_ROLE_KEY", "super-secret")

	states := EnvStates()

	var foundURL, foundAnon, foundSecret bool
	for _, s := range states {
		switch s.Name {
		case "BENCHCTL_STORE_URL":
			foundURL = true
			if !s.Set {
				t.Error("BENCHCTL_STORE_URL: Set = false, want true for set-but-empty var")
			}
			if s.Value != "" {
				t.Errorf("BENCHCTL_STORE_URL: Value = %q, want empty", s.Value)
			}
		case "BENCHCTL_STORE_ANON_KEY":
			foundAnon = true
			if !s.Set || s.Value != "anon-123" {
				t.Errorf("BENCHCTL_STORE_ANON_KEY: Set=%v Value=%q", s.Set, s.Value)
			}
		case "BENCHCTL_STORE_SERVICE_ROLE_KEY":
			foundSecret = true
			if !s.Secret {
				t.Error("BENCHCTL_STORE_SERVICE_ROLE_KEY: Secret = false, want true")
			}
			if s.Value != "" {
				t.Errorf("BENCHCTL_STORE_SERVICE_ROLE_KEY: Value = %q, want empty (secret)", s.Value)
			}
			if !s.Set {
				t.Error("BENCHCTL_STORE_SERVICE_ROLE_KEY: Set = false, want true")
			}
		}
	}
	if !foundURL || !foundAnon || !foundSecret {
		t.Fatalf("missing expected entries: url=%v anon=%v secret=%v", foundURL, foundAnon, foundSecret)
	}

	if states[0].Name != "BENCHCTL_CONFIG_FILE" {
		t.Fatalf("states[0].Name = %q, want BENCHCTL_CONFIG_FILE", states[0].Name)
	}
}
