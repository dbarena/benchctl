// Package config resolves benchctl settings. Each setting resolves on its own
// from four layers, and the first that has a value wins:
//
//  1. Environment variables: BENCHCTL_*, e.g. BENCHCTL_STORE_MODE. Secrets
//     come only from here.
//  2. Home configuration: ~/.benchctl/config.yaml (see Path), the user's own
//     settings, written by Set and Unset.
//  3. Shared configuration: <prefix>/share/benchctl/config.yaml next to the
//     binary (see SharedConfigPath), read-only. It lets whoever distributes
//     benchctl to a group of users give them common settings, such as a
//     remote state store: they ship the file alongside the binary, and a
//     change reaches everyone with their next update.
//  4. Default configuration: defaults.yaml, compiled into the binary.
package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultConfigTemplate is written verbatim to a new home configuration file.
// It is a hand-authored file (not encoder output) so it can carry explanatory
// comments. It consists of comments only and sets no keys: anything in the
// home configuration overrides the shared and default configuration, so a new
// file must not hide them. Set adds keys below the comments as the user sets
// them.
//
//go:embed template.yaml
var defaultConfigTemplate string

// defaultConfig is the default configuration: the lowest-priority layer, used
// when no other layer sets a key.
//
//go:embed defaults.yaml
var defaultConfig []byte

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

// Config holds the effective settings, resolved from all four layers (see the
// package doc).
type Config struct {
	Store    StoreConfig       `yaml:"store"`
	Metrics  MetricsConfig     `yaml:"metrics"`
	Username string            `yaml:"username"`
	Tags     map[string]string `yaml:"tags"`
	Supabase SupabaseConfig    `yaml:"-"`

	// sources records, per dotted key, which layer the effective value came
	// from. Populated by Load.
	sources map[string]Source
	// overridesShared records the dotted keys whose home configuration value
	// hides a value from the shared configuration. Populated by Load.
	overridesShared map[string]bool
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

// Environment variables that locate the configuration files. They are not
// settings, so they live outside envTable.
const (
	envConfigFile       = "BENCHCTL_CONFIG_FILE"
	envSharedConfigFile = "BENCHCTL_SHARED_CONFIG_FILE"
)

// pathEnvVars lists the variables above in the order EnvStates reports them.
var pathEnvVars = []string{envConfigFile, envSharedConfigFile}

// Path returns the location of the home configuration file.
//
// BENCHCTL_CONFIG_FILE overrides the default location when set to a
// non-empty value. An empty value has no special meaning: unset and
// empty-string BENCHCTL_CONFIG_FILE both fall back to the default path.
func Path() (string, error) {
	if p := os.Getenv(envConfigFile); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home dir: %w", err)
	}
	return filepath.Join(home, ".benchctl", "config.yaml"), nil
}

// SharedConfigPath returns the location of the shared configuration file, or
// "" when there is none.
//
// BENCHCTL_SHARED_CONFIG_FILE names it explicitly; set but empty disables the
// shared configuration. Otherwise benchctl looks for share/benchctl/config.yaml
// next to its own binary (after resolving symlinks).
func SharedConfigPath() string {
	if p, ok := os.LookupEnv(envSharedConfigFile); ok {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return sharedConfigBesideExecutable(exe)
}

// sharedConfigBesideExecutable returns <prefix>/share/benchctl/config.yaml for an
// executable at <prefix>/bin/benchctl, or "" if that file does not exist.
func sharedConfigBesideExecutable(exe string) string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	p := filepath.Join(filepath.Dir(exe), "..", "share", "benchctl", "config.yaml")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Load resolves every setting from the four layers described in the package
// doc. It creates the home configuration file from the template if it does
// not yet exist. An empty value counts as unset in every layer. The returned
// Config always has a non-nil Tags map and a valid Store.Mode.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	sharedPath := SharedConfigPath()

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
	if err := parseConfig(data, path, &cfg); err != nil {
		return nil, err
	}

	var def Config
	if err := parseConfig(defaultConfig, "default configuration", &def); err != nil {
		return nil, err
	}

	var shared Config
	if sharedPath != "" {
		sdata, err := os.ReadFile(sharedPath)
		if err != nil {
			return nil, fmt.Errorf("config: read shared configuration %s: %w", sharedPath, err)
		}
		if err := parseConfig(sdata, sharedPath, &shared); err != nil {
			return nil, err
		}
	}

	cfg.sources = make(map[string]Source, len(envTable))
	cfg.overridesShared = make(map[string]bool)
	for _, e := range envTable {
		envVal := os.Getenv(e.env)
		switch {
		case envVal != "":
			e.set(&cfg, envVal)
			cfg.sources[e.key] = SourceEnv
		case e.get(&cfg) != "":
			cfg.sources[e.key] = SourceHome
			// Flag it even if the value equals the shared one: the home value
			// still wins, so if the shared value changes later, this user
			// keeps the old one.
			cfg.overridesShared[e.key] = e.get(&shared) != ""
		case e.get(&shared) != "":
			e.set(&cfg, e.get(&shared))
			cfg.sources[e.key] = SourceShared
		case e.get(&def) != "":
			e.set(&cfg, e.get(&def))
			cfg.sources[e.key] = SourceDefault
		default:
			cfg.sources[e.key] = SourceUnset
		}
	}

	// Tags merge per name: home configuration tags win over shared ones.
	for name, value := range shared.Tags {
		if _, ok := cfg.Tags[name]; ok {
			continue
		}
		if cfg.Tags == nil {
			cfg.Tags = map[string]string{}
		}
		cfg.Tags[name] = value
	}

	if cfg.Store.Mode != ModeLocal && cfg.Store.Mode != ModeRemote {
		return nil, fmt.Errorf("config: store.mode %q is invalid (valid values: %q, %q)", cfg.Store.Mode, ModeLocal, ModeRemote)
	}

	if cfg.Tags == nil {
		cfg.Tags = map[string]string{}
	}

	return &cfg, nil
}

// parseConfig decodes a config document into cfg, rejecting unknown keys.
func parseConfig(data []byte, path string, cfg *Config) error {
	if err := rejectDottedKeys(data, path); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && err != io.EOF {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// rejectDottedKeys reports a top-level key written in the dotted form that
// "benchctl config set" takes, such as "store.mode: local". YAML reads that as
// one unknown key named "store.mode"; the error says how to write it instead.
// Documents that do not parse are left for the decoder to report.
func rejectDottedKeys(data []byte, path string) error {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		k := doc.Content[i]
		section, field, err := setTarget(k.Value)
		if err != nil || section == "" {
			continue
		}
		return fmt.Errorf("config: parse %s: line %d: write %q as nested YAML (%q, then %q indented below it), or run 'benchctl config set %s <value>'",
			path, k.Line, k.Value, section+":", field+": "+doc.Content[i+1].Value, k.Value)
	}
	return nil
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

// Source reports which layer the effective value at dottedKey (e.g.
// "store.url") came from, or SourceUnset if none has a value.
func (c *Config) Source(dottedKey string) Source {
	if c.sources == nil {
		return SourceUnset
	}
	return c.sources[dottedKey]
}

// OverridesShared reports whether the home configuration sets dottedKey
// although the shared configuration has a value for it too. The home value
// keeps winning when the shared configuration changes; Unset removes it.
func (c *Config) OverridesShared(dottedKey string) bool {
	return c.overridesShared[dottedKey]
}

// KeysOverridingShared returns every dotted key for which OverridesShared is
// true, in the order "benchctl config show" lists them.
func (c *Config) KeysOverridingShared() []string {
	var keys []string
	for _, e := range envTable {
		if c.overridesShared[e.key] {
			keys = append(keys, e.key)
		}
	}
	return keys
}

// Source identifies the layer a setting's effective value came from.
type Source int

const (
	SourceUnset   Source = iota
	SourceHome           // home configuration
	SourceEnv            // environment variable
	SourceShared         // shared configuration
	SourceDefault        // default configuration
)

// String returns a human-readable label for s.
func (s Source) String() string {
	switch s {
	case SourceHome:
		return "home configuration"
	case SourceEnv:
		return "env"
	case SourceShared:
		return "shared configuration"
	case SourceDefault:
		return "default configuration"
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
// variable, in display order, led by BENCHCTL_CONFIG_FILE and
// BENCHCTL_SHARED_CONFIG_FILE. Set is true when
// a variable is present in the environment even if its value is empty, so
// callers can distinguish "unset" from "set but empty". Value is always ""
// for secrets.
func EnvStates() []EnvState {
	states := make([]EnvState, 0, len(pathEnvVars)+len(envTable))

	for _, name := range pathEnvVars {
		if v, ok := os.LookupEnv(name); ok {
			states = append(states, EnvState{Name: name, Value: v, Set: true})
		} else {
			states = append(states, EnvState{Name: name})
		}
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

// Set writes a single dotted key to the home configuration file, preserving all existing
// comments. Accepted keys are "store.mode", "store.url", "store.anon_key",
// "store.refresh_interval", "metrics.endpoint", "metrics.username",
// "username", and "tags.<name>".
// Secret keys are rejected with the environment variable to set instead. Set
// and Unset are the only writers of the home configuration file: there is no bulk-save
// path, since re-marshalling the Config struct would discard the comments
// they are careful to keep.
func Set(key, value string) error {
	path, root, err := readDocument()
	if err != nil {
		return err
	}
	doc := root.Content[0]

	if err := rejectSecret(key); err != nil {
		return err
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
	return writeDocument(path, root)
}

// Unset removes a single dotted key from the home configuration file,
// preserving all other content and comments, so the key falls back to the
// shared configuration, if any, or the default configuration. It reports
// whether the key was present. Accepted keys are the same as for Set.
func Unset(key string) (bool, error) {
	path, root, err := readDocument()
	if err != nil {
		return false, err
	}
	doc := root.Content[0]

	if err := rejectSecret(key); err != nil {
		return false, err
	}
	section, field, err := setTarget(key)
	if err != nil {
		return false, err
	}
	mapping := doc
	if section != "" {
		// readDocument has already rejected a malformed section via Load, so
		// the only error left here is a missing section: nothing to remove.
		if mapping, err = mappingValue(doc, section, false); err != nil {
			return false, nil
		}
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != field {
			continue
		}
		mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
		if section != "" && len(mapping.Content) == 0 {
			removeKey(doc, section)
		}
		return true, writeDocument(path, root)
	}
	return false, nil
}

// removeKey deletes key and its value from the mapping node m, if present.
func removeKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// readDocument loads the home configuration file (creating it from the
// template if missing) as a YAML node tree, so callers can edit it without
// losing comments. The root's single child is always the top-level mapping.
func readDocument() (string, *yaml.Node, error) {
	if _, err := Load(); err != nil {
		return "", nil, err
	}
	path, err := Path()
	if err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if len(root.Content) == 0 {
		// A file of only comments, such as the template, parses to nothing and
		// its comments are dropped. Keep them as the document's head comment.
		root = yaml.Node{
			Kind:        yaml.DocumentNode,
			HeadComment: strings.TrimSpace(string(data)),
			Content:     []*yaml.Node{{Kind: yaml.MappingNode}},
		}
	}
	if root.Content[0].Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("config: %s: top level must be a mapping of settings", path)
	}
	return path, &root, nil
}

// writeDocument encodes root back to the home configuration file at path. A document
// without any keys is written as its comments alone rather than as "{}".
func writeDocument(path string, root *yaml.Node) error {
	if len(root.Content[0].Content) == 0 {
		var data []byte
		if root.HeadComment != "" {
			data = []byte(root.HeadComment + "\n")
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("config: write %s: %w", path, err)
		}
		return nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
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

// rejectSecret returns an error for keys that only come from the environment.
func rejectSecret(key string) error {
	for _, e := range envTable {
		if e.secret && e.key == key {
			return fmt.Errorf("config: %s is set via the %s environment variable, not the config file", key, e.env)
		}
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
// mapping is appended and returned. A key with no value ("metrics:" followed
// only by comments parses as null) is turned into an empty mapping in place;
// otherwise entries appended to it would be silently dropped on encode.
func mappingValue(doc *yaml.Node, key string, create bool) (*yaml.Node, error) {
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != key {
			continue
		}
		v := doc.Content[i+1]
		switch {
		case v.Kind == yaml.MappingNode:
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
			v.Kind, v.Tag, v.Value, v.Style = yaml.MappingNode, "", "", 0
		default:
			return nil, fmt.Errorf("config: %q must be a mapping of settings", key)
		}
		return v, nil
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
