package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dbarena/benchctl/internal/engine"
)

// A run with several projects writes targets.json for the load driver, and
// ships it only while the file is there.
func TestMgmtProvision_WritesTargetsFile(t *testing.T) {
	f := newFakeAPI(t)
	p := f.provider()
	outputs, err := p.Provision(context.Background(), "run-1", f.config("project_count", "3"))
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	path := outputs[outputKeyTargetsFile]
	if path != filepath.Join(workDirForRun("run-1"), "targets.json") {
		t.Fatalf("targets_file = %q", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read targets: %v", err)
	}
	var targets []target
	if err := json.Unmarshal(b, &targets); err != nil || len(targets) != 3 ||
		targets[1] != (target{Ref: "ref-run-1-2", APIURL: "https://ref-run-1-2.supabase.red", Key: "pub-ref-run-1-2"}) {
		t.Errorf("targets.json = %s, %v", b, err)
	}
	if got, want := p.TargetFiles(outputs), map[string]string{path: "targets.json"}; !maps.Equal(got, want) {
		t.Errorf("TargetFiles() = %v, want %v", got, want)
	}
	if got := p.TargetFiles(engine.Outputs{outputKeyTargetsFile: filepath.Join(filepath.Dir(path), "gone.json")}); got != nil {
		t.Errorf("TargetFiles() = %v for a missing file, want nil", got)
	}
}

// seed_sql runs once in every project, and a 5xx right after the project turns
// healthy is retried.
func TestMgmtProvision_SeedSQLRunsInEveryProject(t *testing.T) {
	f := newFakeAPI(t)
	var queries sync.Map
	var fails atomic.Int32
	f.override = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !strings.HasSuffix(r.URL.Path, "/database/query") {
			return false
		}
		if fails.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		queries.Store(r.URL.Path, string(body))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `[]`)
		return true
	}
	cfg := f.config("project_count", "2", "seed_sql", "create table t(id int);")
	if _, err := f.provider().Provision(context.Background(), "run-1", cfg); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	for _, ref := range []string{"ref-run-1-1", "ref-run-1-2"} {
		got, _ := queries.Load("/v1/projects/" + ref + "/database/query")
		if got != `{"query":"create table t(id int);"}` {
			t.Errorf("%s ran %v", ref, got)
		}
	}
}
