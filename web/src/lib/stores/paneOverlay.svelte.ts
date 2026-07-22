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
let overlayCount = $state(0);

export const paneOverlay = {
	/** True while ≥1 mobile detail-pane overlay is active. Read by the workspace layout. */
	get mobileOverlayActive() {
		return overlayCount > 0;
	},
	/** Register a newly-active mobile overlay. Pair every call with `leave()`. */
	enter() {
		overlayCount += 1;
	},
	/** Release a mobile overlay that stopped being active (unmount / breakpoint / close). */
	leave() {
		overlayCount = Math.max(0, overlayCount - 1);
	},
};
