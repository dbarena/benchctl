package gotpc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

// ---- buildCmd tests ----

// fullOutputs returns a connection map with all required fields populated;
// individual tests override specific keys to focus on the behavior under test.
func fullOutputs() engine.Outputs {
	return engine.Outputs{
		"host":     "db.example.com",
		"port":     "5433",
		"user":     "alice",
		"password": "secret",
		"db":       "mydb",
	}
}

func TestBuildCmd_ExecutableAndSubcommands(t *testing.T) {
	step := schema.SuiteStep{Command: "prepare"}
	name, args, err := buildCmd(fullOutputs(), step)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	if name != "go-tpc" {
		t.Errorf("name = %q, want go-tpc", name)
	}
	if len(args) < 2 || args[0] != "tpcc" || args[1] != "prepare" {
		t.Errorf("subcommands not at start of args: %v", args)
	}
}

func TestBuildCmd_ConnectionFromOutputs(t *testing.T) {
	outputs := fullOutputs()
	outputs["sslmode"] = "require"
	_, args, err := buildCmd(outputs, schema.SuiteStep{Command: "run"})
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}

	assertFlag(t, args, "--driver", "postgres")
	assertFlag(t, args, "-H", "db.example.com")
	assertFlag(t, args, "-P", "5433")
	assertFlag(t, args, "-U", "alice")
	assertFlag(t, args, "-p", "secret")
	assertFlag(t, args, "-D", "mydb")
	assertFlag(t, args, "--conn-params", "sslmode=require")
}

func TestBuildCmd_SslmodeDefaultsToDisable(t *testing.T) {
	_, args, err := buildCmd(fullOutputs(), schema.SuiteStep{Command: "run"})
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	assertFlag(t, args, "--conn-params", "sslmode=disable")
}

func TestBuildCmd_SslNegotiationFromOutputs(t *testing.T) {
	outputs := fullOutputs()
	outputs["sslmode"] = "require"
	outputs["sslnegotiation"] = "direct"
	_, args, err := buildCmd(outputs, schema.SuiteStep{Command: "run"})
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	assertFlag(t, args, "--conn-params", "sslmode=require&sslnegotiation=direct")
}

func TestBuildCmd_SslNegotiationAbsentWithoutOutput(t *testing.T) {
	outputs := fullOutputs()
	outputs["sslmode"] = "require"
	_, args, err := buildCmd(outputs, schema.SuiteStep{Command: "run"})
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	assertFlag(t, args, "--conn-params", "sslmode=require")
}

func TestBuildCmd_MissingConnectionParamsErrors(t *testing.T) {
	for _, k := range []string{"host", "port", "user", "password", "db"} {
		t.Run(k, func(t *testing.T) {
			outputs := fullOutputs()
			delete(outputs, k)
			_, _, err := buildCmd(outputs, schema.SuiteStep{Command: "run"})
			if err == nil {
				t.Fatalf("expected error for missing %s", k)
			}
			if !strings.Contains(err.Error(), k) {
				t.Errorf("error %q should mention missing field %q", err, k)
			}
		})
	}
}

func TestBuildCmd_StepArgsAsDoubleDashFlags(t *testing.T) {
	step := schema.SuiteStep{
		Command: "run",
		Args:    map[string]string{"warehouses": "50", "threads": "8"},
	}
	_, args, err := buildCmd(fullOutputs(), step)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}

	assertContains(t, args, "--warehouses=50")
	assertContains(t, args, "--threads=8")
}

func TestBuildCmd_BoolFlagEmitsBareFlag(t *testing.T) {
	step := schema.SuiteStep{
		Command: "prepare",
		Args:    map[string]string{"no-check": ""},
	}
	_, args, err := buildCmd(fullOutputs(), step)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}

	assertContains(t, args, "--no-check")
	// must not emit "--no-check=" or a spurious value token
	for _, a := range args {
		if strings.HasPrefix(a, "--no-check=") {
			t.Errorf("expected bare --no-check, got %q", a)
		}
	}
}

func TestBuildCmd_StepArgsSorted(t *testing.T) {
	// Verify deterministic ordering of workload flags.
	step := schema.SuiteStep{
		Command: "run",
		Args:    map[string]string{"threads": "4", "time": "5m", "warehouses": "10"},
	}
	_, args, err := buildCmd(fullOutputs(), step)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}

	// Find positions of the three --flags (now --key=value tokens)
	pos := map[string]int{}
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--threads="):
			pos["--threads"] = i
		case strings.HasPrefix(a, "--time="):
			pos["--time"] = i
		case strings.HasPrefix(a, "--warehouses="):
			pos["--warehouses"] = i
		}
	}
	if pos["--threads"] > pos["--time"] || pos["--time"] > pos["--warehouses"] {
		t.Errorf("args not sorted: positions %v in %v", pos, args)
	}
}

func TestBuildCmd_EmptyCommand(t *testing.T) {
	// Empty command short-circuits before connection validation.
	name, args, err := buildCmd(engine.Outputs{}, schema.SuiteStep{})
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	if name != "go-tpc" {
		t.Errorf("name = %q, want go-tpc", name)
	}
	if len(args) != 0 {
		t.Errorf("expected empty args, got %v", args)
	}
}

// sampleOutput is generic captured go-tpc stdout, used as the fake process
// output in Prepare/Run tests below (mockRunnerWithScratchFiles, in
// summary_file_test.go, is what actually supplies structured metrics now).
const sampleOutput = `
[Current] NEW_ORDER - Takes(s): 10.0, Count: 48, TPM: 288.0
[Summary] NEW_ORDER - Takes(s): 299.9, Count: 1290, TPM: 258.1
tpm: 258.1
`

// findPoint returns the value of the first MetricPoint matching family and all
// provided labels, and whether it was found.
func findPoint(m engine.Metrics, family string, labels map[string]string) (float64, bool) {
	sm, ok := m[engine.StructuredKey].(engine.StructuredMetrics)
	if !ok {
		return 0, false
	}
	for _, p := range sm.Points {
		if p.Family != family {
			continue
		}
		match := true
		for k, v := range labels {
			if p.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return p.Value, true
		}
	}
	return 0, false
}

// ---- Prepare / Run tests ----

func TestPrepare_Success(t *testing.T) {
	var capturedName string
	var capturedArgs []string
	mock := func(_ context.Context, _ io.Writer, name string, args ...string) ([]byte, error) {
		capturedName = name
		capturedArgs = args
		return []byte("prepare ok"), nil
	}
	a := newWithRunner(mock)
	step := schema.SuiteStep{
		Command: "prepare",
		Args:    map[string]string{"warehouses": "10"},
	}
	metrics, err := a.Run(context.Background(), fullOutputs(), step)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if capturedName != "go-tpc" {
		t.Errorf("executable = %q, want go-tpc", capturedName)
	}
	assertContains(t, capturedArgs, "--warehouses=10")
	if len(metrics) != 0 {
		t.Errorf("metrics = %v, want empty: a prepare step measures nothing", metrics)
	}
	for _, arg := range capturedArgs {
		if strings.HasPrefix(arg, "--summary-file") || strings.HasPrefix(arg, "--raw-samples-file") {
			t.Errorf("prepare passed %q; measurement files belong to a run step", arg)
		}
	}
}

func TestPrepare_Failure(t *testing.T) {
	mock := func(_ context.Context, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return []byte("error output"), errors.New("exit 1")
	}
	a := newWithRunner(mock)
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{Command: "prepare"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "go-tpc prepare") {
		t.Errorf("error %q missing 'go-tpc prepare'", err)
	}
}

func TestRun_Success(t *testing.T) {
	a := newWithRunner(mockRunnerWithScratchFiles(sampleSummaryJSON, sampleRawSamplesCSV))
	step := schema.SuiteStep{
		Command: "run",
		Args:    map[string]string{"warehouses": "10", "threads": "4", "time": "5m"},
	}
	metrics, err := a.Run(context.Background(), fullOutputs(), step)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v, ok := findPoint(metrics, "tpcc_tpm", map[string]string{"transaction": "NEW_ORDER", "status": "ok"}); !ok || v != 258.1 {
		t.Errorf("tpcc_tpm NEW_ORDER = %v (ok=%v), want 258.1", v, ok)
	}
}

func TestRun_Failure(t *testing.T) {
	mock := func(_ context.Context, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return []byte("fatal error"), errors.New("exit 1")
	}
	a := newWithRunner(mock)
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{Command: "run"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "go-tpc run") {
		t.Errorf("error %q missing 'go-tpc run'", err)
	}
}

// ---- VersionInfo tests ----

func TestVersionInfo_Success(t *testing.T) {
	const sample = "2026/09/07 08:43:53 maxprocs: Leaving GOMAXPROCS=10: CPU quota undefined\n" +
		"Git Commit Hash: eb6de81936ece341c9d5ed35217d4b460552954b\n" +
		"UTC Build Time: 2026-08-24 11:46:30\n" +
		"Release version: latest-20-geb6de81\n"
	mock := func(_ context.Context, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return []byte(sample), nil
	}
	a := newWithRunner(mock)
	info := a.VersionInfo(context.Background())
	if got := info["gotpc_version"]; got != "latest-20-geb6de81" {
		t.Errorf("gotpc_version = %q, want latest-20-geb6de81", got)
	}
}

func TestVersionInfo_Failure(t *testing.T) {
	mock := func(_ context.Context, _ io.Writer, _ string, _ ...string) ([]byte, error) {
		return []byte("error output"), errors.New("exit 1")
	}
	a := newWithRunner(mock)
	if info := a.VersionInfo(context.Background()); info != nil {
		t.Errorf("VersionInfo = %v, want nil", info)
	}
}

// ---- tailBuffer tests ----

func TestTailBuffer_UnderCap_ReturnsVerbatim(t *testing.T) {
	tb := &tailBuffer{}
	if _, err := tb.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := string(tb.Bytes()); got != "hello" {
		t.Errorf("Bytes() = %q, want %q", got, "hello")
	}
}

func TestTailBuffer_OverCap_KeepsOnlyTheTail(t *testing.T) {
	tb := &tailBuffer{}
	// Write well past maxTailBytes, in chunks, as execRun's io.MultiWriter would.
	chunk := strings.Repeat("a", 1024)
	var want strings.Builder
	for i := 0; i < 100; i++ {
		if _, err := tb.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		want.WriteString(chunk)
	}

	got := tb.Bytes()
	if len(got) > maxTailBytes+len("...[truncated]...\n") {
		t.Errorf("Bytes() length = %d, want at most %d", len(got), maxTailBytes+len("...[truncated]...\n"))
	}
	if !strings.HasPrefix(string(got), "...[truncated]...\n") {
		t.Errorf("Bytes() = %q, want truncation marker prefix", string(got))
	}
	wantTail := want.String()
	wantTail = wantTail[len(wantTail)-maxTailBytes:]
	if !strings.HasSuffix(string(got), wantTail) {
		t.Error("Bytes() does not end with the actual tail of what was written")
	}
}

// ---- helpers ----

// assertFlag checks that args contains the two-token sequence [flag, value].
// Use this for connection flags (-H, -P, --driver, --conn-params) which keep
// the space-separated form. For workload args use assertContains.
func assertFlag(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("flag %s %s not found in args: %v", flag, value, args)
}

// assertContains checks that args contains the exact token s.
func assertContains(t *testing.T, args []string, s string) {
	t.Helper()
	for _, a := range args {
		if a == s {
			return
		}
	}
	t.Errorf("%q not found in args: %v", s, args)
}
