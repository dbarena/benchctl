package supabase

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dbarena/benchctl/internal/diagnostics"
)

// configSnapshots contains the Management API calls to capture relevant
// diagnostic data for a Supabase project.
//
// Deliberately absent:
//
//	/upgrade/eligibility   current_app_version duplicates project.json's
//	                       database.version, postgres_engine and
//	                       release_channel
//	/advisors              static schema lints (unused index, missing primary
//	                       key) that say nothing about a run on a fixed schema
//	/health, `services`    describe the platform, not this project
//	/analytics/.../metrics an instantaneous Prometheus scrape with no
//	                       retention, so a post-run read is one sample taken
//	                       after the benchmark ended
var configSnapshots = []struct {
	file string
	path string
	// keep, when set, names the one top-level field worth retaining.
	keep string
	why  string
}{
	{file: "project.json", why: "region, status and the exact Postgres version"},
	// The IO ceiling, and the only place to find it: this is Supabase's
	// equivalent of describe-db-instances' Iops and StorageThroughput.
	{file: "disk.json", path: "/config/disk", why: "disk type, size, provisioned IOPS and throughput"},
	{file: "disk_usage.json", path: "/config/disk/util", why: "filesystem bytes used, for disk sizing"},
	// Overrides only. Empty means none were applied, which is worth recording
	// even though Supabase exposes no way to read the effective values.
	{file: "postgres_config.json", path: "/config/database/postgres", why: "Postgres GUC overrides, if any"},
	// selected_addons carries the compute variant: cores, memory, and the
	// baseline and burst disk IO ceilings. available_addons is a 10 KB price
	// list of products this project did not buy, which is not verbose so much
	// as unrelated.
	{file: "addons.json", path: "/billing/addons", keep: "selected_addons", why: "compute size"},
}

// collectConfig snapshots the project's configuration.
func (c *Collector) collectConfig(ctx context.Context, req diagnostics.Request, res *diagnostics.Result) {
	for _, s := range configSnapshots {
		body, requestURL, err := c.get(ctx, s.path, nil)
		if err == nil && s.keep != "" {
			body, err = keepField(body, s.keep)
		}
		req.Save(res, req.Dest, s.file, "GET "+requestURL, body, err)
	}
}

// keepField reduces a JSON object to one top-level field. An unexpected shape
// is returned untouched rather than discarded.
func keepField(raw []byte, field string) ([]byte, error) {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	kept, ok := parsed[field]
	if !ok {
		return raw, nil
	}
	out, err := json.MarshalIndent(map[string]json.RawMessage{field: kept}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
