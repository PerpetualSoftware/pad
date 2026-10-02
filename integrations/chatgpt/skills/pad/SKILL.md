---
name: pad
description: Work with the user's Pad workspaces through the Pad tools. Find, read, create and update tasks, bugs, ideas, plans and docs; comment on them, link them, and see what to work on next. Use it whenever the user talks about their Pad projects or items.
---

# Pad

Pad is a project workspace. A workspace holds **collections** (such as Tasks, Bugs, Ideas, Plans and Docs). Each collection holds **items**. Every item has a short **ref** like `TASK-12` or `BUG-4`, fields such as status and priority, optional markdown content, comments, and links to other items.

You work with Pad only through the Pad tools listed below. Refer to items by their ref, never by any other identifier.

## Connect to a workspace first

1. Call `list_workspaces`. If it returns none, tell the user to create a workspace at https://app.getpad.dev and come back. Stop there.
2. If there is more than one workspace and the user hasn't named one, ask which one they mean.
3. Call `get_workspace_overview` for that workspace. It returns the workspace's collections and their fields, its always-on conventions, its playbooks, and what is happening now.
4. Pass that workspace's slug as `workspace` on **every** later tool call. Nothing remembers it between calls.

If the user switches workspaces, call `get_workspace_overview` for the new one before continuing.

## Follow the workspace's conventions

The overview's conventions are rules this team has written down for how work is done in this workspace. Follow them when you create or change items. They override your own defaults.

## Find and read

- "What's going on?" or "how are we doing?" → `project_dashboard`.
- "What should I work on?" → `what_next` for one recommendation, `ready_items` for the open, unblocked backlog in priority order.
- "What changed?" → `recent_activity`, optionally `since` a date.
- "Find anything about X" → `search`. To browse one collection or one status → `list_items`. Both return summaries.
- To read one item in full → `get_item`. For its discussion → `list_comments`. For what blocks it and what it blocks → `item_dependencies`. For its past versions → `item_history`.
- To see which collections exist and which values a field accepts → `list_collections`.

Answer with what the user asked for. Quote refs so they can find the items in Pad. Don't paste whole item bodies unless asked.

## Create and change

- Before `create_item`, check the collection and its valid field values (from the overview or `list_collections`). Pick the collection that fits: an idea goes to Ideas, a defect to Bugs, a unit of work to Tasks. If the fit is unclear, ask.
- Show the user what you are about to create or change, and get a yes, before calling `create_item` or `update_item`.
- `update_item` changes only what you pass. Every content change is saved as a version first, so the user can undo it from the item's History in Pad.
- When you change an item's status, also call `add_comment` saying why. The comment is the record of the decision.
- If `update_item` says the item is being edited in Pad right now, tell the user and try again shortly. Don't retry in a loop.
- `link_items` records relationships such as one item blocking another.
- To answer someone in a discussion, use `add_comment` with `reply_to`.

## Archive and restore

`archive_item` hides an item from lists and search. **Always ask the user to confirm before archiving**, naming the item by ref and title. `restore_item` brings an archived item back. Archiving is the only way to remove an item here; there is no permanent delete.

## Playbooks

Playbooks are step-by-step procedures the team has written down, such as how to triage a bug. When the user asks to do something a playbook covers, use `get_playbook` (by its ref or invocation slug) and follow its steps with the tools you have, telling the user as you go. If a step needs something these tools cannot do, say so and leave that step to the user in Pad.

## What these tools don't do

You can't create or delete workspaces, invite people, change collections or their fields, manage roles, or delete items permanently. When the user asks for one of these, say it is done in Pad at https://app.getpad.dev and don't try to work around it.

## Principles

1. Use refs, never internal identifiers.
2. Confirm before creating, changing or archiving anything.
3. Comment on every status change.
4. Pass the workspace slug on every call.
5. Keep answers short and specific to the request.
