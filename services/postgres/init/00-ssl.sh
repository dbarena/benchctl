#!/bin/bash
set -euxo pipefail

# No-op unless a real sslmode was requested (default "disable" preserves
# today's behavior). Runs against the temporary init server, which is always
# started without SSL — see server.crt/server.key comment below.
if [ "${SSLMODE:-disable}" = "disable" ]; then
  exit 0
fi

# Self-signed cert, generated into $PGDATA so Postgres finds it via its
# default ssl_cert_file/ssl_key_file ("server.crt"/"server.key", relative to
# the data directory) with no further GUC configuration needed.
cd "$PGDATA"
openssl req -new -x509 -days 3650 -nodes -text \
  -out server.crt -keyout server.key -subj "/CN=benchctl"
chmod 600 server.key

# ssl=on is set here (rather than as a `postgres -c ssl=on` command-line
# flag) because the entrypoint's temporary server -- used to run this very
# script -- is started with the same command-line flags before any init
# script runs, and would fail to find server.crt yet. ALTER SYSTEM writes to
# postgresql.auto.conf, which only takes effect on the *next* server start
# (the final one, after this script and the temp server have finished).
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -c "ALTER SYSTEM SET ssl = 'on';"
