# Stage 1: Build web UI
FROM node:22-alpine AS web-builder
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
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go-builder /app/pad /usr/local/bin/pad

# Create a non-root user (uid 1000) and data directory owned by it
RUN adduser -D -u 1000 -h /home/pad pad \
    && mkdir -p /data \
    && chown -R pad:pad /data
ENV PAD_DATA_DIR=/data
ENV PAD_HOST=0.0.0.0

USER pad
EXPOSE 7777
VOLUME /data

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:7777/api/v1/health || exit 1

ENTRYPOINT ["pad"]
CMD ["server", "start"]
