// "An overlay covers the page" (TASK-3520), for the screen-reader browse
// cursor.
//
// The command palette, a mobile docked sheet and the notification panel each
// keep Tab inside themselves (TASK-2235), but a screen reader's browse cursor
// is not Tab: it walks the DOM, and walked straight past the overlay into the
// page its backdrop covers. Making the covered regions `inert` takes them out
// of that walk.
//
// viewerBackdrop.ts cannot do it: it owns `inert` on `document.body`'s
// children, and these overlays render INSIDE the app wrapper (the palette in
// the root layout, the sheets inside BottomNav, the panel inside Sidebar), so
// a lease there would inert the overlay asking for it. Instead the layouts
// read this store and put `inert` on the regions that do not hold the overlay,
// the same way paneOverlay drives the mobile detail pane's background. Those
// are nested elements, never body children, so there is no ownership conflict
// with viewerBackdrop: a viewer opened from a sheet inerts the body child that
// contains them, and its release clears only its own writes.
//
// `keepBottomNav`: a docked sheet, and the palette docked above the nav on
// mobile, leave the bottom nav live by design (its slot toggles them), and the
// nav lives inside <main>. Such an overlay asks the WORKSPACE layout to inert
// the page around the nav instead of the root layout inerting <main>.
//
// Ref-counted entries, written only from the overlays' $effects and read only
// by the layouts (CONVE-1688): `untrack` the read in the writers, as
// paneOverlay does, so a write never makes its effect depend on itself.
import { untrack } from 'svelte';

interface Entry {
	keepBottomNav: boolean;
}

// Raw: entries are compared by identity, and a deep $state would proxy them.
let entries = $state.raw<Entry[]>([]);

export const coveredPage = {
	/** True while any overlay covers the page. */
	get active(): boolean {
		return entries.length > 0;
	},
	/** True while a covering overlay needs the bottom nav to stay live. */
	get keepsBottomNav(): boolean {
		return entries.some((e) => e.keepBottomNav);
	},
	/** Register a covering overlay; call the returned function when it stops covering. */
	enter(opts: { keepBottomNav: boolean }): () => void {
		const entry: Entry = { keepBottomNav: opts.keepBottomNav };
		entries = [...untrack(() => entries), entry];
		let left = false;
		return () => {
			if (left) return;
			left = true;
			entries = untrack(() => entries).filter((e) => e !== entry);
		};
	}
};

/** Test-only. */
export function __resetCoveredPageForTests(): void {
	entries = [];
}
