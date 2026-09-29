package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// collectInstance snapshots the instance configuration and returns the raw
// response for collectParameters.
//
// StorageOperationStatus appears while a volume is initialising or optimising,
// when performance is below provisioned, and a non-empty
// PendingModifiedValues means the scenario's configuration was not in effect.
func (c *Collector) collectInstance(ctx context.Context, req diagnostics.Request, res *diagnostics.Result) []byte {
	out, argv, err := c.run(ctx, "rds", "describe-db-instances", "--db-instance-identifier", c.instanceID)
	if !c.save(res, req, req.Dest, "instance.json", argv, out, err) {
		return nil
	}
	return out
}

// collectParameters snapshots the whole resolved parameter group, not just
// --source user: values RDS derives from instance memory (shared_buffers and
// friends) are reported as "system" and differ across instance classes.
func (c *Collector) collectParameters(ctx context.Context, req diagnostics.Request, instance []byte, res *diagnostics.Result) {
	group, err := parameterGroupName(instance)
	if err != nil {
		c.save(res, req, req.Dest, "parameters.json", "", nil, err)
		return
	}
	out, argv, err := c.run(ctx, "rds", "describe-db-parameters", "--db-parameter-group-name", group)
	c.save(res, req, req.Dest, "parameters.json", argv, out, err)
}

// parameterGroupName reads the group out of a describe-db-instances response
// rather than fetching the same record twice. A status other than "in-sync"
// means the instance is not running the parameters the scenario set.
func parameterGroupName(instance []byte) (string, error) {
	if len(instance) == 0 {
		return "", errors.New("the instance description could not be read, so the parameter group is unknown")
	}
	var parsed struct {
		DBInstances []struct {
			DBParameterGroups []struct {
				Name   string `json:"DBParameterGroupName"`
				Status string `json:"ParameterApplyStatus"`
			} `json:"DBParameterGroups"`
		} `json:"DBInstances"`
	}
	if err := json.Unmarshal(instance, &parsed); err != nil {
		return "", fmt.Errorf("parse instance description: %w", err)
	}
	if len(parsed.DBInstances) == 0 || len(parsed.DBInstances[0].DBParameterGroups) == 0 {
		return "", errors.New("the instance description reports no parameter group")
	}
	g := parsed.DBInstances[0].DBParameterGroups[0]
	if g.Status != "" && g.Status != "in-sync" {
		return g.Name, fmt.Errorf("parameter group %s is %q, so the instance is not running the parameters the scenario set", g.Name, g.Status)
	}
	return g.Name, nil
}

// collectEvents captures the instance's event history over the run: backups
// (which briefly suspend I/O on a single-AZ primary), storage autoscaling,
// failovers, parameter applies.
func (c *Collector) collectEvents(ctx context.Context, req diagnostics.Request, s span, res *diagnostics.Result) {
	out, argv, err := c.run(ctx,
		"rds", "describe-events",
		"--source-type", "db-instance",
		"--source-identifier", c.instanceID,
		"--start-time", rfc3339(s.start),
		"--end-time", rfc3339(s.end),
	)
	c.save(res, req, req.Dest, "events.json", argv, out, err)
}
