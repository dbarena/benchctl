package supabase

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/dbarena/benchctl/internal/engine"
)

// manifestFile is the name of a run's project manifest in its working
// directory.
const manifestFile = "projects.jsonl"

// Manifest event types.
const (
	eventCreating = "creating"
	eventCreated  = "created"
	eventReady    = "ready"
	eventDeleted  = "deleted"
)

// manifest records a run's projects, one JSON line per event. Each line is
// one unbuffered write, so a ref is on disk as soon as create returns and a
// crash can't lose it.
type manifest struct {
	f *os.File
}

// manifestEvent is one manifest line. E is "creating" (written before the
// create request, so it has a name and no ref), "created", "ready" or
// "deleted".
type manifestEvent struct {
	E      string `json:"e"`
	Name   string `json:"name,omitempty"`
	Ref    string `json:"ref,omitempty"`
	APIURL string `json:"api_url,omitempty"`
	Key    string `json:"key,omitempty"`
}

func openManifest(workDir string) (*manifest, error) {
	f, err := os.OpenFile(filepath.Join(workDir, manifestFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &manifest{f: f}, nil
}

// write appends ev. O_APPEND keeps concurrent lines whole.
func (m *manifest) write(ev manifestEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = m.f.Write(append(b, '\n'))
	return err
}

func (m *manifest) close() error { return m.f.Close() }

// manifestPath returns where the manifest of the run in outputs is: the
// recorded path, else the run's working directory. A single-project run
// records no path.
func manifestPath(outputs engine.Outputs) string {
	if path := outputs[outputKeyProjectsManifest]; path != "" {
		return path
	}
	if runID := outputs[outputKeyRunID]; runID != "" {
		return filepath.Join(workDirForRun(runID), manifestFile)
	}
	return ""
}

// readManifest returns the events in the manifest at path, in order. A line
// cut short by a crash is skipped.
func readManifest(path string) ([]manifestEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []manifestEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev manifestEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.E != "" {
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return events, nil
}

// undeletedRefs returns the refs created in events and not yet deleted, in
// creation order.
func undeletedRefs(events []manifestEvent) []string {
	var refs []string
	created, deleted := map[string]bool{}, map[string]bool{}
	for _, ev := range events {
		switch {
		case ev.E == eventCreated && !created[ev.Ref]:
			created[ev.Ref] = true
			refs = append(refs, ev.Ref)
		case ev.E == eventDeleted:
			deleted[ev.Ref] = true
		}
	}
	return slices.DeleteFunc(refs, func(ref string) bool { return deleted[ref] })
}

// orphanNames returns the names whose create was started and never recorded
// as created: the request may have made a project nobody wrote down.
func orphanNames(events []manifestEvent) []string {
	var names []string
	created := map[string]bool{}
	for _, ev := range events {
		if ev.E == eventCreated {
			created[ev.Name] = true
		}
	}
	for _, ev := range events {
		if ev.E == eventCreating && !created[ev.Name] && !slices.Contains(names, ev.Name) {
			names = append(names, ev.Name)
		}
	}
	return names
}

// target is one entry of targets.json, the list of ready projects a load
// driver probes.
type target struct {
	Ref    string `json:"ref"`
	APIURL string `json:"api_url"`
	Key    string `json:"key"`
}

// writeTargets writes the ready projects to path as a JSON array.
func writeTargets(path string, projects []project) error {
	targets := make([]target, len(projects))
	for i, pr := range projects {
		targets[i] = target{Ref: pr.ref, APIURL: apiURL(pr.dbHost), Key: pr.publishableKey}
	}
	b, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write targets: %w", err)
	}
	return nil
}
