// Keeps the collection list's reading position across the split pane opening
// and closing (BUG-3165).
//
// The list's scroll container CHANGES with the pane: with no pane the page
// scrolls in the layout's `.main-content`; with `?item=` open,
// `.collection-page.pane-open` clips to the viewport and the list scrolls in its
// own `.list-column`. Nothing carried the position across that switch, so
// opening an item from a scrolled list dropped the list to its top (the clicked
// row off screen) and closing it jumped the list again.
//
// A pixel copy is wrong: the column is narrower than the page, so rows reflow
// to a different height. Instead an ANCHOR ROW is kept at the same place on
// screen — the pane item's own row when it is visible (the row the reader just
// clicked, or is closing), otherwise the first row showing at the top of the
// old container.
//
// Rows are found by `data-item-key` (`itemUrlId`, the value `?item=` carries)
// or `data-item-slug`, both stamped by `ItemCard` and `TableView`.

export interface ListAnchor {
	/** `data-item-key` of the anchor row. */
	key: string;
	/** The row's top edge in VIEWPORT coordinates when captured. */
	clientTop: number;
}

function rows(scroller: HTMLElement): HTMLElement[] {
	return Array.from(scroller.querySelectorAll<HTMLElement>('[data-item-key]'));
}

function findRow(scroller: HTMLElement, keyOrSlug: string): HTMLElement | null {
	for (const row of rows(scroller)) {
		if (row.dataset.itemKey === keyOrSlug || row.dataset.itemSlug === keyOrSlug) return row;
	}
	return null;
}

/**
 * The anchor for `scroller` as it is rendered NOW: the `preferred` item's row
 * when any of it is inside the scroller's visible band, else the first row whose
 * bottom is below the band's top. Null when no row is laid out (hidden list,
 * empty list) — the caller then leaves the scroll position alone.
 */
export function captureListAnchor(
	scroller: HTMLElement,
	preferred: string | null,
): ListAnchor | null {
	const band = scroller.getBoundingClientRect();
	if (band.height <= 0) return null;
	if (preferred) {
		const row = findRow(scroller, preferred);
		if (row) {
			const r = row.getBoundingClientRect();
			if (r.height > 0 && r.bottom > band.top && r.top < band.bottom) {
				return { key: row.dataset.itemKey!, clientTop: r.top };
			}
		}
	}
	for (const row of rows(scroller)) {
		const r = row.getBoundingClientRect();
		if (r.height > 0 && r.bottom > band.top + 1) {
			return { key: row.dataset.itemKey!, clientTop: r.top };
		}
	}
	return null;
}

/**
 * Scroll `scroller` so the anchor row's top is back at `anchor.clientTop`.
 * Returns the scrollTop it wrote, or null when the row is not in `scroller`.
 */
export function applyListAnchor(scroller: HTMLElement, anchor: ListAnchor): number | null {
	const row = findRow(scroller, anchor.key);
	if (!row) return null;
	const delta = row.getBoundingClientRect().top - anchor.clientTop;
	if (Math.abs(delta) >= 1) scroller.scrollTop += delta;
	return scroller.scrollTop;
}

/**
 * Apply `anchor` now, then keep re-applying for a few frames while the new
 * layout settles (the pane's content and the reflowed rows can still move the
 * row). Stops as soon as anything else moves the scroller — the reader
 * scrolling, or a restore that owns it — or after `budgetMs`. Returns a cancel.
 */
export function holdListAnchor(
	getScroller: () => HTMLElement | null,
	anchor: ListAnchor,
	budgetMs = 500,
): () => void {
	let cancelled = false;
	let frame = 0;
	const start = performance.now();
	const first = getScroller();
	let scroller = first;
	let last = first ? applyListAnchor(first, anchor) : null;
	if (last === null) return () => {};
	const tick = () => {
		frame = 0;
		if (cancelled) return;
		const now = getScroller();
		// Another scroller (the pane toggled again) or an outside write: stop.
		if (now !== scroller || !now || now.scrollTop !== last) return;
		last = applyListAnchor(now, anchor);
		if (last === null || performance.now() - start > budgetMs) return;
		frame = requestAnimationFrame(tick);
	};
	frame = requestAnimationFrame(tick);
	return () => {
		cancelled = true;
		if (frame) cancelAnimationFrame(frame);
		scroller = null;
	};
}
