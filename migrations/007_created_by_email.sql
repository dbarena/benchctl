-- Persist the launching user's email at run-creation time so `benchctl
-- status` can show a human-readable CREATED BY for every user, not just
-- the current viewer (their UUID has no reverse lookup without admin API
-- access). Apply with:  psql "$DATABASE_URL" -f migrations/007_created_by_email.sql

ALTER TABLE runs ADD COLUMN IF NOT EXISTS created_by_email TEXT;
