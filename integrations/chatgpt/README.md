# Pad plugin for ChatGPT

The plugin package for OpenAI's plugin directory (TASK-3321 U4), in the Agent Plugins 1.0.0 format that OpenAI's submission takes (developers.openai.com/plugins/deploy/submission):

| File | What it is |
|---|---|
| `plugin.json` | Manifest: package identity, listing metadata under `extensions["com.openai"].interface`, and the review test cases (exactly 5 positive and 3 negative) under `extensions["com.openai"].review.test_cases`. |
| `mcp.json` | The one MCP server: Pad Cloud's ChatGPT catalog at `https://mcp.getpad.dev/mcp/chatgpt`. |
| `skills/pad/SKILL.md` | The MCP-only Pad skill. It names only the 21 tools in `internal/mcp/chatgpt_catalog.go`, with no CLI, shell or local paths. |
| `assets/` | `logo.png` and `composer-icon.png`, currently copies of `web/static/icon-512.png`. |

`internal/mcp/chatgpt_plugin_package_test.go` keeps this package honest. It checks that every tool the skill or a test case names is in the ChatGPT catalog, that the skill carries no CLI or filesystem marks, and that the submission's documented limits hold: field lengths, test case counts, icon size and squareness, brand color contrast, a single MCP server, and no apps or hooks. Rename a catalog tool and that test fails until this package follows.

## Not ready to submit

Nothing here has been submitted or registered. The package is not submittable until:

- the `/mcp/chatgpt` mount ships on Pad Cloud (TASK-3321 U2), with the minimized responses (U3) and the versioned `update_item` (U1b) it depends on;
- `https://www.getpad.dev/support` exists (it answers 404 today; `supportURL` points at it);
- the reviewer demo account, demo video, domain challenge token and org verification are done (ops).

## Domain verification

Pad Cloud serves OpenAI's domain challenge at `https://mcp.getpad.dev/.well-known/openai-apps-challenge` once the token from the plugin dashboard is set as `PAD_OPENAI_APPS_CHALLENGE` (or `openai_apps_challenge` in the config file) in the pad-cloud environment. The body is the token alone, as text/plain. It answers only on cloud and only on the MCP URL's host; anywhere else, and while the variable is unset, it is the same JSON 404 as any other unpublished `/.well-known` path. A value with whitespace or a control character inside it is refused at startup with a warning rather than served. The token is configuration, so it never goes in this package.

The full list, with the draft submission metadata and the demo account's seed data, is DOC-3328 ("ChatGPT plugin submission draft") in the Pad workspace.

## Resetting the reviewer's demo workspace

The test cases run against a workspace called Acme Launch (`acme-launch`), whose contents are specified in DOC-3328 section 4. `scripts/chatgpt-review-seed.sh` resets it to that state through the pad CLI, as whichever account the CLI is signed in as. It creates no account: set the reviewer account up first, then `pad auth login` as it against the target instance.

```bash
PAD_URL=https://app.getpad.dev scripts/chatgpt-review-seed.sh --workspace acme-launch --yes
PAD_URL=https://app.getpad.dev scripts/chatgpt-review-seed.sh --workspace acme-launch --verify-only
```

A reset archives every live item outside Conventions and Playbooks, creates the workspace (startup template) and a Bugs collection if they are missing, re-creates the seed items, links and comments, and then checks every test case's precondition. `--verify-only` runs only the check. Test cases P4 and P5 change the workspace, so reset it between review runs. The test cases name items by title, so the new refs each reset creates do not matter. Set `PAD_REVIEW_SECOND_TOKEN` (an API token of a second member of the workspace) to have the bug's two comments come from someone else.

## Building the ZIP

Zip the contents of this directory, not the directory itself, so `plugin.json` sits at the root of the archive. Leave out `README.md`. Never add reviewer credentials, the challenge token or any other secret: those go in the submission form, not the package.

```bash
cd integrations/chatgpt && python3 -m zipfile -c ../../pad-chatgpt-plugin.zip plugin.json mcp.json skills assets
```

(`zip -r` with the same arguments works too, where `zip` is installed.)
