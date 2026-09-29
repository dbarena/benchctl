// Package diagnostics collects provider-side observability data for a
// finished benchmark run: Performance Insights and CloudWatch for RDS, Cloud
// Monitoring and Cloud Logging for Cloud SQL, the Management API for
// Supabase.
//
// It runs orchestrator-side, from `benchctl fetch`, for two reasons. The load
// driver has no cloud credentials (neither the EC2 nor the GCE module attaches
// an instance profile or service-account scopes), and the data has to be
// pulled before `benchctl teardown` destroys the instance it describes.
//
// Everything here is best effort. A missing credential, a disabled feature, or
// a partial API response becomes a warning; it must never fail the fetch,
// because dbarenactl halts an entire sweep after three consecutive fetch
// failures.
package diagnostics

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Window is the wall-clock interval a provider-side query should cover. One
// per measured suite step, recovered from the step_windows.json the runner
// writes, or a single whole-run window when that file is unavailable.
type Window struct {
	Name      string            `json:"name"`
	Benchmark string            `json:"benchmark,omitempty"`
	Iteration int               `json:"iteration,omitempty"`
	Fixture   map[string]string `json:"fixture,omitempty"`
	Start     time.Time         `json:"start"`
	End       time.Time         `json:"end"`
}

// Slug is the per-window directory name, built from the fields that
// distinguish one window from another within a run. Fixture values are sorted
// so the name is stable across runs of the same sweep.
func (w Window) Slug() string {
	parts := []string{sanitize(w.Name)}
	if w.Iteration > 0 {
		parts = append(parts, "iter"+strconv.Itoa(w.Iteration))
	}
	for _, k := range slices.Sorted(maps.Keys(w.Fixture)) {
		parts = append(parts, sanitize(k)+"-"+sanitize(w.Fixture[k]))
	}
	return strings.Join(parts, "_")
}

// Request is everything a Collector needs. Config is deliberately absent: a
// collector reads what it needs from Outputs, which is what provisioning
// recorded, so `benchctl fetch` needs neither the scenario file nor a live
// driver instance.
type Request struct {
	RunID string
	// Outputs is the run's target outputs, carrying the identifiers each
	// provider is addressed by: dbi_resource_id and region for RDS,
	// instance_name and project_id for Cloud SQL, project_ref for Supabase.
	Outputs map[string]string
	Windows []Window
	// Dest is the diagnostics directory, already created.
	Dest string
	// Progress, when set, reports what a collector is working on. Collection
	// runs for tens of seconds against a dozen APIs, and silence for that
	// long is indistinguishable from a hang.
	Progress func(format string, args ...any)
}

// Reportf reports progress when the caller asked for it, and is a no-op
// otherwise so collectors need no nil checks.
func (r Request) Reportf(format string, args ...any) {
	if r.Progress != nil {
		r.Progress(format, args...)
	}
}

// WindowDir returns the subdirectory of Dest that w's artifacts belong in,
// creating it on demand. Collectors that write one file per run rather than
// per window should write straight into Dest instead.
func (r Request) WindowDir(w Window) (string, error) {
	dir := filepath.Join(r.Dest, w.Slug())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Span is the interval covering every window. Sources that are not usefully
// sliced per step, such as a Postgres log or an instance's event history,
// read better as one continuous record.
func (r Request) Span() (start, end time.Time) {
	for i, w := range r.Windows {
		if i == 0 || w.Start.Before(start) {
			start = w.Start
		}
		if i == 0 || w.End.After(end) {
			end = w.End
		}
	}
	return start, end
}

// Save writes one artifact into dir and records it on res, or records why it
// could not. Every collection step funnels through here so no failure is
// silent. Reports whether the artifact was written.
func (r Request) Save(res *Result, dir, name, command string, data []byte, err error) bool {
	rel := r.rel(filepath.Join(dir, name))
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", rel, err))
		return false
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("write %s: %v", rel, err))
		return false
	}
	res.Artifacts = append(res.Artifacts, Artifact{File: rel, Command: command})
	return true
}

// rel renders a path relative to Dest for the index, falling back to the
// absolute path: an ugly entry beats losing the record of a file that exists.
func (r Request) rel(path string) string {
	out, err := filepath.Rel(r.Dest, path)
	if err != nil {
		return path
	}
	return out
}

// RFC3339 is the timestamp format the AWS and Google APIs accept.
func RFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Artifact is one collected file and the command or URL that produced it, so
// a reader can tell what was asked for and reproduce it.
type Artifact struct {
	// File is relative to the diagnostics directory.
	File string `json:"file"`
	// Command is the argv or URL behind the file.
	Command string `json:"command"`
}

// Result is what a Collector produced. A collector that reached some sources
// and not others returns both: the artifacts it got, and a warning per source
// it could not reach.
type Result struct {
	Artifacts []Artifact
	Warnings  []string
}

// Collector gathers provider-side data for one platform.
//
// Collect should return an error only when nothing at all could be collected.
// Anything partial belongs in Result.Warnings, so a disabled feature or a
// single 403 still yields whatever else was reachable.
type Collector interface {
	Name() string
	Collect(ctx context.Context, req Request) (Result, error)
}

// Preflight is an optional interface a Collector may implement to check its
// prerequisites (CLI on PATH, required identifiers present, credentials
// usable) before any collection runs. Mirrors engine.CollectorPreflight.
//
// A failed preflight skips collection and is reported as a warning; it does
// not fail the fetch.
type Preflight interface {
	Preflight(req Request) error
}

// sanitize replaces characters that are unsafe in a filename with
// underscores, matching what the stdout collector does for results_*.json so
// diagnostics directories sort alongside them.
func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	return sb.String()
}
