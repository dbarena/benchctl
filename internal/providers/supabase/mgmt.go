package supabase

// The Management API path. It is opt-in (config.management_api: true) and meant
// as an escape hatch for runs the CLI path cannot serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbarena/benchctl/internal/consolelog"
	"github.com/dbarena/benchctl/internal/engine"
)

const (
	// A run with several projects lists them in a file instead, because every
	// output becomes an argument to the workload: projects_manifest is the
	// run's manifest (see manifest), and projects_ready how many are ready.
	outputKeyProjectsManifest = "projects_manifest"
	outputKeyProjectsReady    = "projects_ready"

	// _supabase_management_api is "true" for a run provisioned through the
	// Management API, so teardown takes the same path.
	outputKeyManagementAPI = "_supabase_management_api"
	// _supabase_management_api_url stores the Management API the projects were
	// created through, so teardown and diagnostics talk to the same one.
	outputKeyManagementAPIURL = "_supabase_management_api_url"
	// _supabase_org_id stores the organisation, so teardown only deletes
	// projects found by name in it.
	outputKeyOrgID = "_supabase_org_id"

	// defaultReadyTimeout is how long a project may take from create to
	// ACTIVE_HEALTHY. A project can legitimately wait a long time at scale (node
	// autoscaling, pods ahead of it in the scheduler), so this only stops a
	// project that is stuck and would hold its slot for ever. A project that
	// fails for real reports a status, which waitForProject checks.
	defaultReadyTimeout = time.Hour

	// progressInterval is how often provision prints its summary line.
	progressInterval = 30 * time.Second
)

// mgmt provisions and tears down projects through the Management API.
type mgmt struct{ *Provider }

// managementAPIURL returns the Management API to use: apiURL when set,
// otherwise the API of a built-in CLI profile. Any other profile is an error,
// so a run can never fall through to production.
func managementAPIURL(apiURL, profile string) (string, error) {
	if apiURL != "" {
		return strings.TrimRight(apiURL, "/"), nil
	}
	if profile == "" {
		profile = "supabase" // the CLI's default profile
	}
	if base, ok := profileAPIBases[profile]; ok {
		return base, nil
	}
	return "", fmt.Errorf("config.profile %q is not a built-in profile; set config.management_api_url", profile)
}

// mgmtOnlyKeys are the config keys only the Management API path reads.
var mgmtOnlyKeys = []string{"management_api_url", "throttle_skip_token", "max_failed_projects", "ready_timeout", "require_host_suffix"}

// boolConfig reads a bool that arrives as a bool, or as a string after
// templating.
func boolConfig(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// parseManagementAPIConfig adds the Management API keys to c. Without
// management_api, a key only that path reads is an error, so a scenario can't
// lose its seed or its throttle token by leaving the flag out.
func parseManagementAPIConfig(cfg map[string]any, c providerConfig) (providerConfig, error) {
	c.ManagementAPI = boolConfig(cfg["management_api"])
	if !c.ManagementAPI {
		for _, key := range mgmtOnlyKeys {
			if v, ok := cfg[key]; ok && v != nil && v != "" {
				return c, fmt.Errorf("config.%s needs config.management_api: true", key)
			}
		}
		return c, nil
	}
	apiURL, _ := cfg["management_api_url"].(string)
	var err error
	if c.ManagementAPIURL, err = managementAPIURL(apiURL, c.Profile); err != nil {
		return c, err
	}
	c.ThrottleSkipToken, _ = cfg["throttle_skip_token"].(string)
	c.RequireHostSuffix, _ = cfg["require_host_suffix"].(string)
	if c.MaxFailedProjects, err = parseIntConfig(cfg, "max_failed_projects"); err != nil {
		return c, err
	}
	if c.MaxFailedProjects < 0 {
		return c, fmt.Errorf("config.max_failed_projects must be at least 0, got %d", c.MaxFailedProjects)
	}
	c.ReadyTimeout = defaultReadyTimeout
	if v, _ := cfg["ready_timeout"].(string); v != "" {
		if c.ReadyTimeout, err = time.ParseDuration(v); err != nil || c.ReadyTimeout <= 0 {
			return c, fmt.Errorf("config.ready_timeout %q is invalid: want a positive duration such as 90m", v)
		}
	}
	return c, nil
}

// provision creates project_count Supabase projects through the Management
// API, several at a time (see max_parallel_projects), and waits for all of
// them to become ready. It reads the keys of Provision, plus:
//
//	management_api   bool     true selects this path
//	management_api_url string Management API base URL; wins over profile
//	                          (optional). Without it, profile must be a
//	                          built-in one: supabase (default),
//	                          supabase-staging or supabase-local
//	throttle_skip_token string sent as x-supabase-skip-throttling, for a
//	                          Management API configured to accept it (optional)
//	max_failed_projects int   how many projects may fail before the run
//	                          cancels the rest (optional, default 0)
//	ready_timeout    string   how long one project may take from create to
//	                          healthy, as a Go duration (optional, default 1h)
//	require_host_suffix string fail a project whose database host does not end
//	                          in this domain, e.g. supabase.red (optional).
//	                          Guards against the platform placing projects
//	                          somewhere other than intended.
//
// A project that fails is left for teardown, which can delete it once the
// API allows. The run goes on while no more than max_failed_projects have
// failed.
//
// A single project's outputs include host, port, user, project_ref,
// project_refs, api_url_<ref> and publishable_key_<ref>. Several projects get
// projects_manifest and projects_ready instead.
func (p mgmt) provision(ctx context.Context, runID string, cfg map[string]any) (engine.Outputs, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}
	client, err := p.newClient(c.ManagementAPIURL, c.ThrottleSkipToken)
	if err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}

	workDir := workDirForRun(runID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("supabase: mkdir %s: %w", workDir, err)
	}
	m, err := openManifest(workDir)
	if err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}
	defer m.close()

	// One password for every project in the run keeps the outputs small.
	password, err := generatePassword()
	if err != nil {
		return nil, fmt.Errorf("supabase: generate password: %w", err)
	}
	names := projectNames(runID, c.ProjectCount)
	run := mgmtRun{
		cfg: c, client: client, password: password, manifest: m,
		progress: startProgress(p.out, len(names), provisionLine),
	}

	// Once more than max_failed_projects have failed, the latest failure
	// cancels the projects still being provisioned. It is also the cause
	// reported, rather than the cancellation it caused.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	projects := make([]project, len(names))
	errs := make([]error, len(names))
	var failed atomic.Int64
	ForEachParallel(len(names), c.MaxParallelProjects, func(i int) {
		if errs[i] = ctx.Err(); errs[i] != nil {
			return
		}
		projects[i], errs[i] = p.provisionProject(ctx, run, names[i])
		if errs[i] == nil || ctx.Err() != nil {
			return
		}
		run.progress.fail(names[i], errs[i])
		if failed.Add(1) > int64(c.MaxFailedProjects) {
			cancel(errs[i])
		}
	})
	run.progress.stop()

	cause := context.Cause(ctx)
	ready := slices.DeleteFunc(slices.Clone(projects), func(pr project) bool { return !pr.ready })
	if cause == nil && len(ready) == 0 {
		cause = errors.Join(errs...)
	}
	if cause != nil {
		// Every project with a ref is left for the runner to tear down.
		created := slices.DeleteFunc(projects, func(pr project) bool { return pr.ref == "" })
		if len(created) == 0 {
			p.removeWorkDir(workDir)
			return nil, fmt.Errorf("supabase: %w", cause)
		}
		return mgmtRefOutputs(c, runID, created), fmt.Errorf("supabase: %w", cause)
	}

	return run.outputs(runID, ready), nil
}

// mgmtRun holds what every project in a run shares.
type mgmtRun struct {
	cfg      providerConfig
	client   *mgmtClient
	password string
	manifest *manifest
	progress *progress
}

// outputs builds the target outputs for a fully provisioned run.
func (r mgmtRun) outputs(runID string, projects []project) engine.Outputs {
	out := mgmtRefOutputs(r.cfg, runID, projects)
	out["password"] = r.password
	out["db"] = "postgres"
	// Supabase projects always require TLS; sslnegotiation=direct skips the
	// plaintext SSLRequest round trip in favor of an immediate TLS handshake.
	out["sslmode"] = "require"
	out["sslnegotiation"] = "direct"
	out[outputKeyVendor] = "supabase"
	if r.cfg.ProjectCount > 1 {
		out[outputKeyProjectsReady] = strconv.Itoa(len(projects))
		return out
	}
	pr := projects[0]
	out[outputKeyAPIURL+pr.ref] = apiURL(pr.dbHost)
	out[outputKeyPublishableKey+pr.ref] = pr.publishableKey
	out["host"], out["port"], out["user"] = connectionDetails(pr.dbHost, pr.ref, r.cfg.ConnectionMode, r.cfg.Region)
	out[outputKeyProjectRef] = pr.ref
	return out
}

// mgmtRefOutputs returns what Teardown needs to delete the given projects, and any
// other project the run created. With several projects, the manifest lists
// them.
func mgmtRefOutputs(c providerConfig, runID string, projects []project) engine.Outputs {
	out := engine.Outputs{
		outputKeyManagementAPI:    "true",
		outputKeyProfile:          c.Profile,
		outputKeyManagementAPIURL: c.ManagementAPIURL,
		outputKeyOrgID:            c.OrgID,
		outputKeyRunID:            runID,
		outputKeyProjectCount:     strconv.Itoa(c.ProjectCount),
	}
	if c.ProjectCount > 1 {
		out[outputKeyProjectsManifest] = filepath.Join(workDirForRun(runID), manifestFile)
		return out
	}
	refs := make([]string, len(projects))
	for i, pr := range projects {
		refs[i] = pr.ref
	}
	out[outputKeyProjectRefs] = strings.Join(refs, ",")
	return out
}

// progress counts a run's projects through each stage. It prints a summary
// line every progressInterval, when every project has reached a stage, and
// when it stops; and a project's failure straight away.
type progress struct {
	out   io.Writer
	total int64
	line  func(*progress) string

	created, healthy, ready, failed, deleted atomic.Int64

	mu   sync.Mutex
	last string

	stopped chan struct{}
	done    chan struct{}
}

func startProgress(out io.Writer, total int, line func(*progress) string) *progress {
	pr := &progress{out: out, total: int64(total), line: line, stopped: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(pr.done)
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		for {
			select {
			case <-pr.stopped:
				return
			case <-t.C:
				pr.print(true)
			}
		}
	}()
	return pr
}

// advance counts one more project into stage.
func (pr *progress) advance(stage *atomic.Int64) {
	if stage.Add(1) == pr.total {
		pr.print(false)
	}
}

func (pr *progress) fail(name string, err error) {
	pr.failed.Add(1)
	consolelog.Println(pr.out, fmt.Sprintf("supabase: project %s failed: %v", name, err))
}

func (pr *progress) stop() {
	close(pr.stopped)
	<-pr.done
	pr.print(false)
}

func provisionLine(pr *progress) string {
	return fmt.Sprintf("supabase: %d/%d created, %d ACTIVE_HEALTHY, %d ready, %d failed",
		pr.created.Load(), pr.total, pr.healthy.Load(), pr.ready.Load(), pr.failed.Load())
}

func teardownLine(pr *progress) string {
	return fmt.Sprintf("supabase: %d/%d deleted", pr.deleted.Load(), pr.total)
}

// print writes the summary line. Unless always is set, a line identical to
// the last one is skipped.
func (pr *progress) print(always bool) {
	line := pr.line(pr)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if !always && line == pr.last {
		return
	}
	pr.last = line
	consolelog.Println(pr.out, line)
}

// createProjectBody is the body of POST /v1/projects. organization_slug, not
// organization_id: the API throttles a create with organization_id in the
// same bucket as deletes.
type createProjectBody struct {
	Name                string `json:"name"`
	OrganizationSlug    string `json:"organization_slug"`
	DBPass              string `json:"db_pass"`
	Region              string `json:"region"`
	DesiredInstanceSize string `json:"desired_instance_size,omitempty"`
	HighAvailability    bool   `json:"high_availability,omitempty"`
	ReleaseChannel      string `json:"release_channel,omitempty"`
	PostgresEngine      string `json:"postgres_engine,omitempty"`
}

// provisionProject creates the project called name, waits for it to become
// ready and fetches its publishable key. Once the project exists, the
// returned project carries its ref, even with an error.
func (p mgmt) provisionProject(ctx context.Context, run mgmtRun, name string) (project, error) {
	c := run.cfg
	// Write the name down before the POST, so a crash anywhere after it still
	// leaves teardown a name to look for. If it can't be written, don't create.
	if err := run.manifest.write(manifestEvent{E: eventCreating, Name: name}); err != nil {
		return project{}, fmt.Errorf("manifest: record %s: %w", name, err)
	}
	ref, err := p.createProject(ctx, run, name)
	if ref == "" {
		return project{}, err
	}
	pr := project{ref: ref}
	if mErr := run.manifest.write(manifestEvent{E: eventCreated, Name: name, Ref: ref}); mErr != nil {
		return pr, errors.Join(err, fmt.Errorf("manifest: record %s: %w", ref, mErr))
	}
	run.progress.advance(&run.progress.created)
	if err != nil {
		return pr, err
	}

	host, err := p.waitForProject(ctx, run.client, ref, c.ReadyTimeout)
	if err != nil {
		return pr, err
	}
	run.progress.advance(&run.progress.healthy)
	if suffix := strings.TrimPrefix(c.RequireHostSuffix, "."); suffix != "" && !strings.HasSuffix(host, "."+suffix) {
		return pr, fmt.Errorf("project %s landed on %s, want a host ending in %s", ref, host, suffix)
	}

	key, err := p.publishableKey(ctx, run.client, ref)
	if err != nil {
		return pr, err
	}
	if err := p.resizeDisk(ctx, run.client.base+"/v1/projects/"+ref+"/config/disk", c.DiskSizeGB, c.DiskIOPS, c.DiskThroughputMibps, c.DiskType); err != nil {
		return pr, err
	}
	if usesPooler(c.ConnectionMode) {
		if err := sleep(ctx, poolerGrace); err != nil {
			return pr, err
		}
	}

	pr.dbHost, pr.publishableKey, pr.ready = host, key, true
	if err := run.manifest.write(manifestEvent{E: eventReady, Ref: ref, APIURL: apiURL(host), Key: key}); err != nil {
		return project{ref: ref}, fmt.Errorf("manifest: %w", err)
	}
	run.progress.advance(&run.progress.ready)
	return pr, nil
}

// createProject creates the project called name and returns its ref. A
// create can fail after the project exists: the response was lost, or the
// name was already taken. Then the project is looked up by name, and its ref
// comes back with the error.
func (p mgmt) createProject(ctx context.Context, run mgmtRun, name string) (string, error) {
	c := run.cfg
	body := createProjectBody{
		Name: name, OrganizationSlug: c.OrgID, DBPass: run.password, Region: c.Region,
		DesiredInstanceSize: c.ProjectSize, HighAvailability: c.HighAvailability,
		ReleaseChannel: c.ReleaseChannel, PostgresEngine: c.PostgresEngine,
	}
	out, err := run.client.doThrottled(ctx, http.MethodPost, "/v1/projects", body)
	if err == nil {
		ref, parseErr := parseProjectRef(out)
		if parseErr == nil {
			return ref, nil
		}
		err = fmt.Errorf("parse project ref: %w", parseErr)
	}
	createErr := fmt.Errorf("create project %s: %w", name, err)
	if errors.Is(err, errNotSent) {
		return "", createErr
	}
	// A cancelled request's error wraps the cancel cause, which can be another
	// project's APIError, so it can't tell us how this create went.
	if ctx.Err() == nil && !mayHaveCreated(err) {
		return "", createErr
	}
	return p.recoverCreatedProject(run.client, c.OrgID, name), createErr
}

// mayHaveCreated reports whether a failed create can still have left a
// project: any failure but an API error that rejected the request, except
// the 400 that says the name is taken.
func mayHaveCreated(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status >= 500 {
		return true
	}
	return apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Message, "already exists")
}

// recoverCreatedProject returns the ref of the project called name, or "" if
// there is none.
//
// It uses GET /v1/projects, not the paginated organisation list, which hides
// projects still in UNKNOWN status. The lookup runs on a fresh context
// because the run context may be the very reason create failed, and it
// retries a few times because the API can take a moment to list a
// just-created project. Recovery is best effort: any error yields "" so the
// caller reports the original create failure unchanged.
func (p mgmt) recoverCreatedProject(client *mgmtClient, orgID, name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for attempt := range recoverLookupAttempts {
		if attempt > 0 {
			if sleep(ctx, p.recoverLookupIntervalOrDefault()) != nil {
				return ""
			}
		}
		out, err := client.doThrottled(ctx, http.MethodGet, "/v1/projects", nil)
		if err != nil {
			continue
		}
		listed, err := parseAPIProjects(out)
		if err != nil {
			continue
		}
		ref, err := refByName(listed, name, orgID)
		if err != nil {
			continue
		}
		consolelog.Println(p.out, fmt.Sprintf("supabase: found project %s by name %s", ref, name))
		return ref
	}
	return ""
}

// deleteProject makes one attempt to delete the project identified by ref.
// One that is already gone counts as deleted. retry reports that the
// Management API won't delete the project yet: it refuses one that is still
// coming up until up to 15 minutes after its last status change.
func (p mgmt) deleteProject(ctx context.Context, client *mgmtClient, ref string) (retry bool, err error) {
	_, err = client.doThrottled(ctx, http.MethodDelete, "/v1/projects/"+ref, nil)
	switch {
	case err == nil, hasStatus(err, http.StatusNotFound):
		return false, nil
	case apiNotReadyForDeletion(err):
		return true, nil
	case hasStatus(err, http.StatusForbidden):
		// Access to a deleted project can go with it, so a 403 may mean the
		// project is already gone.
		if _, getErr := client.do(ctx, http.MethodGet, "/v1/projects/"+ref, nil); hasStatus(getErr, http.StatusNotFound) {
			return false, nil
		}
	}
	return false, fmt.Errorf("delete project %s: %w", ref, err)
}

// apiNotReadyForDeletion reports whether a failed delete hit the Management
// API's block on deleting a project that is still being created.
func apiNotReadyForDeletion(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "not ready for deletion")
}

// teardown deletes every project in the run's manifest that isn't deleted
// yet, marking each one deleted as it goes, so a rerun picks up where an
// interrupted one stopped. A run without a manifest, or whose manifest
// records no create (benchctl died before the first create returned), has
// the projects listed in outputs deleted, and any other project in its
// organisation named after the run. Once every project is deleted it also
// removes the local per-run working directory (see workDirForRun).
func (p mgmt) teardown(ctx context.Context, outputs engine.Outputs) error {
	path := manifestPath(outputs)
	events, readErr := readManifest(path)
	fromManifest := readErr == nil && len(events) > 0
	refs, names := projectRefs(outputs), runProjectNames(outputs)
	switch {
	case readErr != nil && path != "" && !os.IsNotExist(readErr):
		return fmt.Errorf("supabase: teardown: %w", readErr)
	case !fromManifest && len(refs) == 0 && len(names) == 0:
		return fmt.Errorf("supabase: teardown: missing %s in outputs", outputKeyProjectRefs)
	}

	base, err := managementAPIURL(outputs[outputKeyManagementAPIURL], outputs[outputKeyProfile])
	if err != nil {
		return fmt.Errorf("supabase: teardown: %w", err)
	}
	client, err := p.newClient(base, "")
	if err != nil {
		return fmt.Errorf("supabase: teardown: %w", err)
	}

	var m *manifest
	if fromManifest {
		refs = undeletedRefs(events)
		if m, err = openManifest(filepath.Dir(path)); err != nil {
			return fmt.Errorf("supabase: teardown: %w", err)
		}
		defer m.close()
		// A create that started and was never recorded as created may have made
		// a project. Look those names up. A clean run has none, so it skips the
		// list, which at 60k projects is large.
		if orphans := orphanNames(events); len(orphans) > 0 {
			found, err := p.refsByName(ctx, client, orphans, outputs[outputKeyOrgID])
			if err != nil {
				return err
			}
			for name, ref := range found {
				if err := m.write(manifestEvent{E: eventCreated, Name: name, Ref: ref}); err != nil {
					return fmt.Errorf("supabase: teardown: manifest: %w", err)
				}
				refs = append(refs, ref)
			}
		}
	} else if refs, err = p.existingRefs(ctx, client, refs, names, outputs[outputKeyOrgID]); err != nil {
		return err
	}

	if err := p.deleteProjects(ctx, client, refs, m); err != nil {
		return fmt.Errorf("supabase: %w", err)
	}

	// Remove local state only once every project is deleted, so the manifest
	// and workdir remain for a rerun or debugging on any failure.
	if runID := outputs[outputKeyRunID]; runID != "" {
		p.removeWorkDir(workDirForRun(runID))
	}
	return nil
}

// deleteProjects deletes refs through the limiter, recording each deletion in
// m when it is set. A project the API won't delete yet goes to the back of
// the queue and is retried no sooner than deleteRetryInterval later.
func (p mgmt) deleteProjects(ctx context.Context, client *mgmtClient, refs []string, m *manifest) error {
	type item struct {
		ref       string
		notBefore time.Time
	}
	// Every ref is either in the queue or with a worker, so a requeue never
	// blocks.
	queue := make(chan item, len(refs))
	for _, ref := range refs {
		queue <- item{ref: ref}
	}
	var pending sync.WaitGroup
	pending.Add(len(refs))
	go func() {
		pending.Wait()
		close(queue)
	}()

	pr := startProgress(p.out, len(refs), teardownLine)
	var mu sync.Mutex
	var errs []error
	ForEachParallel(defaultMaxParallelProjects, defaultMaxParallelProjects, func(int) {
		for it := range queue {
			err := sleep(ctx, time.Until(it.notBefore))
			if err == nil {
				var retry bool
				if retry, err = p.deleteProject(ctx, client, it.ref); retry {
					queue <- item{ref: it.ref, notBefore: time.Now().Add(p.deleteRetryIntervalOrDefault())}
					continue
				}
			}
			if err == nil && m != nil {
				if mErr := m.write(manifestEvent{E: eventDeleted, Ref: it.ref}); mErr != nil {
					err = fmt.Errorf("manifest: %w", mErr)
				}
			}
			switch {
			case err == nil:
				pr.advance(&pr.deleted)
			case ctx.Err() == nil:
				// Interrupted deletes are summed up once, below.
				consolelog.Println(p.out, fmt.Sprintf("supabase: %v", err))
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			pending.Done()
		}
	})
	pr.stop()

	if ctx.Err() != nil {
		errs = append(errs, fmt.Errorf("teardown stopped with %d/%d deleted: %w", pr.deleted.Load(), len(refs), ctx.Err()))
	}
	return errors.Join(errs...)
}

// existingRefs returns the refs that GET /v1/projects still lists, so a
// redundant teardown (after a retry or a manual delete) succeeds instead of
// leaving the run un-terminated.
//
// It adds any listed project in orgID with one of the given names. That
// catches the projects whose ref never reached the run's outputs: benchctl
// was killed mid-provision, a create's response was lost, or the project
// failed. An empty orgID, from a run before it was recorded, matches on name
// alone.
func (p mgmt) existingRefs(ctx context.Context, client *mgmtClient, refs, names []string, orgID string) ([]string, error) {
	listed, err := p.listProjects(ctx, client)
	if err != nil {
		return nil, err
	}
	listedRefs := make(map[string]bool, len(listed))
	for _, lp := range listed {
		listedRefs[lp.Ref] = true
	}
	isName := make(map[string]bool, len(names))
	for _, name := range names {
		isName[name] = true
	}
	existing := slices.DeleteFunc(slices.Clone(refs), func(ref string) bool {
		if !listedRefs[ref] {
			consolelog.Println(p.out, fmt.Sprintf("supabase: project %s already deleted", ref))
		}
		return !listedRefs[ref]
	})
	for _, lp := range listed {
		if !isName[lp.Name] || slices.Contains(refs, lp.Ref) || !inOrg(lp, orgID) {
			continue
		}
		consolelog.Println(p.out, fmt.Sprintf("supabase: found project %s by name (%s)", lp.Ref, lp.Name))
		existing = append(existing, lp.Ref)
	}
	return existing, nil
}

// refsByName returns the ref of each listed project in orgID called one of
// names, keyed by name.
func (p mgmt) refsByName(ctx context.Context, client *mgmtClient, names []string, orgID string) (map[string]string, error) {
	listed, err := p.listProjects(ctx, client)
	if err != nil {
		return nil, err
	}
	isName := make(map[string]bool, len(names))
	for _, name := range names {
		isName[name] = true
	}
	found := map[string]string{}
	for _, lp := range listed {
		if isName[lp.Name] && inOrg(lp, orgID) {
			consolelog.Println(p.out, fmt.Sprintf("supabase: found project %s by name (%s)", lp.Ref, lp.Name))
			found[lp.Name] = lp.Ref
		}
	}
	return found, nil
}

// listProjects returns every project the token can see.
func (p mgmt) listProjects(ctx context.Context, client *mgmtClient) ([]listedProject, error) {
	out, err := client.doThrottled(ctx, http.MethodGet, "/v1/projects", nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: list projects: %w", err)
	}
	listed, err := parseAPIProjects(out)
	if err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}
	return listed, nil
}

// waitForProject polls GET /v1/projects/{ref} until the project reaches
// ACTIVE_HEALTHY, then returns its database host. A project that reports a
// status it cannot recover from fails at once. Any other status, COMING_UP
// included, is waited on for up to timeout.
func (p mgmt) waitForProject(ctx context.Context, client *mgmtClient, ref string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	status := "not listed yet"
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("project %s was still %s after %s", ref, status, timeout)
		}
		out, err := client.doThrottled(ctx, http.MethodGet, "/v1/projects/"+ref, nil)
		// The API can take a moment to find a just-created project.
		if err != nil && !hasStatus(err, http.StatusNotFound) && !transient(ctx, err) {
			return "", fmt.Errorf("get project %s: %w", ref, err)
		}
		if err == nil {
			var pr listedProject
			if err := json.Unmarshal(out, &pr); err != nil {
				return "", fmt.Errorf("parse project %s: %w", ref, err)
			}
			switch status = pr.Status; status {
			case "ACTIVE_HEALTHY":
				return pr.Database.Host, nil
			case "INIT_FAILED", "REMOVED", "GOING_DOWN":
				return "", fmt.Errorf("project %s reached status %s", ref, status)
			}
		}
		if err := sleep(ctx, p.pollIntervalOrDefault()); err != nil {
			return "", err
		}
	}
}

// transient reports whether a poll that failed with err is worth repeating:
// the request didn't get an answer, or the API failed with a 5xx.
func transient(ctx context.Context, err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	return ctx.Err() == nil
}

// publishableKey fetches the project's publishable API key. New projects have
// the legacy anon key disabled, and the publishable key can appear a little
// after the project is ACTIVE_HEALTHY, so it is polled for.
func (p mgmt) publishableKey(ctx context.Context, client *mgmtClient, ref string) (string, error) {
	timeout := p.pollTimeoutOrDefault()
	deadline := time.Now().Add(timeout)
	for {
		out, err := client.doThrottled(ctx, http.MethodGet, "/v1/projects/"+ref+"/api-keys", nil)
		if err != nil && !transient(ctx, err) {
			return "", fmt.Errorf("api-keys %s: %w", ref, err)
		}
		if err == nil {
			var key string
			if key, err = parseAPIKeys(out); !errors.Is(err, errNoPublishableKey) {
				return key, err
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("project %s: %w after %s", ref, err, timeout)
		}
		if err := sleep(ctx, p.pollIntervalOrDefault()); err != nil {
			return "", err
		}
	}
}

var errNoPublishableKey = errors.New("no publishable key in api-keys response")

// parseAPIKeys returns the publishable key from a GET
// /v1/projects/{ref}/api-keys response.
func parseAPIKeys(data []byte) (string, error) {
	var keys []struct {
		Type   string `json:"type"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &keys); err != nil {
		return "", fmt.Errorf("parse api-keys JSON: %w", err)
	}
	for _, k := range keys {
		if k.Type == "publishable" && k.APIKey != "" {
			return k.APIKey, nil
		}
	}
	return "", errNoPublishableKey
}

// parseAPIProjects parses a GET /v1/projects response.
func parseAPIProjects(data []byte) ([]listedProject, error) {
	var projects []listedProject
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("parse projects list JSON: %w", err)
	}
	return projects, nil
}
