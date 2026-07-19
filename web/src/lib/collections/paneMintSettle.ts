// paneMintSettle — coalesces rapid popstate-driven `?item=` changes (a HELD
// browser Back/Forward) into a single collab teardown/mint for the split
// pane's `<ItemDetail>` instance (PLAN-2154 Architecture D).
//
// Every `?item=` change re-drives ItemDetail's `loadData`/`collabKey`
// effects (destroy the old Y.Doc/WS provider, mint a fresh one, replay the
// op-log) — cheap for a single deliberate open/drill/close, expensive when a
// user holds Back/Forward and the browser fires a `popstate` per history
// entry traversed. `j`/`k` pane-follow already protects the same mint path
// with a ~140ms debounce (`PANE_FOLLOW_DEBOUNCE_MS`, `[collection]/+page.svelte`),
// but popstate bypasses that entirely — it flows straight through
// `beforeNavigate`'s same-pathname early return to the `openItemRef`
// derived. This module is the popstate-side mirror of that same coalescing.
//
// Direct actions (`openItemPane`/`navigatePaneTo`/`closeItemPane` — a `goto`
// push/replace, `nav.type !== 'popstate'`) apply IMMEDIATELY: they're
// already a single deliberate action, not a burst, and delaying one would
// only add latency users would notice. Only `nav.type === 'popstate'`
// changes settle.
//
// Framework-agnostic (no `$state`/`page`/`goto`) so it's unit-testable with
// vitest fake timers without mounting the route — same pattern as
// `contentSaver.svelte.ts` and `paneController.ts`.

/** Default settle window in ms — mirrors `PANE_FOLLOW_DEBOUNCE_MS`. */
export const PANE_MINT_SETTLE_MS = 140;

export interface PaneMintSettleConfig {
	/** Settle window in ms (default `PANE_MINT_SETTLE_MS`). */
	settleMs?: number;
	/**
	 * Fired with the settled `?item=` ref: immediately for a non-popstate
	 * nav, or once a popstate burst quiesces for `settleMs` with no further
	 * popstate. Only the LAST ref of a burst is ever applied — intermediate
	 * refs traversed mid-burst are dropped, which is the coalescing itself.
	 */
	onSettle: (ref: string | null) => void;
}

export interface PaneMintSettle {
	/**
	 * Report a completed navigation. `navType` is SvelteKit's `nav.type`
	 * (e.g. `'popstate' | 'link' | 'goto'`); `ref` is the `?item=` value at
	 * the navigation's destination. Every call — of EITHER kind — cancels
	 * any previously pending settle first, so a burst always collapses to
	 * the most recent call: a non-popstate nav mid-burst applies right away
	 * (and can't be clobbered by a stale popstate timer landing later); a
	 * fresh popstate mid-burst re-arms the window against the new ref.
	 */
	onNavigate(navType: string, ref: string | null): void;
	/** Cancel any pending settle without applying it (component teardown). */
	cancel(): void;
}

export function createPaneMintSettle(config: PaneMintSettleConfig): PaneMintSettle {
	const settleMs = config.settleMs ?? PANE_MINT_SETTLE_MS;
	let timer: ReturnType<typeof setTimeout> | undefined;

	function cancel(): void {
		if (timer !== undefined) {
			clearTimeout(timer);
			timer = undefined;
		}
	}

	function onNavigate(navType: string, ref: string | null): void {
		cancel();
		if (navType !== 'popstate') {
			config.onSettle(ref);
			return;
		}
		timer = setTimeout(() => {
			timer = undefined;
			config.onSettle(ref);
		}, settleMs);
	}

	return { onNavigate, cancel };
}
