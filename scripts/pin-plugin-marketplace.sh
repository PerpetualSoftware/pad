#!/usr/bin/env bash
# pin-plugin-marketplace.sh <tag> — point the Claude Code plugin marketplace
# at a release tag (TASK-3487).
#
# `.claude-plugin/marketplace.json` on main is what `/plugin marketplace add
# PerpetualSoftware/pad` reads. Its pad entry used to say `"source":
# "./plugin"`, so every user ran the plugin as of main, ahead of the CLI they
# had installed. After this runs, the entry is a `git-subdir` source pinned to
# the tag:
#
#   {"source": "git-subdir", "url": "https://github.com/PerpetualSoftware/pad.git",
#    "path": "plugin", "ref": "<tag>"}
#
# The url is the full https URL, never the owner/repo shorthand: Claude Code
# clones the shorthand over SSH (git@github.com:), so a user with no GitHub
# SSH key could not install the plugin even though the repo is public
# (measured with `claude plugin install` 2.1.294).
#
# and users get the plugin as of that release. With no `version` field, Claude
# Code versions a git-subdir plugin by its commit SHA, so moving `ref` is what
# delivers an update (BUG-3463's "no pinned version" still holds).
#
# STABLE TAGS ONLY. The Homebrew cask never receives a prerelease (goreleaser
# `skip_upload: auto`, BUG-2524), so most installed CLIs are stable releases.
# A plugin that tracked rc tags could tell the agent to use a command the
# user's CLI does not have yet. Pinned to stable, plugin vX pairs with CLI vX.
#
# It also refuses a tag whose plugin/.claude-plugin/plugin.json pins a
# `version`: that tag would freeze every install at the pinned string.
#
# It rewrites only the pad entry's `source`; every other byte of the file is
# kept. Re-running for the tag already pinned changes nothing and exits 0.
#
# Usage:
#   scripts/pin-plugin-marketplace.sh v0.18.0           # rewrite the file
#   scripts/pin-plugin-marketplace.sh --push v0.18.0    # also commit and push
#
# --push commits as github-actions[bot] and pushes to the current branch,
# retrying a rejected push after a fetch and rebase (the nix heal job's
# pattern). The release workflow runs it after the release is published; a
# push that fails every retry exits 3 so the caller can warn without failing
# the release.
set -euo pipefail

push=0
if [ "${1:-}" = "--push" ]; then
	push=1
	shift
fi
tag="${1:-}"
die() {
	echo "pin-plugin-marketplace: $*" >&2
	exit 2
}

[ -n "$tag" ] || die "usage: $0 [--push] <vX.Y.Z>"
if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	die "$tag is not a stable release tag (vX.Y.Z); the plugin follows stable releases only"
fi

root=$(git rev-parse --show-toplevel)
cd "$root"
market=".claude-plugin/marketplace.json"
[ -f "$market" ] || die "$market not found"

git rev-parse -q --verify "refs/tags/$tag^{commit}" >/dev/null ||
	git fetch -q origin "refs/tags/$tag:refs/tags/$tag" ||
	die "tag $tag not found"

manifest=$(git show "$tag:plugin/.claude-plugin/plugin.json" 2>/dev/null) ||
	die "$tag has no plugin/.claude-plugin/plugin.json"
if printf '%s' "$manifest" | python3 -c 'import json,sys; sys.exit(0 if "version" in json.load(sys.stdin) else 1)'; then
	die "$tag's plugin.json pins a version; installs from it would never update (BUG-3463)"
fi

changed=$(python3 - "$market" "$tag" <<'PY'
import json, sys
path, tag = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as f:
    text = f.read()
doc = json.loads(text)
want = {"source": "git-subdir", "url": "https://github.com/PerpetualSoftware/pad.git", "path": "plugin", "ref": tag}
hits = [p for p in doc.get("plugins", []) if p.get("name") == "pad"]
if len(hits) != 1:
    sys.exit("pin-plugin-marketplace: expected exactly one plugin named pad, found %d" % len(hits))
if hits[0].get("source") == want:
    print("no")
    sys.exit(0)
hits[0]["source"] = want
with open(path, "w", encoding="utf-8") as f:
    f.write(json.dumps(doc, indent=2, ensure_ascii=False) + "\n")
print("yes")
PY
)

if [ "$changed" = "no" ]; then
	echo "pin-plugin-marketplace: already pinned to $tag"
	exit 0
fi
echo "pin-plugin-marketplace: pinned the pad plugin to $tag"
[ "$push" = 1 ] || exit 0

git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
git add "$market"
git commit -q -m "chore(plugin): the marketplace follows $tag (TASK-3487)"
branch=$(git rev-parse --abbrev-ref HEAD)
for attempt in 1 2 3 4 5; do
	if git push -q origin "HEAD:$branch"; then
		echo "pin-plugin-marketplace: pushed to $branch"
		exit 0
	fi
	echo "pin-plugin-marketplace: push rejected (attempt $attempt); rebasing onto origin/$branch" >&2
	git fetch -q origin "$branch"
	if ! git rebase -q "origin/$branch"; then
		git rebase --abort || true
		echo "pin-plugin-marketplace: rebase conflicted; giving up" >&2
		exit 3
	fi
	sleep $((attempt * 5))
done
echo "pin-plugin-marketplace: push failed after 5 attempts" >&2
exit 3
