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

## Application-Level Export/Import

For portable workspace backups that work across SQLite and PostgreSQL:

```bash
# Export a workspace to JSON
pad workspace export > my-workspace.json

# Import into any Pad instance (SQLite or PostgreSQL)
pad workspace import < my-workspace.json

# Import with a new name
pad workspace import --name "imported-workspace" < my-workspace.json
```

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
