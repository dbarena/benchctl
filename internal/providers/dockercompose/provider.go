// Package dockercompose implements a TargetProvider that manages the
// benchmark target database via docker compose.
package dockercompose

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

// cmdRunner executes a shell command, tees combined output to out (if non-nil),
// and always returns the captured bytes for error reporting.
type cmdRunner func(ctx context.Context, extraEnv []string, out io.Writer, name string, args ...string) ([]byte, error)

// Provider implements engine.TargetProvider using docker compose.
// Service lifecycle is managed per-benchmark via BenchmarkLifecycleProvider:
// StartBenchmark starts the named service, StopBenchmark stops it.
type Provider struct {
	run cmdRunner
	out io.Writer // where to stream command output; set by New(), overridden by cfg["quiet"]
}

// New returns a Provider that streams docker compose output to stdout.
func New() *Provider {
	return &Provider{run: execRun, out: os.Stdout}
}

// newWithRunner returns a Provider with an injected runner and no output streaming, for testing.
func newWithRunner(r cmdRunner) *Provider {
	return &Provider{run: r, out: io.Discard}
}

// Provision validates the service definitions and returns empty outputs.
// Services are started on demand via StartBenchmark before each benchmark entry.
//
// Expected config keys:
//
//	services  []any   service definitions (required)
//	quiet     bool    suppress docker compose output (default: false)
func (p *Provider) Provision(_ context.Context, _ string, cfg map[string]any) (engine.Outputs, error) {
	defs, err := schema.ParseServiceDefs(cfg["services"])
	if err != nil {
		return nil, fmt.Errorf("docker-compose: config.services: %w", err)
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("docker-compose: config.services must contain at least one entry")
	}
	return engine.Outputs{}, nil
}

// StartBenchmark starts the named service and returns its connection outputs
// (service.<name>.host, service.<name>.port, service.<name>.user, etc.).
func (p *Provider) StartBenchmark(ctx context.Context, cfg map[string]any, service string) (engine.Outputs, error) {
	svc, err := findService(cfg, service)
	if err != nil {
		return nil, fmt.Errorf("docker-compose: %w", err)
	}
	composePath, err := engine.ResolveComposePath(svc.Definition, svc.Name)
	if err != nil {
		return nil, fmt.Errorf("docker-compose: render template: %w", err)
	}
	cPort, err := containerPort(composePath, svc.Name)
	if err != nil {
		return nil, fmt.Errorf("docker-compose: %w", err)
	}
	envSlice := append(buildEnv(svc.Vars), "SERVICE_NAME="+svc.Name)
	out := writerFor(p.out, cfg)

	if captured, err := p.run(ctx, envSlice, out, "docker", "compose", "-f", composePath, "pull", svc.Name); err != nil {
		return nil, fmt.Errorf("docker compose pull: %w\noutput: %s", err, captured)
	}
	imageDigest, digestErr := resolveImageDigest(ctx, p.run, envSlice, composePath)
	if digestErr != nil {
		fmt.Fprintf(out, "warning: could not resolve image digest for service %s: %v\n", svc.Name, digestErr)
	}
	if captured, err := p.run(ctx, envSlice, out, "docker", "compose", "-f", composePath, "up", "-d", "--wait", svc.Name); err != nil {
		p.stopServiceBestEffort(envSlice, out, composePath, svc.Name)
		return nil, fmt.Errorf("docker compose up: %w\noutput: %s", err, captured)
	}
	portOut, err := p.run(ctx, envSlice, io.Discard, "docker", "compose", "-f", composePath, "port", svc.Name, cPort)
	if err != nil {
		p.stopServiceBestEffort(envSlice, out, composePath, svc.Name)
		return nil, fmt.Errorf("docker compose port: %w\noutput: %s", err, portOut)
	}

	host, port, err := parseHostPort(strings.TrimSpace(string(portOut)))
	if err != nil {
		p.stopServiceBestEffort(envSlice, out, composePath, svc.Name)
		return nil, fmt.Errorf("docker-compose: %w", err)
	}

	outputs := engine.Outputs{
		"service." + svc.Name + ".host": host,
		"service." + svc.Name + ".port": port,
	}
	for _, k := range []string{"user", "password", "db", "sslmode", "sslnegotiation"} {
		if v := svc.Vars[k]; v != "" {
			outputs["service."+svc.Name+"."+k] = v
		}
	}
	if imageDigest != "" {
		outputs["service."+svc.Name+".image_digest"] = imageDigest
	}
	return outputs, nil
}

// resolveImageDigest returns the immutable repo@sha256 digest of the image
// that was just pulled for the compose file at composePath, or an error if it
// could not be determined (e.g. a locally-built image with no registry
// digest). Callers treat this as best-effort reproducibility metadata and
// must never fail the benchmark over it.
func resolveImageDigest(ctx context.Context, run cmdRunner, envSlice []string, composePath string) (string, error) {
	imageOut, err := run(ctx, envSlice, io.Discard, "docker", "compose", "-f", composePath, "config", "--images")
	if err != nil {
		return "", fmt.Errorf("resolve image name: %w", err)
	}
	image := strings.TrimSpace(string(imageOut))
	if image == "" {
		return "", fmt.Errorf("resolve image name: empty output")
	}
	digestOut, err := run(ctx, envSlice, io.Discard, "docker", "inspect", "--format", "{{index .RepoDigests 0}}", image)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", image, err)
	}
	return strings.TrimSpace(string(digestOut)), nil
}

// stopServiceBestEffort tears down a service's containers after StartBenchmark
// fails partway through (after "up --wait" has already started them), using a
// fresh background context so a cancelled run context doesn't also abort the
// cleanup. StartBenchmark's failure return carries no outputs, so the
// runner's activeService is never set and no teardown closure will run for
// this service, so this is the only place anything stops the containers
// "docker compose up --wait" already started.
func (p *Provider) stopServiceBestEffort(envSlice []string, out io.Writer, composePath, name string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if captured, err := p.run(cleanupCtx, envSlice, out, "docker", "compose", "-f", composePath, "down", "--volumes"); err != nil {
		fmt.Fprintf(out, "warning: docker-compose: cleanup after failed start of service %s: %v\noutput: %s\n", name, err, captured)
	}
}

// StopBenchmark stops the named service and removes its containers and volumes.
func (p *Provider) StopBenchmark(ctx context.Context, cfg map[string]any, service string) error {
	svc, err := findService(cfg, service)
	if err != nil {
		return fmt.Errorf("docker-compose: %w", err)
	}
	composePath, err := engine.ResolveComposePath(svc.Definition, svc.Name)
	if err != nil {
		return fmt.Errorf("docker-compose: render template: %w", err)
	}
	envSlice := append(buildEnv(svc.Vars), "SERVICE_NAME="+svc.Name)
	out := writerFor(p.out, cfg)
	if captured, err := p.run(ctx, envSlice, out, "docker", "compose", "-f", composePath, "down", "--volumes"); err != nil {
		return fmt.Errorf("docker compose down: %w\noutput: %s", err, captured)
	}
	return nil
}

// Teardown is a no-op; service lifecycle is fully managed by StartBenchmark
// and StopBenchmark (and a deferred cleanup registered by the runner on failure).
func (p *Provider) Teardown(_ context.Context, _ engine.Outputs) error {
	return nil
}

// findService locates the named service in cfg["services"] and returns it.
func findService(cfg map[string]any, name string) (schema.ServiceDef, error) {
	defs, err := schema.ParseServiceDefs(cfg["services"])
	if err != nil {
		return schema.ServiceDef{}, fmt.Errorf("config.services: %w", err)
	}
	for _, svc := range defs {
		if svc.Name == name {
			return svc, nil
		}
	}
	return schema.ServiceDef{}, fmt.Errorf("service %q not found in config.services", name)
}

// containerPort reads the Docker Compose file at composePath and returns the
// container-side port for the named service. It handles all Docker Compose port
// formats: "HOST:CONTAINER", "IP:HOST:CONTAINER", "CONTAINER", and bare integers.
func containerPort(composePath, serviceName string) (string, error) {
	data, err := os.ReadFile(composePath)
	if err != nil {
		return "", fmt.Errorf("reading compose file %s: %w", composePath, err)
	}
	var compose map[string]any
	if err := yaml.Unmarshal(data, &compose); err != nil {
		return "", fmt.Errorf("parsing compose file %s: %w", composePath, err)
	}
	services, _ := compose["services"].(map[string]any)
	svc, ok := services[serviceName].(map[string]any)
	if !ok {
		return "", fmt.Errorf("service %q not found in %s", serviceName, composePath)
	}
	ports, _ := svc["ports"].([]any)
	if len(ports) == 0 {
		return "", fmt.Errorf("service %q in %s has no ports declared", serviceName, composePath)
	}
	entry := fmt.Sprintf("%v", ports[0])
	// Split on ":" and take the last segment, the container port, regardless of
	// whether the format is "CONTAINER", "HOST:CONTAINER", or "IP:HOST:CONTAINER".
	parts := strings.Split(entry, ":")
	return parts[len(parts)-1], nil
}

// buildEnv converts a vars map into KEY=VALUE environment strings.
func buildEnv(vars map[string]string) []string {
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, varToEnvKey(k)+"="+v)
	}
	return env
}

// varToEnvKey converts a service var key to an environment variable name.
// "user" and "password" are special-cased to avoid clashing with standard shell
// environment variables: USER is always set to the login name in shell sessions,
// which would cause Docker Compose to substitute the wrong value.
func varToEnvKey(k string) string {
	switch k {
	case "user":
		return "PG_USER"
	case "password":
		return "PG_PASSWORD"
	}
	return strings.ToUpper(strings.ReplaceAll(k, "-", "_"))
}

// writerFor returns io.Discard when quiet mode is set, otherwise p.out.
func writerFor(out io.Writer, cfg map[string]any) io.Writer {
	if quiet, _ := cfg["quiet"].(bool); quiet {
		return io.Discard
	}
	return out
}

// parseHostPort parses a "host:port" string as returned by `docker compose port`.
// An unspecified host (0.0.0.0 or empty) is normalised to "localhost".
func parseHostPort(s string) (host, port string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("docker compose port returned empty output; is the port mapped in the compose file?")
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", fmt.Errorf("parsing host:port from %q: %w", s, err)
	}
	if h == "" || h == "0.0.0.0" {
		h = "localhost"
	}
	return h, p, nil
}

// execRun is the real cmdRunner. It prints the command to out, then tees
// combined output to out while also capturing it for error messages.
func execRun(ctx context.Context, extraEnv []string, out io.Writer, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var buf bytes.Buffer
	w := io.Writer(&buf)
	if out != nil && out != io.Discard {
		fmt.Fprintf(out, "$ %s %s\n", name, strings.Join(args, " "))
		w = io.MultiWriter(out, &buf)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return buf.Bytes(), err
}
