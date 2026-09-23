// Package config manages the benchctl user configuration file ~/.benchctl/config.yaml.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultConfigTemplate is written verbatim to a new config file. It is a
// hand-authored string (not encoder output) so it can carry explanatory
// comments, and it always contains the "store", "metrics" and "tags"
// sections so that Set only ever needs to append into an existing mapping.
const defaultConfigTemplate = `# benchctl configuration. Manage with ` + "`benchctl config show`" + ` and
# ` + "`benchctl config set <key> <value>`" + `.

store:
  # Where run state is recorded. "local" keeps it in ~/.benchctl/state.db.
  # "remote" records it in a shared Supabase project so the whole team sees
  # active runs, and requires url and anon_key below.
  mode: local
  url: ""
  anon_key: ""
  # In "local" mode, how long state read back from a remote driver instance
  # over SSH stays fresh before the next read refreshes it. Lower it for tighter
  # ` + "`benchctl wait`" + ` resolution, raise it on a slow link.
  refresh_interval: 30s
  # Credentials come from the environment only:
  #   BENCHCTL_STORE_SERVICE_ROLE_KEY  service-role key, for CI
  #   BENCHCTL_STORE_AUTH_TOKEN        bearer token, set on remote drivers
  # Interactive users run ` + "`benchctl auth login`" + ` instead.

metrics:
  # VictoriaMetrics base URL for the "victoriametrics" collector.
  endpoint: ""
  username: ""
  # Credentials come from the environment only:
  #   BENCHCTL_METRICS_TOKEN     bearer token
  #   BENCHCTL_METRICS_PASSWORD  password for the username above

# Recorded as the owner of each run.
username: ""

# Applied to every provisioned cloud resource, in addition to the tags
# benchctl always sets itself.
tags: {}

# The supabase target provider reads its management API token from the
# environment only: BENCHCTL_SUPABASE_ACCESS_TOKEN
`

// Mode selects where benchctl records run state.
type Mode string

const (
	// ModeLocal keeps run state in a local SQLite database at
	// ~/.benchctl/state.db.
	ModeLocal Mode = "local"
	// ModeRemote records run state in a shared Supabase project.
	ModeRemote Mode = "remote"
)

// StoreConfig holds the connection details for the run-state store.
type StoreConfig struct {
	Mode    Mode   `yaml:"mode"`
	URL     string `yaml:"url"`
	AnonKey string `yaml:"anon_key"`
	// RefreshInterval is a Go duration string. It applies to local mode only:
	// a run handed off to a remote driver instance is read back from that
	// instance over SSH, and this is how long each snapshot stays fresh. Empty
	// means runstate.DefaultRefreshInterval.
	RefreshInterval string `yaml:"refresh_interval"`
	// ServiceRoleKey and AuthToken come from the environment only; they are
	// never read from or written to the config file.
	ServiceRoleKey string `yaml:"-"`
	AuthToken      string `yaml:"-"`
}

// MetricsConfig holds the connection details for the metrics collector.
type MetricsConfig struct {
	Endpoint string `yaml:"endpoint"`
	Username string `yaml:"username"`
	// Token and Password come from the environment only; they are never read
	// from or written to the config file.
	Token    string `yaml:"-"`
	Password string `yaml:"-"`
}

// SupabaseConfig holds credentials for the Supabase target provider. It is
// populated from the environment only.
type SupabaseConfig struct {
	AccessToken string `yaml:"-"`
}

// Config is the parsed contents of ~/.benchctl/config.yaml, overlaid with
// any BENCHCTL_* environment variables that are set.
type Config struct {
	Store    StoreConfig       `yaml:"store"`
	Metrics  MetricsConfig     `yaml:"metrics"`
	Username string            `yaml:"username"`
	Tags     map[string]string `yaml:"tags"`
	Supabase SupabaseConfig    `yaml:"-"`

	// sources records, per dotted key, whether the effective value came from
	// the environment or the config file. Populated by Load.
	sources map[string]Source
}

// envEntry describes one BENCHCTL_* environment variable: the dotted config
// key it overlays, whether it holds a secret, and how to read/write it on a
// *Config. This table is the single source of truth for the env overlay in
// Load, RemoteEnv, EnvStates and Source.
type envEntry struct {
	env    string
	secret bool
	key    string // dotted config key, e.g. "store.url"
	get    func(*Config) string
	set    func(*Config, string)
}

// envTable lists every BENCHCTL_* variable, in the order EnvStates and
// RemoteEnv should present them. get/set are used instead of a *string
// pointer so that Store.Mode (a Mode, not a string) can share the same
// table as every other entry without an unsafe conversion.
var envTable = []envEntry{
	{
		env: "BENCHCTL_STORE_MODE",
		key: "store.mode",
		get: func(c *Config) string { return string(c.Store.Mode) },
		set: func(c *Config, v string) { c.Store.Mode = Mode(v) },
	},
	{
		env: "BENCHCTL_STORE_URL",
		key: "store.url",
		get: func(c *Config) string { return c.Store.URL },
		set: func(c *Config, v string) { c.Store.URL = v },
	},
	{
		env: "BENCHCTL_STORE_ANON_KEY",
		key: "store.anon_key",
		get: func(c *Config) string { return c.Store.AnonKey },
		set: func(c *Config, v string) { c.Store.AnonKey = v },
	},
	{
		env: "BENCHCTL_STORE_REFRESH_INTERVAL",
		key: "store.refresh_interval",
		get: func(c *Config) string { return c.Store.RefreshInterval },
		set: func(c *Config, v string) { c.Store.RefreshInterval = v },
	},
	{
		env:    "BENCHCTL_STORE_SERVICE_ROLE_KEY",
		key:    "store.service_role_key",
		secret: true,
		get:    func(c *Config) string { return c.Store.ServiceRoleKey },
		set:    func(c *Config, v string) { c.Store.ServiceRoleKey = v },
	},
	{
		env:    "BENCHCTL_STORE_AUTH_TOKEN",
		key:    "store.auth_token",
		secret: true,
		get:    func(c *Config) string { return c.Store.AuthToken },
		set:    func(c *Config, v string) { c.Store.AuthToken = v },
	},
	{
		env: "BENCHCTL_METRICS_ENDPOINT",
		key: "metrics.endpoint",
		get: func(c *Config) string { return c.Metrics.Endpoint },
		set: func(c *Config, v string) { c.Metrics.Endpoint = v },
	},
	{
		env:    "BENCHCTL_METRICS_TOKEN",
		key:    "metrics.token",
		secret: true,
		get:    func(c *Config) string { return c.Metrics.Token },
		set:    func(c *Config, v string) { c.Metrics.Token = v },
	},
	{
		env: "BENCHCTL_METRICS_USERNAME",
		key: "metrics.username",
		get: func(c *Config) string { return c.Metrics.Username },
		set: func(c *Config, v string) { c.Metrics.Username = v },
	},
	{
		env:    "BENCHCTL_METRICS_PASSWORD",
		key:    "metrics.password",
		secret: true,
		get:    func(c *Config) string { return c.Metrics.Password },
		set:    func(c *Config, v string) { c.Metrics.Password = v },
	},
	{
		env:    "BENCHCTL_SUPABASE_ACCESS_TOKEN",
		key:    "supabase.access_token",
		secret: true,
		get:    func(c *Config) string { return c.Supabase.AccessToken },
		set:    func(c *Config, v string) { c.Supabase.AccessToken = v },
	},
	{
		env: "BENCHCTL_USERNAME",
		key: "username",
		get: func(c *Config) string { return c.Username },
		set: func(c *Config, v string) { c.Username = v },
	},
}

// Path returns the location of the config file.
//
// BENCHCTL_CONFIG_FILE overrides the default location when set to a
// non-empty value. An empty value has no special meaning: unset and
// empty-string BENCHCTL_CONFIG_FILE both fall back to the default path.
func Path() (string, error) {
	if p := os.Getenv("BENCHCTL_CONFIG_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home dir: %w", err)
	}
	return filepath.Join(home, ".benchctl", "config.yaml"), nil
}

// Load reads and parses the config file, creating it from the default
// template if it does not yet exist, then overlays any set BENCHCTL_*
// environment variables on top. The returned Config always has a non-nil
// Tags map and a valid Store.Mode.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			return nil, fmt.Errorf("config: create %s: %w", filepath.Dir(path), mkErr)
		}
		if writeErr := os.WriteFile(path, []byte(defaultConfigTemplate), 0o600); writeErr != nil {
			return nil, fmt.Errorf("config: write %s: %w", path, writeErr)
		}
		data = []byte(defaultConfigTemplate)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	cfg.sources = make(map[string]Source, len(envTable))
	for _, e := range envTable {
		envVal := os.Getenv(e.env)
		switch {
		case envVal != "":
			e.set(&cfg, envVal)
			cfg.sources[e.key] = SourceEnv
		case e.get(&cfg) != "":
			cfg.sources[e.key] = SourceFile
		default:
			cfg.sources[e.key] = SourceUnset
		}
	}

	if cfg.Store.Mode == "" {
		cfg.Store.Mode = ModeLocal
	}
	if cfg.Store.Mode != ModeLocal && cfg.Store.Mode != ModeRemote {
		return nil, fmt.Errorf("config: store.mode %q is invalid (valid values: %q, %q)", cfg.Store.Mode, ModeLocal, ModeRemote)
	}

	if cfg.Tags == nil {
		cfg.Tags = map[string]string{}
	}

	return &cfg, nil
}

// Validate checks invariants that Load does not enforce, so that read-only
// commands such as "benchctl config show" still work on an incomplete
// configuration. Callers that need a usable config call Validate explicitly.
func (c *Config) Validate() error {
	if _, err := c.StoreRefreshInterval(); err != nil {
		return err
	}
	if c.Store.Mode != ModeRemote {
		return nil
	}
	// os.LookupEnv, not os.Getenv: a service-role key that's exported but
	// empty must be rejected here, before resolveAuthToken silently falls
	// back to a cached personal credential that bypasses the override this
	// variable exists to provide.
	if v, ok := os.LookupEnv("BENCHCTL_STORE_SERVICE_ROLE_KEY"); ok && v == "" {
		return fmt.Errorf("config: BENCHCTL_STORE_SERVICE_ROLE_KEY is set but empty; unset it or provide a value")
	}
	if c.Store.URL == "" {
		return fmt.Errorf("config: store.mode is %q but store.url is empty; set it with 'benchctl config set store.url <url>'", ModeRemote)
	}
	if c.Store.AnonKey == "" {
		return fmt.Errorf("config: store.mode is %q but store.anon_key is empty; set it with 'benchctl config set store.anon_key <key>'", ModeRemote)
	}
	return nil
}

// StoreRefreshInterval parses store.refresh_interval. A zero duration means
// "unset"; the store applies its own default.
func (c *Config) StoreRefreshInterval() (time.Duration, error) {
	if c.Store.RefreshInterval == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.Store.RefreshInterval)
	if err != nil {
		return 0, fmt.Errorf("config: store.refresh_interval %q is not a duration (e.g. 30s, 2m): %w", c.Store.RefreshInterval, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("config: store.refresh_interval %q must not be negative", c.Store.RefreshInterval)
	}
	return d, nil
}

// RemoteEnv returns the BENCHCTL_* environment variables needed to reproduce
// this configuration in a remote benchctl process, omitting empty values.
// It never includes BENCHCTL_CONFIG_FILE.
func (c *Config) RemoteEnv() map[string]string {
	env := make(map[string]string, len(envTable))
	for _, e := range envTable {
		if v := e.get(c); v != "" {
			env[e.env] = v
		}
	}
	return env
}

// Source reports whether the effective value at dottedKey (e.g. "store.url")
// came from the environment, the config file, or neither.
func (c *Config) Source(dottedKey string) Source {
	if c.sources == nil {
		return SourceUnset
	}
	return c.sources[dottedKey]
}

// Source identifies where a configuration value's effective value came from.
type Source int

const (
	SourceUnset Source = iota
	SourceFile
	SourceEnv
)

// String returns a human-readable label for s.
func (s Source) String() string {
	switch s {
	case SourceFile:
		return "config file"
	case SourceEnv:
		return "env"
	default:
		return "unset"
	}
}

// EnvState describes the current state of a single benchctl environment
// variable, for display in commands like "benchctl config show".
type EnvState struct {
	Name   string
	Value  string // "" for secrets and for unset vars
	Set    bool
	Secret bool
}

// EnvStates reports the current state of every benchctl environment
// variable, in display order, led by BENCHCTL_CONFIG_FILE. Set is true when
// a variable is present in the environment even if its value is empty, so
// callers can distinguish "unset" from "set but empty". Value is always ""
// for secrets.
func EnvStates() []EnvState {
	states := make([]EnvState, 0, len(envTable)+1)

	if v, ok := os.LookupEnv("BENCHCTL_CONFIG_FILE"); ok {
		states = append(states, EnvState{Name: "BENCHCTL_CONFIG_FILE", Value: v, Set: true})
	} else {
		states = append(states, EnvState{Name: "BENCHCTL_CONFIG_FILE"})
	}

	for _, e := range envTable {
		v, ok := os.LookupEnv(e.env)
		es := EnvState{Name: e.env, Set: ok, Secret: e.secret}
		if ok && !e.secret {
			es.Value = v
		}
		states = append(states, es)
	}
	return states
}

// Set writes a single dotted key to the config file, preserving all existing
// comments. Accepted keys are "store.mode", "store.url", "store.anon_key",
// "store.refresh_interval", "metrics.endpoint", "metrics.username",
// "username", and "tags.<name>".
// Secret keys are rejected with the environment variable to set instead. Set
// is the only writer of the config file: there is no bulk-save path, since
// re-marshalling the Config struct would discard the comments this function
// is careful to keep.
func Set(key, value string) error {
	if _, err := Load(); err != nil {
		return err
	}

	path, err := Path()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if len(root.Content) == 0 {
		return fmt.Errorf("config: %s: empty document", path)
	}
	doc := root.Content[0]

	for _, e := range envTable {
		if e.secret && e.key == key {
			return fmt.Errorf("config: %s is set via the %s environment variable, not the config file", key, e.env)
		}
	}

	if key == "store.mode" && value != string(ModeLocal) && value != string(ModeRemote) {
		return fmt.Errorf("config: invalid store.mode %q (valid values: %q, %q)", value, ModeLocal, ModeRemote)
	}

	section, field, err := setTarget(key)
	if err != nil {
		return err
	}
	mapping := doc
	if section != "" {
		if mapping, err = mappingValue(doc, section, true); err != nil {
			return err
		}
	}
	setScalar(mapping, field, value)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return fmt.Errorf("config: encode %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("config: encode %s: %w", path, err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// setTarget maps a dotted config key to the top-level mapping section it
// lives in (empty for keys at the document root) and the field name within
// that section. It returns an error for unknown keys, mirroring the keys
// documented on Set.
func setTarget(key string) (section, field string, err error) {
	switch {
	case key == "store.mode" || key == "store.url" || key == "store.anon_key" || key == "store.refresh_interval":
		return "store", strings.TrimPrefix(key, "store."), nil
	case key == "metrics.endpoint" || key == "metrics.username":
		return "metrics", strings.TrimPrefix(key, "metrics."), nil
	case key == "username":
		return "", "username", nil
	case strings.HasPrefix(key, "tags."):
		name := strings.TrimPrefix(key, "tags.")
		if name == "" {
			return "", "", fmt.Errorf("config: invalid key %q: tag name must not be empty", key)
		}
		return "tags", name, nil
	default:
		return "", "", fmt.Errorf("config: unknown key %q; valid keys are store.mode, store.url, store.anon_key, store.refresh_interval, metrics.endpoint, metrics.username, username, and tags.<name>", key)
	}
}

// mappingValue returns the mapping node for the given top-level key within
// doc (a mapping node). If create is true and the key is absent, an empty
// mapping is appended and returned.
func mappingValue(doc *yaml.Node, key string, create bool) (*yaml.Node, error) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == key {
			return doc.Content[i+1], nil
		}
	}
	if !create {
		return nil, fmt.Errorf("config: missing %q section", key)
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode}
	doc.Content = append(doc.Content, keyNode, valueNode)
	return valueNode, nil
}

// setScalar sets mapping[field] = value, updating the value node in place if
// present and appending a new key/value pair otherwise. Appending the first
// entry into a flow-style (empty) mapping switches it to block style so the
// result is not emitted on a single line.
func setScalar(mapping *yaml.Node, field, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == field {
			v := mapping.Content[i+1]
			v.Kind = yaml.ScalarNode
			v.Tag = ""
			v.Style = 0
			v.Value = value
			return
		}
	}
	if len(mapping.Content) == 0 {
		mapping.Style = 0
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: field}
	valueNode := &yaml.Node{Kind: yaml.ScalarNode, Value: value}
	mapping.Content = append(mapping.Content, keyNode, valueNode)
}
