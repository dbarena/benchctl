package pgbench

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
	"github.com/dbarena/benchctl/internal/schema"
)

// Outputs of pgbench 17.12, captured verbatim.
const (
	timedOutput = `pgbench (17.12 (Homebrew))
progress: 1.0 s, 16520.0 tps, lat 0.241 ms stddev 0.093, 0 failed
progress: 2.0 s, 17197.9 tps, lat 0.232 ms stddev 0.104, 0 failed
progress: 3.0 s, 16718.0 tps, lat 0.239 ms stddev 0.092, 0 failed
transaction type: <builtin: TPC-B (sort of)>
scaling factor: 2
query mode: simple
number of clients: 4
number of threads: 2
maximum number of tries: 1
duration: 3 s
number of transactions actually processed: 50464
number of failed transactions: 0 (0.000%)
latency average = 0.237 ms
latency stddev = 0.097 ms
initial connection time = 2.496 ms
tps = 16826.471016 (without initial connection time)
`
	countedOutput = `pgbench (17.12 (Homebrew))
starting vacuum...end.
transaction type: <builtin: select only>
scaling factor: 2
query mode: prepared
number of clients: 2
number of threads: 1
maximum number of tries: 1
number of transactions per client: 500
number of transactions actually processed: 1000/1000
number of failed transactions: 0 (0.000%)
latency average = 0.018 ms
initial connection time = 2.219 ms
tps = 114155.251142 (without initial connection time)
`
	rateLimitedOutput = `pgbench (17.12 (Homebrew))
progress: 1.0 s, 514.7 tps, lat 0.577 ms stddev 0.439, 0 failed, lag 0.403 ms, 6 skipped
progress: 2.0 s, 516.3 tps, lat 0.562 ms stddev 0.435, 0 failed, lag 0.393 ms, 0 skipped
transaction type: <builtin: select only>
scaling factor: 2
query mode: simple
number of clients: 2
number of threads: 1
maximum number of tries: 1
duration: 2 s
number of transactions actually processed: 1032
number of failed transactions: 0 (0.000%)
number of transactions skipped: 6 (0.578%)
number of transactions above the 1.0 ms latency limit: 148/1032 (14.341%)
latency average = 0.570 ms
latency stddev = 0.437 ms
rate limit schedule lag: avg 0.398 (max 3.432) ms
initial connection time = 2.433 ms
tps = 516.596669 (without initial connection time)
`
	abortedOutput = `pgbench (17.12 (Homebrew))
pgbench: error: client 1 script 0 aborted in command 0 query 0: ERROR:  relation "no_such_table" does not exist
transaction type: /tmp/bad.sql
scaling factor: 1
query mode: simple
number of clients: 2
number of threads: 1
maximum number of tries: 1
duration: 2 s
number of transactions actually processed: 0
number of failed transactions: 0 (NaN%)
pgbench: error: Run was aborted; the above results are incomplete.
`
	// pgbench 13 and older: two tps lines, no failed counts, no
	// initial connection time.
	legacyOutput = `transaction type: <builtin: TPC-B (sort of)>
scaling factor: 10
query mode: prepared
number of clients: 8
number of threads: 8
duration: 60 s
number of transactions actually processed: 600000
latency average = 0.800 ms
tps = 9990.000000 (including connections establishing)
tps = 10000.000000 (excluding connections establishing)
`
)

func fullOutputs() engine.Outputs {
	return engine.Outputs{
		"host":     "db.example.com",
		"port":     "5433",
		"user":     "alice",
		"password": "secret",
		"db":       "mydb",
	}
}

// call records one cmdRunner invocation.
type call struct {
	env    []string
	name   string
	args   []string
	script string // content of the --file script, read while the run was live
}

// fakeRunner streams output to the writer the adapter passes, like execRun,
// and records what it was asked to run.
func fakeRunner(output string, err error, calls *[]call) cmdRunner {
	return func(_ context.Context, out io.Writer, env []string, name string, args ...string) ([]byte, error) {
		c := call{env: env, name: name, args: args}
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "--file="); ok {
				data, rerr := os.ReadFile(path)
				if rerr != nil {
					return nil, rerr
				}
				c.script = string(data)
			}
		}
		*calls = append(*calls, c)
		if out != nil {
			io.WriteString(out, output)
		}
		return []byte(output), err
	}
}

// ---- Run ----

func TestRun_InitPassesInitializeAndOptions(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner("done in 0.15 s\n", nil, &calls))
	m, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{
		Name: "load", Command: schema.PgbenchCommandInit,
		Args: map[string]string{"scale": "100", "init-steps": "dtGvp", "unlogged-tables": ""},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("init returned metrics %v, want none", m)
	}
	want := []string{"--initialize", "--init-steps=dtGvp", "--scale=100", "--unlogged-tables"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("args = %v, want %v", calls[0].args, want)
	}
}

func TestRun_InitRejectsScript(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner("", nil, &calls))
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{
		Name: "load", Command: schema.PgbenchCommandInit, Args: map[string]string{schema.PgbenchArgScript: "SELECT 1;"},
	})
	if err == nil || !strings.Contains(err.Error(), schema.PgbenchArgScript) {
		t.Fatalf("err = %v, want one naming args.%s", err, schema.PgbenchArgScript)
	}
	if len(calls) != 0 {
		t.Errorf("pgbench was run %d times, want 0", len(calls))
	}
}

func TestRun_RunReportsSummaryAndProgress(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner(timedOutput, nil, &calls))
	m, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{
		Name: "bench", Command: schema.PgbenchCommandRun,
		Args: map[string]string{"client": "4", "jobs": "2", "time": "3", "no-vacuum": ""},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"--client=4", "--jobs=2", "--no-vacuum", "--time=3", "--progress=1"}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("args = %v, want %v", calls[0].args, want)
	}

	sm, ok := m[engine.StructuredKey].(engine.StructuredMetrics)
	if !ok {
		t.Fatalf("no structured metrics in %v", m)
	}
	assertPoint(t, sm, "pgbench_tps", nil, 16826.471016)
	assertPoint(t, sm, "pgbench_transactions", nil, 50464)
	assertPoint(t, sm, "pgbench_failed_transactions", nil, 0)
	assertPoint(t, sm, "pgbench_latency_ms", map[string]string{"stat": "avg"}, 0.237)
	assertPoint(t, sm, "pgbench_latency_ms", map[string]string{"stat": "stddev"}, 0.097)
	assertPoint(t, sm, "pgbench_connection_time_ms", nil, 2.496)
	assertPoint(t, sm, "pgbench_duration_seconds", nil, 3)

	wantCSV := progressHeader + "\n" +
		"1.0,16520.0,0.241,0.093,0\n" +
		"2.0,17197.9,0.232,0.104,0\n" +
		"3.0,16718.0,0.239,0.092,0\n"
	if got := m["raw_samples_csv"]; got != wantCSV {
		t.Errorf("raw_samples_csv =\n%v\nwant\n%v", got, wantCSV)
	}
}

func TestRun_ScenarioProgressIntervalWins(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner(timedOutput, nil, &calls))
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{
		Command: schema.PgbenchCommandRun, Args: map[string]string{"progress": "10"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(calls[0].args, []string{"--progress=10"}) {
		t.Errorf("args = %v, want only --progress=10", calls[0].args)
	}
}

func TestRun_ScriptGoesThroughAScratchFile(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner(timedOutput, nil, &calls))
	script := "\\set aid random(1, 100000 * :scale)\nSELECT abalance FROM pgbench_accounts WHERE aid = :aid;\n"
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{
		Command: schema.PgbenchCommandRun, Args: map[string]string{schema.PgbenchArgScript: script, "scale": "10"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := calls[0]
	if c.script != script {
		t.Errorf("script file held %q, want %q", c.script, script)
	}
	var path string
	for _, arg := range c.args {
		if strings.HasPrefix(arg, "--"+schema.PgbenchArgScript) {
			t.Errorf("args.%s leaked through as %q", schema.PgbenchArgScript, arg)
		}
		if p, ok := strings.CutPrefix(arg, "--file="); ok {
			path = p
		}
	}
	if path == "" {
		t.Fatalf("no --file in args %v", c.args)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("script file %s still exists after the run (stat err %v)", path, err)
	}
}

func TestRun_ConnectionThroughEnvironment(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner(timedOutput, nil, &calls))
	outputs := fullOutputs()
	outputs["sslmode"] = "require"
	outputs["sslnegotiation"] = "direct"
	if _, err := a.Run(context.Background(), outputs, schema.SuiteStep{Command: schema.PgbenchCommandRun}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := calls[0]
	for _, want := range []string{
		"PGHOST=db.example.com", "PGPORT=5433", "PGUSER=alice", "PGPASSWORD=secret",
		"PGDATABASE=mydb", "PGSSLMODE=require", "PGSSLNEGOTIATION=direct",
	} {
		if !slices.Contains(c.env, want) {
			t.Errorf("env %v lacks %s", c.env, want)
		}
	}
	for _, arg := range c.args {
		if strings.Contains(arg, "secret") {
			t.Errorf("password on the command line: %v", c.args)
		}
	}
}

func TestRun_SslmodeDefaultsToDisable(t *testing.T) {
	env, err := connEnv(fullOutputs())
	if err != nil {
		t.Fatalf("connEnv: %v", err)
	}
	if !slices.Contains(env, "PGSSLMODE=disable") {
		t.Errorf("env %v lacks PGSSLMODE=disable", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "PGSSLNEGOTIATION=") {
			t.Errorf("unexpected %s without sslnegotiation output", e)
		}
	}
}

func TestRun_MissingConnectionParams(t *testing.T) {
	var calls []call
	a := newWithRunner(fakeRunner(timedOutput, nil, &calls))
	outputs := fullOutputs()
	delete(outputs, "password")
	delete(outputs, "db")
	_, err := a.Run(context.Background(), outputs, schema.SuiteStep{Command: schema.PgbenchCommandRun})
	if err == nil || !strings.Contains(err.Error(), "password") || !strings.Contains(err.Error(), "db") {
		t.Fatalf("err = %v, want one naming password and db", err)
	}
	if len(calls) != 0 {
		t.Errorf("pgbench was run %d times, want 0", len(calls))
	}
}

func TestRun_ReservedArgsRejected(t *testing.T) {
	for _, k := range reservedArgs {
		t.Run(k, func(t *testing.T) {
			var calls []call
			a := newWithRunner(fakeRunner(timedOutput, nil, &calls))
			_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{
				Name: "bench", Command: schema.PgbenchCommandRun, Args: map[string]string{k: "x"},
			})
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Fatalf("err = %v, want one naming %q", err, k)
			}
			if len(calls) != 0 {
				t.Errorf("pgbench was run %d times, want 0", len(calls))
			}
		})
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	a := newWithRunner(fakeRunner("", nil, new([]call)))
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{Name: "x", Command: "prepare"})
	if err == nil || !strings.Contains(err.Error(), `"prepare"`) {
		t.Fatalf("err = %v, want one naming the command", err)
	}
}

func TestRun_FailedRunReturnsOutput(t *testing.T) {
	a := newWithRunner(fakeRunner(abortedOutput, errors.New("exit status 2"), new([]call)))
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{Command: schema.PgbenchCommandRun})
	if err == nil || !strings.Contains(err.Error(), "Run was aborted") {
		t.Fatalf("err = %v, want one carrying pgbench's output", err)
	}
}

func TestRun_NoSummaryIsAnError(t *testing.T) {
	a := newWithRunner(fakeRunner("pgbench (17.12)\n", nil, new([]call)))
	_, err := a.Run(context.Background(), fullOutputs(), schema.SuiteStep{Command: schema.PgbenchCommandRun})
	if err == nil || !strings.Contains(err.Error(), "no run summary") {
		t.Fatalf("err = %v, want a missing-summary error", err)
	}
}

// ---- parseSummary ----

func TestParseSummary_CountedRun(t *testing.T) {
	s, err := parseSummary([]byte(countedOutput))
	if err != nil {
		t.Fatalf("parseSummary: %v", err)
	}
	if s.transactions != 1000 || s.tps != 114155.251142 {
		t.Errorf("transactions, tps = %v, %v; want 1000, 114155.251142", s.transactions, s.tps)
	}
	if s.hasLatencyStddev || s.hasDuration {
		t.Errorf("stddev/duration reported for a run that printed neither: %+v", s)
	}
}

func TestParseSummary_RateLimited(t *testing.T) {
	s, err := parseSummary([]byte(rateLimitedOutput))
	if err != nil {
		t.Fatalf("parseSummary: %v", err)
	}
	if s.tps != 516.596669 || s.transactions != 1032 || s.latencyAvgMs != 0.570 {
		t.Errorf("summary = %+v", s)
	}
}

func TestParseSummary_LegacyPrefersExcludingConnections(t *testing.T) {
	s, err := parseSummary([]byte(legacyOutput))
	if err != nil {
		t.Fatalf("parseSummary: %v", err)
	}
	if s.tps != 10000 {
		t.Errorf("tps = %v, want the excluding-connections figure 10000", s.tps)
	}
	if s.hasFailed || s.hasConnTime {
		t.Errorf("failed/connection time reported for a version that prints neither: %+v", s)
	}
	sm := s.structured()
	for _, p := range sm.Points {
		if p.Family == "pgbench_failed_transactions" || p.Family == "pgbench_connection_time_ms" {
			t.Errorf("unexpected point %s", p.Family)
		}
	}
}

func TestParseSummary_AbortedRunHasNoSummary(t *testing.T) {
	if _, err := parseSummary([]byte(abortedOutput)); err == nil {
		t.Fatal("parseSummary accepted an aborted run without tps")
	}
}

// ---- progressWriter ----

func TestProgressWriter_SplitWritesAndOlderFormat(t *testing.T) {
	p := &progressWriter{}
	// pgbench 14 has no failed count; the report arrives in two writes.
	io.WriteString(p, "progress: 5.0 s, 100.0 tp")
	io.WriteString(p, "s, lat 1.500 ms stddev 0.200\nnot a report\n")
	io.WriteString(p, rateLimitedOutput)
	want := progressHeader + "\n" +
		"5.0,100.0,1.500,0.200,0\n" +
		"1.0,514.7,0.577,0.439,0\n" +
		"2.0,516.3,0.562,0.435,0\n"
	if got := p.csv(); got != want {
		t.Errorf("csv =\n%s\nwant\n%s", got, want)
	}
}

func TestProgressWriter_NoReports(t *testing.T) {
	p := &progressWriter{}
	io.WriteString(p, countedOutput)
	if got := p.csv(); got != "" {
		t.Errorf("csv = %q, want empty", got)
	}
}

// ---- VersionInfo / tools ----

func TestVersionInfo(t *testing.T) {
	a := newWithRunner(fakeRunner("pgbench (PostgreSQL) 17.12 (Homebrew)\n", nil, new([]call)))
	got := a.VersionInfo(context.Background())
	if got["pgbench_version"] != "17.12" {
		t.Errorf("VersionInfo = %v, want pgbench_version 17.12", got)
	}
}

func TestVersionInfo_FailureIsNotFatal(t *testing.T) {
	a := newWithRunner(fakeRunner("", errors.New("boom"), new([]call)))
	if got := a.VersionInfo(context.Background()); got != nil {
		t.Errorf("VersionInfo = %v, want nil", got)
	}
}

func TestNewestPgbench(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"debian/16", "debian/9", "brew/postgresql@17", "brew/postgresql@15"} {
		bin := filepath.Join(root, dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "pgbench"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := newestPgbench(filepath.Join(root, "debian", "*", "bin", "pgbench"),
		filepath.Join(root, "brew", "postgresql@*", "bin", "pgbench"))
	if want := filepath.Join(root, "brew", "postgresql@17", "bin", "pgbench"); got != want {
		t.Errorf("newestPgbench = %s, want %s", got, want)
	}
	if got := newestPgbench(filepath.Join(root, "*", "bin", "nothing")); got != "" {
		t.Errorf("newestPgbench with no match = %q, want empty", got)
	}
}

func assertPoint(t *testing.T, sm engine.StructuredMetrics, family string, labels map[string]string, want float64) {
	t.Helper()
	for _, p := range sm.Points {
		if p.Family != family {
			continue
		}
		match := true
		for k, v := range labels {
			if p.Labels[k] != v {
				match = false
			}
		}
		if match {
			if p.Value != want {
				t.Errorf("%s%v = %v, want %v", family, labels, p.Value, want)
			}
			return
		}
	}
	t.Errorf("no point %s%v in %v", family, labels, sm.Points)
}
