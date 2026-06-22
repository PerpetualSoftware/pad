---
pad_artifact: playbook
format_version: 1
title: Ship a change
status: active
trigger: on-intent
scope: workspace
invocation_slug: ship
arguments:
  - description: the item to ship
    name: ref
    required: true
    type: ref
  - default: squash
    name: merge-strategy
    required: false
    type: enum
provenance:
  workspace: demo
  exported_at: "2026-06-22T00:00:00Z"
  author: xarmian
  format_version: 1
---

## Steps

1. Run the tests
2. Open a PR
