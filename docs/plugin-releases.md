# The Claude Code plugin follows stable releases

`/plugin marketplace add PerpetualSoftware/pad` reads `.claude-plugin/marketplace.json` on `main`. Since TASK-3487 its `pad` entry is pinned to a stable release tag:

```json
"source": {
  "source": "git-subdir",
  "url": "https://github.com/PerpetualSoftware/pad.git",
  "path": "plugin",
  "ref": "v0.18.0"
}
```

so users get the plugin as of that release, not as of `main`. The `url` is the full https URL on purpose: Claude Code clones the `owner/repo` shorthand over SSH, which fails for anyone without a GitHub SSH key. Until the first stable tag that carries an unpinned `plugin/` (v0.18.0), the entry stays `"./plugin"`; pinning to an older tag would hand everyone a plugin that pins `version: 0.3.3` and predates the pane.

## How the pin moves

The release workflow's `pin-plugin` job runs after a **stable** tag's release (`vX.Y.Z`, never `-rc.N`) and calls `scripts/pin-plugin-marketplace.sh --push <tag>`, which rewrites only the entry's `source` and pushes to `main`. Users pick it up on `/plugin marketplace update` or auto-update: with no `version` field, Claude Code versions a `git-subdir` plugin by its commit, so a new `ref` is a new version.

Stable only, because the Homebrew cask never receives an rc: plugin vX pairs with CLI vX. Against an older CLI, the pane keeps working (it uses long-standing commands, with an MCP fallback), and the `pad` skill tells the user to upgrade when the CLI refuses a command.

If the push fails, the job does not fail the release. It warns and opens an issue; pin by hand from an up-to-date `main`:

```bash
scripts/pin-plugin-marketplace.sh --push v0.18.0
```

## Tracking `main` on purpose (maintainers)

Once the pin lands, an install from the marketplace is on the last stable release. To run the plugin as it is on your checkout instead:

- **One session:** `claude --plugin-dir /path/to/pad/plugin`. It loads in place, so edits take effect on `/reload-plugins`, and it takes precedence over an installed `pad@pad` of the same name.
- **Every session:** export `CLAUDE_CODE_PLUGIN_DIRS=/path/to/pad/plugin` in your shell profile. It is `--plugin-dir`'s environment form: the plugin loads in place as `pad@inline` and takes precedence over the installed `pad@pad`. Unset it to return to the stable install.

Adding your checkout as a local marketplace does **not** track your working tree: that checkout's own `marketplace.json` is pinned too, so it installs the tag.
