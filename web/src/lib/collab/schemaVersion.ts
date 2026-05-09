/**
 * Client-side Tiptap schema version stamp (TASK-1268, PLAN-1248).
 *
 * The CollabProvider sends this on every WebSocket connect via a
 * `?schema_version=...` query parameter. The server (1) rejects the
 * upgrade outright if the value doesn't match its own
 * `DefaultSchemaVersion`, and (2) prunes the op-log on first
 * mismatched-vs-persisted connect so the new client doesn't replay
 * old-schema ops that may be incompatible.
 *
 * **Bump rule.** Any of these changes is a breaking schema bump and
 * REQUIRES incrementing this constant + the matching
 * `internal/collab/manager.go::DefaultSchemaVersion` in the same
 * commit:
 *
 *   - Adding/removing a Tiptap extension whose ProseMirror schema
 *     contributes nodes or marks (e.g. swapping StarterKit for a
 *     custom kit, adding a TaskList that wasn't there before).
 *   - A coordinated `@tiptap/core` + `@tiptap/extension-collaboration`
 *     + `@tiptap/y-tiptap` minor bump that the changelog flags as a
 *     ProseMirror node-spec change.
 *   - Changing the Y.Doc top-level fragment shape (e.g. moving
 *     content from `'default'` to `'body'`).
 *
 * Pure CSS, UI, or behavioural changes that don't alter the
 * persisted document shape DO NOT bump this value. When in doubt,
 * load an item edited under the old version after your change and
 * confirm the rendered tree is identical.
 *
 * **Why a string, not a number.** Future tags may want suffixes
 * (e.g. `'2-rc1'`) without changing the equality semantics. The
 * server treats it as an opaque exact-match string.
 */
export const SCHEMA_VERSION = '1';
