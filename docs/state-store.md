# Run state store

benchctl records every run in a state store. Set `store.mode` to `remote` to give every team member visibility into active runs, or leave it at the default `local` to keep run state on this machine.

Both modes support every command, including `--async` runs on an EC2 or GCE driver. `remote` adds sharing: only the machine that started a locally recorded run can follow it.

## Store selection

`store.mode` selects the store implementation. benchctl resolves it in this order:

1. `BENCHCTL_STORE_MODE` environment variable
2. `store.mode` in `~/.benchctl/config.yaml`
3. Default: `local`

Every benchctl setting resolves this way. An environment variable set to an
empty string counts as unset, so resolution falls through to the config file.

`local` keeps state in a SQLite database at `~/.benchctl/state.db` and needs no authentication.

`remote` uses a Supabase (PostgREST) store and requires `store.url` and `store.anon_key`. Both follow the same precedence: `BENCHCTL_STORE_URL` environment variable, then `store.url` in the config file; `BENCHCTL_STORE_ANON_KEY` environment variable, then `store.anon_key` in the config file.

| `store.mode` | Store | Location | Auth | Visible to |
|---|---|---|---|---|
| `local` (default) | SQLite | `~/.benchctl/state.db` | None | This machine |
| `remote` | Supabase (PostgREST) | `runs` table at `store.url` | `store.anon_key` (config or `BENCHCTL_STORE_ANON_KEY`) | Everyone with access to the project |

Set `store.mode` with `benchctl config set store.mode remote`, and set `store.url` and `store.anon_key` the same way. View resolved settings with `benchctl config show`.

## Async runs in local mode

An `--async` run executes on the driver instance, not on the machine that
started it, so the local record would go stale the moment benchctl hands off.
Rather than require a shared database, local mode reads the run back from the
driver instance on demand:

1. `benchctl run --async` copies the run record to the driver instance and
   imports it into that instance's own store before starting `benchctl resume`
   there. From then on the driver instance holds the authoritative copy.
2. `benchctl status`, `wait`, `connect`, `fetch` and `teardown` read that copy
   back by running `benchctl state export` on the driver instance, over the
   same SSH connection `benchctl connect` uses.
3. benchctl caches each snapshot locally for `store.refresh_interval` (default
   `30s`), so repeated reads do not open a new connection every time. Once a
   run reaches a terminal state, benchctl stops refreshing it.

When the driver instance is unreachable, after teardown or during a network
outage, reads fall back to the last snapshot instead of failing, and
`benchctl status` marks the run `[stale, <age> old]`. `benchctl wait` keeps
polling.

Two consequences worth knowing:

- `benchctl wait` notices a run finishing up to `store.refresh_interval` late.
  On runs that last hours this is immaterial; lower `refresh_interval` if you
  want tighter resolution, raise it on a slow link.
- Local mode leaves the OpenTofu working directory out of the run record. The
  directory already sits at `~/.benchctl/tofu-state/<run-id>` on this machine,
  where `benchctl teardown` looks for it. `remote` mode archives it into the
  record, so teardown can run from another machine or a CI runner.

## Authentication

### Developers (interactive)

```bash
./benchctl auth login   # opens browser, GitHub SSO, stores JWT at ~/.benchctl/credentials
./benchctl auth status  # verify auth and store access
```

The access token is short-lived but renews automatically from the stored refresh token. Log in again only when the refresh token expires.

The remote EC2 driver instance receives only the access token and cannot refresh it. Once the token expires, store updates from the driver fail silently while the benchmark continues. For long unattended runs, use the service role key.

### CI and admins (service role key)

```bash
export BENCHCTL_STORE_SERVICE_ROLE_KEY=<service-role-key>
```

The service role key bypasses RLS, never expires, and grants full store access. Retrieve it from **Project Settings → API → Project API keys** in the Supabase dashboard.

## Admin: setting up a new state store

Follow these steps to create or switch to a new Supabase project as the state store.

1. Create a new Supabase project dedicated to benchctl (keep operational state isolated from application projects).

2. Apply the migration:

   ```bash
   DB_URL="postgresql://postgres.{project-ref}@{pooler-host}:{port}/postgres"

   PGPASSWORD=<db-password> psql "$DB_URL" -f migrations/001_init.sql
   ```

   The connection string and database password are in **Project Settings → Database**.

3. After applying the migration, reload the PostgREST schema cache so the new columns become visible immediately:

   ```bash
   PGPASSWORD=<db-password> psql "$DB_URL" -c "NOTIFY pgrst, 'reload schema'"
   ```

| Migration | What it does |
|---|---|
| `001_init.sql` | Creates the `runs` table with the full column set, enables RLS, and adds ownership-scoped insert/update/delete policies |
