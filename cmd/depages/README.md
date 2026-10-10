# depages

Flags dependencies younger than a threshold (TASK-1391). It is the companion
to Dependabot's cooldown (TASK-1390, `.github/dependabot.yml`), which holds a
release for 7 days (14 for a major) before proposing it but cannot see a
version added by hand.

```sh
go run ./cmd/depages --days 7                 # every dependency in the tree
go run ./cmd/depages --days 7 --base origin/main   # only what is new since origin/main
```

It checks Go modules (`go.sum`), npm packages (`web/package-lock.json`) and
GitHub Actions pins (`uses: owner/repo@<sha>` in `.github/workflows`), dating
each from the Go module proxy, the npm registry packument and the GitHub
commits API (set `GH_TOKEN` to avoid the unauthenticated rate limit).

## What it covers

| Source | Covered | Dated by |
|---|---|---|
| Go modules (`go.sum`, content lines; pseudo-versions included) | yes | the module proxy's `.info` time |
| npm packages from the registry (`web/package-lock.json`, lockfile v2/v3; an alias as the package it installs) | yes | the packument's per-version `time` |
| npm git or tarball dependencies | no | they have no registry publish time |
| A v1 lockfile | refused, exit 2 | |
| Action pins `owner/repo[/path]@<sha>` in any YAML under `.github`, and in any `action.yml` / `action.yaml` in the repo | yes | the commit's committer date |
| `docker://` action images | no | not pinned by a commit SHA; no publish time here |

An action is dated by its COMMIT, deliberately: the threat is freshly
published code, and a pin to an old commit is old code whatever tag points at
it.

Exit status: `0` when nothing is too young (a dependency that cannot be dated
is printed as a warning, not a failure); `1` when one is younger than `--days`
and not allowed; `2` on a usage or local error.

CI runs it on every pull request as **Dependency age (advisory)**, against
the base branch (`--base HEAD^1` on the merge commit). It is not a required
check: a red result is a prompt, not a block.

## Allowing a young version

Add a line to `.github/dep-age-allow.txt`:

```
<go|npm|action> <name>@<version>  <reason>
```

for example `npm @tiptap/core@3.31.5  coordinated Tiptap bump (security fix)`.
The reason is required, so every exception says why. Remove the line once the
version is older than the threshold.
