# `cmdhelp.schema.json`

Formal JSON Schema (draft 2020-12) for the [`cmdhelp` v0.1](https://getpad.dev/cmdhelp) wire format — the structured output produced by `<cmd> help --format json` on any conforming CLI.

## What this is

`cmdhelp` is a tiny vendor-neutral convention for CLI tools to expose their documentation and invocation schema to LLMs and LLM harnesses (Claude Code, Cursor, MCP servers, IDE plugins). Each conforming CLI emits two formats from one source of truth:

- **Markdown** (`--format md`) — drop-in context for an LLM to *read*.
- **JSON** (`--format json`) — structured schema for an LLM (or its harness) to *call*. ← **this schema describes that JSON.**

See the full v0.1 spec on [getpad.dev/cmdhelp](https://getpad.dev/cmdhelp) (or `IDEA-927` in this workspace).

## Who consumes this

- **CLI authors** validate their `--format json` output as a CI step so the wire format never silently drifts.
- **Wrappers** (MCP servers, IDE plugins, agent harnesses) validate received `cmdhelp` documents before parsing, so a buggy producer surfaces as a clear schema error rather than a mysterious downstream failure.
- **Spec implementers** use it as the authoritative reference for which fields are required, which are optional, and what shapes are allowed.

## Usage

The schema is published at the canonical URL inside Pad's reference implementation, and committed into this repository at `schema/cmdhelp.schema.json`.

### Validating a `cmdhelp` document with `ajv`

```bash
npm install -g ajv-cli
pad help --format json > /tmp/cmdhelp.json
ajv validate -s schema/cmdhelp.schema.json -d /tmp/cmdhelp.json --spec=draft2020
```

### Validating with Python `jsonschema`

```bash
pip install 'jsonschema[format]'
python -c "
import json, jsonschema, sys
schema = json.load(open('schema/cmdhelp.schema.json'))
doc    = json.load(sys.stdin)
jsonschema.validate(doc, schema)
print('OK')
" < <(pad help --format json)
```

### Validating in Go (used by Pad's own test suite)

The test suite in `internal/cmdhelp/` validates the live `pad help --format json` output against this schema as part of `go test ./...`. A drift between emitter and schema fails CI. (See `TASK-938` for the test-side contract.)

## v0.1 highlights

- **Required top-level keys:** `cmdhelp_version`, `binary`, `commands`.
- **Argument-type vocabulary** (closed set + `x-*` extension namespace): `string`, `int`, `float`, `bool`, `enum` (required); `path`, `url`, `duration`, `date`, `datetime`, `json`, `ref` (recommended); `x-<tool-specific>` (extension).
- **Boolean flag arity** (spec §5.3): bool flags are presence switches by default; valued booleans use `enum: ["true", "false"]`; negation via the optional `negate_flag` field.
- **`exit_codes` union** (spec §5.2): each entry MAY be a terse string OR a rich object `{ when, recovery?, message_template? }`. Consumers MUST handle both shapes.
- **Dynamic enums** (spec §7): args/flags that depend on session state (workspace, profile, account) declare an `enum_source: "dynamic:<command>"` and MAY include the resolved values in `enum`.
- **Wire format version** (spec §9): `MAJOR.MINOR` only — never includes a PATCH component. Validated by pattern `^[0-9]+\.[0-9]+$`.

## Versioning

This file's schema describes **cmdhelp v0.1**. When the spec moves to a new MAJOR or MINOR, this file is updated in lockstep and the `$id` URL bumps.

Forward-compat: tools advertising `cmdhelp/0.1` MUST tolerate unknown fields (the schema sets `additionalProperties: true` at every level where extensibility is desirable). Add fields freely in MINOR bumps.

## Provenance

Drafted in the design conversation captured under `IDEA-927`, frozen 2026-05-01 after four rounds of Codex review, reference implementation tracked under `PLAN-930`.
