# Stage 1: Build web UI
FROM node:22-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.25-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/build ./web/build
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION} -X main.commit=$(git rev-parse --short HEAD 2>/dev/null) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o pad ./cmd/pad

# Stage 3: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go-builder /app/pad /usr/local/bin/pad

# Data directory
RUN mkdir -p /data
ENV PAD_DATA_DIR=/data
ENV PAD_HOST=0.0.0.0

EXPOSE 7777
VOLUME /data

ENTRYPOINT ["pad"]
CMD ["server"]
