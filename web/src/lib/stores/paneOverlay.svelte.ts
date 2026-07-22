// Ref-counted signal for "a mobile full-screen detail-pane overlay is active"
// (PLAN-2105 / TASK-2131). At ≤768px the detail pane (PaneHost's `.item-pane`)
// restyles to a full-viewport modal overlay covering the app-shell chrome.
// While it's up, the workspace layout marks the chrome siblings it renders —
// `MobileContextBar` + `BottomNav` — `inert` so they leave the focus order AND
// the screen-reader tree. A JS focus trap plus `aria-modal="true"` on the pane
// can't fully constrain an SR virtual cursor, so the background chrome must
// physically drop out of the a11y tree — the same treatment the collection
// host already gives its list column (`inert={viewport.isMobile && openItemRef}`).
// The chrome lives in `+layout.svelte`, ABOVE the pane host, so the host can't
// reach it by prop; it hoists the fact into this store instead.
//
// Ref-counted rather than a bare boolean so a brief overlap of two mounted
// overlays — e.g. a client route change from the collection host to the
// full-page item host where the incoming PaneHost's effect runs before the
// outgoing one's cleanup — can't clear the signal early. PaneHost is the only
// writer (one `enter`/`leave` per active overlay via an `$effect`); the layout
// is the only reader. This one-way split satisfies CONVE-1688 — a store must
// never read state it also writes.
import { untrack } from 'svelte';

let overlayCount = $state(0);

// The mutators are called from a PaneHost `$effect` (the only writer). A naive
// `overlayCount += 1` READS `overlayCount` inside that tracked scope, so the
// effect would take a reactive dependency on the very signal it writes —
// enter() dirties the effect, which re-runs, enter()s again, ad infinitum:
// `effect_update_depth_exceeded`, which aborts the flush and strands the rest
// of the subtree's reactivity (BUG-2284 — the mobile pane rendered empty
// because `paneMintForRoute` stopped recomputing). `untrack` the read so a
// write never establishes that self-dependency; the write still notifies the
// layout reader normally.
export const paneOverlay = {
	/** True while ≥1 mobile detail-pane overlay is active. Read by the workspace layout. */
	get mobileOverlayActive() {
		return overlayCount > 0;
	},
	/** Register a newly-active mobile overlay. Pair every call with `leave()`. */
	enter() {
		overlayCount = untrack(() => overlayCount) + 1;
	},
	/** Release a mobile overlay that stopped being active (unmount / breakpoint / close). */
	leave() {
		overlayCount = Math.max(0, untrack(() => overlayCount) - 1);
	},
};
