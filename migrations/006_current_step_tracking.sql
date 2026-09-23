-- Track the fixture/iteration/step a run is currently executing, so external
-- tooling (e.g. a pprof-capture script polling `benchctl status -o json`)
-- can tell which fixture is active and how long its current step has run.
-- Apply with:  psql "$DATABASE_URL" -f migrations/006_current_step_tracking.sql

ALTER TABLE runs ADD COLUMN IF NOT EXISTS current_fixture JSONB;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS current_iteration INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS current_step TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS step_started_at TIMESTAMPTZ;
