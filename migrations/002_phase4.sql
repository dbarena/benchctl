-- Phase 4: access control & operations.
-- Apply with:  psql "$DATABASE_URL" -f migrations/002_phase4.sql

-- created_by identifies the initiator of a run:
--   human users  → their Supabase auth UUID (auth.uid() from the JWT sub claim)
--   CI           → 'ci/nightly' or 'github:{actor}' (service role bypasses RLS)
--   local store  → '' (ignored; LocalStore has no RLS)
ALTER TABLE runs ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';

-- last_heartbeat is written every ~30s by the driver during workload.execute.
-- 'benchctl status' uses it to flag runs whose driver has gone silent as [stale].
ALTER TABLE runs ADD COLUMN IF NOT EXISTS last_heartbeat TIMESTAMPTZ;

-- Drop the Phase 3 permissive placeholder policies.
DROP POLICY IF EXISTS "all can insert" ON runs;
DROP POLICY IF EXISTS "all can update" ON runs;

-- Users may only insert runs attributed to themselves.
-- Service role key (CI) bypasses RLS entirely — no WITH CHECK is applied.
CREATE POLICY "own insert" ON runs
  FOR INSERT WITH CHECK (created_by = auth.uid()::text);

-- Users may only update their own runs (e.g. via 'benchctl teardown').
-- EngOps / CI use the service role key which bypasses this policy.
CREATE POLICY "own update" ON runs
  FOR UPDATE USING (created_by = auth.uid()::text);

-- Users may delete their own runs.
-- 'benchctl purge' with the service role key can delete any run (RLS bypassed).
CREATE POLICY "own delete" ON runs
  FOR DELETE USING (created_by = auth.uid()::text);
