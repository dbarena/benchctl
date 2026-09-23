// Package gcpcloudsql adapts the deployments/gcp-cloudsql/postgres module to
// benchctl's generic opentofu provider. Its opentofu.PreApplyFunc adopts the
// module's shared, never-destroyed VPC network and peering resources into each
// run's otherwise empty tofu state via `tofu import` before every apply.
package gcpcloudsql

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/dbarena/benchctl/internal/providers/opentofu"
)

// PreApply implements opentofu.PreApplyFunc and is safe to wire in at every
// opentofu.New() call site: it returns vars unchanged unless
// cfg["gcp_network_name"] is set, which only the Cloud SQL module does.
//
// When it does act, it resolves the GCP project id from vars (falling back to
// GOOGLE_CLOUD_PROJECT, then gcloud's active config) and adopts the module's
// shared network and peering resources into the run's tofu state. It also sets
// vars["wait_for_peering_propagation"], which gates the module's 180s
// propagation wait: false when the peering connection was adopted from an
// earlier run, true when the following apply creates it fresh.
func PreApply(ctx context.Context, out io.Writer, run opentofu.RunFunc, cfg, vars map[string]any) (map[string]any, error) {
	networkName, _ := cfg["gcp_network_name"].(string)
	if networkName == "" {
		return vars, nil
	}
	if vars == nil {
		vars = make(map[string]any)
	}

	fmt.Fprintf(out, "==> gcpcloudsql: importing existing GCP network resources (if any) for %q\n", networkName)

	projectID, err := resolveGCPProjectID(ctx, vars)
	if err != nil {
		return nil, err
	}
	region, _ := vars["region"].(string)
	if region == "" {
		region = "us-central1" // matches the module's own variables.tf default
	}
	peeringAdopted := importSharedGCPNetworkResources(ctx, out, run, projectID, networkName, region)
	vars["wait_for_peering_propagation"] = !peeringAdopted
	return vars, nil
}

// resolveGCPProjectID determines the GCP project id to use for the pre-apply
// shared-network import step, mirroring exactly what the Terraform google
// provider itself does when project_id is left empty
// (deployments/gcp-cloudsql/postgres/main.tf's provider block): prefer an
// explicit vars["project_id"], then GOOGLE_CLOUD_PROJECT, then gcloud's
// active config.
func resolveGCPProjectID(ctx context.Context, vars map[string]any) (string, error) {
	if v, ok := vars["project_id"].(string); ok && v != "" {
		return v, nil
	}
	// GOOGLE_CLOUD_PROJECT is a standard GCP convention (read directly by the
	// Google Cloud SDK and client libraries, not a benchctl-specific
	// variable), so it is exempt from going through internal/config.
	if v := os.Getenv("GOOGLE_CLOUD_PROJECT"); v != "" {
		return v, nil
	}
	out, err := exec.CommandContext(ctx, "gcloud", "config", "get-value", "project").Output()
	if err != nil {
		return "", fmt.Errorf("resolve GCP project id: %w", err)
	}
	project := strings.TrimSpace(string(out))
	if project == "" || project == "(unset)" {
		return "", fmt.Errorf("resolve GCP project id: no project_id var, GOOGLE_CLOUD_PROJECT, or gcloud active project set")
	}
	return project, nil
}

// importSharedGCPNetworkResources runs `tofu import` for each shared network
// and peering resource declared in deployments/gcp-cloudsql/postgres/main.tf,
// adopting the ones that already exist in GCP into this run's otherwise-empty
// tofu state. The module's own convention is that the subnetwork shares the
// network's name.
//
// It logs import errors rather than returning them, because the usual cause is
// that the resource does not exist yet and the apply that follows creates it.
// An auth or permission problem surfaces on that apply instead.
//
// It reports whether the google_service_networking_connection import
// succeeded, which decides whether the module still needs its 180s
// propagation wait.
func importSharedGCPNetworkResources(ctx context.Context, out io.Writer, run opentofu.RunFunc, projectID, networkName, region string) (peeringAdopted bool) {
	imports := []struct {
		address string
		id      string
	}{
		{"google_compute_network.this", networkName},
		{"google_compute_subnetwork.this", fmt.Sprintf("%s/%s", region, networkName)},
		{"google_compute_global_address.private_ip_alloc", networkName + "-psa"},
		// Use the fully-qualified network id, not the shorter
		// "<network>:<service>" form. Both import successfully, but the short
		// form stores `network` in state as a bare name where a real Create
		// stores the full self-link path. The next plan then sees `network` as
		// changed and forces a destroy+recreate of the peering connection,
		// the disruption this import step exists to avoid. That destroy can
		// also hang upstream (hashicorp/terraform-provider-google#16275, see
		// the comment in deployments/gcp-cloudsql/postgres/main.tf).
		// Confirmed against a real GCP project.
		{
			"google_service_networking_connection.this",
			fmt.Sprintf("projects/%s/global/networks/%s:servicenetworking.googleapis.com", projectID, networkName),
		},
	}

	for i, imp := range imports {
		err := run(ctx, "import", "-input=false", imp.address, imp.id)
		if i == len(imports)-1 {
			peeringAdopted = err == nil
		}
		if err != nil {
			fmt.Fprintf(out, "==> gcpcloudsql: %s not found (or already exists elsewhere); apply will create it fresh\n", imp.address)
		}
	}
	return peeringAdopted
}
