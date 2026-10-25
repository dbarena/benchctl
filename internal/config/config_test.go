package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// setConfigFile points BENCHCTL_CONFIG_FILE at a fresh temp file and clears
// every table-driven BENCHCTL_* variable, so tests are isolated from
// whatever a developer or CI happens to have set in the ambient shell.
func setConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("BENCHCTL_CONFIG_FILE", path)
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", "")
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
		t.Errorf("Store.Mode = %q, want %q (default configuration)", cfg.Store.Mode, ModeLocal)
	}
	if cfg.Store.URL != "" || cfg.Store.AnonKey != "" {
		t.Fatalf("Store = %+v, want zero value", cfg.Store)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if string(data) != defaultConfigTemplate {
		t.Fatalf("new file is not the template:\n%s", data)
	}
}

// The template for a new user configuration must contain only comments, no
// keys. A key with a value would override the shared and default
// configuration from the first run.
func TestTemplateHasNoKeys(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(defaultConfigTemplate), &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Content) != 0 {
		t.Fatalf("template contains YAML content, want comments only:\n%s", defaultConfigTemplate)
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
  org: example-org
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
	if cfg.Tags["org"] != "example-org" {
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
	if src := cfg.Source("store.url"); src != SourceUser {
		t.Errorf("Source(store.url) = %v, want SourceUser", src)
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
	if err := os.WriteFile(path, []byte("tags: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Set("tags.org", "example-org"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "{org: example-org}") || strings.Contains(content, "{ org: example-org }") {
		t.Fatalf("expected block style, got flow style:\n%s", content)
	}
	if !strings.Contains(content, "org: example-org") {
		t.Fatalf("tag missing:\n%s", content)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after Set: %v", err)
	}
	if cfg.Tags["org"] != "example-org" {
		t.Fatalf("Tags[org] = %q, want example-org", cfg.Tags["org"])
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
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", "")
	for _, e := range envTable {
		t.Setenv(e.env, "")
	}

	if err := Set("store.url", "https://example.supabase.co"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("user configuration not created in nested dir: %v", err)
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
	if src := cfg.Source("store.url"); src != SourceUser {
		t.Errorf("Source(store.url) = %v, want SourceUser", src)
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

// writeShared writes a shared configuration file and points BENCHCTL_SHARED_CONFIG_FILE at it.
func writeShared(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write shared configuration: %v", err)
	}
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", path)
	return path
}

const testShared = `store:
  mode: remote
  url: "https://shared.supabase.co"
  anon_key: "shared-anon"
metrics:
  endpoint: "https://metrics.example.com"
tags:
  org: example-org
  team: team-a
`

// A fresh install (no user configuration yet) must get every value from the
// shared configuration: the template written on first run may not shadow any.
func TestLoad_SharedFillsFreshConfig(t *testing.T) {
	setConfigFile(t)
	writeShared(t, testShared)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Mode != ModeRemote || cfg.Store.URL != "https://shared.supabase.co" || cfg.Store.AnonKey != "shared-anon" {
		t.Fatalf("Store = %+v, want values from the shared configuration", cfg.Store)
	}
	for _, key := range []string{"store.mode", "store.url", "store.anon_key", "metrics.endpoint"} {
		if src := cfg.Source(key); src != SourceShared {
			t.Errorf("Source(%s) = %v, want shared configuration", key, src)
		}
	}
	if cfg.Tags["team"] != "team-a" {
		t.Errorf("Tags = %v, want default tags", cfg.Tags)
	}
}

// Each key resolves independently, first match wins: env, then the user
// configuration, then the shared configuration, then the default configuration.
func TestLoad_PrecedenceEnvHomeShared(t *testing.T) {
	path := setConfigFile(t)
	writeShared(t, testShared)
	if err := os.WriteFile(path, []byte("store:\n  mode: local\ntags:\n  team: mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BENCHCTL_METRICS_ENDPOINT", "https://from-env.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		key, got, want string
		src            Source
	}{
		{"store.mode", string(cfg.Store.Mode), "local", SourceUser},
		{"store.url", cfg.Store.URL, "https://shared.supabase.co", SourceShared},
		{"metrics.endpoint", cfg.Metrics.Endpoint, "https://from-env.example.com", SourceEnv},
		{"store.refresh_interval", cfg.Store.RefreshInterval, "30s", SourceDefault},
	}
	for _, c := range checks {
		if c.got != c.want || cfg.Source(c.key) != c.src {
			t.Errorf("%s = %q (%v), want %q (%v)", c.key, c.got, cfg.Source(c.key), c.want, c.src)
		}
	}
	if cfg.Tags["team"] != "mine" || cfg.Tags["org"] != "example-org" {
		t.Errorf("Tags = %v, want user team plus default org", cfg.Tags)
	}
}

// The shared configuration never leaks into the user configuration: config set
// writes only the key it is given.
func TestSet_DoesNotCopyShared(t *testing.T) {
	path := setConfigFile(t)
	writeShared(t, testShared)

	if err := Set("username", "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "shared-anon") || strings.Contains(string(data), "shared.supabase.co") {
		t.Fatalf("shared configuration copied into the user configuration:\n%s", data)
	}
}

func TestLoad_MissingSharedFileIsAnError(t *testing.T) {
	setConfigFile(t)
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "shared configuration") {
		t.Fatalf("Load error = %v, want a shared configuration error", err)
	}
}

func TestLoad_SharedFileRejectsUnknownKeys(t *testing.T) {
	setConfigFile(t)
	writeShared(t, "store:\n  bogus: 1\n")

	if _, err := Load(); err == nil {
		t.Fatal("want an error for an unknown key in the shared configuration")
	}
}

// Homebrew-style layout: <prefix>/bin/benchctl is a symlink into the keg, and
// the shared configuration lives at <keg>/share/benchctl/config.yaml.
func TestSharedConfigBesideExecutable(t *testing.T) {
	root := t.TempDir()
	keg := filepath.Join(root, "Cellar", "benchctl", "1.0")
	for _, dir := range []string{filepath.Join(keg, "bin"), filepath.Join(keg, "share", "benchctl"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(keg, "bin", "benchctl")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "benchctl")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}

	if got := sharedConfigBesideExecutable(link); got != "" {
		t.Fatalf("no shared configuration yet: got %q, want empty", got)
	}
	want := filepath.Join(keg, "share", "benchctl", "config.yaml")
	if err := os.WriteFile(want, []byte(testShared), 0o644); err != nil {
		t.Fatal(err)
	}
	got := sharedConfigBesideExecutable(link)
	if resolvedGot, _ := filepath.EvalSymlinks(got); resolvedGot != mustEval(t, want) {
		t.Fatalf("sharedConfigBesideExecutable(symlink) = %q, want %q", got, want)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// BENCHCTL_SHARED_CONFIG_FILE set but empty disables the shared configuration,
// even if a file next to the binary would otherwise be found.
func TestSharedConfigPath_EmptyEnvDisables(t *testing.T) {
	t.Setenv("BENCHCTL_SHARED_CONFIG_FILE", "")
	if got := SharedConfigPath(); got != "" {
		t.Fatalf("SharedConfigPath() = %q, want empty", got)
	}
}

// Unset removes only the given key, which then falls back to the shared
// configuration.
func TestUnset_FallsBackToShared(t *testing.T) {
	setConfigFile(t)
	writeShared(t, testShared)
	if err := Set("store.url", "https://mine.supabase.co"); err != nil {
		t.Fatal(err)
	}
	if err := Set("username", "alice"); err != nil {
		t.Fatal(err)
	}

	removed, err := Unset("store.url")
	if err != nil || !removed {
		t.Fatalf("Unset = %v, %v; want true, nil", removed, err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.URL != "https://shared.supabase.co" || cfg.Source("store.url") != SourceShared {
		t.Errorf("store.url = %q (%v), want the default", cfg.Store.URL, cfg.Source("store.url"))
	}
	if cfg.Username != "alice" {
		t.Errorf("username = %q, want it untouched", cfg.Username)
	}
}

func TestUnset_AbsentKeyAndTags(t *testing.T) {
	setConfigFile(t)
	if err := Set("tags.team", "team-a"); err != nil {
		t.Fatal(err)
	}

	if removed, err := Unset("metrics.username"); err != nil || removed {
		t.Errorf("Unset(absent) = %v, %v; want false, nil", removed, err)
	}
	if removed, err := Unset("tags.team"); err != nil || !removed {
		t.Fatalf("Unset(tags.team) = %v, %v; want true, nil", removed, err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tags["team"]; ok {
		t.Errorf("tags.team still set: %v", cfg.Tags)
	}
	// The emptied tags section must still accept new tags.
	if err := Set("tags.org", "example-org"); err != nil {
		t.Fatalf("Set after emptying tags: %v", err)
	}
}

func TestUnset_RejectsSecretsAndUnknownKeys(t *testing.T) {
	setConfigFile(t)
	if _, err := Unset("metrics.token"); err == nil || !strings.Contains(err.Error(), "BENCHCTL_METRICS_TOKEN") {
		t.Errorf("Unset(secret) error = %v", err)
	}
	if _, err := Unset("bogus"); err == nil {
		t.Error("Unset(unknown) should fail")
	}
}

// A user configuration value overrides the shared configuration only when that
// has a value for the key too, and even an identical copy counts.
func TestUserOverridesShared(t *testing.T) {
	path := setConfigFile(t)
	writeShared(t, testShared)
	if err := os.WriteFile(path, []byte("store:\n  url: \"https://shared.supabase.co\"\nusername: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UserOverridesShared("store.url") {
		t.Error("store.url equal to the shared value should still count as overriding it")
	}
	if cfg.UserOverridesShared("username") {
		t.Error("username is not in the shared configuration, so it overrides nothing")
	}
	if got := cfg.UserOverrideKeys(); len(got) != 1 || got[0] != "store.url" {
		t.Errorf("UserOverrideKeys() = %v, want [store.url]", got)
	}
	if cfg.UserOverridesShared("store.anon_key") {
		t.Error("store.anon_key comes from the shared configuration, so it does not override anything")
	}
}

// A user value counts as overriding the shared one even when an environment
// variable wins over both.
func TestUserOverridesShared_EvenWhenEnvWins(t *testing.T) {
	path := setConfigFile(t)
	writeShared(t, testShared)
	t.Setenv("BENCHCTL_STORE_MODE", "local")
	if err := os.WriteFile(path, []byte("store:\n  mode: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UserOverridesShared("store.mode") {
		t.Error("store.mode is set in the user and shared configuration, so the user value should count as overriding, even though the environment wins")
	}
}

// Tags are tracked like other keys, and an empty user tag falls through to the
// shared one.
func TestLoad_TagOverridesAndEmptyFallthrough(t *testing.T) {
	path := setConfigFile(t)
	writeShared(t, testShared)
	if err := os.WriteFile(path, []byte("tags:\n  org: \"\"\n  team: mine\n  extra: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Tags["org"]; got != "example-org" {
		t.Errorf("tags.org = %q, want example-org (empty user tag falls through)", got)
	}
	if got := cfg.Source("tags.org"); got != SourceShared {
		t.Errorf("Source(tags.org) = %v, want shared", got)
	}
	if got := cfg.Source("tags.team"); got != SourceUser {
		t.Errorf("Source(tags.team) = %v, want user", got)
	}
	if got := cfg.UserOverrideKeys(); len(got) != 1 || got[0] != "tags.team" {
		t.Errorf("UserOverrideKeys() = %v, want [tags.team]", got)
	}
}

func TestLoad_EmptyTagsAreDropped(t *testing.T) {
	path := setConfigFile(t)
	writeShared(t, "tags:\n  region: \"\"\n")
	if err := os.WriteFile(path, []byte("tags:\n  org: \"\"\n  team: \"\"\n  keep: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tags) != 1 || cfg.Tags["keep"] != "x" {
		t.Errorf("Tags = %v, want only keep=x", cfg.Tags)
	}
	for _, name := range []string{"org", "team", "region"} {
		if got := cfg.Source("tags." + name); got != SourceUnset {
			t.Errorf("Source(tags.%s) = %v, want unset", name, got)
		}
	}
}

// With no other layer, every value comes from the default configuration.
func TestLoad_DefaultConfiguration(t *testing.T) {
	setConfigFile(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for key, want := range map[string]string{"store.mode": "local", "store.refresh_interval": "30s"} {
		var got string
		switch key {
		case "store.mode":
			got = string(cfg.Store.Mode)
		case "store.refresh_interval":
			got = cfg.Store.RefreshInterval
		}
		if got != want || cfg.Source(key) != SourceDefault {
			t.Errorf("%s = %q (%v), want %q (default configuration)", key, got, cfg.Source(key), want)
		}
	}
	if d, err := cfg.StoreRefreshInterval(); err != nil || d.String() != "30s" {
		t.Errorf("StoreRefreshInterval = %v, %v; want 30s", d, err)
	}
}

// Set adds keys below the template's comments without disturbing them, and
// unsetting every key again restores the template exactly.
func TestSetUnset_PreserveTemplate(t *testing.T) {
	path := setConfigFile(t)

	if err := Set("store.mode", "remote"); err != nil {
		t.Fatal(err)
	}
	if err := Set("username", "alice"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := defaultConfigTemplate + "\nstore:\n  mode: remote\nusername: alice\n"
	if string(data) != want {
		t.Errorf("after Set:\n%s\nwant:\n%s", data, want)
	}

	for _, key := range []string{"store.mode", "username"} {
		if removed, err := Unset(key); err != nil || !removed {
			t.Fatalf("Unset(%s) = %v, %v", key, removed, err)
		}
	}
	data, _ = os.ReadFile(path)
	if string(data) != defaultConfigTemplate {
		t.Errorf("after Unset:\n%s\nwant the template back", data)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after Unset: %v", err)
	}
	if cfg.Store.Mode != ModeLocal || cfg.Source("store.mode") != SourceDefault {
		t.Errorf("store.mode = %q (%v), want local (default configuration)", cfg.Store.Mode, cfg.Source("store.mode"))
	}
}

// Unsetting the last key of a section removes the section too, rather than
// leaving "store: {}" behind.
func TestUnset_DropsEmptySection(t *testing.T) {
	path := setConfigFile(t)
	if err := os.WriteFile(path, []byte("store:\n  url: https://x.supabase.co\nusername: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := Unset("store.url"); err != nil || !removed {
		t.Fatalf("Unset = %v, %v", removed, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "username: alice\n" {
		t.Errorf("after Unset:\n%s", data)
	}
}

// An empty value is still a key in the file, as written by older templates;
// Unset removes it like any other.
func TestUnset_EmptyValue(t *testing.T) {
	path := setConfigFile(t)
	if err := os.WriteFile(path, []byte("store:\n  mode: local\n  url: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := Unset("store.url"); err != nil || !removed {
		t.Fatalf("Unset = %v, %v", removed, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "store:\n  mode: local\n" {
		t.Errorf("after Unset:\n%s", data)
	}
}

// A section with no value (e.g. only comments under "metrics:") parses as
// null. Set must still write into it instead of silently dropping the value.
func TestSet_IntoEmptySection(t *testing.T) {
	path := setConfigFile(t)
	if err := os.WriteFile(path, []byte("metrics:\n  # endpoint: \"\"\nstore:\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Set("metrics.endpoint", "https://vm.example.com"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Set("store.mode", "remote"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.Endpoint != "https://vm.example.com" || cfg.Source("metrics.endpoint") != SourceUser {
		t.Errorf("metrics.endpoint = %q (%v), want the value just set", cfg.Metrics.Endpoint, cfg.Source("metrics.endpoint"))
	}
	if cfg.Store.Mode != ModeRemote {
		t.Errorf("store.mode = %q, want remote", cfg.Store.Mode)
	}
}

// Unset on an empty section is a no-op, not an error.
func TestUnset_EmptySection(t *testing.T) {
	path := setConfigFile(t)
	if err := os.WriteFile(path, []byte("metrics:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := Unset("metrics.endpoint"); err != nil || removed {
		t.Fatalf("Unset = %v, %v; want false, nil", removed, err)
	}
}

// A dotted key such as "store.mode: local" is how config set names a key, not
// valid YAML for this file: the error says how to write it instead.
func TestLoad_DottedKeyExplainsNesting(t *testing.T) {
	path := setConfigFile(t)
	if err := os.WriteFile(path, []byte("# comment\nstore.mode: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a dotted key")
	}
	for _, want := range []string{"line 2", `"store.mode"`, `"store:"`, `"mode: local"`, "benchctl config set store.mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
