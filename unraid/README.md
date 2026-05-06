# Pad on Unraid

Run [Pad](https://getpad.dev) on your Unraid server — local-first project management for developers and AI coding agents. Single Go binary, SQLite by default, your data stays on your box.

This directory holds the [Community Applications](https://forums.unraid.net/topic/38582-plug-in-community-applications/) (CA) template (`pad.xml`) and supporting docs. The full install walkthrough with screenshots lives at <https://getpad.dev/docs/install/unraid>.

## Prerequisites

- Unraid 6.12 or later.
- Community Applications plugin installed.

## Install

Once Pad is approved in the CA index (tracked in [PLAN-1166](https://github.com/PerpetualSoftware/pad)), the easy path:

1. Open **Apps** in the Unraid web UI.
2. Search for **Pad**.
3. Click **Install** → adjust the form fields if you want non-default ports / appdata path → **Apply**.
4. Wait ~10 seconds for the container to come up healthy.
5. Click the container's **Logs** button. Look for the box that starts with `Pad first-run setup` and copy the URL — it looks like `http://<your-tower>:7777/setup#token=<TOKEN>`.
6. Paste that URL into your browser. The token is read from the URL fragment (browser-only — never sent to a server log) and stripped from the address bar before you create the admin account. Fill in email, name, password → done.

### Pre-CA install (manual, for early adopters)

Before Pad lands in CA, you have two routes — both work as long as your Unraid + CA plugin is current:

#### Route A — CA "Add Repository" (recommended)

1. Open the **Apps** tab → **Settings** (top-right gear) → **Add Repository**.
2. Paste: `https://github.com/PerpetualSoftware/pad/tree/main/unraid/`
   - The trailing slash matters; CA scans for `*.xml` under that path.
3. Save. Pad now appears under **Apps** the same way it will after CA approval.

#### Route B — direct sideload (fallback)

If Route A doesn't pick up the template (older CA plugin, GitHub rate limit, etc.):

```bash
ssh root@<your-tower>
wget -O /boot/config/plugins/dockerMan/templates-user/my-pad.xml \
  https://raw.githubusercontent.com/PerpetualSoftware/pad/main/unraid/pad.xml
```

Then reload **Docker → Add Container** in the web UI and pick **Pad** from the template dropdown.

## Where your data lives

All persistent state lives under your appdata path (default `/mnt/user/appdata/pad/`):

| Path | What | Critical? |
| --- | --- | --- |
| `pad.db` + `pad.db-wal` + `pad.db-shm` | SQLite database | yes |
| `encryption.key` | Encryption key for sensitive fields (TOTP seeds, OAuth tokens) | **yes — losing this bricks encrypted data** |
| `attachments/` | Uploaded attachment blobs | yes |
| `logs/server.log` | Server log | nice-to-have |
| `config.toml` | Workspace config | nice-to-have |
| `pad.pid` | PID file | ephemeral |
| `.bootstrap-token` | First-run setup token (auto-deleted on first admin claim) | one-time |

One mount, complete coverage. The default appdata field in the template gives you all of this without thinking about it.

## Backups

Always stop the container first so SQLite isn't mid-write. Then:

```bash
# Backup
tar -C /mnt/user/appdata -czf "pad-backup-$(date +%F).tar.gz" pad/

# Restore
tar -C /mnt/user/appdata -xzf "pad-backup-2026-05-06.tar.gz"
```

The `-C` flag makes both archive and restore relative to `/mnt/user/appdata`, so the `pad/` directory inside the tarball always lands at `/mnt/user/appdata/pad/` regardless of where you run the command. Without `-C`, GNU tar warns "Removing leading /" and stores relative paths anyway, but extracting from a different cwd would scatter the data — easy to get wrong.

The container's entrypoint runs a `chown -R` to your configured `PUID:PGID` on every start, so PUID/PGID don't have to match between source and destination Unraid hosts.

## Upgrading

In Unraid → **Docker** → click the Pad container → **Force Update**. CA pulls a fresh `:latest`, recreates the container, and your appdata persists.

Pad releases are at <https://github.com/PerpetualSoftware/pad/releases>. Watching that repo on GitHub gets you a notification when a new version ships.

## Reverse proxy

If you front Pad with **SWAG** or **NGINX Proxy Manager** (recommended for HTTPS + a real hostname):

1. In the template's **Public URL** field, enter your external URL: `https://pad.example.com`.
2. In your reverse proxy, point `pad.example.com` at `<unraid-host>:7777`.
3. Standard reverse-proxy headers are fine — Pad doesn't need any special config.

The **Public URL** is required if you want emailed invitations to point at your real hostname (otherwise links point at `http://<unraid-ip>:7777`, which recipients can't reach).

## Email (optional)

If you want Pad to send workspace invitations by email, fill in:

- **Maileroo API Key** — sign up at <https://maileroo.com> (free tier is fine).
- **Email From** — sender address on a domain you control.
- **Email From Name** — display name (defaults to "Pad").

Without these, invitations fall back to copyable join codes you paste into the invitee's CLI. Not worse — just different.

## Troubleshooting

- **Port 7777 already in use** → change the **WebUI Port** field to a free port (e.g. 7778).
- **`/data` write errors in the logs** → the entrypoint validates `PUID`/`PGID` and rejects 0 or non-numeric. Stick to defaults (99/100) unless you have a specific reason; the entrypoint chowns appdata on every start, so a wrong-uid recovery is a single restart away.
- **Image won't pull** → verify GHCR is publicly pullable: `docker pull ghcr.io/perpetualsoftware/pad:latest` from any machine.
- **Reverse proxy returns 502** → the **Public URL** field must match the public-facing scheme + host (e.g. `https://pad.example.com`, not `http://`); also verify your proxy passes the request to the container's mapped port.
- **First-run banner not showing in logs** → it logs once on first start when zero users exist. If users already exist (you've already bootstrapped), the banner is suppressed. Restart only re-shows it on a fresh / wiped appdata.

## Support

- **Forum thread** *(once opened — see HT-1174)*: link will be added here and in the template's `<Support>` field.
- **GitHub Issues**: <https://github.com/PerpetualSoftware/pad/issues> for bug reports and feature requests.
- **Documentation**: <https://getpad.dev/docs/install/unraid> *(coming via TASK-1172)*.

## Note on the icon

The template's `<Icon>` URL points at `unraid/icon.png`, which is delivered by [TASK-1170](https://github.com/PerpetualSoftware/pad). Until that file lands the URL 404s and the CA listing renders without an icon — cosmetic only, doesn't block install.
