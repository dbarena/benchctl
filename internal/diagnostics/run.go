package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// IndexFilename is the manifest written into the diagnostics directory. It
// records what was collected, over which windows, by which command, and what
// failed — so an absent file can be told apart from one that was never
// attempted.
const IndexFilename = "index.json"

// DirName is the subdirectory of the fetch destination that diagnostics are
// written into, keeping them clearly apart from the driver-side artifacts
// (results_*.json, raw_samples_*.csv) that share the destination.
const DirName = "diagnostics"

// Index is the contents of IndexFilename.
type Index struct {
	RunID       string     `json:"run_id"`
	Collector   string     `json:"collector"`
	CollectedAt time.Time  `json:"collected_at"`
	Windows     []Window   `json:"windows"`
	Artifacts   []Artifact `json:"artifacts"`
	Warnings    []string   `json:"warnings,omitempty"`
}

// Run drives one collector and writes the index.
//
// It returns an error only when the local filesystem gets in the way: the
// destination cannot be created, or the index cannot be written. Everything
// the collector reports, including a hard failure, comes back as a warning in
// the index and on out. Callers must not fail on a collection problem —
// dbarenactl stops a whole sweep after three consecutive fetch failures, so
// a missing IAM permission would take the sweep down with it.
func Run(ctx context.Context, out io.Writer, c Collector, req Request) (Index, error) {
	idx := Index{
		RunID:       req.RunID,
		Collector:   c.Name(),
		CollectedAt: time.Now().UTC(),
		Windows:     req.Windows,
		Artifacts:   []Artifact{},
	}

	if err := os.MkdirAll(req.Dest, 0o755); err != nil {
		return idx, fmt.Errorf("create diagnostics directory %s: %w", req.Dest, err)
	}

	if pf, ok := c.(Preflight); ok {
		if err := pf.Preflight(req); err != nil {
			idx.Warnings = append(idx.Warnings, "preflight: "+err.Error())
			warn(out, "diagnostics: skipping %s: %v", c.Name(), err)
			return idx, writeIndex(req.Dest, idx)
		}
	}

	res, err := c.Collect(ctx, req)
	if err != nil {
		idx.Warnings = append(idx.Warnings, err.Error())
		warn(out, "diagnostics: %s: %v", c.Name(), err)
	}
	if len(res.Artifacts) > 0 {
		idx.Artifacts = res.Artifacts
	}
	idx.Warnings = append(idx.Warnings, res.Warnings...)
	for _, w := range res.Warnings {
		warn(out, "diagnostics: %s: %s", c.Name(), w)
	}

	return idx, writeIndex(req.Dest, idx)
}

func writeIndex(dest string, idx Index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", IndexFilename, err)
	}
	path := filepath.Join(dest, IndexFilename)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func warn(out io.Writer, format string, args ...any) {
	if out != nil {
		fmt.Fprintf(out, format+"\n", args...)
	}
}
