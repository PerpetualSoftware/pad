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
