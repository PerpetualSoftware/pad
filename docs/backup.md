# Backup & Restore Guide

## Overview

Pad provides built-in tooling for database backup, restore, and migration between SQLite and PostgreSQL.

| Command | Description |
|---------|-------------|
| `pad db backup` | Database backup — SQLite (`VACUUM INTO`, default) or PostgreSQL (`pg_dump`) |
| `pad db restore <file>` | Database restore — SQLite (file copy) or PostgreSQL (`psql`) |
| `pad db migrate-to-pg` | One-time SQLite → PostgreSQL migration |
| `pad workspace export` | Application-level JSON export (portable) |
| `pad workspace import` | Application-level JSON import |

`pad db backup` / `pad db restore` auto-detect the driver: PostgreSQL when
`PAD_DB_DRIVER=postgres` (or `PAD_DATABASE_URL` is set), SQLite otherwise. The
SQLite database path is resolved exactly as the server resolves it —
`PAD_DB_PATH` > `PAD_DATA_DIR/pad.db` > `~/.pad/pad.db` — so it works inside the
Docker image (which sets `PAD_DATA_DIR=/data`) without extra flags.

## SQLite Backups

SQLite stores everything in a single file (default: `~/.pad/pad.db`). The
canonical, online-safe way to back it up is `pad db backup`:

```bash
# Online-safe single-file backup via VACUUM INTO (safe while the server runs)
pad db backup -o ~/backups/pad-$(date +%Y%m%d).db

# Omit -o to get a timestamped pad-backup-YYYYMMDD-HHMMSS.db in the cwd
pad db backup
```

`pad db backup` uses SQLite's `VACUUM INTO`, which reads a consistent snapshot
through the SQLite engine and writes a single fully-checkpointed file — no
`-wal`/`-shm` sidecars to juggle, and no torn copy if the server is mid-write.
The database path is resolved the same way the server resolves it (see above),
so no `--from`/path flag is needed.

### Docker

Inside the official image (`PAD_DATA_DIR=/data`), run the backup through the
container so it resolves `/data/pad.db` automatically:

```bash
# Write the backup to the mounted /data volume, then copy it off-host
docker exec <container> pad db backup -o /data/backup.db
docker cp <container>:/data/backup.db ./pad-backup.db
```

Avoid `cp`-ing `pad.db` out from under a running server — a plain file copy can
tear against in-flight WAL writes and silently lose the `-wal` contents.

### Restore

```bash
# Stop the server first — restore refuses to run while it detects a live
# server (a running WAL checkpoint could clobber the restored file).
pad server stop
pad db restore ~/backups/pad-20250101.db
pad server start
```

Restore writes the backup over the resolved database path and clears any stale
`-wal`/`-shm` sidecars. Use `--force` to skip the confirmation prompt and
override the live-server guard (not recommended while the server is running).

## PostgreSQL Backups

### Manual Backup

```bash
# Create a SQL dump
pad db backup

# Specify output file
pad db backup --output /backups/pad-backup.sql
```

Requires:
- `pg_dump` installed
- `PAD_DATABASE_URL` environment variable set

### Automated Backups (Cron)

```bash
# Add to crontab: daily backup at 2 AM
0 2 * * * PAD_DATABASE_URL="postgres://pad:secret@localhost:5432/pad" /usr/local/bin/pad db backup --cron --output /backups/pad-$(date +\%Y\%m\%d).sql
```

The `--cron` flag uses structured log output suitable for log aggregation systems.

Both PostgreSQL commands accept `postgres://` / `postgresql://` URIs and
libpq keyword/value connection strings in `PAD_DATABASE_URL`. Database passwords
are passed to the native client through its environment, not its arguments.
For a PostgreSQL service configuration, keep the password in that service
rather than also supplying an inline password; service settings override
environment defaults. Likewise, configure an SSL key passphrase (`sslpassword`)
in a service file, since libpq has no corresponding environment variable.

### Restore

```bash
# Restore from backup (will prompt for confirmation)
pad db restore /backups/pad-backup.sql

# Skip confirmation (for automated restore)
pad db restore --force /backups/pad-backup.sql
```

### Cloud Database Snapshots

For managed PostgreSQL (AWS RDS, Google Cloud SQL, Azure Database):

- **AWS RDS**: Use automated backups + manual snapshots via the AWS Console or CLI
- **Google Cloud SQL**: Enable automated backups in instance settings
- **Azure**: Configure automated backups via the portal

These are generally preferred over `pg_dump` for large databases as they use filesystem-level snapshots.

## Migrating SQLite → PostgreSQL

When graduating from a local SQLite setup to production PostgreSQL:

```bash
# 1. Set up PostgreSQL and create the database
createdb pad

# 2. Run Pad once against PostgreSQL to create the schema
PAD_DB_DRIVER=postgres PAD_DATABASE_URL="postgres://pad:secret@localhost:5432/pad" pad server start &
# Wait a few seconds for migrations to run, then stop it
kill %1

# 3. Migrate workspace data
pad db migrate-to-pg \
  --from ~/.pad/pad.db \
  --to "postgres://pad:secret@localhost:5432/pad"

# 4. Create an admin account on the new database
PAD_DB_DRIVER=postgres PAD_DATABASE_URL="postgres://pad:secret@localhost:5432/pad" pad auth setup

# 5. Start the server with PostgreSQL
PAD_DB_DRIVER=postgres PAD_DATABASE_URL="postgres://pad:secret@localhost:5432/pad" pad server start
```

**What gets migrated:**
- Workspaces, collections, items, comments
- Item links (dependencies)
- Item versions (history)

**What does NOT get migrated:**
- User accounts and sessions (re-create with `pad auth setup`)
- Platform settings (reconfigure in admin panel)
- Activity/audit log (starts fresh)
- **The collaborative edit log.** Item bodies are migrated as they stand in the
  `items` table. Edits made in a browser tab that has not yet flushed them live
  only in the collaborative op-log, and that log does not travel — so the
  migration refuses to run while any item is in that state (see below).

### Stop the server first — the migration enforces it

`pad db migrate-to-pg` refuses to run while a Pad server answers on the
configured host and port:

```
the Pad server appears to be running at 127.0.0.1:7777 — stop it first
('pad server stop') so nothing can append collaborative edits while this
migration reads the database, or re-run with --force to override.
```

This is step 3 of the procedure above, enforced rather than merely documented.
The reason is not the WAL (that is `pad db restore`'s reason) but the op-log: a
live server is the only thing that can append a collaborative edit, and the
source read and the destination write live in two different databases, so no
transaction can span them. With the server stopped, an edit cannot land
mid-migration at all; with it running, no amount of re-checking makes the two
atomic.

Like `pad db restore`'s equivalent check, the probe asks whether *something*
healthy answers on that host and port, not whether it is serving this particular
database — so `--force` exists for the false positive. It also cannot see a second
server on a *different* port against the same SQLite file. So the guarantee is
"no reachable server at the configured address", not "nothing can possibly
append"; under `--force`, or against a server the probe cannot see, the two
checks below are what remain. If the config cannot be loaded at all the check is
skipped and refuses unless `--force` is given, so a run that reaches the copy
has either answered the question or been told to ignore it.

That gap — another process attached to the same database file that no address
probe can see — is closed from the other end by the marker below (BUG-3072).
The address probe stays as the cheap early refusal; neither door claims to be
complete on its own.

### The source database is marked when the migration succeeds

A successful `pad db migrate-to-pg` is the only command in Pad that ABANDONS
its source: afterwards you point the server at PostgreSQL and the SQLite file
stops being read. So when every workspace has been imported, and inside the
same transaction the migration held over the whole run, it marks the SQLite
file as migrated. From that moment:

- **The file refuses to be opened by `pad`.** `pad server start` and `pad db
  nul` against it fail with a message naming the PostgreSQL database the data
  went to.
- **Any handle that is still open refuses writes**, on any port, in any
  process, from any binary. This is the case that matters: a browser tab
  editing an item holds a connection that the migration's write lock only
  DEFERS — SQLite's `busy_timeout` belongs to the appender's connection, not to
  ours, so without the marker that edit would simply land a few seconds later,
  in the file you are about to abandon, and nothing would say so. With it, the
  write fails and the server logs the refusal.
- **Reads still work.** `pad db backup` does not go through the refusal (it
  opens its own connection and runs `VACUUM INTO`), and `sqlite3`, `cp` and any
  other tool are unaffected. The file is abandoned, not corrupt — take a copy
  before you delete it.

A migration that fails part-way does NOT mark the source: the transaction rolls
back and the SQLite database is left exactly as it was, so you can fix the
problem and re-run. The destination may be partially populated in that case —
no transaction spans two databases — and the error says so; drop and recreate
the PostgreSQL database before re-running.

Because the marker is permanent, a migration cannot be re-run against a file it
has already migrated. That is deliberate: the second run would copy a database
that has been frozen since the first.

### Unflushed collaborative edits

`pad db migrate-to-pg` **refuses** if any item's stored body is behind its live
collaborative document, and names the items:

```
2 item(s) have edits that exist only in the collaborative op-log,
which this migration does NOT carry. ...

  Beta (beta):
    TASK-7       half-typed body
    TASK-9       another one

Open each of those items in the web UI so the tab flushes its pending edits
into the database, then re-run this command. ...
```

The check covers every workspace *before* the first import runs, so a refusal
always means nothing has been migrated.

A second check runs on each bundle *after* it is exported and *before* it is
imported, because the pre-flight reads the database at one instant and the export
reads it at another. If an edit lands in between — only possible under `--force`,
since otherwise no server is running to make one — that workspace is refused too.
That refusal does **not** claim nothing has been migrated: earlier workspaces in
the run are already in PostgreSQL, and it says so with the count.

Opening the item in the web UI is the only remedy, and that is structural rather
than an omission: Pad's collaboration server is a dumb relay that stores opaque
Yjs updates without parsing them, so nothing server-side can turn those updates
back into markdown — only a client can. This is also why the escape hatch is
named for what it does to the data:

```bash
pad db migrate-to-pg --discard-unflushed-edits
```

That migrates anyway and **permanently loses those edits**, printing the same
list as a warning so the loss is on the terminal record. Every other route is
safer than this one and needs no flag: `pad db backup` / `pad db restore` copy
the whole database (`VACUUM INTO` / `pg_dump`), op-log included, and the
application-level export/import below leaves the source database in place, so a
stale body there can be re-exported once a tab has flushed. The migration is the
only operation that abandons its source.

## Application-Level Export/Import

For portable workspace backups that work across SQLite and PostgreSQL:

```bash
# Export a workspace to JSON
pad workspace export > my-workspace.json

# Import into any Pad instance (SQLite or PostgreSQL).
# The file is an ARGUMENT, not stdin — `pad workspace import < file` fails
# with "accepts 1 arg(s), received 0".
pad workspace import my-workspace.json

# Import with a new name
pad workspace import --name "imported-workspace" my-workspace.json
```

Item reference numbers are preserved when the imported items have positive,
workspace-unique numbers, including gaps left by deleted items. This keeps
references such as `[[TASK-2]]` pointing to the same task after restoration or
SQLite→PostgreSQL migration. Legacy archives with duplicate, missing, or invalid
numbers retain the sequential-renumbering fallback; references in those archives
may need manual repair.

### Bodies that were stale when the bundle was written

An export is a snapshot of the `items` table, and a browser tab can hold edits
that have not yet flushed into it. Such an item is exported with the body the
table holds plus a marker:

```json
{ "id": "...", "content": "the stored body", "content_state": "applied_pending_flush" }
```

The key is present only for affected items, so a bundle from a workspace with
nothing pending is byte-identical to one produced before the marker existed, and
older binaries ignore it (no bundle decoder rejects unknown keys). On import the
count comes back as a response header:

```
X-Pad-Import-Stale-Bodies: 2
```

The import **accepts** those items — refusing would make a backup unrestorable
for a reason nobody can fix from the destination — and the destination rows are
NOT themselves marked, because nothing in the destination is ahead of them. The
header is a provenance note: those bodies were already behind their source's live
document when the bundle was cut. The source workspace still has the real text in
its op-log, so re-exporting after a tab has flushed produces a clean bundle. The
one case where that is not true is `pad db migrate-to-pg`, which is why that
command refuses instead of warning.

### One case where an export is not importable

A workspace whose stored data contains a **NUL character** exports fine and is
refused on import, with a 400 naming the cause. This is not a corruption of
your backup — it is the import applying a rule the write path now applies too
(BUG-2803): Pad does not accept a NUL in a text or JSON value. That is an
application rule, not a universal storage fact — PostgreSQL does refuse a NUL
outright, but SQLite accepts one in a TEXT column, which is why the rule has to
be enforced rather than assumed, and why the paragraphs below matter.

**The rule is now enforced by the database as well as by the binary.** It used
to live only in the running build, which meant any window where an older binary
served the same SQLite database could still create such rows — a rollback, a
staged rollout, a second old instance pointed at the same file. A schema
migration now installs triggers that refuse the write in the database itself,
so an older binary writing to an upgraded file is refused too (BUG-2813). The
window that remains is a SQLite database an upgraded binary has never opened:
until its migrations run, it has no triggers.

Only SQLite ever needed this. PostgreSQL refuses a NUL in a text or JSON column
itself, at every binary version, so a PostgreSQL instance never stored such a
value regardless of which build wrote it.

None of that helps a row that was **already** stored, which is what the two
commands below are for.

### Finding and repairing affected rows

`pad db scan-nul` reports every stored value carrying a NUL — which table and
column, which row, and which workspace — and changes nothing:

```bash
pad db scan-nul                       # the live database
pad db scan-nul --from /backups/pad-20260901.db   # or a backup file
```

`pad db repair-nul` rewrites those values, replacing each NUL with U+FFFD (the
Unicode replacement character) and leaving the rest of the value byte for byte
as it was. **It changes stored content**, which is why it is a separate command
and never part of a migration — running a schema upgrade should not rewrite
your text on your behalf. Run the scan first; it is the dry run. Running the
repair twice is safe.

```bash
pad server stop
pad db repair-nul                     # lists what it will change, then asks
pad server start
```

A row whose **primary key** is the value carrying the NUL is reported and left
alone: repairing it would change the row's identity and could collide with
another row. `email_optouts` is the only table where that can happen today.

### Migrating to PostgreSQL

`pad db migrate-to-pg` now runs the same scan as a **preflight**. If the source
database carries any affected rows it lists them, prints the repair command,
and exits without moving anything — rather than failing partway through the
copy against PostgreSQL's JSONB parser, which is what it used to do.

A JSON value with LITERAL duplicate keys — `{"a":"...","a":"..."}` — used to
hide anything in the shadowed copy, because Pad's checks decoded it the way the
JSON decoder does and kept only the last. PostgreSQL refuses it all the same.
Since BUG-2812 the checks read every occurrence, so such a row is an ordinary
violation: listed by the scan and fixed by `pad db repair-nul`.

The preflight also asks the destination directly: any value that merely
*mentions* a NUL escape and is not already a violation is cast on the target
database before anything moves, and the migration is refused if PostgreSQL
rejects it. That check is exact in both directions — a document that only
writes *about* the escape is accepted, as it should be. `pad db scan-nul` lists
those values under a separate heading, and `pad db repair-nul` leaves them byte
for byte as they were.

Two things to know about that check:

- **It errs toward refusing.** If the destination cannot be reached, or a listed
  row cannot be read back, the migration is refused rather than attempted — an
  unchecked value is not a passed one. Re-run once the destination is reachable.
- **It can refuse a migration that would have worked.** The check casts the
  value as it is stored, and one column — a workspace's `settings` — is
  normalised on the way in, which happens to drop the hidden value. Such a row
  is still a value Pad refuses to write today, so `pad db repair-nul` clears it
  and the migration proceeds.

### Importing an export that predates the rule

If you have an export file taken from an affected database, the import still
refuses it by default and the 400 names the remedy. Passing `--repair-nul`
applies the same U+FFFD substitution to the payload as it is imported:

```bash
pad workspace import --repair-nul my-workspace.json
```

The default stays strict, and the flag is your consent to the rewrite; the
command reports how many values it changed. It repairs the payload the way the
server reads it, so it reaches a NUL wherever an export can carry one —
including inside an item's `fields` blob, which travels through an export as a
quoted document rather than as plain text.

Repairing the source database with `pad db repair-nul` and re-exporting gives
the same result without a rewrite at import time, and is the better option when
you still have the source instance.


This format is database-agnostic and can be used to:
- Transfer workspaces between Pad instances
- Create workspace templates
- Back up individual workspaces

## Backup Strategy Recommendations

### Small Teams (SQLite)

```
Daily: pad db backup --cron -o /backups/pad-$(date +%Y%m%d).db  (online-safe VACUUM INTO)
Weekly: Rotate old backups (keep 4 weeks)
```

### Production (PostgreSQL)

```
Continuous: WAL archiving (point-in-time recovery)
Daily: pg_dump via 'pad db backup --cron'
Weekly: Full filesystem snapshot (if using managed DB)
Monthly: Test restore procedure
```

### Disaster Recovery Checklist

- [ ] Backups are being created on schedule
- [ ] Backups are stored off-site (different region/provider)
- [ ] Restore procedure has been tested recently
- [ ] Recovery time objective (RTO) is documented
- [ ] Recovery point objective (RPO) is documented
