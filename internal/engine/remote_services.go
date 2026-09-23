package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbarena/benchctl/internal/bundle"
	"github.com/dbarena/benchctl/internal/schema"
)

const remoteServicesDir = "/home/ubuntu/services"

// cloudInitTimeout bounds how long deployRemoteServices waits for cloud-init
// to finish. Real bootstraps finish this in well under a minute; a stuck
// user-data step should fail fast rather than blocking indefinitely.
const cloudInitTimeout = 15 * time.Minute

// outputKeyDriverTargetKeyPath is the Outputs key used to carry the driver-side
// copy of the target SSH private key. It is set by RunAsync after copying the
// local key to the driver instance, and read by remote service helpers when Resume
// runs on the driver. The original ssh_private_key_path is preserved unchanged so
// that `benchctl connect target` (which runs locally) continues to work.
const outputKeyDriverTargetKeyPath = "_driver_target_key_path"

// targetKeyPath returns the SSH private key path to use when connecting to the
// target from the current execution context. On a driver instance Resume uses
// _driver_target_key_path (the key copied there by RunAsync); on the local
// orchestrator the original ssh_private_key_path is used.
func targetKeyPath(outputs Outputs) string {
	if kp := outputs[outputKeyDriverTargetKeyPath]; kp != "" {
		return kp
	}
	return outputs["ssh_private_key_path"]
}

// deployRemoteServices deploys every service definition to the remote target.
// It waits for SSH and cloud-init, then for each service copies the compose
// directory, writes a .env file from the service vars, and pulls the image.
// It starts no containers; startRemoteService does that.
func (r *Runner) deployRemoteServices(ctx context.Context, cfg map[string]any, outputs Outputs) error {
	services, err := schema.ParseServiceDefs(cfg["services"])
	if err != nil {
		return fmt.Errorf("parse services: %w", err)
	}
	if len(services) == 0 {
		return nil
	}

	ip := outputs["public_ip"]
	keyPath := outputs["ssh_private_key_path"]
	user := outputs["ssh_user"]
	if user == "" {
		user = "ubuntu"
	}
	target := user + "@" + ip

	r.logf("Waiting for SSH on %s", ip)
	if err := WaitForSSH(ctx, r.Out, ip, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for SSH: %w", err)
	}
	r.logf("Waiting for cloud-init")
	cloudInitCtx, cancelCloudInit := context.WithTimeout(ctx, cloudInitTimeout)
	err = RunSSH(cloudInitCtx, r.Out, keyPath, user, ip, "cloud-init status --wait")
	cancelCloudInit()
	if err != nil {
		return fmt.Errorf("cloud-init wait (exceeded %s?): %w", cloudInitTimeout, err)
	}
	if err := RunSSH(ctx, r.Out, keyPath, user, ip, "mkdir -p "+remoteServicesDir); err != nil {
		return fmt.Errorf("mkdir services dir: %w", err)
	}

	for _, svc := range services {
		r.logf("Deploying service [%s]", svc.Name)
		digest, err := r.prepareRemoteService(ctx, keyPath, user, ip, target, svc)
		if err != nil {
			return fmt.Errorf("deploy service %s: %w", svc.Name, err)
		}
		if digest != "" {
			outputs["service."+svc.Name+".image_digest"] = digest
		}
	}
	return nil
}

// prepareRemoteService deploys svc to the remote target and returns the
// immutable repo@sha256 digest of the image that was pulled, or "" if it
// could not be resolved (best-effort; never fails the deploy).
func (r *Runner) prepareRemoteService(ctx context.Context, keyPath, user, ip, target string, svc schema.ServiceDef) (string, error) {
	absTmpl, cleanup, err := bundle.File(svc.Definition)
	if err != nil {
		return "", fmt.Errorf("resolve definition path: %w", err)
	}
	defer cleanup()
	// Render template locally before SCP so the per-service rendered compose file
	// is included in the directory transfer.
	absDefinition, err := ResolveComposePath(absTmpl, svc.Name)
	if err != nil {
		return "", fmt.Errorf("render compose template: %w", err)
	}
	localServiceDir := filepath.Dir(absDefinition)
	remoteServiceDir := remoteServicesDir + "/" + svc.Name

	// Remove any stale service directory so SCP creates a fresh copy with
	// the correct name rather than copying into an existing directory.
	if err := RunSSH(ctx, r.Out, keyPath, user, ip, "rm -rf "+remoteServiceDir); err != nil {
		return "", fmt.Errorf("remove stale service dir: %w", err)
	}
	// SCP to the exact target path (no trailing slash). When the target path
	// does not exist, scp -r creates it as a copy of localServiceDir, giving
	// each service its own correctly-named directory regardless of the local
	// source directory name.
	if err := ScpDir(ctx, r.Out, keyPath, localServiceDir, target, remoteServiceDir); err != nil {
		return "", fmt.Errorf("scp service dir: %w", err)
	}

	lines := []string{"SERVICE_NAME=" + svc.Name}
	for k, v := range svc.Vars {
		lines = append(lines, varToEnvKey(k)+"="+v)
	}
	envContent := strings.Join(lines, "\n") + "\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(envContent))
	writeEnvCmd := fmt.Sprintf("echo %s | base64 -d > %s/.env", encoded, remoteServiceDir)
	if err := RunSSH(ctx, r.Out, keyPath, user, ip, writeEnvCmd); err != nil {
		return "", fmt.Errorf("write .env: %w", err)
	}

	remoteComposeFile := remoteServiceDir + "/" + filepath.Base(absDefinition)
	pullCmd := fmt.Sprintf("retry -t 5 -d 5,10,20,40,60 -- docker compose -f %s pull", remoteComposeFile)
	if err := RunSSH(ctx, r.Out, keyPath, user, ip, pullCmd); err != nil {
		return "", fmt.Errorf("docker compose pull: %w", err)
	}

	digest, err := resolveRemoteImageDigest(ctx, keyPath, user, ip, remoteComposeFile)
	if err != nil {
		fmt.Fprintf(r.Out, "warning: could not resolve image digest for service %s: %v\n", svc.Name, err)
		return "", nil
	}
	return digest, nil
}

// resolveRemoteImageDigest returns the immutable repo@sha256 digest of the
// image just pulled for the compose file at remoteComposeFile, or an error if
// it could not be determined (e.g. a locally-built image with no registry
// digest). Callers treat this as best-effort reproducibility metadata and
// must never fail the deploy over it.
func resolveRemoteImageDigest(ctx context.Context, keyPath, user, ip, remoteComposeFile string) (string, error) {
	cmd := fmt.Sprintf(
		`IMAGE=$(docker compose -f %s config --images | head -1) && docker inspect --format '{{index .RepoDigests 0}}' "$IMAGE"`,
		remoteComposeFile,
	)
	out, err := RunSSHCapture(ctx, keyPath, user, ip, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// startRemoteService starts the named service on the remote target and returns
// its connection outputs (service.<name>.host, service.<name>.port, etc.).
func (r *Runner) startRemoteService(ctx context.Context, cfg map[string]any, outputs Outputs, service string) (Outputs, error) {
	svc, err := findRemoteService(cfg, service)
	if err != nil {
		return nil, err
	}

	ip := outputs["public_ip"]
	keyPath := targetKeyPath(outputs)
	user := outputs["ssh_user"]
	if user == "" {
		user = "ubuntu"
	}

	remoteComposeFile := remoteServiceDir(svc)
	upCmd := fmt.Sprintf("docker compose -f %s up -d --wait %s", remoteComposeFile, svc.Name)
	if err := RunSSH(ctx, r.Out, keyPath, user, ip, upCmd); err != nil {
		// up --wait can fail its own health check after already starting the
		// container(s). The caller only learns of a running service via the
		// Outputs this function returns, so on failure it returns none and
		// nothing else ever stops them. Clean up inline instead, using a
		// fresh background context so a cancelled run context does not also
		// abort the cleanup.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		downCmd := fmt.Sprintf("docker compose -f %s down --volumes", remoteComposeFile)
		if downErr := RunSSH(cleanupCtx, r.Out, keyPath, user, ip, downCmd); downErr != nil {
			fmt.Fprintf(r.Out, "warning: cleanup after failed start of service %s: %v\n", svc.Name, downErr)
		}
		return nil, fmt.Errorf("docker compose up: %w", err)
	}

	prefix := "service." + svc.Name + "."
	result := Outputs{
		prefix + "host": outputs["host"],
		prefix + "port": outputs["port"],
	}
	for _, k := range []string{"user", "password", "db", "sslmode", "sslnegotiation"} {
		if v := svc.Vars[k]; v != "" {
			result[prefix+k] = v
		}
	}
	return result, nil
}

// stopRemoteService stops the named service on the remote target and removes
// its containers and volumes.
func (r *Runner) stopRemoteService(ctx context.Context, cfg map[string]any, outputs Outputs, service string) error {
	svc, err := findRemoteService(cfg, service)
	if err != nil {
		return err
	}

	ip := outputs["public_ip"]
	keyPath := targetKeyPath(outputs)
	user := outputs["ssh_user"]
	if user == "" {
		user = "ubuntu"
	}

	downCmd := fmt.Sprintf("docker compose -f %s down --volumes", remoteServiceDir(svc))
	if err := RunSSH(ctx, r.Out, keyPath, user, ip, downCmd); err != nil {
		return fmt.Errorf("docker compose down: %w", err)
	}
	return nil
}

func findRemoteService(cfg map[string]any, name string) (schema.ServiceDef, error) {
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

func remoteServiceDir(svc schema.ServiceDef) string {
	base := filepath.Base(svc.Definition)
	if strings.HasSuffix(base, ".tmpl.yml") {
		base = composeRenderedBase(base, svc.Name)
	}
	return remoteServicesDir + "/" + svc.Name + "/" + base
}

func varToEnvKey(k string) string {
	// Avoid clashing with standard shell environment variables. Docker Compose
	// shell env vars supersede .env file values, so "user" must not map to
	// "USER" (always set to the SSH login name on target machines).
	switch k {
	case "user":
		return "PG_USER"
	case "password":
		return "PG_PASSWORD"
	}
	return strings.ToUpper(strings.ReplaceAll(k, "-", "_"))
}

// copyTargetKeyToDriver copies the target SSH private key to the driver instance
// and returns the driver-side absolute path. If localKeyPath is empty, inaccessible,
// or driverOutputs has no public_ip, the original localKeyPath is returned unchanged.
// The caller should update all output maps with the returned path before persisting state.
func (r *Runner) copyTargetKeyToDriver(ctx context.Context, localKeyPath string, driverOutputs Outputs) (string, error) {
	if localKeyPath == "" {
		return localKeyPath, nil
	}
	driverIP := driverOutputs["public_ip"]
	if driverIP == "" {
		return localKeyPath, nil
	}
	if _, err := os.Stat(localKeyPath); err != nil {
		// Already a remote path, or otherwise inaccessible locally, so nothing to copy.
		return localKeyPath, nil
	}
	content, err := os.ReadFile(localKeyPath)
	if err != nil {
		return "", fmt.Errorf("read target key: %w", err)
	}
	driverUser := driverOutputs["ssh_user"]
	if driverUser == "" {
		driverUser = "ubuntu"
	}
	driverKeyPath := driverOutputs["ssh_private_key_path"]
	remoteKeyPath := "/home/" + driverUser + "/target.pem"
	encoded := base64.StdEncoding.EncodeToString(content)
	cmd := fmt.Sprintf("echo %s | base64 -d > %s && chmod 600 %s", encoded, remoteKeyPath, remoteKeyPath)
	r.logf("Copying target SSH key to driver")
	if err := WaitForSSH(ctx, r.Out, driverIP, 5*time.Minute); err != nil {
		return "", fmt.Errorf("wait for driver SSH: %w", err)
	}
	if err := RunSSH(ctx, r.Out, driverKeyPath, driverUser, driverIP, cmd); err != nil {
		return "", fmt.Errorf("copy target key to driver: %w", err)
	}
	return remoteKeyPath, nil
}
