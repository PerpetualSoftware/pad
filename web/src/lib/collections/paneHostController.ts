// Pane-navigation controller GLUE — the Svelte/SvelteKit-bound execution layer
// that turns the pure decision logic in `./paneController` into real
// `goto(..., {state})` / `history.go` navigations (PLAN-2154 Architecture A/E).
//
// `./paneController` holds the framework-agnostic ARITHMETIC (depth/ownership
// state machine, the three-way close, the drill/lateral/reset plans) and is
// exhaustively unit-tested in isolation. THIS module is the thin glue that
// EXECUTES those plans against the live `page.state` / `history` — it reads the
// current stamp off SvelteKit's `page.state`, asks the pure planner what to do,
// and performs the returned navigation, fencing the async `history.go`
// traversals against duplicate gestures (R14).
//
// It was inline in the collection route (`[collection]/+page.svelte`) through
// Phase 1; PLAN-2154 Phase 2 (Architecture E, TASK-2170) extracts it here so
// BOTH the collection page AND the full-page item host mount ONE controller —
// no duplicated state machine, one place the drill/reset/close arithmetic lives.
//
// It stays framework-BOUND (imports `goto`/`page`/`afterNavigate`), so unlike
// the pure `./paneController` it isn't unit-tested directly — its correctness is
// covered end-to-end by the pane e2e suite (`e2e/pane-controller.spec.ts`,
// `e2e/pane-async-race.spec.ts`, …). Everything COLLECTION-SPECIFIC (the j/k
// pane-follow, the list-row focus-return target, the unsaved-draft leave guard)
// stays in the host and is handed in as a small set of injected callbacks.

import { goto, afterNavigate } from '$app/navigation';
import { page } from '$app/state';
import { browser } from '$app/environment';
import { itemUrlId, type Item, type PaneTarget } from '$lib/types';
import {
	readPaneState,
	planPaneDrill,
	planLateralOpen,
	planPaneClose,
	type ResolvedPaneState,
} from '$lib/collections/paneController';
import { resolvePaneTarget } from '$lib/collections/paneTarget';

/**
 * The host-specific hooks the controller calls back into. Everything the
 * controller needs that ISN'T generic pane navigation — the current pane ref,
 * the collection page's j/k follow, its list-row focus-return capture, the
 * pane-region focus (owned by the shell), and the unsaved-draft leave guard.
 */
export interface PaneControllerDeps {
	/** The current `?item=` ref (the host's `openItemRef` derived). */
	getOpenItemRef: () => string | null;
	/**
	 * Cancel a pending j/k pane-follow debounce (collection page only; a no-op
	 * on the full-page host, which has no list to follow). Called at the start
	 * of every drill/close/reset so a stale follow can't clobber the new state
	 * (R3 / R14).
	 */
	cancelFollow: () => void;
	/**
	 * Move keyboard focus into the stable pane region (owned by the shell,
	 * which binds `paneEl`). Fired synchronously on each in-pane hop before the
	 * `{#key itemSlug}` remount destroys the just-activated control (R1).
	 */
	focusPaneRegion: () => void;
	/**
	 * Capture the element that opened the pane, so an eventual close can return
	 * focus to it (list-row fallback; collection page only). Called ONLY on a
	 * first-open — a re-target keeps the original trigger (TASK-2122).
	 */
	captureReturnFocus: () => void;
	/**
	 * Toggle the host's unsaved-draft navigation guard around a controller-
	 * initiated `goto` (the collection-rename away-nav). No-op on hosts with no
	 * draft guard.
	 */
	setBypassNavGuard: (bypass: boolean) => void;
}

/** The controller surface the host wires into its markup + handlers. */
export interface PaneController {
	/** Current pane depth+ownership, read from SvelteKit `page.state`. */
	currentPaneState: () => ResolvedPaneState;
	/** True while a controller `history.go` traversal is settling (re-entrancy guard). */
	paneNavInFlight: () => boolean;
	/** Drop any in-flight `history.go` continuation + its fallback timer (call from onDestroy). */
	clearPaneGo: () => void;
	/** Lateral / list open: first-open (push), depth-0 re-target (replace), or depth>0 stack RESET. */
	openItemPane: (item: Item) => void;
	/**
	 * The ref-first counterpart to `openItemPane(item)` — first-open / re-target /
	 * stack-reset the pane to an ALREADY-RESOLVED canonical `?item=` ref, applying
	 * the exact same lateral-open semantics (`planLateralOpen`). `openItemPane` is a
	 * thin wrapper over this that first derives the ref from an `Item` via
	 * `itemUrlId`. Hosts whose content-links hand up a resolved ref rather than a
	 * full `Item` — the full-page item host's MASTER content-links (PLAN-2154
	 * Architecture E / TASK-2174), which FIRST-OPEN the pane (a depth-0
	 * `paneOwned:true` push) instead of drilling — wire straight into this.
	 */
	openItemPaneByRef: (ref: string) => void;
	/** In-pane DRILL to an already-resolved canonical `?item=` ref. */
	navigatePaneTo: (target: string) => void;
	/** `ItemDetail`'s `onOpenTarget` seam: resolve a raw `PaneTarget`, then drill. */
	handleOpenTarget: (target: PaneTarget) => void;
	/** The pane chrome's Back chevron / `ItemDetail`'s `onBack`: pop exactly one drill level. */
	handlePaneBack: () => void;
	/** `ItemDetail`'s `onNavigateAway`: collection-rename (keep pane) vs item-move (drop pane). */
	handlePaneNavigateAway: (url: string) => void;
	/** Close the pane via the three-way, ownership-aware, staged unwind. */
	closeItemPane: () => void;
}

export function createPaneController(deps: PaneControllerDeps): PaneController {
	// ── Pane-navigation controller: depth/ownership state machine ──────────
	// (PLAN-2154 Architecture A / TASK-2157). The pane is a navigable
	// mini-browser: `openItemPane` handles lateral/list opens (first-open + at-
	// depth-0 re-target + at-depth>0 stack RESET) and `navigatePaneTo` handles
	// in-pane DRILLS. Depth + ownership live in SvelteKit `page.state` (NOT raw
	// `history.state` — Kit nests app state under `sveltekit:states`), so they
	// follow opaque Back/Forward, survive `history.go`, and reconstruct on
	// cold-load. The pure decision logic is in `./paneController` (unit-tested);
	// this wiring EXECUTES the returned plans.

	/** Current pane depth+ownership, read from SvelteKit `page.state`. */
	function currentPaneState(): ResolvedPaneState {
		return readPaneState(page.state);
	}

	// R14 fence: a monotonically-increasing sequence bumped at the START of every
	// controller action (open/drill/close). A two-phase `history.go` continuation
	// captures it and BAILS if a newer action superseded it (belt-and-suspenders
	// now that `paneNavInFlight()` blocks a fresh gesture mid-traversal).
	let controllerActionSeq = 0;
	// EVERY controller `history.go` (owned close, cold-base close, detached-open
	// reset) marks navigation in-flight from the moment it's issued until its
	// traversal settles (its own popstate) or a bounded fallback. `history.go`
	// has no completion promise, so without this a duplicate gesture — a
	// double-click ✕, an ESC racing a click-away — could stack a SECOND traversal
	// before the first's popstate lands and OVERSHOOT the intended entry. Blocks
	// re-entrancy; bounded so it can never stick (a stuck flag would freeze every
	// pane gesture). Steady-state depth-0 opens/re-targets issue no `history.go`,
	// so this only arms on a close/reset (R14; Codex review).
	const PANE_GO_SETTLE_MS = 500;
	let paneGoInFlight = false;
	let paneGoTimer: ReturnType<typeof setTimeout> | null = null;
	// The "then write" continuation of a STAGED go (cold-base close / reset).
	// `run()` RETURNS whether it fired: false means "not at the destination yet"
	// (a competing traversal landed us elsewhere), so it stays armed for this
	// go's own popstate rather than firing against the wrong entry. Null for a
	// one-phase owned close (nothing to write after the traversal).
	let pendingPaneLatch: { seq: number; run: () => boolean } | null = null;

	function paneNavInFlight(): boolean {
		return paneGoInFlight;
	}

	function clearPaneGo() {
		paneGoInFlight = false;
		pendingPaneLatch = null;
		if (paneGoTimer) {
			clearTimeout(paneGoTimer);
			paneGoTimer = null;
		}
	}

	// Issue a controller traversal, fencing against a duplicate gesture stacking
	// a second one. `latch` is the optional post-traversal write (cold-base /
	// reset), fired from the settling popstate.
	function paneHistoryGo(delta: number, latch?: () => boolean) {
		paneGoInFlight = true;
		pendingPaneLatch = latch ? { seq: controllerActionSeq, run: latch } : null;
		if (paneGoTimer) clearTimeout(paneGoTimer);
		paneGoTimer = setTimeout(() => {
			paneGoTimer = null;
			// Gave up waiting for the settling popstate (the traversal was
			// superseded): best-effort fire the latch (its own preconditions no-op
			// it off-target), then release the guard UNCONDITIONALLY so gestures
			// can't stay blocked.
			const l = pendingPaneLatch;
			paneGoInFlight = false;
			pendingPaneLatch = null;
			l?.run();
		}, PANE_GO_SETTLE_MS);
		history.go(delta);
	}

	// Settle the in-flight traversal on its own POPSTATE. `popstate` also covers
	// an unrelated browser Back/Forward, so a STAGED latch is not consumed
	// eagerly: `run()` verifies it reached its destination (the pane base — depth
	// 0 with `?item=` present) and reports whether it fired; if not, we stay
	// in-flight for this go's own popstate (bounded by the fallback timer). A
	// one-phase owned close has no latch and simply releases the guard.
	afterNavigate((nav) => {
		if (!paneGoInFlight) return;
		if (nav.type !== 'popstate') return; // wait for the traversal's own popstate
		const latch = pendingPaneLatch;
		// Staged latch not yet at its destination → stay in-flight.
		if (latch && latch.seq === controllerActionSeq && !latch.run()) return;
		clearPaneGo();
	});

	// Re-assert pane focus AFTER a popstate pop settles (BUG-2278). The
	// synchronous `focusPaneRegion()` belt in `handlePaneBack` / the depth-aware
	// ESC pop is not self-sufficient on the pop path: `handlePaneBack` pops via
	// `history.go(-1)` (a popstate, which SvelteKit cannot carry `keepFocus`
	// through), and since @sveltejs/kit 2.66 (#15452) the client blurs the active
	// element to `<body>` BEFORE the component update. That early blur fires only
	// `focusout` (no `focusin`), and Kit's end-of-nav `reset_focus()` `body.focus()`
	// is then a no-op that dispatches no `focusin` — so PaneHost's focusin-only
	// backstop (which pre-2.66 piggybacked on that `focusin(body)` to pull focus
	// back into the pane) is starved and focus strands on `<body>`. Re-parking
	// focus on the stable `paneEl` here — after the settle, on the next frame so it
	// runs after Kit's microtask-scheduled `reset_focus` — removes the dependency
	// on that incidental event. No-op when the pane closed (`focusPaneRegion` bails
	// on `!openItemRef`) or focus already landed inside the pane (the drill path,
	// which uses `goto({keepFocus:true})`, never hits this).
	afterNavigate((nav) => {
		if (nav.type !== 'popstate' || !browser) return;
		requestAnimationFrame(() => {
			if (document.activeElement === document.body) deps.focusPaneRegion();
		});
	});

	// Lateral / list open from an ALREADY-RESOLVED canonical `?item=` ref
	// (PLAN-2154 Architecture E / TASK-2174). This is the whole body of the
	// pre-refactor `openItemPane`; `openItemPane(item)` below is now a thin
	// wrapper that derives `targetRef` from an `Item` and calls in, so its
	// behavior — and the collection host's every `openItemPane(item)` call site —
	// is byte-identical. The full-page item host's MASTER content-links (which
	// hand up a resolved ref, not an `Item`) first-open the pane through here.
	function openItemPaneByRef(targetRef: string) {
		if (paneNavInFlight()) return;
		controllerActionSeq++;
		const url = new URL(page.url);
		const alreadyOpen = url.searchParams.has('item');
		// Capture the trigger on the FIRST open only — re-targeting (j/k follow /
		// row re-click on an open pane) keeps the ORIGINAL trigger as the
		// fallback return target (TASK-2122).
		if (!alreadyOpen && browser) {
			deps.captureReturnFocus();
		}
		const plan = planLateralOpen(alreadyOpen, currentPaneState());
		if (plan.kind === 'reset') {
			// Detached (depth>0) direct row click → NEW top-level open: collapse
			// the drill stack back to the base, then re-target the base to this
			// item from a one-shot latch (`history.go` can't be awaited). Base
			// ownership is preserved so the subsequent close still unwinds
			// correctly. A pending j/k follow must not fire mid-reset.
			deps.cancelFollow();
			const resetState = plan.resetState;
			paneHistoryGo(plan.goDelta, () => {
				if (!browser) return false;
				// Only re-target once the stack has actually collapsed to the pane
				// BASE — a depth-0 entry that still carries `?item=`. Requiring
				// `?item=` (not just depth 0) rejects a competing browser Back that
				// landed on the pre-pane entry (which has no `?item=`), so the reset
				// can't be written onto the wrong entry; it stays armed for this go's
				// own popstate (Codex review).
				if (!page.url.searchParams.has('item')) return false;
				if (currentPaneState().paneDepth !== 0) return false;
				const u = new URL(page.url);
				u.searchParams.set('item', targetRef);
				goto(`${u.pathname}${u.search}`, {
					replaceState: true,
					noScroll: true,
					keepFocus: true,
					state: resetState,
				});
				return true;
			});
			return;
		}
		url.searchParams.set('item', targetRef);
		goto(`${url.pathname}${url.search}`, {
			replaceState: plan.kind === 'replace',
			noScroll: true,
			keepFocus: true,
			state: plan.state,
		});
	}

	// Lateral / list open from an `Item` (the collection host's row / j-k
	// selection). A thin wrapper over `openItemPaneByRef` — the ONLY thing it
	// adds is deriving the canonical `?item=` ref from the item — so every
	// existing `openItemPane(item)` call site is byte-identical to the
	// pre-refactor function (PLAN-2154 Architecture E / TASK-2174).
	function openItemPane(item: Item) {
		openItemPaneByRef(itemUrlId(item));
	}

	// In-pane DRILL (Architecture A). `target` is an already-resolved canonical
	// `?item=` ref — `handleOpenTarget` below resolves a raw `PaneTarget`
	// (TASK-2158) into this shape before calling in. Same-ref guard + soft
	// depth cap + ownership INHERITED from the current entry, all decided by
	// `planPaneDrill`.
	function navigatePaneTo(target: string) {
		if (paneNavInFlight()) return;
		controllerActionSeq++;
		// A pending j/k follow scheduled at a shallower depth must not fire after
		// this drill and clobber it (R3 / R14).
		deps.cancelFollow();
		const plan = planPaneDrill(deps.getOpenItemRef(), target, currentPaneState());
		if (plan.kind === 'noop') return;
		const url = new URL(page.url);
		url.searchParams.set('item', target);
		goto(`${url.pathname}${url.search}`, {
			replaceState: plan.kind === 'replace',
			noScroll: true,
			keepFocus: true,
			state: plan.state,
		});
		// Focus per hop (R1): pull focus into the stable pane region NOW, before
		// this drill's `{#key itemSlug}` remount destroys the just-clicked link
		// and dumps focus to `<body>`. Covers the editor-link keyboard path too —
		// EditorLinkPopover.handleHrefClick hides the popover (removing the
		// focused anchor) synchronously before calling in here.
		deps.focusPaneRegion();
	}

	// `ItemDetail`'s `onOpenTarget` seam (PLAN-2154 Architecture B / TASK-2158).
	// `resolvePaneTarget` (`./paneTarget`) turns whatever ref/slug/href shape the
	// link surface hands up into the canonical `?item=` value `navigatePaneTo`
	// expects; a target that resolves to nothing is silently dropped (nothing
	// to open).
	function handleOpenTarget(target: PaneTarget) {
		const resolved = resolvePaneTarget(target);
		if (!resolved) return;
		navigatePaneTo(resolved);
	}

	// The pane chrome's Back chevron (PLAN-2154 Architecture C / TASK-2164) —
	// `ItemDetail`'s `onBack`, rendered only once `page.state.paneDepth > 0`.
	// Pops exactly one drill level through the same FENCED `paneHistoryGo` /
	// `paneNavInFlight()` guard the depth-aware ESC handler uses (R14), not a
	// bare `history.back()` — a rapid double-click, or a click racing ESC/close,
	// can't queue a second traversal and overshoot. The depth check is
	// defense-in-depth: the chevron only renders at depth>0, but a stale click
	// could in principle race a continuation that already unwound the stack back
	// to the base.
	function handlePaneBack() {
		if (currentPaneState().paneDepth > 0 && !paneNavInFlight()) {
			paneHistoryGo(-1);
			// Focus per hop (R1) — same rationale as `navigatePaneTo`: the pop
			// remounts the pane content and unmounts the just-clicked Back button,
			// so move focus onto the stable region before that drop can reach
			// `<body>` (where the next `j`/`k` would steal to list-nav).
			deps.focusPaneRegion();
		}
	}

	// The pane's ItemDetail fires `onNavigateAway` for TWO distinct cases, which
	// we tell apart by whether the target URL still carries `?item=` (i.e. keeps
	// the pane open):
	//
	//  • COLLECTION RENAME (`/user/ws/NEWSLUG?item=X`) — the pathname changed
	//    (old→new slug) but the pane stays open. This needs the rename-specific
	//    handling: replaceState (not push) so it adds no uncounted history entry
	//    that would desync the close-depth arithmetic (R8); a FRESH UNOWNED
	//    depth-0 stamp (every predecessor entry — pre-pane base + any drills —
	//    still points at the now-dead OLD slug, so carrying ownership forward
	//    would make an owned close `history.go` onto a 404; an unowned base
	//    closes by dropping `?item=` in place on the valid new route); and a
	//    bypass of the unsaved-draft guard (the rename already committed
	//    server-side and this route component is REUSED across the same-route
	//    pathname change, so its quick-create drafts survive — a "Stay" prompt
	//    would only strand the user on the dead old slug). This fixes the
	//    IMPERATIVE close; browser Back still traverses to the old-slug
	//    predecessor (an inherent rename-in-history limitation — past entries
	//    can't be rewritten — outside the controller's reach).
	//
	//  • ITEM MOVE to a different collection (`/user/ws/COLL/SLUG`, a full-page
	//    item route with NO `?item=`) — this leaves the collection page entirely
	//    (the pane closes, the component unmounts, its drafts WOULD be lost), so
	//    it must retain the ORIGINAL guarded push: the unsaved-draft prompt has
	//    to fire, and there is no pane to stamp (Codex review).
	function handlePaneNavigateAway(url: string) {
		let keepsPane = false;
		try {
			keepsPane = new URL(url, 'http://pad.invalid').searchParams.has('item');
		} catch {
			keepsPane = false;
		}
		if (!keepsPane) {
			// Item move (or any away-nav that drops the pane) — original behavior:
			// a guarded navigation so the unsaved-draft prompt still fires.
			void goto(url);
			return;
		}
		// Collection rename — rebase ownership + bypass the (now-spurious) draft
		// guard for the committed, component-reusing pathname change.
		deps.setBypassNavGuard(true);
		void goto(url, {
			replaceState: true,
			noScroll: true,
			keepFocus: true,
			state: { paneDepth: 0, paneOwned: false },
		}).finally(() => {
			deps.setBypassNavGuard(false);
		});
	}

	function closeItemPane() {
		if (paneNavInFlight()) return;
		controllerActionSeq++;
		// A pending j/k pane-follow must not re-open the pane after an explicit
		// close (e.g. ESC while a follow debounce is in flight).
		deps.cancelFollow();
		// Focus return to the originating row is handled by the host's close-
		// detection effect (keyed off `openItemRef` going truthy→null), so it
		// covers browser Back / delete alike — not just this imperative close path.
		const plan = planPaneClose(currentPaneState());
		if (plan.kind === 'replace-delete') {
			// Cold-loaded base with no drills: drop `?item=` in place. No pre-pane
			// history entry to unwind to.
			const url = new URL(page.url);
			url.searchParams.delete('item');
			goto(`${url.pathname}${url.search}`, {
				replaceState: true,
				noScroll: true,
				keepFocus: true,
			});
			return;
		}
		if (plan.kind === 'owned-go') {
			// Owned: unwind the pushed base + every drill back to the pre-pane URL
			// (which carries no `?item=`, so the pane closes on arrival). This is
			// PLAN-2154 R8's mandated close — and it makes an explicit ✕/ESC close
			// IDENTICAL to the browser Back that already closed the pane in
			// PLAN-2105 (a single Back pops the first-open push). Consequence: a
			// list filter/view/search change made WHILE the pane was open — which
			// `updateUrlFilters` replaceState'd onto the pane entry — is not
			// carried to the pre-pane URL, exactly as browser Back already
			// behaves. A one-phase traversal (no latch); the in-flight fence stops
			// a duplicate ✕/ESC from stacking a second go(-1) that would overshoot.
			paneHistoryGo(plan.goDelta);
			return;
		}
		// cold-base-go: go back to the cold base (still carries `?item=`), then
		// delete it from a latch fired on the settling popstate.
		paneHistoryGo(plan.goDelta, () => {
			if (!browser) return false;
			// Only delete once we've reached the cold base that still shows the
			// pane (depth 0, `?item=` present); a competing traversal landing
			// elsewhere leaves it armed (R14 fence-on-continuation).
			if (!page.url.searchParams.has('item')) return false;
			if (currentPaneState().paneDepth !== 0) return false;
			const u = new URL(page.url);
			u.searchParams.delete('item');
			goto(`${u.pathname}${u.search}`, {
				replaceState: true,
				noScroll: true,
				keepFocus: true,
			});
			return true;
		});
	}

	return {
		currentPaneState,
		paneNavInFlight,
		clearPaneGo,
		openItemPane,
		openItemPaneByRef,
		navigatePaneTo,
		handleOpenTarget,
		handlePaneBack,
		handlePaneNavigateAway,
		closeItemPane,
	};
}
