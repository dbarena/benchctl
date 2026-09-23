-- benchctl run state table
-- Apply with:  psql "$DATABASE_URL" -f migrations/001_create_runs.sql

CREATE TABLE IF NOT EXISTS runs (
    run_id          TEXT        PRIMARY KEY,
    scenario_name   TEXT        NOT NULL,
    scenario_path   TEXT        NOT NULL,
    target_provider TEXT        NOT NULL DEFAULT '',
    driver_provider TEXT        NOT NULL DEFAULT '',
    started_at      TIMESTAMPTZ NOT NULL,
    inputs          JSONB       NOT NULL DEFAULT '{}',
    phases          JSONB       NOT NULL DEFAULT '{}',
    target_outputs  JSONB,
    driver_outputs  JSONB,
    completed_at    TIMESTAMPTZ,
    error           TEXT
);

ALTER TABLE runs ENABLE ROW LEVEL SECURITY;

-- Phase 3: permissive policies — tighten in Phase 4 when created_by + auth.uid() are wired.
CREATE POLICY "all can view"   ON runs FOR SELECT USING (true);
CREATE POLICY "all can insert" ON runs FOR INSERT WITH CHECK (true);
CREATE POLICY "all can update" ON runs FOR UPDATE USING (true);
