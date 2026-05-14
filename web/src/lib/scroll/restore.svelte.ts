// Page-level scroll-position restoration that survives async content loading.
//
// Background — BUG-1425 / TASK-755
// ────────────────────────────────
// SvelteKit's built-in scroll restoration runs synchronously right after the
// page is mounted. Almost every page in this app fetches its data inside the
// component (in `loadData()` / `$effect`), not in a `+page.ts` `load`
// function, so when the user back-navigates the document is still skeleton-
// height when restoration fires — the saved scrollY gets clamped to ~0.
//
// TASK-755 worked around this on the collection listing page only, using a
// bespoke localStorage entry + double-RAF restore gated on `loading`. The
// item detail page (and everything else) was never covered, hence BUG-1425.
//
// This module replaces TASK-755's bespoke code with a reusable helper built
// on SvelteKit's `snapshot` API. Each page that wants scroll restoration
// calls `createScrollRestoration` and re-exports the resulting `snapshot`
// object. The factory layers:
//
//   1. SvelteKit snapshot (sessionStorage, per history entry). Canonical
//      path. Capture runs on navigate-away; restore runs on back/forward.
//      Per-entry semantics naturally distinguish "back to where I was"
//      from "fresh nav to the same URL."
//   2. A `ready()` gate. SvelteKit may call `restore(y)` synchronously
//      after the page mounts (or after params change, for routes that
//      reuse a component instance), but we don't want to scroll until
//      the page's data has rendered. The caller's `ready()` is the
//      single source of truth for "content matches the URL." For pages
//      that reload on URL change, the caller should verify both
//      `!loading` AND that the content's identity matches the URL —
//      e.g. `item?.slug === itemSlug` on the item detail page. For
//      pages that don't reload on URL change (e.g. roles), a plain
//      `!loading` is fine; the helper fires as soon as ready is true.
//   3. Double `requestAnimationFrame`. Same trick as TASK-755 — gives
//      layout one extra frame to settle after content paints before
//      calling `scrollTo`, so the offset isn't clamped by stale
//      heights.
//   4. Per-key one-shot guard (`restoredKey`). For routes that reuse a
//      component instance across URLs, the lifetime-scoped `restored`
//      boolean from round 1 blocked the second back-nav. Keying the
//      guard by `persistKey()` lets each new URL get a fresh shot.
//   5. Optional `persistKey` localStorage fallback. sessionStorage is
//      per-tab; closing and reopening a tab (or the workspace
//      switcher's TASK-754 cross-tab `goto()` route restoration) loses
//      the snapshot. The fallback fires on mount AND on every
//      persistKey change, and clears `pending`/`restoredKey` on each
//      key transition so a return-to-previously-restored-URL works.

import type { Snapshot } from '@sveltejs/kit';
import { onDestroy } from 'svelte';

// The app's primary scroll container lives in the root layout
// (`.main-content { overflow-y: auto }`), not `window`. Capturing
// `window.scrollY` always yields 0 and `window.scrollTo` is a no-op
// — which silently broke TASK-755's listing restore AND every round
// of BUG-1425 fixes until the diagnostic logging caught it.
//
// `getScrollTarget()` returns the element to operate on. We look up
// `.main-content` lazily so the function works for any caller within
// the workspace layout; for pages outside the layout it falls back
// to `window`. The lookup is cheap and re-runs on every capture /
// restore so DOM swaps between mounts are handled.
function getScrollTarget(): HTMLElement | Window | null {
	if (typeof document === 'undefined') return null;
	const el = document.querySelector<HTMLElement>('.main-content');
	if (el) return el;
	if (typeof window !== 'undefined') return window;
	return null;
}

function readScrollY(target: HTMLElement | Window | null): number {
	if (!target) return 0;
	if (target instanceof Window) return target.scrollY;
	return target.scrollTop;
}

function writeScrollY(target: HTMLElement | Window | null, y: number) {
	if (!target) return;
	target.scrollTo({ top: y, behavior: 'instant' as ScrollBehavior });
}

export interface ScrollRestorationOptions {
	/**
	 * Returns true once the page's primary content matches the current
	 * URL and is rendered enough to scroll to a saved offset. Read
	 * inside a `$effect`, so it must touch reactive state.
	 *
	 * For routes that reuse a component instance across URLs (e.g.
	 * `[coll]/[slug]` for every item), include an identity check so
	 * `ready()` returns false while the previous URL's content is still
	 * rendered. Example for the item page:
	 *
	 * ```ts
	 * ready: () => !loading && item?.slug === itemSlug
	 * ```
	 *
	 * For pages that don't reload on URL change, a plain `!loading` is
	 * sufficient — the restore fires as soon as ready is true.
	 */
	ready: () => boolean;
	/**
	 * Optional cross-session/cross-tab persistence key, AND the per-route
	 * identity used for the one-shot guard. When provided:
	 *
	 *   - Capture mirrors `scrollY` to `localStorage[key()]` so a tab
	 *     close + re-entry via the workspace switcher (TASK-754) still
	 *     restores position.
	 *   - The restore effect gates its one-shot fire by `key()`, so
	 *     routes that reuse a component instance across URLs get a
	 *     fresh restore for each new entry.
	 *   - On every `key()` change the LS fallback re-reads and the
	 *     `pending`/`restoredKey` state is cleared, so returning to a
	 *     previously-restored key re-arms correctly.
	 *
	 * Return `null` to skip persistence and use lifetime-scoped one-shot
	 * semantics (one restore per component instance — appropriate for
	 * routes that always remount on entry).
	 */
	persistKey?: () => string | null;
}

export interface ScrollRestoration {
	snapshot: Snapshot<number>;
}

/**
 * Wire scroll-position restoration into a `+page.svelte`.
 *
 * ```svelte
 * <script lang="ts">
 *   import { createScrollRestoration } from '$lib/scroll/restore.svelte';
 *
 *   let loading = $state(true);
 *   let item = $state<Item | null>(null);
 *
 *   const restoration = createScrollRestoration({
 *     ready: () => !loading && item?.slug === itemSlug,
 *     persistKey: () => `pad-last-scroll-${wsSlug}-${page.url.pathname}`,
 *   });
 *   export const snapshot = restoration.snapshot;
 * </script>
 * ```
 *
 * Must be called at the top level of a component's `<script>` block so
 * the `$effect` calls inside have a component context.
 */
export function createScrollRestoration(
	opts: ScrollRestorationOptions,
): ScrollRestoration {
	// Parked restore value waiting for ready(). Stored together with the
	// `persistKey` captured at restore-time so a value saved for entry A
	// can't apply later to entry B.
	let pending = $state<{ y: number; key: string | null } | null>(null);

	// The persistKey value that snapshot.restore was last called with
	// (or `undefined` if never called). The LS fallback uses this to
	// detect "a SvelteKit snapshot has already been provided for this
	// entry — don't overwrite it with a stale LS value." A bare boolean
	// like the previous `snapshotRestoreCalled` was unsafe: the LS
	// `$effect` reset it on every key change, blowing away a signal set
	// by a snapshot.restore that beat the effect to the microtask queue
	// (Codex BUG-1425 round 5 P1).
	let snapshotKey: string | null | undefined = undefined;

	// Per-key one-shot guard. `undefined` = never restored (or just
	// reset after a fresh snapshot.restore or key change); a string/null
	// value = already restored for that key. Non-reactive on purpose: a
	// previous version made this a $state boolean that the gated effect
	// wrote to, which invalidated itself and cancelled the RAF via
	// cleanup-then-rerun (Codex round 1 of BUG-1425). Writing a plain
	// variable doesn't establish a dependency.
	let restoredKey: string | null | undefined = undefined;

	// Mount status. RAF lifetime is decoupled from any effect cleanup so
	// unrelated re-runs can't cancel an in-flight restore; the inner RAF
	// callback checks `alive` AND re-reads `persistKey()` to bail if the
	// route changed mid-flight.
	let alive = true;
	onDestroy(() => {
		alive = false;
	});

	// localStorage fallback. Fires on mount AND on every `persistKey()`
	// change so workspace-switcher `goto()` navigations that reuse the
	// component instance (no popstate, no snapshot.restore) still pick
	// up a saved scroll position.
	//
	// On each key transition the effect re-arms by clearing
	// `restoredKey`, then defers one microtask so SvelteKit has a
	// chance to call snapshot.restore first. The microtask checks
	// `snapshotKey === key` to skip when snapshot won the race — that's
	// the per-key signal that survives effect re-runs (vs. the bare
	// boolean we used previously, which Codex round 5 P1 caught us
	// blowing away here).
	//
	// `pending` is intentionally NOT cleared on key change: pending
	// values for the previous key are filtered by the gated effect's
	// `pending.key !== key` check, and clearing would clobber a
	// snapshot.restore that beat the effect's microtask. If the new
	// key has its own snapshot OR an LS value, those will overwrite
	// `pending` in due course; if it has neither, the old pending
	// stays parked but inert.
	if (opts.persistKey) {
		let lastSeenKey: string | null | undefined = undefined;
		$effect(() => {
			const key = opts.persistKey!(); // track persistKey result
			if (key === lastSeenKey) return; // dedupe re-runs on unchanged key
			lastSeenKey = key;
			// Re-arm the per-key one-shot so a return-to-previously-
			// restored URL fires again.
			restoredKey = undefined;
			// Invalidate `snapshotKey` when it's not for the new key.
			// This lets goto-return-to-previously-restored (no popstate,
			// no fresh snapshot.restore) re-read LS instead of skipping.
			// If snapshot.restore for the NEW key already fired before
			// this effect (popstate beat the effect to the microtask
			// queue), `snapshotKey === key` and we preserve it.
			// Codex BUG-1425 round 6 P1.
			if (snapshotKey !== key) {
				snapshotKey = undefined;
			}
			// Clear `pending` when it's for an old key. Stale pending
			// would otherwise block the LS read for the new key (the
			// `pending.key === key` skip below) AND, if it happens to
			// match the new key by coincidence, fire stale offset.
			// Pending for the CURRENT key is preserved — a snapshot.
			// restore that beat the effect populated it, and the LS
			// microtask will honor `snapshotKey === key` to no-op.
			if (pending !== null && pending.key !== key) {
				pending = null;
			}
			// Defer one microtask so SvelteKit has a chance to call
			// snapshot.restore for this entry first.
			queueMicrotask(() => {
				// Bail if the user navigated yet again before drain.
				if (key !== lastSeenKey) return;
				// Snapshot already won for this key — don't overwrite.
				if (snapshotKey === key) return;
				// Already have a pending value for this key from a
				// snapshot.restore that arrived between the sync body
				// above and this microtask. (We only skip LS in this
				// case; we don't return early on stale pending because
				// the sync body above already cleared it.)
				if (pending !== null && pending.key === key) return;
				try {
					if (!key) return;
					const raw = localStorage.getItem(key);
					if (!raw) return;
					const y = Number(raw);
					if (!Number.isFinite(y) || y <= 0) return;
					pending = { y, key };
				} catch {
					// localStorage unavailable — silent no-op.
				}
			});
		});
	}

	// Gated restore. Reads `pending`, `ready()`, and the current
	// `persistKey()` — all tracked, so it re-fires when any of them
	// changes.
	//
	// Firing rules:
	//   - `pending` must be set and its captured key matches current
	//     persistKey (otherwise the value was saved for a different
	//     entry).
	//   - `ready()` returns true (caller is responsible for verifying
	//     content matches URL — see ScrollRestorationOptions).
	//   - The current key must not already be in `restoredKey`.
	//
	// Then schedule a double-RAF and call scrollTo, with an inner
	// `alive` + key-drift check so an in-flight RAF can't scroll a
	// page the user has since navigated away from.
	//
	// IMPORTANT — never write `pending` from inside this effect. It's
	// tracked here; the write would invalidate the effect and Svelte's
	// cleanup-then-rerun semantics would cancel the queued RAF before
	// scrollTo fires (BUG-1425 round 1 regression).
	$effect(() => {
		if (pending === null) return;
		const key = opts.persistKey?.() ?? null;
		if (pending.key !== key) return; // pending saved for a different entry
		if (!opts.ready()) return;
		if (restoredKey === key) return; // already restored for this key

		const y = pending.y;
		restoredKey = key;

		// Retry-until-tall-enough-AND-stable scroll loop.
		//
		// `ready()` going true means data has arrived, but Tiptap renders
		// content across many frames AND some property-card fields
		// (relationships, children counts, computed fields) populate
		// asynchronously AFTER the initial body render. At the moment
		// of our first scrollTo, .main-content's scrollHeight is a
		// fraction of its eventual size; scrollTo(y) clamps to the
		// current max. Worse, if we exit the loop as soon as scrollY
		// hits y but content ABOVE us keeps growing (property card
		// taking another 200px once relationships resolve), the layout
		// shifts down and we end up visually too far down the page —
		// scrollY=y now points at a different DOM position than at
		// save time. BUG-1425 round 11.
		//
		// The loop continues until BOTH conditions hold:
		//   (a) actual scroll reached the saved y, AND
		//   (b) scrollHeight has been unchanged for `STABLE_FRAMES`
		//       consecutive frames (~250ms).
		//
		// On every frame we re-issue scrollTo(y) so growth ABOVE the
		// target shifts us back to the correct DOM position. Bail
		// outs:
		//   - alive=false (component unmounted),
		//   - pending was cancelled or the route drifted,
		//   - user scrolled to a position we didn't set (they're
		//     driving — stop fighting them),
		//   - 2s overall budget exhausted.
		const startTime = performance.now();
		const budgetMs = 2000;
		const STABLE_FRAMES = 15; // ~250ms at 60fps
		let lastApplied = -1;
		let lastScrollHeight = -1;
		let stableFrames = 0;
		// User-input detection for the "user is driving" bail. We can't
		// use a scrollY diff for this because the browser's default
		// `overflow-anchor: auto` adjusts scrollTop when content layout
		// shifts (e.g., ChildItems' loading spinner being replaced by
		// the full sections grows content above us and the browser
		// silently bumps scrollY to keep the visible anchor stable).
		// That browser-driven scroll change isn't user input but our
		// previous diff check treated it as one and bailed mid-restore,
		// leaving the user 200-400px past their target. BUG-1425 round 12.
		let userScrolled = false;
		const markUserScroll = () => {
			userScrolled = true;
		};
		const isScrollKey = (e: KeyboardEvent) =>
			e.key === 'ArrowUp' ||
			e.key === 'ArrowDown' ||
			e.key === 'ArrowLeft' ||
			e.key === 'ArrowRight' ||
			e.key === 'PageUp' ||
			e.key === 'PageDown' ||
			e.key === 'Home' ||
			e.key === 'End' ||
			e.key === ' ';
		const handleKey = (e: KeyboardEvent) => {
			if (isScrollKey(e)) userScrolled = true;
		};
		window.addEventListener('wheel', markUserScroll, { passive: true });
		window.addEventListener('touchmove', markUserScroll, { passive: true });
		window.addEventListener('keydown', handleKey, { passive: true });
		const cleanupListeners = () => {
			window.removeEventListener('wheel', markUserScroll);
			window.removeEventListener('touchmove', markUserScroll);
			window.removeEventListener('keydown', handleKey);
		};

		const tryScroll = () => {
			if (!alive) {
				cleanupListeners();
				return;
			}
			// Abort if pending was cancelled or the route drifted.
			if (pending === null || pending.key !== key || pending.y !== y) {
				cleanupListeners();
				return;
			}
			if (userScrolled) {
				cleanupListeners();
				return;
			}

			const target = getScrollTarget();
			if (!target) {
				cleanupListeners();
				return;
			}

			const scrollable: HTMLElement | Element =
				target instanceof Window
					? document.documentElement
					: (target as HTMLElement);
			const currentScrollHeight = scrollable.scrollHeight;
			if (currentScrollHeight !== lastScrollHeight) {
				stableFrames = 0;
				lastScrollHeight = currentScrollHeight;
			} else {
				stableFrames++;
			}

			writeScrollY(target, y);
			const after = readScrollY(target);
			lastApplied = after;

			if (after >= y && stableFrames >= STABLE_FRAMES) {
				cleanupListeners();
				return;
			}
			if (performance.now() - startTime > budgetMs) {
				cleanupListeners();
				return;
			}
			requestAnimationFrame(tryScroll);
		};
		// Two RAFs before the first attempt to let initial paint settle.
		requestAnimationFrame(() => requestAnimationFrame(tryScroll));
	});

	return {
		snapshot: {
			capture: () => {
				const y = readScrollY(getScrollTarget());
				// Mirror to localStorage for cross-tab restoration. Only
				// writes meaningful (>0) positions; clears the entry on
				// y===0 so a stale value can't replay against a user
				// who is now at the top.
				if (opts.persistKey) {
					try {
						const key = opts.persistKey();
						if (key) {
							if (y > 0) localStorage.setItem(key, String(y));
							else localStorage.removeItem(key);
						}
					} catch {
						// localStorage unavailable — silent no-op.
					}
				}
				return y;
			},
			restore: (y) => {
				const key = opts.persistKey?.() ?? null;
				// Mark THIS key as snapshot-claimed so the LS fallback's
				// microtask sees `snapshotKey === key` and skips. This
				// replaces the unsafe `snapshotRestoreCalled` boolean
				// (Codex BUG-1425 round 5 P1).
				snapshotKey = key;
				// Always reset the per-key guard — even when y is 0 or
				// invalid — so a subsequent valid restore can fire.
				restoredKey = undefined;
				if (typeof y !== 'number' || !Number.isFinite(y) || y <= 0) {
					// Snapshot says "user was at top" (or invalid).
					// Clear any pending value (e.g. one populated by the
					// LS fallback racing ahead of this restore call).
					// The freshly-arrived snapshot is more authoritative
					// than the cross-tab LS mirror — if the user
					// genuinely ended at top, we honor that. Codex
					// round 5 P2-B.
					pending = null;
					return;
				}
				pending = { y, key };
			},
		},
	};
}
