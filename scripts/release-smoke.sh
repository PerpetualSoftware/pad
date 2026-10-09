#!/usr/bin/env bash
# release-smoke.sh — the release gate's per-binary check (TASK-2035).
#
# Called by GoReleaser as a builds[].hooks.post on every target, right after
# that binary is built and BEFORE anything is archived, signed, notarized,
# pushed or uploaded. A non-zero exit fails the release at the build stage,
# so a binary that does not start is never published. There is deliberately
# no switch to skip it (lead ruling): a flake gets a re-run, not a bypass.
#
#   release-smoke.sh <binary> <target> <version> <full-commit>
#
# <target> is GoReleaser's build target, e.g. linux_amd64_v1 or
# darwin_arm64_v8.0: GOOS, GOARCH, then the arch variant.
#
# Every target gets the STATIC check: the file is a Go executable for the
# platform it claims, built from this repository's cmd/pad with this
# version and commit (`go version -m`, which reads any GOOS/GOARCH). A target this runner can
# execute (linux/amd64 natively, linux/arm64 under qemu-user) also BOOTS:
# `--version` names the version and commit, and `server start` answers
# /api/v1/health with the same version and commit and an embedded web bundle
# built from the same commit. darwin and windows cannot run here; ci.yml's
# native-smoke job boots them from source on every PR.
set -euo pipefail

bin=${1:?binary} target=${2:?target} version=${3:?version} commit=${4:?full commit}
IFS=_ read -r goos goarch _ <<<"$target"
# The hook inherits the GoReleaser step's environment, which carries the
# macOS signing and notary secrets. The binary under test never sees them:
# it runs with an empty environment plus what it needs.
clean() { env -i PATH=/usr/bin:/bin HOME="${work:-/nonexistent}" "$@"; }
say() { printf 'release-smoke [%s/%s]: %s\n' "$goos" "$goarch" "$*" >&2; }
fail() { say "FAIL: $*"; exit 1; }

# --- Static: what the file is ------------------------------------------------
info=$(go version -m "$bin" 2>&1) || fail "not a Go executable: $info"
field() { awk -v k="$1" '$1 == "build" && index($2, k "=") == 1 { sub("^" k "=", "", $2); print $2 }' <<<"$info"; }
[[ $(awk 'NR == 2 && $1 == "path" { print $2 }' <<<"$info") == github.com/PerpetualSoftware/pad/cmd/pad ]] ||
	fail "main package is not github.com/PerpetualSoftware/pad/cmd/pad:"$'\n'"$info"
[[ $(field GOOS) == "$goos" ]] || fail "GOOS is '$(field GOOS)', want $goos"
[[ $(field GOARCH) == "$goarch" ]] || fail "GOARCH is '$(field GOARCH)', want $goarch"
# GoReleaser builds without VCS stamping (no vcs.revision), so the version
# and commit are read from the -ldflags the build recorded, which is what
# `--version` and /health report.
ldflags=$(sed -n 's/^[[:space:]]*build[[:space:]]*-ldflags=//p' <<<"$info")
[[ $ldflags == *"-X main.version=$version "* ]] || fail "ldflags do not set main.version=$version: $ldflags"
built_commit=$(sed -n 's/.*-X main\.commit=\([0-9a-f]*\).*/\1/p' <<<"$ldflags")
[[ -n $built_commit && $commit == "$built_commit"* ]] || fail "ldflags set main.commit='$built_commit', not a prefix of $commit"
say "static check passed"

# --- Boot: only where this runner can execute the target ---------------------
host_os=$(go env GOOS) host_arch=$(go env GOARCH)
if [[ $goos != "$host_os" ]]; then
	say "boot skipped: cannot execute $goos on $host_os (native-smoke covers it per PR)"
	exit 0
fi
if [[ $goarch != "$host_arch" && ! ( $goos == linux && -e /proc/sys/fs/binfmt_misc/qemu-${goarch/arm64/aarch64} ) ]]; then
	fail "cannot execute $goarch here and no qemu-user binfmt is registered; the workflow installs qemu-user-static first"
fi

line=$(clean "$bin" --version 2>&1) || fail "--version exited non-zero: $line"
short=$(sed -n 's/^pad version [^ ]* (\([0-9a-f]*\) .*/\1/p' <<<"$line")
[[ $line == "pad version $version ("* ]] || fail "--version said '$line', want version $version"
# --version prints the commit shortened for display, so it is a prefix check.
[[ -n $short && $commit == "$short"* ]] || fail "--version names commit '$short', not a prefix of $commit"

work=$(mktemp -d)
pid=
cleanup() {
	if [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT
port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
clean PAD_DATA_DIR="$work/data" "$bin" server start --host 127.0.0.1 --port "$port" >"$work/server.log" 2>&1 &
pid=$!

health=
for _ in $(seq 1 60); do
	if ! kill -0 "$pid" 2>/dev/null; then
		fail "the server exited during startup. Log:"$'\n'"$(tail -40 "$work/server.log")"
	fi
	if health=$(curl -fsS --max-time 2 "http://127.0.0.1:$port/api/v1/health" 2>/dev/null); then break; fi
	health=
	sleep 0.5
done
[[ -n $health ]] || fail "no /api/v1/health answer within 30s. Log:"$'\n'"$(tail -40 "$work/server.log")"

python3 - "$health" "$version" "$commit" "$built_commit" <<'PY' || fail "health check failed: $health"
import json, sys
h, version, commit, built = json.loads(sys.argv[1]), *sys.argv[2:]
problems = []
if h.get("status") != "ok": problems.append(f"status {h.get('status')!r}")
if h.get("version") != version: problems.append(f"version {h.get('version')!r}, want {version!r}")
if h.get("commit") != built: problems.append(f"commit {h.get('commit')!r}, want {built!r} (the ldflags value)")
web = h.get("web") or {}
if not web.get("files"): problems.append("no embedded web bundle")
if web.get("source_commit") != commit: problems.append(f"web built from {web.get('source_commit')!r}, want {commit!r}")
if problems:
    print("; ".join(problems), file=sys.stderr)
    sys.exit(1)
PY

kill "$pid"
for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
kill -0 "$pid" 2>/dev/null && fail "the server did not stop on SIGTERM within 5s"
pid=
say "boot check passed (version $version, commit $built_commit, web from the same commit)"
