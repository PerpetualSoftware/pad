# Stage 1: Build web UI
FROM node:24-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.26-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/build ./web/build

# Build metadata. All three are caller-passed via --build-arg (see
# pad-cloud/scripts/build-pad.sh for the production wrapper that
# resolves them from the host's pad checkout).
#
# Why all three are passed in vs. computed inside the container:
#
#   - .dockerignore intentionally excludes .git/, so an in-container
#     `git rev-parse` substitution returns empty (with `2>/dev/null`
#     swallowing the error) — the previous Dockerfile shipped "dev"
#     forever because of this. We don't want to add .git/ to the
#     context just for this; pre-computing on the host is the
#     standard pattern.
#   - `date` would work in-container but a fresh `date` value on
#     every build invalidates layer caching for this RUN. Passing
#     as ARG lets the caller decide cache semantics.
#
# Defaults are deliberately ugly-but-honest so a `docker build .`
# without args produces a binary whose pad_version makes the
# misconfiguration obvious ("dev (unknown)") rather than hiding it.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o pad ./cmd/pad

# Stage 3: Runtime
FROM alpine:3.23
# ca-certificates: TLS roots for outbound HTTPS (e.g. Maileroo email).
# tzdata: timezone names for Go's time package.
# shadow: provides usermod / groupmod (BusyBox's adduser/addgroup don't
#         ship modify equivalents — needed by docker-entrypoint.sh).
# su-exec: lightweight alpine equivalent of gosu — execs the target in
#          the same process so SIGTERM propagates correctly. Used by the
#          entrypoint shim AND by the conditional healthcheck below.
RUN apk add --no-cache ca-certificates tzdata shadow su-exec
COPY --from=go-builder /app/pad /usr/local/bin/pad
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# In-image `pad` user/group at uid/gid 1000 — the entrypoint shim remaps
# these to match PUID/PGID at container start (defaults 99/100, the
# Unraid `nobody:users` convention). The actual numeric IDs can be
# anything; 1000 is just the historical default.
RUN addgroup -g 1000 pad \
    && adduser -D -u 1000 -G pad -h /home/pad pad \
    && mkdir -p /data \
    && chown -R pad:pad /data
ENV PAD_DATA_DIR=/data
ENV PAD_HOST=0.0.0.0

# IMPORTANT: NO `USER pad` directive — container starts as root so the
# entrypoint shim can chown /data + remap user/group ids before
# dropping privileges via `su-exec pad`. See TASK-1168 / PLAN-1166.

EXPOSE 7777
VOLUME /data

# Healthcheck adapts to the caller's chosen execution model:
#
#   • Default invocation (container starts as root, entrypoint drops to pad):
#     healthcheck runs as ROOT (Docker uses the image USER, which is root
#     because we removed the USER directive). Wrap with su-exec so it
#     matches the main process's unprivileged UID.
#
#   • `docker run --user 1234` invocation: healthcheck ALSO runs as 1234.
#     `su-exec pad ...` would fail because 1234 lacks the privilege to
#     switch to user `pad`. Just run wget directly in that branch.
#
# start-period bumped 10s → 60s. The always-chown-R policy (D4) means a
# user restoring a backup with a large attachment store can legitimately
# spend tens of seconds in the entrypoint before pad starts listening;
# 60s covers ~600k files at 10k files/sec on local SSD.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD sh -c 'if [ "$(id -u)" = "0" ]; then exec su-exec pad wget -q --spider http://localhost:7777/api/v1/health; fi; exec wget -q --spider http://localhost:7777/api/v1/health'

ENTRYPOINT ["docker-entrypoint.sh", "pad"]
CMD ["server", "start"]
