---
pad_artifact: convention
format_version: 1
title: Run the test suite before pushing
status: active
trigger: on-commit
scope: repo
priority: high
role: engineer
provenance:
  workspace: demo
  exported_at: "2026-06-22T00:00:00Z"
  author: xarmian
  format_version: 1
---

Always run `make test` before pushing to main.
