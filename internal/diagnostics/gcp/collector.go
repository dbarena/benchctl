package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/diagnostics"
)

// Output keys the collector addresses the Google APIs by, exported by
// deployments/gcp-cloudsql/postgres.
const (
	outputProjectID    = "project_id"
	outputInstanceName = "instance_name"
	outputDatabase     = "db"
)

// Collector implements diagnostics.Collector for Google Cloud targets.
type Collector struct {
	doer  httpDoer
	token tokenSource
	// quotaProject names the project API quota is billed to; see
	// adcQuotaProject for why it has to be sent by hand.
	quotaProject func() string

	monitoringHost string
	loggingHost    string
	sqlAdminHost   string

	// Resolved once in Preflight.
	projectID  string
	instance   string
	databaseID string // "<project>:<instance>", how Monitoring and Logging name it
	database   string // the benchmark database, for per-database metrics
}

var (
	_ diagnostics.Collector = (*Collector)(nil)
	_ diagnostics.Preflight = (*Collector)(nil)
)

func New(_ *config.Config) *Collector {
	return &Collector{
		doer:           http.DefaultClient,
		token:          cachedToken(adcToken),
		quotaProject:   adcQuotaProject,
		monitoringHost: defaultMonitoringHost,
		loggingHost:    defaultLoggingHost,
		sqlAdminHost:   defaultSQLAdminHost,
	}
}

func (c *Collector) Name() string { return diagnostics.VendorGCP }

// Preflight resolves the identifiers and checks prerequisites once, so a
// missing credential yields one message rather than one per source.
func (c *Collector) Preflight(req diagnostics.Request) error {
	if _, err := exec.LookPath("gcloud"); err != nil {
		return errors.New("gcloud is not on PATH; it is needed to mint an access token")
	}

	c.projectID = req.Outputs[outputProjectID]
	c.instance = req.Outputs[outputInstanceName]
	if c.projectID == "" || c.instance == "" {
		return fmt.Errorf("target declares vendor %q but exports no Cloud SQL identifiers (%s, %s)",
			diagnostics.VendorGCP, outputProjectID, outputInstanceName)
	}
	c.databaseID = c.projectID + ":" + c.instance
	c.database = req.Outputs[outputDatabase]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.token(ctx); err != nil {
		return fmt.Errorf("no usable application default credentials; run `gcloud auth application-default login`: %w", err)
	}
	return nil
}

// Collect gathers everything reachable and reports what it could not.
func (c *Collector) Collect(ctx context.Context, req diagnostics.Request) (diagnostics.Result, error) {
	var res diagnostics.Result
	start, end := req.Span()

	// Whole-run artifacts: anything not usefully sliced per window.
	req.Reportf("instance configuration and operation history")
	c.collectInstance(ctx, req, &res)
	c.collectOperations(ctx, req, &res)
	req.Reportf("Postgres log from Cloud Logging")
	c.collectLog(ctx, req, start, end, &res)

	for _, w := range req.Windows {
		dir, err := req.WindowDir(w)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("window %s: %v", w.Slug(), err))
			continue
		}
		// The slowest step by far: Cloud Monitoring takes one request per
		// metric type, so this is where a fetch spends its time.
		req.Reportf("%d Cloud Monitoring metrics for window %s", len(cloudSQLMetrics), w.Slug())
		c.collectMonitoring(ctx, req, w, dir, &res)
	}

	if len(res.Artifacts) == 0 {
		return res, errors.New("every Google Cloud source failed; see the warnings above")
	}
	return res, nil
}
