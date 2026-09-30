package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dbarena/benchctl/internal/config"
	"github.com/dbarena/benchctl/internal/diagnostics"
)

// Output keys the collector addresses the AWS APIs by, all exported by
// deployments/rds/postgres.
const (
	outputRegion        = "region"
	outputDBInstanceID  = "db_instance_identifier"
	outputDBIResourceID = "dbi_resource_id"
)

// Collector implements diagnostics.Collector for AWS targets.
type Collector struct {
	// checkCLI overrides the PATH lookup in Preflight; nil uses the real one.
	checkCLI func() error

	exec cmdRunner

	// Resolved once in Preflight so the per-source methods need not
	// re-validate the outputs.
	region     string
	instanceID string
	resourceID string
}

var (
	_ diagnostics.Collector = (*Collector)(nil)
	_ diagnostics.Preflight = (*Collector)(nil)
)

func New(_ *config.Config) *Collector { return &Collector{exec: execAWS} }

func (c *Collector) Name() string { return diagnostics.VendorAWS }

// Preflight resolves the identifiers and checks prerequisites once, so a
// missing CLI or credential yields one message rather than a dozen.
func (c *Collector) Preflight(req diagnostics.Request) error {
	if err := c.requireCLI(); err != nil {
		return err
	}

	c.region = req.Outputs[outputRegion]
	c.instanceID = req.Outputs[outputDBInstanceID]
	c.resourceID = req.Outputs[outputDBIResourceID]

	if c.instanceID == "" || c.resourceID == "" {
		// Self-hosted Postgres on EC2 correctly declares vendor "aws" too,
		// but exports no RDS identifiers and nothing here addresses an EC2
		// instance yet.
		return fmt.Errorf("target declares vendor %q but exports no RDS identifiers (%s, %s); only RDS targets are supported so far",
			diagnostics.VendorAWS, outputDBInstanceID, outputDBIResourceID)
	}
	if c.region == "" {
		return fmt.Errorf("target exports no %q, so the AWS APIs cannot be addressed; re-provision with a module that exports it", outputRegion)
	}

	// One read-only call, so expired credentials fail once and clearly.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := c.run(ctx, "sts", "get-caller-identity"); err != nil {
		return fmt.Errorf("AWS credentials are not usable (is AWS_PROFILE set?): %w", err)
	}
	return nil
}

// Collect gathers everything reachable and reports what it could not. A run
// with Performance Insights off should still yield CloudWatch.
func (c *Collector) Collect(ctx context.Context, req diagnostics.Request) (diagnostics.Result, error) {
	var res diagnostics.Result
	start, end := req.Span()

	// Whole-run artifacts: anything not usefully sliced per window.
	req.Reportf("instance configuration, parameters and event history")
	instance := c.collectInstance(ctx, req, &res)
	c.collectParameters(ctx, req, instance, &res)
	c.collectEvents(ctx, req, start, end, &res)
	c.collectPIMetadata(ctx, req, &res)
	req.Reportf("Postgres log from CloudWatch Logs")
	c.collectLog(ctx, req, start, end, &res)

	for _, w := range req.Windows {
		dir, err := req.WindowDir(w)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("window %s: %v", w.Slug(), err))
			continue
		}
		req.Reportf("CloudWatch and Performance Insights for window %s", w.Slug())
		c.collectCloudWatch(ctx, req, w, dir, &res)
		c.collectPILoad(ctx, req, w, dir, &res)
		c.collectPITopSQL(ctx, req, w, dir, &res)
		c.collectPICounters(ctx, req, w, dir, &res)
	}

	if len(res.Artifacts) == 0 {
		return res, errors.New("every AWS source failed; see the warnings above")
	}
	return res, nil
}

// checkCLI is injected so tests do not depend on whether the host has the
// CLI installed. Nil means use the real PATH lookup.
func (c *Collector) requireCLI() error {
	if c.checkCLI != nil {
		return c.checkCLI()
	}
	return diagnostics.RequireCLI("aws")
}
