#!/bin/sh
# docker-entrypoint.sh — UID/GID remapping shim for the pad container.
#
# Allows operators to run pad as their host's preferred uid/gid by setting
# PUID and PGID env vars at container start. Mainly for Unraid users
# (default 99/100 = nobody:users) but useful on Synology / TrueNAS / any
# Docker host where the appdata volume is owned by something other than
# uid 1000.
#
# Pattern adopted from LinuxServer.io. See TASK-1168 / PLAN-1166.
#
# Compatibility: /bin/sh (BusyBox-compatible). No bash-isms.

set -e

# Pass-through path: caller specified a uid via `docker run --user`. They
# know what they want; we don't try to outsmart them. No chown, no
# privilege drop — just exec the target program directly.
if [ "$(id -u)" != "0" ]; then
    exec "$@"
fi

# Remap path: container started as root (the default).
#
# Use ${VAR-default} (no colon), not ${VAR:-default}. The colon form
# substitutes the default when VAR is unset OR empty, which would silently
# accept `docker run -e PUID= -e PGID=` and mask a real operator typo.
# The colon-less form only substitutes when VAR is unset, so an explicit
# empty value falls through to validation and is rejected.
PUID="${PUID-99}"
PGID="${PGID-100}"

# Validate: must be positive integers. Rejecting 0 explicitly is critical
# — PUID=0 (root) or PGID=0 (root group) would defeat the unprivileged-
# user invariant the whole shim exists to enforce. Empty / non-numeric
# also fail loudly so an operator typo doesn't silently default.
case "$PUID" in
    ''|*[!0-9]*)
        echo "docker-entrypoint: PUID must be a positive integer, got '$PUID'" >&2
        exit 1
        ;;
esac
case "$PGID" in
    ''|*[!0-9]*)
        echo "docker-entrypoint: PGID must be a positive integer, got '$PGID'" >&2
        exit 1
        ;;
esac
if [ "$PUID" = "0" ]; then
    echo "docker-entrypoint: PUID=0 (root) is rejected — set a non-zero uid to keep pad unprivileged" >&2
    exit 1
fi
if [ "$PGID" = "0" ]; then
    echo "docker-entrypoint: PGID=0 (root group) is rejected — set a non-zero gid to keep pad unprivileged" >&2
    exit 1
fi

# Remap the in-image `pad` user/group only when values differ — skips
# the noisy "id changed" log entry on every warm restart in the steady
# state.
#
# Use `id -u/-g pad` (BusyBox supports both flags) rather than
# `getent group pad`: getent lives in the optional `musl-utils` package
# on alpine, NOT in BusyBox's default applet list, so the entrypoint
# would crash before chown/exec on a fresh `apk add` install that
# didn't include it.
current_uid="$(id -u pad)"
current_gid="$(id -g pad)"
if [ "$current_gid" != "$PGID" ]; then
    groupmod -o -g "$PGID" pad
fi
if [ "$current_uid" != "$PUID" ] || [ "$current_gid" != "$PGID" ]; then
    # Combined -u + -g ensures the user's /etc/passwd primary-gid field
    # is updated to PGID even when only the group renumber happened.
    # `groupmod` alone changes the group's gid but leaves the user's
    # /etc/passwd line referencing the OLD gid number — su-exec would
    # then launch pad with the wrong process gid.
    usermod -o -u "$PUID" -g "$PGID" pad
fi

# Always chown -R the data directory.
#
# Statting only `/data` itself is unsafe: a user who restored a backup
# tarball might have /data owned by the target uid but /data/pad.db,
# /data/encryption.key, or /data/attachments/* owned differently, and a
# shallow check would skip the recursive chown — leaving pad unable to
# read its own DB. The few seconds of chown cost on warm restart with
# large attachment stores is the correct trade for guaranteed correctness.
#
# Warn-and-continue (not fail-fast). chown -R can fail on individual
# files for non-fatal reasons (immutable bits, root-squashed NFS,
# partial permission gaps). Refusing to start pad over any single
# failed file would lock the operator out of an otherwise-fine /data.
# chown's per-file errors hit stderr already (visible in `docker logs`);
# the trailing aggregated warning surfaces a single clear "investigate
# this" line. The `||` form is set-e-safe.
chown -R "$PUID:$PGID" /data \
    || echo "docker-entrypoint: warning: chown -R /data had errors (see above). pad will start anyway; some files may not be writable." >&2

# Drop privileges and exec. su-exec replaces the shell process so signals
# (SIGTERM, SIGINT) propagate to the pad process correctly — without
# this, docker stop's SIGTERM would hit the shell instead of pad.
exec su-exec pad "$@"
