-- PERFENG-11: track successful infrastructure teardown.
-- Apply with:  psql "$DATABASE_URL" -f migrations/003_perfeng11.sql

-- terminated_at is set by `benchctl teardown` after infrastructure is
-- successfully destroyed. Distinguishes "completed, infra still up" from
-- "completed, infra torn down".
ALTER TABLE runs ADD COLUMN IF NOT EXISTS terminated_at TIMESTAMPTZ;
