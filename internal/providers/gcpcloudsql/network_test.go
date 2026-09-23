package gcpcloudsql

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

// fakeGcloud installs an executable shell script named "gcloud" on PATH for
// the duration of the test, so resolveGCPProjectID can be tested without a
// real gcloud installation or network access. body is the script's shell
// body (after the shebang).
func fakeGcloud(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gcloud script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := dir + "/gcloud"
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResolveGCPProjectID_PrefersVars(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
	got, err := resolveGCPProjectID(context.Background(), map[string]any{"project_id": "vars-project"})
	if err != nil {
		t.Fatalf("resolveGCPProjectID: %v", err)
	}
	if got != "vars-project" {
		t.Errorf("got %q, want %q", got, "vars-project")
	}
}

func TestResolveGCPProjectID_FallsBackToEnv(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")
	got, err := resolveGCPProjectID(context.Background(), map[string]any{"project_id": ""})
	if err != nil {
		t.Fatalf("resolveGCPProjectID: %v", err)
	}
	if got != "env-project" {
		t.Errorf("got %q, want %q", got, "env-project")
	}
}

func TestResolveGCPProjectID_FallsBackToGcloud(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	fakeGcloud(t, `echo gcloud-active-project`)

	got, err := resolveGCPProjectID(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("resolveGCPProjectID: %v", err)
	}
	if got != "gcloud-active-project" {
		t.Errorf("got %q, want %q", got, "gcloud-active-project")
	}
}

func TestResolveGCPProjectID_ErrorsWhenNothingResolves(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	fakeGcloud(t, `echo "(unset)"`)

	if _, err := resolveGCPProjectID(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

// recordingRun returns a RunFunc that logs every call's args (space-joined,
// one entry per call) into *calls, and fails whenever exitErr's predicate
// matches the args, letting tests simulate `tofu import` succeeding or
// failing per-resource without shelling out to a real tofu binary.
func recordingRun(calls *[]string, fails func(args []string) bool) func(ctx context.Context, args ...string) error {
	return func(_ context.Context, args ...string) error {
		*calls = append(*calls, strings.Join(args, " "))
		if fails != nil && fails(args) {
			return fmt.Errorf("import failed")
		}
		return nil
	}
}

func TestImportSharedGCPNetworkResources_AllAdopted(t *testing.T) {
	var calls []string
	run := recordingRun(&calls, nil)

	peeringAdopted := importSharedGCPNetworkResources(context.Background(), io.Discard, run, "my-project", "benchctl-gcp-cloudsql-shared", "us-central1")

	if !peeringAdopted {
		t.Errorf("peeringAdopted = false, want true (all imports succeeded)")
	}

	wantCalls := []string{
		"import -input=false google_compute_network.this benchctl-gcp-cloudsql-shared",
		"import -input=false google_compute_subnetwork.this us-central1/benchctl-gcp-cloudsql-shared",
		"import -input=false google_compute_global_address.private_ip_alloc benchctl-gcp-cloudsql-shared-psa",
		"import -input=false google_service_networking_connection.this projects/my-project/global/networks/benchctl-gcp-cloudsql-shared:servicenetworking.googleapis.com",
	}
	for _, want := range wantCalls {
		found := false
		for _, got := range calls {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("invocation log missing call %q\ngot calls:\n%s", want, strings.Join(calls, "\n"))
		}
	}
}

func TestImportSharedGCPNetworkResources_NoneExistYet(t *testing.T) {
	var calls []string
	run := recordingRun(&calls, func([]string) bool { return true }) // every import fails, as it would on a genuine first run

	peeringAdopted := importSharedGCPNetworkResources(context.Background(), io.Discard, run, "my-project", "benchctl-gcp-cloudsql-shared", "us-central1")

	if peeringAdopted {
		t.Errorf("peeringAdopted = true, want false (no imports succeeded)")
	}
}

func TestImportSharedGCPNetworkResources_OnlyPeeringMissing(t *testing.T) {
	// Simulates an unlikely but possible partial-adoption state: the network/
	// subnet/address exist but the peering connection doesn't yet.
	var calls []string
	run := recordingRun(&calls, func(args []string) bool {
		for _, a := range args {
			if strings.Contains(a, "google_service_networking_connection") {
				return true
			}
		}
		return false
	})

	peeringAdopted := importSharedGCPNetworkResources(context.Background(), io.Discard, run, "my-project", "benchctl-gcp-cloudsql-shared", "us-central1")

	if peeringAdopted {
		t.Errorf("peeringAdopted = true, want false (peering import failed)")
	}
}

func TestPreApply_NoOpWithoutNetworkName(t *testing.T) {
	var calls []string
	run := recordingRun(&calls, nil)

	vars, err := PreApply(context.Background(), io.Discard, run, map[string]any{}, map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("PreApply: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("expected no tofu import calls, got %v", calls)
	}
	if vars["foo"] != "bar" {
		t.Errorf("vars mutated unexpectedly: %v", vars)
	}
}

func TestPreApply(t *testing.T) {
	tests := []struct {
		name               string
		peeringExists      bool
		wantWaitForPeering bool
	}{
		{name: "peering adopted", peeringExists: true, wantWaitForPeering: false},
		{name: "peering created fresh", peeringExists: false, wantWaitForPeering: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			run := recordingRun(&calls, func([]string) bool { return !tt.peeringExists })

			cfg := map[string]any{"gcp_network_name": "benchctl-gcp-cloudsql-shared"}
			vars, err := PreApply(context.Background(), io.Discard, run, cfg, map[string]any{"project_id": "my-project"})
			if err != nil {
				t.Fatalf("PreApply: %v", err)
			}
			if vars["wait_for_peering_propagation"] != tt.wantWaitForPeering {
				t.Errorf("vars[\"wait_for_peering_propagation\"] = %v, want %v", vars["wait_for_peering_propagation"], tt.wantWaitForPeering)
			}
		})
	}
}
