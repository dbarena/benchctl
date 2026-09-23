-- PERFENG-25: add metadata JSONB column to runs table.
-- Apply with:  psql "$DATABASE_URL" -f migrations/004_perfeng25.sql

-- metadata holds a flat string map populated after collection: scenario labels,
-- workload.info results, and collector config (effective_date, endpoint).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
