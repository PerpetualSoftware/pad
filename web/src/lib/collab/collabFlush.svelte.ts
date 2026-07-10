// collabFlusher — the collaborative (Yjs) snapshot-flush path extracted from
// the item detail page ([collection]/[slug]/+page.svelte) as part of TASK-2082,
// the follow-on to TASK-2029's raw-markdown `contentSaver` extraction.
//
// Background: under live collab the Y.Doc + op-log are canonical for editor
// state, but downstream consumers (search, share-page, exports, API readers)
// read `items.content`. So the page flushes a markdown snapshot of the live
// Y.Doc into `items.content` via the `?source=collab-snapshot` bypass on a 5s
// idle timer, on editor teardown, and on `beforeunload` (TASK-1260 / TASK-1319
// / BUG-1899 / BUG-1941). That machinery — the 5s debounce timer, the per-item
// `lastFlushedContent` dedupe state, and the two-stage dedupe decision — is
// what this module owns.
//
// It mirrors `contentSaver.svelte.ts`'s pattern: the module owns its own timer
// and pending/dedupe state, and every piece of Svelte-reactive, API, or
// editor/provider access the page can't hand off is INJECTED as a callback.
// The module makes no Svelte / API / DOM calls itself, so it's unit-testable in
// plain node.
//
// What stays in the page (injected via `CollabFlusherConfig`):
//   - `isRecovering()`   — reads the page's plain `forceRefreshInFlight` guard.
//   - `normalize()`      — `unescapeDocLinks` (markdown util).
//   - `serialize()`      — `markdownToWikiLinks` + `cleanBrokenLinks` against a
//                          workspace's live link index (page-owned `localIndex`).
//   - `readEditorMarkdown()` — reads the live Tiptap editor's markdown storage.
//   - `isActiveItem()`   — reads the page's reactive `item` to gate per-item
//                          dedupe seeding.
//   - `save()`           — the actual `api.items.flushCollabContent` PATCH plus
//                          ALL reactive bookkeeping (saveStatus, editorStore,
//                          toast, showSaved) and the post-await force-refresh
//                          check + op-log-cursor read. Returns the outcome so
//                          the module can record `lastFlushedContent`.
//
// NOTE (CONVE-1688): `lastFlushedContent` and `timer` are plain closure
// variables, NOT `$state`. They're handler-only trackers; a `$state` written
// inside the page's flush handlers that an $effect also read would silently
// wedge the effect scheduler in PROD. This file is `.svelte.ts` for colocation
// with the item-page module conventions but uses no runes.

import { shouldDedupeEditorSpace } from './flushDedupe';

/** The (workspace, item) a flush targets, captured at provider-mint time (NOT
 *  read from live reactive state at flush time) so a navigation in flight can't
 *  mis-route a PATCH to a different item. Mirrors the page's
 *  `activeCollabContext` shape exactly. */
export interface CollabFlushContext {
	wsSlug: string;
	itemId: string;
	/** `item.content` at load — what the server already has. The baseline arm
	 *  of the dedupe that makes a no-edit VIEW a no-op (BUG-1899). */
	baseline: string;
	/** Editor-markdown-space projection of the Y.Doc seed for the editor-space
	 *  short-circuit (BUG-1941). Null when no seed was captured this session. */
	seedMd: string | null;
}

// CollabFlushResult discriminates the four outcomes a flush can produce:
//   - 'flushed' — PATCH succeeded; items.content now matches.
//   - 'deduped' — skipped because the server already has this markdown (either
//     lastFlushedContent already matched, the per-item baseline matched, or the
//     editor-space short-circuit fired). Callers like the rich→raw toggle treat
//     it as equivalent to 'flushed' for seeding purposes.
//   - 'failed'  — PATCH errored. The toggle path bails so we don't enter raw
//     mode with stale state.
//   - 'skipped' — TASK-1319 force_refresh-recovery path: the local Y.Doc state
//     is known stale relative to the canonical server content, so the flush was
//     refused. Distinct from 'deduped' so callers can refuse to seed from it.
export type CollabFlushResult = 'flushed' | 'deduped' | 'failed' | 'skipped';

export interface CollabSaveInput {
	ws: string;
	itemId: string;
	/** The fully-serialized storage-form markdown to PATCH. */
	toSave: string;
	keepalive: boolean;
}

export interface CollabFlusherConfig {
	/** Idle debounce window in ms for the 5s snapshot flush (default 5000). */
	idleMs?: number;
	/** True while force-refresh recovery is in flight — any Y.Doc-derived
	 *  markdown is known stale, so scheduling and running both bail. Reads the
	 *  page's plain `forceRefreshInFlight`. */
	isRecovering: () => boolean;
	/** Normalize editor markdown into the space the dedupe compares in
	 *  (`unescapeDocLinks`). */
	normalize: (markdown: string) => string;
	/** Serialize normalized markdown into storage form for the given workspace:
	 *  `markdownToWikiLinks` (against `ws`'s live link index) + `cleanBrokenLinks`. */
	serialize: (normalizedMarkdown: string, ws: string) => string;
	/** Read the live editor's markdown for a flush-now. Returns null when the
	 *  editor is unavailable or reading its storage throws (so the caller no-ops). */
	readEditorMarkdown: () => string | null;
	/** True when `itemId` is still the page's active item — gates per-item
	 *  `lastFlushedContent` seeding so a stale flush can't pollute the new
	 *  page's dedupe state. Reads the page's reactive `item`. */
	isActiveItem: (itemId: string) => boolean;
	/** Perform the actual PATCH + reactive bookkeeping. Owns saveStatus /
	 *  editorStore / toast / showSaved, the op-log-cursor read, and the
	 *  post-await force-refresh check (returning 'skipped' when it fires).
	 *  Resolves 'flushed' on success, 'failed' on PATCH error. */
	save: (input: CollabSaveInput) => Promise<'flushed' | 'failed' | 'skipped'>;
}

export interface CollabFlusher {
	/** Arm (or re-arm) the idle debounce to flush `markdown` for `ctx`. No-op
	 *  when recovering or `ctx` is null. Cancels any prior pending flush first,
	 *  so rapid edits coalesce into one flush of the last markdown. */
	schedule(ctx: CollabFlushContext | null, markdown: string): void;
	/** Run a flush immediately for the given `markdown` + `ctx`, applying the
	 *  full recovery/dedupe gauntlet before the injected PATCH. Used by the
	 *  rich→raw toggle, which acts on the returned outcome. Does NOT touch the
	 *  debounce timer (the caller cancels it explicitly). */
	flush(ctx: CollabFlushContext, markdown: string, keepalive: boolean): Promise<CollabFlushResult>;
	/** Cancel the debounce, read the live editor markdown, and flush it now.
	 *  Returns false (no-op) when the editor is unavailable. Used by editor
	 *  teardown + beforeunload with keepalive=true to land the snapshot before
	 *  the provider tears down. */
	flushNow(ctx: CollabFlushContext, keepalive: boolean): boolean;
	/** Cancel the pending debounce without flushing. Leaves dedupe state intact. */
	cancel(): void;
	/** Reset the per-item dedupe baseline (`lastFlushedContent`). Call on item
	 *  swap and after any out-of-band raw save that changed items.content via a
	 *  path this dedupe doesn't see. */
	resetDedup(): void;
	/** The last content this session successfully flushed, or null before the
	 *  first flush / after a reset. Exposed for assertions/diagnostics. */
	readonly lastFlushed: string | null;
}

const DEFAULT_IDLE_MS = 5_000;

export function createCollabFlusher(config: CollabFlusherConfig): CollabFlusher {
	const idleMs = config.idleMs ?? DEFAULT_IDLE_MS;
	let timer: ReturnType<typeof setTimeout> | undefined;
	// Per-item dedupe state: the last content we successfully flushed this
	// session for the active item. Coalesces redundant multi-tab PATCHes and,
	// combined with `ctx.baseline`, suppresses the no-edit VIEW flush (BUG-1899).
	let lastFlushedContent: string | null = null;
	// Monotonic reset counter. Every resetDedup() (item swap, out-of-band raw
	// save) bumps it. A flush captures it before awaiting the PATCH and refuses
	// to record `lastFlushedContent` if it changed while the PATCH was in flight
	// — a reset during the flush means the snapshot we were about to commit is
	// now stale relative to whatever triggered the reset, so committing it would
	// re-pollute the dedupe baseline the reset just cleared. Guards the extra
	// promise turn `await config.save()` introduces over the page's original
	// inline await. Per Codex review (TASK-2082).
	let resetGeneration = 0;

	function cancel(): void {
		if (timer !== undefined) {
			clearTimeout(timer);
			timer = undefined;
		}
	}

	function schedule(ctx: CollabFlushContext | null, markdown: string): void {
		cancel();
		// Force-refresh recovery in flight: block any new flush scheduling. The
		// provider is destroyed but the editor component is still mounted; a
		// local edit during this window must NOT arm a flush against the stale
		// Y.Doc state. Per Codex round 7 [P1] of TASK-1319.
		if (config.isRecovering()) return;
		if (!ctx) return;
		timer = setTimeout(() => {
			timer = undefined;
			void flush(ctx, markdown, false);
		}, idleMs);
	}

	async function flush(
		ctx: CollabFlushContext,
		markdown: string,
		keepalive: boolean,
	): Promise<CollabFlushResult> {
		// Force-refresh recovery is in flight: any markdown derived from the
		// soon-to-be-discarded Y.Doc is stale relative to canonical server
		// content. Skipping here covers every direct caller (beforeunload,
		// raw-toggle, flushNow) without per-call-site guards. Per Codex round 8
		// [P1] of TASK-1319.
		if (config.isRecovering()) return 'skipped';
		const normalizedMarkdown = config.normalize(markdown);
		// Editor-markdown-space short-circuit (BUG-1941, regression of BUG-1899).
		// BEFORE re-serializing through markdownToWikiLinks — which depends on
		// the FLUSH-TIME wiki-link index and can diverge from a stable seed on
		// index drift — check whether this is a pure no-edit view of the exact
		// markdown seeded into the Y.Doc this session. shouldDedupeEditorSpace
		// scopes this to the baseline arm only (lastFlushedContent === null); see
		// its own doc comment for the revert-safety rationale the storage-space
		// compare below still owns.
		if (shouldDedupeEditorSpace(lastFlushedContent, ctx.seedMd, normalizedMarkdown)) {
			return 'deduped';
		}
		// Serialize against the CAPTURED workspace (the item being flushed), not
		// live route state — a background/unmount flush can fire after navigating
		// to another workspace; the PATCH already targets `ctx.wsSlug`, so the
		// link index must match it. Per Codex review (round 1).
		const toSave = config.serialize(normalizedMarkdown, ctx.wsSlug);
		// Dedupe against what the server already has for this item. Once we've
		// flushed this session that's `lastFlushedContent`; before any flush it's
		// the per-item `baseline` captured at load. The baseline arm makes merely
		// VIEWING an item a no-op — a real edit changes `toSave`, so only genuine
		// no-ops are suppressed. (Revert-safe: after a real flush
		// `lastFlushedContent` is non-null and takes precedence.)
		const serverContent = lastFlushedContent ?? ctx.baseline;
		if (serverContent === toSave) return 'deduped';

		// Snapshot the reset generation across the PATCH so a resetDedup() that
		// lands while it's in flight invalidates the record below.
		const gen = resetGeneration;
		const result = await config.save({
			ws: ctx.wsSlug,
			itemId: ctx.itemId,
			toSave,
			keepalive,
		});
		// lastFlushedContent is per-item; only seed it if:
		//   - the PATCH actually flushed (save() returns 'skipped' when a
		//     force_refresh landed mid-flight, so we never record a stale base),
		//   - no resetDedup() ran during the PATCH (item swap / raw save would
		//     otherwise be re-polluted by this now-stale snapshot), AND
		//   - the item we just flushed is still the active one.
		if (
			result === 'flushed' &&
			resetGeneration === gen &&
			config.isActiveItem(ctx.itemId)
		) {
			lastFlushedContent = toSave;
		}
		return result;
	}

	function flushNow(ctx: CollabFlushContext, keepalive: boolean): boolean {
		cancel();
		const md = config.readEditorMarkdown();
		if (md === null) return false;
		// flush is async but its return value is irrelevant for synchronous
		// callers — fire-and-forget under keepalive=true is the contract on the
		// unmount / beforeunload path.
		void flush(ctx, md, keepalive);
		return true;
	}

	function resetDedup(): void {
		lastFlushedContent = null;
		// Invalidate any in-flight flush's pending record (see `gen` in flush()).
		resetGeneration++;
	}

	return {
		schedule,
		flush,
		flushNow,
		cancel,
		resetDedup,
		get lastFlushed() {
			return lastFlushedContent;
		},
	};
}
