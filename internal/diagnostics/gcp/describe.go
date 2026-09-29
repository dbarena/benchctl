package gcp

import (
	"context"
	"net/url"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// collectInstance snapshots the Cloud SQL instance configuration.
//
// Read afterwards to confirm the instance ran as asked, not just for
// provenance: settings.storageAutoResize should be false, which
// deployments/gcp-cloudsql/postgres pins so the disk cannot grow mid-run;
// settings.dataDiskProvisionedIops and dataDiskProvisionedThroughput should
// match the scenario; and databaseInstalledVersion gives the minor version,
// which decides whether pg_stat_io exists.
func (c *Collector) collectInstance(ctx context.Context, req diagnostics.Request, res *diagnostics.Result) {
	endpoint := c.sqlAdminHost + "/v1/projects/" + c.projectID + "/instances/" + url.PathEscape(c.instance)
	body, requestURL, err := c.get(ctx, endpoint, nil)
	req.Save(res, req.Dest, "instance.json", "GET "+requestURL, body, err)
}

// collectOperations captures the instance's operation history.
//
// This is Cloud SQL's equivalent to RDS events: an UPDATE, RESTART,
// BACKUP_VOLUME or maintenance operation landing inside a window can help
// explain drastic changes in throughput. The API takes no time range, so the
// whole history is returned and filtered when read.
func (c *Collector) collectOperations(ctx context.Context, req diagnostics.Request, res *diagnostics.Result) {
	endpoint := c.sqlAdminHost + "/v1/projects/" + c.projectID + "/operations"
	body, requestURL, err := c.get(ctx, endpoint, url.Values{"instance": {c.instance}})
	req.Save(res, req.Dest, "operations.json", "GET "+requestURL, body, err)
}
