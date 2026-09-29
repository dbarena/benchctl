package aws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if _, err := exec.LookPath("aws"); err != nil {
		return errors.New("the aws CLI is not on PATH")
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
	span := spanOf(req.Windows)

	// Whole-run artifacts: anything not usefully sliced per window.
	instance := c.collectInstance(ctx, req, &res)
	c.collectParameters(ctx, req, instance, &res)
	c.collectEvents(ctx, req, span, &res)
	c.collectPIMetadata(ctx, req, &res)
	c.collectLog(ctx, req, span, &res)

	for _, w := range req.Windows {
		dir, err := req.WindowDir(w)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("window %s: %v", w.Slug(), err))
			continue
		}
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

// span covers every window. The Postgres log and the event history read
// better as one continuous record than sliced per step.
type span struct{ start, end time.Time }

func spanOf(windows []diagnostics.Window) span {
	var s span
	for i, w := range windows {
		if i == 0 || w.Start.Before(s.start) {
			s.start = w.Start
		}
		if i == 0 || w.End.After(s.end) {
			s.end = w.End
		}
	}
	return s
}

// save writes one artifact and records it, or records why it could not.
// Every step funnels through here so no failure is silent.
func (c *Collector) save(res *diagnostics.Result, req diagnostics.Request, dir, name, argv string, data []byte, err error) bool {
	rel := mustRel(req.Dest, filepath.Join(dir, name))
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", rel, err))
		return false
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("write %s: %v", rel, err))
		return false
	}
	res.Artifacts = append(res.Artifacts, diagnostics.Artifact{File: rel, Command: argv})
	return true
}

// mustRel renders a path relative to the diagnostics directory, falling back
// to the absolute path: an ugly index entry beats losing the record.
func mustRel(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}

// rfc3339 is the format the pi, cloudwatch and rds CLIs accept.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
