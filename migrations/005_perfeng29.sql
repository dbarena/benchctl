-- PERFENG-29: store OpenTofu work dir as tar+gzip+base64 blob per run.
-- Populated after provision when a remote store is configured; empty for
-- local-store runs (docker-compose, local driver) where temp dirs persist.
-- Apply with:  psql "$DATABASE_URL" -f migrations/005_perfeng29.sql

ALTER TABLE runs ADD COLUMN IF NOT EXISTS tofu_state TEXT;
