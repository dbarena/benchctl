-- benchctl run state table.
-- Apply with:  psql "$DATABASE_URL" -f migrations/001_init.sql

CREATE TABLE IF NOT EXISTS runs (
    run_id             TEXT        PRIMARY KEY,
    scenario_name      TEXT        NOT NULL,
    scenario_path      TEXT        NOT NULL,
    target_provider    TEXT        NOT NULL DEFAULT '',
    driver_provider    TEXT        NOT NULL DEFAULT '',
    started_at         TIMESTAMPTZ NOT NULL,
    inputs             JSONB       NOT NULL DEFAULT '{}',
    phases             JSONB       NOT NULL DEFAULT '{}',
    target_outputs     JSONB,
    driver_outputs     JSONB,
    completed_at       TIMESTAMPTZ,
    error              TEXT,

    -- created_by identifies the initiator of a run:
    --   human users  → their Supabase auth UUID (auth.uid() from the JWT sub claim)
    --   CI           → 'ci/nightly' or 'github:{actor}' (service role bypasses RLS)
    --   local store  → '' (ignored; LocalStore has no RLS)
    created_by         TEXT        NOT NULL DEFAULT '',

    -- last_heartbeat is written every ~30s by the driver during workload.execute.
    -- 'benchctl status' uses it to flag runs whose driver has gone silent as [stale].
    last_heartbeat     TIMESTAMPTZ,

    -- terminated_at is set by `benchctl teardown` after infrastructure is
    -- successfully destroyed. Distinguishes "completed, infra still up" from
    -- "completed, infra torn down".
    terminated_at      TIMESTAMPTZ,

    -- metadata holds a flat string map populated after collection: scenario labels,
    -- workload.info results, and collector config (effective_date, endpoint).
    metadata           JSONB       NOT NULL DEFAULT '{}',

    -- tofu_state stores the OpenTofu work dir as a tar+gzip+base64 blob per run.
    -- Populated after provision when a remote store is configured; empty for
    -- local-store runs (docker-compose, local driver) where temp dirs persist.
    tofu_state         TEXT,

    -- current_fixture/current_iteration/current_step/step_started_at track the
    -- fixture/iteration/step a run is currently executing, so external tooling
    -- (e.g. a pprof-capture script polling `benchctl status -o json`) can tell
    -- which fixture is active and how long its current step has run.
    current_fixture    JSONB,
    current_iteration  INTEGER     NOT NULL DEFAULT 0,
    current_step       TEXT,
    step_started_at    TIMESTAMPTZ,

    -- created_by_email persists the launching user's email at run-creation time
    -- so `benchctl status` can show a human-readable CREATED BY for every user,
    -- not just the current viewer (their UUID has no reverse lookup without
    -- admin API access).
    created_by_email   TEXT
);

ALTER TABLE runs ENABLE ROW LEVEL SECURITY;

-- Users may only view/insert/update/delete runs attributed to themselves.
-- Service role key (CI, EngOps tooling) bypasses RLS entirely.
CREATE POLICY "all can view" ON runs FOR SELECT USING (true);

CREATE POLICY "own insert" ON runs
  FOR INSERT WITH CHECK (created_by = auth.uid()::text);

CREATE POLICY "own update" ON runs
  FOR UPDATE USING (created_by = auth.uid()::text);

CREATE POLICY "own delete" ON runs
  FOR DELETE USING (created_by = auth.uid()::text);

-- Table privileges are separate from RLS: RLS restricts *rows*, but without
-- an explicit GRANT, a role has no access to the table at all. Supabase
-- currently auto-grants anon/authenticated/service_role on new public-schema
-- tables by default (changing per supabase/discussions#45329); revoke anon
-- explicitly and grant only the roles benchctl actually uses.
REVOKE ALL ON runs FROM anon, PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON runs TO authenticated, service_role;
