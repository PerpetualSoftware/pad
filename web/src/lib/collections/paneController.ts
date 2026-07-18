// Pane-navigation controller — the depth/ownership state machine that turns
// the collection page's split-pane detail view (PLAN-2105) into a navigable
// mini-browser with a back stack (PLAN-2154 / IDEA-2153, Architecture A).
//
// This module holds the PURE decision logic, framework-agnostic and free of
// `$state`/`page`/`goto` so it's exhaustively unit-testable without mounting
// the 4200-line route. `+page.svelte` reads the current `{paneDepth,
// paneOwned}` off SvelteKit's `page.state` (NEVER raw `history.state` — Kit
// nests app state under `sveltekit:states`), passes it here, and EXECUTES the
// returned plan via `goto(..., { state })` / `history.go`. Keeping the
// arithmetic pure is what makes the three-way close and the ownership model
// reviewable in isolation — the exact class of bug (opaque Back/Forward,
// cold-load close off the base, late-async clobbers) this controller lives in.
//
// The vocabulary:
//   • depth — how many in-pane drill hops deep we are. 0 = first-open or a
//     cold-loaded shared `?item=` URL (the "base").
//   • ownership — whether THIS session pushed the pane's base entry, so there
//     is a pre-pane history entry to unwind to on close. Created ONLY by a
//     first-open (`openItemPane` on a closed pane); INHERITED by every drill
//     (`navigatePaneTo`). A cold-loaded `?item=` has no stamp → unowned, which
//     is what keeps the UNOWNED-close branches reachable (a cold A→B drill
//     must NOT `go(-2)` off the base).

/**
 * Depth + ownership stamp carried in SvelteKit `page.state` for the split-pane
 * mini-browser. Both fields are optional at the type level because a
 * cold-loaded / never-stamped history entry has no `page.state` of its own;
 * {@link readPaneState} normalizes those absences to the base defaults.
 */
export interface PaneHistoryState {
	/** In-pane drill depth. 0 = first-open / cold-load base. */
	paneDepth?: number;
	/**
	 * True when this session minted the pane's base entry (first-open), so a
	 * pre-pane history entry exists to unwind to. False for a cold-loaded
	 * shared `?item=` URL. Drills copy (inherit) this from the current entry.
	 */
	paneOwned?: boolean;
}

/** The normalized, always-present form after {@link readPaneState}. */
export interface ResolvedPaneState {
	paneDepth: number;
	paneOwned: boolean;
}

/**
 * Soft cap on drill depth (D4). Past it a drill REPLACES instead of pushing,
 * so a pathological very-deep distinct chain can't grow the browser history
 * unbounded. Chosen in the plan's ~15–20 band; the exact value isn't
 * load-bearing — it only bounds an extreme.
 */
export const PANE_DEPTH_SOFT_CAP = 20;

/**
 * Normalize an opaque `page.state`-shaped value into a definite
 * `{paneDepth, paneOwned}`. A cold-loaded or never-stamped entry (`undefined`,
 * `null`, `{}`) resolves to the base: depth 0, UNOWNED — the invariant that
 * keeps a cold-load A→B drill from wrongly unwinding off the base on close.
 * Negative / non-integer / non-number depths are floored to a safe 0.
 */
export function readPaneState(state: PaneHistoryState | null | undefined): ResolvedPaneState {
	const s = state ?? {};
	const rawDepth = s.paneDepth;
	const paneDepth =
		typeof rawDepth === 'number' && Number.isFinite(rawDepth) && rawDepth > 0
			? Math.floor(rawDepth)
			: 0;
	return { paneDepth, paneOwned: s.paneOwned === true };
}

/**
 * A single-entry history write: `push` mints a new entry, `replace` rewrites
 * the current one. Both carry the `page.state` stamp to emit.
 */
export type PaneWritePlan = { kind: 'push' | 'replace'; state: ResolvedPaneState };

/**
 * Detached lateral open (a direct list-row click at depth>0): a NEW top-level
 * open, not a replace of the drilled entry. Unwind the drill stack back to the
 * base with `history.go(goDelta)`, THEN (from a one-shot `afterNavigate` latch
 * — `history.go` has no completion promise) re-target the base to the new item,
 * PRESERVING the base's ownership (`resetState`).
 */
export type PaneResetPlan = { kind: 'reset'; goDelta: number; resetState: ResolvedPaneState };

/** What a DRILL (`navigatePaneTo`) decides: a same-ref/cycle no-op, or a write. */
export type PaneDrillPlan = { kind: 'noop' } | PaneWritePlan;

/** What a LATERAL open (`openItemPane`) decides: a write, or a detached reset. */
export type PaneLateralPlan = PaneWritePlan | PaneResetPlan;

/** The staged close plan (R8). Three ownership-aware branches. */
export type PaneClosePlan =
	/** UNOWNED & depth 0 (cold-loaded base, no pre-pane entry): drop `?item=`
	 *  in place with `replaceState`. Never `go` off the base. */
	| { kind: 'replace-delete' }
	/** OWNED: unwind the pushed base entry + every drill back to the pre-pane
	 *  URL in one `history.go(goDelta)`. No follow-up write needed — the
	 *  destination has no `?item=`, so the pane closes naturally. */
	| { kind: 'owned-go'; goDelta: number }
	/** UNOWNED & depth>0 (cold base, then drilled): `history.go(goDelta)` back
	 *  to the cold base, THEN a latched `replaceState`-delete of `?item=` (the
	 *  base still carries it). Two-phase because `history.go` can't be awaited. */
	| { kind: 'cold-base-go'; goDelta: number };

/**
 * Plan a DRILL (`navigatePaneTo`) — re-target the pane in place, deeper into
 * the stack. Ownership is INHERITED from the current entry (drills copy it),
 * so a cold-loaded base stays unowned all the way down.
 *
 *  - same-ref guard (D4): a drill to the item already shown is a no-op — kills
 *    the common `A→B→A` oscillation with zero stack;
 *  - soft depth cap (D4): at/above the cap, REPLACE (bound history growth);
 *  - otherwise PUSH at depth+1, carrying the inherited ownership.
 *
 * `targetRef` is the already-resolved canonical `?item=` value; a falsy target
 * is a no-op (nothing to open).
 */
export function planPaneDrill(
	currentRef: string | null,
	targetRef: string | null | undefined,
	current: ResolvedPaneState,
	softCap: number = PANE_DEPTH_SOFT_CAP,
): PaneDrillPlan {
	if (!targetRef) return { kind: 'noop' };
	// D4 same-ref guard — skip the push when re-targeting the current item.
	if (currentRef !== null && targetRef === currentRef) return { kind: 'noop' };
	if (current.paneDepth >= softCap) {
		// At the cap: replace, holding depth + inherited ownership steady.
		return { kind: 'replace', state: { paneDepth: current.paneDepth, paneOwned: current.paneOwned } };
	}
	// Drill: push one level deeper, INHERITING ownership from the current entry.
	return { kind: 'push', state: { paneDepth: current.paneDepth + 1, paneOwned: current.paneOwned } };
}

/**
 * Plan a LATERAL open (`openItemPane` — a list/board/table row click, Enter,
 * or a j/k pane-follow settle). Three cases:
 *
 *  - pane CLOSED → first-open: PUSH `{depth:0, owned:true}`. Only a first-open
 *    mints ownership (there's now a pre-pane entry to unwind to);
 *  - pane open at depth 0 → re-target: REPLACE, preserving the current
 *    ownership (an owned first-open stays owned; a cold-loaded base stays
 *    unowned) — the PLAN-2105 "hold j/k settles with one history entry" fix;
 *  - pane open at depth>0 (detached) → RESET: a direct row click is a NEW
 *    top-level open, not a replace of the drilled entry — collapse the stack
 *    (`go(-depth)`) then re-target the base, preserving the base's ownership.
 *    (`j`/`k` never reaches here: it's gated inert at depth>0.)
 */
export function planLateralOpen(paneOpen: boolean, current: ResolvedPaneState): PaneLateralPlan {
	if (!paneOpen) {
		// First-open mints ownership.
		return { kind: 'push', state: { paneDepth: 0, paneOwned: true } };
	}
	if (current.paneDepth === 0) {
		// Re-target at the base: replace, preserving ownership.
		return { kind: 'replace', state: { paneDepth: 0, paneOwned: current.paneOwned } };
	}
	// Detached: collapse the stack, then re-open the base (ownership preserved —
	// drills inherited it from the base, so current.paneOwned === base ownership).
	return {
		kind: 'reset',
		goDelta: -current.paneDepth,
		resetState: { paneDepth: 0, paneOwned: current.paneOwned },
	};
}

/**
 * Plan the three-way, ownership-aware, staged CLOSE (R8). See
 * {@link PaneClosePlan} for the branch semantics.
 */
export function planPaneClose(current: ResolvedPaneState): PaneClosePlan {
	if (current.paneOwned) {
		// Unwind the pushed base + every drill back to the pre-pane URL.
		return { kind: 'owned-go', goDelta: -(current.paneDepth + 1) };
	}
	if (current.paneDepth > 0) {
		// Cold base, then drilled: go back to the cold base, latch a delete.
		return { kind: 'cold-base-go', goDelta: -current.paneDepth };
	}
	// Cold-loaded base with no drills: drop `?item=` in place.
	return { kind: 'replace-delete' };
}
