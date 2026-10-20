# Building a Pad Connected App

This guide is for a developer who wants to build an app that installs into a Pad workspace. It covers the manifest an app publishes, how a workspace owner installs it, how the app authenticates, the app API it calls, the webhooks Pad sends it, and what the owner can do to it afterwards.

It describes what the server does today. Where a behaviour is a limit rather than a feature, it is called out under [Limitations](#limitations).

## Overview

A Connected App is a web service you run at an `https` origin. It is not a plugin and no code of yours runs inside Pad. You publish a JSON manifest at a well-known URL; a workspace owner points Pad at your origin, reviews what the manifest asks for, and confirms. Pad then provisions:

- **A bot member.** The app gets its own workspace member, named after the manifest `title`. It holds the role `editor` when the manifest's service access is `write`, `viewer` when it is `read`. It is never an owner.
- **Companion collections.** The collections declared in `companion_pack` are created in the workspace (or adopted from an earlier install of the same origin). These are the only collections the app can write to.
- **A read ceiling.** The app can read its own companion collections plus the workspace's system collections (Conventions and Playbooks). It cannot read anything else.
- **An OAuth client.** A confidential client (`client_secret_basic`) used to obtain tokens. The client secret is handed to the app once, through an install code.
- **Optionally a webhook and item actions**, if the manifest declares `events` and `item_actions`.

The app then talks to Pad over `/api/app/v1` with one of two kinds of token:

| Token | Acts as | Obtained with |
|---|---|---|
| Service token | The app's bot | `client_credentials` |
| Delegated token | A workspace member who signed in through the app | `authorization_code` + PKCE (and `refresh_token`) |

Neither token works on the normal `/api/v1` API, and no session or personal access token works on `/api/app/v1`.

## Prerequisites on the Pad server

- **An admin must enable Apps.** On a self-hosted server it is off by default. An instance admin turns it on in **Console → Admin → Settings → Apps** (or `PUT /api/v1/admin/apps {"enabled": true}`). Until then the app API and the workspace's app routes answer `404`, except the workspace's apps list (`GET /api/v1/workspaces/{ws}/apps`), which answers `200` with `available: false`.
- **The server needs an `https` public origin and an OAuth server.** Apps are available on a self-hosted server only when Apps is enabled and the server's OAuth issuer is `https`, because every app credential is an OAuth token. `GET /api/v1/admin/apps` reports `available` and `https_issuer`.
- **Apps do not depend on the MCP setting.** `/oauth/authorize`, its consent step and `/oauth/token` are served for install clients even when MCP is off. Other OAuth endpoints (dynamic registration, revocation, introspection, the discovery document) still follow the MCP setting.
- **Pad Cloud is operator-managed.** Apps are always available on Pad Cloud, and the admin settings cannot be changed there (`403 managed_by_operator`).
- **Private destinations.** Pad fetches your manifest and posts your webhooks from the server, and refuses private addresses. A self-hosted admin can allow specific private origins with `private_origins` (each entry has an `origin`, an `allowed` list of addresses or CIDRs, and `fetch` / `webhook` flags). On Pad Cloud no private destination is reachable. This matters mainly for local development.

## The manifest

Publish the manifest at:

```
{base_url}/.well-known/pad-app.json
```

`base_url` must be a bare `https` origin: no path, query, fragment or credentials, and the default port is dropped. The normalized origin is the app's identity. The manifest you serve must name the same origin it was fetched from.

Validation is strict. An unknown key in the manifest, or inside a collection schema, is refused rather than ignored. (`config_schema` is free-form JSON and is not checked this way.) Pad reports the first failure with the JSON path that caused it (`422 invalid_manifest`, `details.path`).

### Field reference

| Field | Required | Description |
|---|---|---|
| `id` | yes | `publisher/name`. Each part is 1 to 64 characters of `a-z`, `0-9`, `.`, `_` and `-`, starting with a letter or digit. |
| `version` | yes | A semantic version, e.g. `1.0.0`. |
| `min_contract` | yes | `{"apps": 1, "events": 1}`. Each value must be between 1 and the contract this server speaks, which is 1. |
| `title` | yes | The app's name. Up to 2000 bytes. Also the bot's display name. |
| `publisher` | yes | Who publishes the app. Up to 2000 bytes. |
| `description` | no | Up to 2000 bytes. |
| `homepage` | no | An `https` URL on `base_url`'s origin. |
| `base_url` | yes | The app's `https` origin. |
| `redirect_uris` | yes | 1 to 16 `https` URLs on `base_url`'s origin. Used for the delegated sign-in. |
| `scopes` | yes | `{"service": {"access": ...}, "delegated": {"access": ...}}`. Each `access` is `"read"` or `"write"`. |
| `events` | no | Up to 64 webhook subscriptions. See below. |
| `webhook_url` | if `events` is set | An `https` URL on `base_url`'s origin. |
| `companion_pack` | yes | `{"collections": [...], "artifacts": [...]}`. |
| `item_actions` | no | Up to 32 actions. See [Item actions](#item-actions-and-context-codes). |
| `config_schema` | no | A JSON Schema object, up to 64 KiB. Pad checks only that it is an object and shows it to the owner at install; it does not evaluate it. Values are never secrets. |
| `docs` | no | An `https` URL on `base_url`'s origin. |

Every URL field (`homepage`, `redirect_uris`, `webhook_url`, `docs`, artifact `url`) must be on exactly the manifest's origin. The whole manifest is capped at 256 KiB.

**`companion_pack.collections`** (1 to 32 required). Each entry:

| Field | Description |
|---|---|
| `key` | A lower-case key (`^[a-z][a-z0-9_-]{0,63}$`). `events` and `item_actions` refer to a collection by this key. |
| `slug` | The collection's slug in the workspace: 2 to 64 characters of `a-z`, `0-9` and `-`, not starting or ending with `-`. Unique within the pack. Some slugs are reserved by Pad and refused (`422 invalid_manifest`). |
| `name` | Display name, up to 200 bytes. |
| `schema` | A Pad collection schema, `{"fields": [...]}`. Required. |

If the workspace already has a collection with that slug that the app did not create, the install is refused with `409 collection_conflict`. If it was created by an earlier install of the same origin that has since been uninstalled, it is adopted. If that install is still installed or disabled, the install is refused (`409 app_already_installed`).

**`companion_pack.artifacts`** (optional, up to 32). Starter content Pad imports as items when the app is installed (for example a convention or playbook). Each entry has a `key`, an `https` `url` on your origin and the `sha256` (64 lower-case hex characters) of the file at that URL. Pad fetches each URL once, verifies the hash, and runs the file through the same normalization as a Pad import. The owner reviews the normalized result before confirming. Artifact content may not contain `pad-attachment:` references or workspace-qualified links (`[[ws::REF]]`).

**`events`**. Each entry is `{"name": ..., "collections": [...]}`. `collections` lists companion collection keys (at least one). Subscribable names:

```
item.created   item.updated   item.status_changed   item.deleted   item.restored
comment.created   comment.updated   comment.deleted
```

**`item_actions`**. Each entry is `{"key", "label", "collections", "path"}`. `label` is required and at most 80 bytes. `collections` lists companion collection keys. `path` is an absolute path on your origin (starts with a single `/`, no scheme or host, no backslash or control character, up to 1024 bytes).

### Minimal manifest

This is the shape the end-to-end install test uses. It declares one companion collection, a service token that can write, delegated sign-in that is read-only, and a webhook.

```json
{
  "id": "acme/support-portal",
  "version": "1.0.0",
  "min_contract": { "apps": 1, "events": 1 },
  "title": "Support Portal",
  "publisher": "Acme",
  "base_url": "https://portal.example.com",
  "redirect_uris": ["https://portal.example.com/oauth/callback"],
  "scopes": {
    "service": { "access": "write" },
    "delegated": { "access": "read" }
  },
  "events": [
    { "name": "item.created", "collections": ["tickets"] },
    { "name": "comment.created", "collections": ["tickets"] }
  ],
  "webhook_url": "https://portal.example.com/hooks",
  "companion_pack": {
    "collections": [
      {
        "key": "tickets",
        "slug": "support-tickets",
        "name": "Support tickets",
        "schema": {
          "fields": [
            { "key": "status", "label": "Status", "type": "select", "options": ["open", "solved"] }
          ]
        }
      }
    ]
  }
}
```

### Fuller manifest

```json
{
  "id": "acme/support-portal",
  "version": "1.2.0",
  "min_contract": { "apps": 1, "events": 1 },
  "title": "Support Portal",
  "description": "Customer tickets from the Acme portal.",
  "publisher": "Acme",
  "homepage": "https://portal.example.com/about",
  "base_url": "https://portal.example.com",
  "redirect_uris": ["https://portal.example.com/oauth/callback"],
  "scopes": {
    "service": { "access": "write" },
    "delegated": { "access": "write" }
  },
  "events": [
    { "name": "item.created", "collections": ["tickets"] },
    { "name": "item.status_changed", "collections": ["tickets"] },
    { "name": "comment.created", "collections": ["tickets"] }
  ],
  "webhook_url": "https://portal.example.com/hooks",
  "companion_pack": {
    "collections": [
      {
        "key": "tickets",
        "slug": "support-tickets",
        "name": "Support tickets",
        "schema": {
          "fields": [
            { "key": "status", "label": "Status", "type": "select", "options": ["open", "solved"] },
            { "key": "customer", "label": "Customer", "type": "text" }
          ]
        }
      }
    ],
    "artifacts": [
      {
        "key": "triage",
        "url": "https://portal.example.com/pad/triage-playbook.json",
        "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
      }
    ]
  },
  "item_actions": [
    {
      "key": "open-in-portal",
      "label": "Open in Support Portal",
      "collections": ["tickets"],
      "path": "/pad/open"
    }
  ],
  "config_schema": {
    "type": "object",
    "properties": { "region": { "type": "string" } }
  },
  "docs": "https://portal.example.com/docs"
}
```

(The all-zero `sha256` is a placeholder; use the real hash of your artifact file.)

## Install flow

Installing is a handshake between the workspace owner and your app. Only a workspace **owner** can install, and an owner whose access is restricted to specific collections cannot (the install grants the app system collections they may not be able to read).

### Owner side

All owner routes are under `/api/v1/workspaces/{ws}/apps` and are owner-only. The web UI drives them from workspace Settings → Apps; the same calls are available over the REST API.

1. **Preview.** `POST /apps/install/preview` with `{"base_url": "https://portal.example.com"}`. Pad fetches `{base_url}/.well-known/pad-app.json` and every artifact once (30 second deadline overall), validates the manifest, checks collection conflicts, and stages the result. The answer is what the owner reviews:
   - identity: `origin`, `app_id`, `version`, `title`, `description`, `publisher`, `homepage`, `manifest_sha256`;
   - `reviewed_by_pad: false` with a `notice` ("Not reviewed by Pad. You are installing code published at this origin.") shown for every app;
   - `service_access` and `delegated_access`, and `reads_system_collections` ("The app can read this workspace's Conventions and Playbooks.");
   - `collections` (each with `adopt` set if an earlier install's collection will be reused), `events`, `webhook_url`, `item_actions`, `redirect_uris`, `config_schema`, `docs`;
   - `artifacts`, each with the raw file, the normalized item Pad would store, and a `changes` list of anything the importer dropped or altered.

   A staged preview expires (`expires_at`). `GET /apps/install/pending/{pendingID}` re-reads it and `DELETE` discards it. Preview failures include `400 invalid_base_url`, `502 manifest_fetch_failed`, `422 invalid_manifest`, `502 artifact_fetch_failed`, `422 artifact_hash_mismatch`, `422 invalid_artifact`, `422 artifact_destination_missing` (the workspace has no collection that accepts an artifact's kind), `409 collection_conflict` and `429 pending_install_limit`.

2. **Confirm.** `POST /apps/install/pending/{pendingID}/confirm` with `{"manifest_sha256": "<hash from the preview>"}`. A `manifest_sha256` that is not the staged manifest's is refused with `409 install_review_mismatch`. Pad then re-normalizes the staged bytes and compares them with what the owner reviewed; if the workspace changed in a way that alters the result, it refuses with `409 install_review_stale` and writes nothing. On success (`201`) it provisions everything in one transaction and answers:

   ```json
   {
     "install_id": "…",
     "install_code": "padic_…",
     "expires_at": "2026-01-01T12:10:00Z",
     "notice": "Give this install code to the app. It works once, for 10 minutes. …",
     "items": [{ "key": "triage", "ref": "PLAY-3", "status": "draft" }]
   }
   ```

3. **Hand the install code to the app.** How is up to you: show a field in your own UI, or have the owner paste it. The code works once and expires after 10 minutes. If it is lost, the owner issues a new one with `POST /apps/{installID}/install-code` (active installs only).

`GET /apps` lists the workspace's installs and `GET /apps/{installID}` shows one install's state, its webhook status and the items its artifacts created.

### App side: redeem the install code

`POST /api/app/v1/install/redeem` with `{"code": "padic_…"}`. This route needs no authentication; the code is the credential.

```json
{
  "install_id": "…",
  "client_id": "…",
  "client_secret": "padapp_…",
  "workspace_id": "…",
  "workspace_slug": "my-workspace",
  "webhook_secret": "…"
}
```

- This is the **only** time the `client_secret` and the `webhook_secret` are returned. Store them.
- `webhook_secret` is present only when the manifest declares a webhook.
- `workspace_id` is the `{ws}` in every workspace-scoped app route. The app API addresses a workspace by its ID, never its slug.
- Every refusal (unknown, used, expired, install disabled) is the same `400 invalid_install_code`.
- The route is rate limited to 10 a minute per address and 5 a minute per install (`429`). Redeeming again with a new code replaces both secrets, so the old ones stop working.

### App side: get a token

The token endpoint is `{pad-origin}/oauth/token`. Do not discover it from `/.well-known/oauth-authorization-server`: a self-hosted server serves that document only when its OAuth is on for MCP clients, and the document describes MCP clients, not app install clients. Request a service token:

```
POST {token_endpoint}
Authorization: Basic base64(urlencode(client_id) ":" urlencode(client_secret))
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials&resource=https://<pad-origin>/api/app/v1
```

`resource` is required and must be the app API URL on Pad's public origin (`{origin}/api/app/v1`). An `audience` parameter, if you send one, must name the same resource. The token is granted the `app` scope and is valid only on the app API. Call it as `Authorization: Bearer <access_token>`.

Call `GET /api/app/v1/me` first: it returns the token's binding, including the `workspace_id` to use in the routes below.

## Authentication

### Service tokens

A service token acts as the app's bot. Its access is the manifest's `scopes.service.access`. A `read` token cannot call a write route (`403 insufficient_access`). The bot sees only its read ceiling (companions plus system collections) and writes only to companions.

### Delegated tokens (sign in with Pad)

A delegated token lets a Pad workspace member use your app and act as themselves. It is available only when the manifest's `scopes.delegated.access` is `read` or `write`.

1. Send the person to Pad's `/oauth/authorize` with your `client_id`, one of your registered `redirect_uris`, `response_type=code`, `resource` set to the app API URL, and a PKCE `code_challenge` with `code_challenge_method=S256`. PKCE with S256 is required.
2. Pad shows a consent page fixed to the install's workspace. The person chooses **Read only** (the default) or, if the manifest offers `write`, **Read and write**, and approves.
3. Exchange the code at the token endpoint with your client credentials. The `refresh_token` grant is also supported.

The person must be a human member of the workspace. A delegated request is re-checked on every call against the person's current state: the token stops working if they are removed from the workspace, their account is disabled, or their credentials change. The effective access is the lesser of what the person consented to and what the manifest currently offers. Their role is their membership role (an owner is treated as an editor; a read-only token acts as a viewer), and what they can see is further limited to the install's collections.

### Revocation

Tokens are checked against live state on every request. A read's response is also re-validated before it is sent, a write re-checks its authority before it commits, and a download re-checks before its first byte. Disabling, rotating or uninstalling the app invalidates its tokens immediately; see [Lifecycle](#lifecycle). An invalid token gets `401` with `WWW-Authenticate: Bearer error="invalid_token"`.

## App API reference

Base: `https://<pad-origin>/api/app/v1`. Bodies and responses are JSON. Workspace-scoped routes live under `/workspaces/{ws}` where `{ws}` is the workspace **ID**; any other value answers `404`. Items are addressed by ID, never by slug or ref.

| Method | Path | Token | Access | Purpose |
|---|---|---|---|---|
| GET | `/me` | either | read | The token's binding (install, workspace, auth kind, access). |
| POST | `/install/redeem` | none | n/a | Redeem an install code (above). |
| POST | `/context/redeem` | service | read | Redeem an item action's context code. |
| GET | `/workspaces/{ws}/me` | either | read | The acting identity and its role. |
| GET | `/workspaces/{ws}/collections` | either | read | Collections in the read ceiling. |
| GET | `/workspaces/{ws}/collections/{collSlug}` | either | read | One collection and its schema. |
| GET | `/workspaces/{ws}/collections/{collSlug}/items` | either | read | List items (paged). |
| GET | `/workspaces/{ws}/items/{itemID}` | either | read | One item. |
| GET | `/workspaces/{ws}/items/{itemID}/comments` | either | read | Comments on an item. |
| POST | `/workspaces/{ws}/collections/{collSlug}/items` | either | write | Create an item in a companion collection. |
| PATCH | `/workspaces/{ws}/items/{itemID}` | either | write | Patch fields on an item the install created. |
| POST | `/workspaces/{ws}/items/{itemID}/comments` | either | write | Add a comment. |
| PATCH | `/workspaces/{ws}/items/{itemID}/comments/{commentID}` | either | write | Edit your own comment. |
| DELETE | `/workspaces/{ws}/items/{itemID}/comments/{commentID}` | either | write | Delete your own comment (`204`). |
| POST | `/workspaces/{ws}/items/{itemID}/attachments` | either | write | Upload a file to an item the install created. |
| GET | `/workspaces/{ws}/attachments/{attachmentID}` | either | read | Attachment metadata. |
| GET, HEAD | `/workspaces/{ws}/attachments/{attachmentID}/content` | either | read | Download (`?variant=` for a variant). |

"Either" means a service or a delegated token. Every response sets `Cache-Control: no-store`.

### Reads

- **Install binding**, `GET /me`: `{install_id, workspace_id, workspace_slug, auth_kind, access, user_id, display_name, is_app}`. `auth_kind` is `service` or `delegated`.
- **Workspace identity**, `GET /workspaces/{ws}/me`: `{user_id, display_name, role, is_app}`.
- **Collections**: `{"collections": [...]}`. Each collection is `{slug, name, icon, is_system, schema: {fields: [{key, type, label, options?, required, default?}]}}`. Relation fields are left out of the schema.
- **Items**: `{id, collection, title, content, fields, etag, created_at, updated_at, created_by_display, via_app}`. `fields` contains only the collection's declared non-relation keys. Pad does not expose slugs, refs, parents, assignees or other internals.
- **Listing items**: `?limit=` (1 to 200, default 50) and `?offset=`; the answer is `{"items": [...], "has_more": bool, "next_offset": n}`.
- **Comments**: `{"comments": [{id, item_id, body, author_display, author_kind, parent_comment_id, created_at, updated_at, edited, deleted}]}`. `author_kind` is `app` when an app's bot wrote the comment, `agent` when an agent did, and `user` otherwise, including a person acting through a delegated token.

### Writes

Writes go only to companion collections, and a person's delegated token must also pass the normal edit check for that person.

- **Create an item**: `POST .../collections/{collSlug}/items` with `{"title", "content", "fields"}`. Answers `201` with the item. Field values are validated against the collection's schema; relation fields cannot be set.
- **Update an item**: `PATCH .../items/{itemID}` with `{"fields_patch": {...}, "expected_etag": "<etag from a read>"}`. Both are required. Only fields can change (no title or content), and only on items this install created. A stale etag answers `412 etag_mismatch`; re-read and retry.
- **Create a comment**: `{"body", "parent_comment_id"?}` answers `201`. Only the comment's author can edit (`{"body"}`) or delete it.
- **Upload an attachment**: `POST .../items/{itemID}/attachments?filename=name.png` with the raw file as the request body. `Content-Length` is required (chunked bodies are refused with `411`), the body must be non-empty and at most 25 MiB, and the item must have been created by this install. Answers `201` with `{id, item_id, filename, mime_type, size, variant, created_at}`.
- **Request bodies reject unknown keys** (`400 bad_request`). The workspace's item limit applies (`403 item_limit_reached`).
- Content and field values may not carry `pad-attachment:` references or workspace-qualified links.

Common errors: `401` for any token problem, `403 insufficient_access` for a read token on a write route, `403 forbidden` for a collection or item the app may not write, `404 not_found` for anything outside the ceiling or workspace, `400 validation_error` for schema violations.

## Webhooks

If the manifest declares `events` and a `webhook_url`, Pad posts matching events to that URL. Only events about the app's companion collections, and only for the collections listed in each subscription, are sent.

### Subscription status

The owner sees the webhook's status, and so can the app's operator:

| Status | Meaning |
|---|---|
| `awaiting_secret` | The webhook exists but the app has not yet redeemed an install code that hands it the signing secret. Nothing is delivered. The owner is told to issue a new install code. |
| `active` | The app holds the secret and the install is active. Events are delivered. |
| `paused` | The install is not active (for example disabled). Nothing is delivered. |

`paused` takes precedence: a disabled install's hook reads `paused` even if it is also waiting for a secret, and no held notice is shown. Enabling the install restores the status the hook had: `active` if the app had already redeemed its secret, or `awaiting_secret` with its notice if it had not. Disabling does not take the secret away; only a rotate does.

Status is reported with `undelivered_dropped`, a count of deliveries dropped after 24 hours (below).

### Request

Pad sends `POST` to `webhook_url` with `Content-Type: application/json`, `User-Agent: Pad-Webhook/2.0` and:

| Header | Value |
|---|---|
| `X-Pad-Signature-256` | `t=<unix seconds>,v1=<hex HMAC-SHA256>` |
| `X-Pad-Install` | The install ID the delivery is for. |

The MAC is HMAC-SHA256, keyed with the `webhook_secret` from redeem, over the string `<t>` + `.` + the raw request body. Verify against the raw bytes you received, before parsing. Refuse a `t` more than 5 minutes from your clock.

```js
import { createHmac, timingSafeEqual } from 'node:crypto';

function verify(secret, signatureHeader, rawBody) {
  const m = /^t=(\d+),v1=([0-9a-f]+)$/.exec(signatureHeader ?? '');
  if (!m) return false;
  if (Math.abs(Date.now() / 1000 - Number(m[1])) > 300) return false;
  const mac = createHmac('sha256', secret);
  mac.update(m[1] + '.');
  mac.update(rawBody); // the Buffer, exactly as received
  const want = Buffer.from(mac.digest('hex'));
  const got = Buffer.from(m[2]);
  return want.length === got.length && timingSafeEqual(want, got);
}
```

### Payloads

Every payload starts with an envelope: `event`, `id` (unique per event), `occurred_at`, `workspace_id` and, when the change was made through an app, `via_app` (the install ID). The rest depends on the event:

- `item.created`, `item.updated`, `item.status_changed`, `item.restored`: `item_id`, `collection_id`, `title`, `content`, `fields`, `creator` (`user_id`, `display`, `kind`, `via_app`), `created_at`, `updated_at`. A payload that Pad could only partly reconstruct carries `"partial": true` and omits `title`, `content` and `fields`.
- `item.deleted`: `item_id`, `collection_id` only.
- `comment.created`, `comment.updated`: `comment_id`, `item_id`, `collection_id`, `parent_comment_id`, `body`, `creator`, `created_at`, `updated_at`.
- `comment.deleted`: `comment_id`, `item_id`, `collection_id`, `parent_comment_id` only.

The payload is built from a snapshot taken when the event happened, not from live state. Members of a bulk operation (a bulk edit or reorder made by a person) are not delivered to apps; resync through the API if you need them.

### Delivery, retries and drops

- A `2xx` response is a delivery.
- A `5xx`, `408`, `425` or `429` response, a timeout (10 seconds per attempt) or a network error is retried: up to 3 attempts with a growing delay, and if those fail the event stays owed and is tried again on later passes.
- Any other `4xx`, or a `3xx` (redirects are never followed), is permanent: the event is not retried.
- An event still undelivered **24 hours** after it occurred is dropped and counted in `undelivered_dropped`. A receiver that sees a day-old change should resync through the API.
- Deliveries are capped at 600 a minute per install (burst 60). Excess is deferred, not dropped.
- Delivery happens only while the install is active. An event processed while the install is disabled is finished without delivery and is not retried after re-enabling, so do not rely on receiving events from a disabled period; resync through the API after re-enabling.
- Delivery is at least once. Use the payload `id` to de-duplicate.

## Item actions and context codes

An item action adds a button to items in the listed companion collections. Clicking it sends the person to your app along with a short-lived, single-use code that identifies the item, so your app can show the right page without a separate sign-in.

1. **Pad offers the action.** `GET /api/v1/workspaces/{ws}/items/{ref}/app-actions` lists the actions available on an item, as `{install_id, app_title, action_key, label}`.
2. **A person mints a code.** `POST /api/v1/workspaces/{ws}/items/{ref}/app-actions/{installID}/{actionKey}` answers `{"url": "<base_url><path>?pad_context=<code>"}`. The browser is sent to that URL. The code is bound to the install, the item, the action and the person who clicked, expires in 120 seconds, and works once.
3. **Your app redeems it.** `POST /api/app/v1/context/redeem` with `{"code": "<pad_context>"}`, authenticated with the app's **service** token (not a delegated one). The answer is `{"action_key": "…", "item": { …app item… }}`. It does not say who clicked.

Treat the code as untrusted until redeemed. Your page should redeem it server-side and create its own session from the result.

Mint and redeem both refuse with a single `404 not_found` body for every reason: an unknown, expired or already used code; another install's code; a moved or deleted item; a disabled or upgraded install; a person who can no longer see the item. A redeem consumes the code before checking anything else, so a mismatch burns it. Do not try to tell the reasons apart; ask the person to click again.

## Lifecycle

The owner manages an install from workspace Settings → Apps, or over the owner routes under `/api/v1/workspaces/{ws}/apps/{installID}`.

| Action | Route | Effect |
|---|---|---|
| Disable | `POST /disable` | Moves the install to `inactive`. All tokens and unredeemed install codes are revoked; the app's API calls get `401`, no new tokens are issued, and webhooks stop (status `paused`). Pad waits for in-flight webhook deliveries to finish first (`503 deliveries_in_flight` with `Retry-After` if they do not; repeat the call). |
| Enable | `POST /enable` | Back to `active`. Tokens issued before the disable stay revoked; the app obtains new ones with its existing client credentials. Do not rely on receiving events from the disabled period; resync through the API. |
| Rotate | `POST /rotate` | Revokes every token, replaces the client secret and the webhook secret, holds the webhook (`awaiting_secret`) and answers a new `install_code`. The app redeems it to receive the new secrets. |
| New install code | `POST /install-code` | Issues a code for an active install without revoking anything. Redeeming it still replaces the client secret and webhook secret. |
| Uninstall | `POST /uninstall` | Deletes the OAuth client, the webhook, item actions and their codes, removes the bot's membership and disables the bot. The install remains as a tombstone (`uninstalled`) and the bot's user record is kept so history stays attributed. Companion collections and the items in them are left in the workspace; a later install of the same origin adopts the collections. |

Disabling or uninstalling does not delete content the app created.

### Upgrades

The owner upgrades an install with `POST /apps/{installID}/upgrade/preview` and `/upgrade/confirm`, which fetch the app's current manifest and show a diff of what changed (scopes, events, collections, config). Narrowing delegated access narrows every live delegated token at once.

## Attribution

Pad records which app caused a change.

- Items and comments created by an app's bot show the app's name: the human-facing JSON for items, comments and version history carries `via_app` (the install ID) and `via_app_name`. The bot's display name is the manifest `title`.
- Comments written by the bot have `author_kind: "app"` in the app API.
- A person who acts through a delegated token keeps their own identity. The write is theirs, recorded as a user action, with `via_app` set to the install so the UI can show "via <app>".
- In webhook payloads, `via_app` marks an event caused by an app write; it is absent for a person's or agent's own write. Use it to ignore your own writes.
- After an uninstall the bot's record is kept, and the app's name continues to resolve from the install tombstone.

## Limitations

- **Write scope.** An app can create items only in its own companion collections, and update fields only on items it created. It cannot delete items, change titles or content after creation, set relation fields, or create collections.
- **Read scope.** An app reads only its companion collections and the workspace's system collections. Items are DTOs with a fixed set of fields.
- **One workspace per install.** A token is bound to a single workspace.
- **Owner-only install.** Members cannot install apps, and Pad does not review the apps its owners install.
- **No `http`.** Every URL in the manifest, and Pad's own issuer, must be `https`.
- **Event set.** Only the eight events listed above can be subscribed to, and only for companion collections.
- **Config.** `config_schema` is shown to the owner in the install preview and compared on upgrade; Pad does not evaluate it.
- **Manifest size.** 256 KiB for the manifest, 32 collections, 32 artifacts, 64 events, 32 item actions, 16 redirect URIs.
