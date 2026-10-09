// A collection page's view mode and sort, as the URL and this browser carry
// them (TASK-2212, audit C34 + C95).
//
// C34: the page writes `?view=table` (paneUrlParams) but its load-time parser
// accepted only list|board, so a shared table link opened as the board for
// everyone whose own browser did not already prefer table. One predicate now
// decides what a view mode is, for the URL, localStorage and a saved view.
//
// C95: the remembered mode and sort were keyed by collection slug alone
// (`pad-view-tasks`), so switching Tasks to the board in one workspace flipped
// it in every workspace with a Tasks collection. They are now keyed by
// workspace too, like the default-saved-view pointer beside them
// (`pad-default-view:<ws>:<coll>`). A workspace with no scoped value yet reads
// the old unscoped key once and copies it, so nobody's preference resets on
// upgrade; the old key is left in place for the other workspaces' first read.

import { SORT_OPTIONS, type SortMode } from './itemSort';

export type ViewMode = 'list' | 'board' | 'table';

export const VIEW_MODES: readonly ViewMode[] = ['list', 'board', 'table'];

export function isViewMode(v: unknown): v is ViewMode {
	return typeof v === 'string' && (VIEW_MODES as readonly string[]).includes(v);
}

export function isSortMode(v: unknown): v is SortMode {
	return typeof v === 'string' && SORT_OPTIONS.some((o) => o.value === v);
}

export function viewModeKey(ws: string, coll: string): string {
	return `pad-view:${ws}:${coll}`;
}

export function sortModeKey(ws: string, coll: string): string {
	return `pad-sort:${ws}:${coll}`;
}

/** The pre-TASK-2212 keys, read only as a one-time fallback. */
export function legacyViewModeKey(coll: string): string {
	return `pad-view-${coll}`;
}

export function legacySortModeKey(coll: string): string {
	return `pad-sort-${coll}`;
}

// Storage can throw (private windows, blocked site data); every access is
// guarded and a failure reads as "nothing remembered".
function read(key: string): string | null {
	try {
		return localStorage.getItem(key);
	} catch {
		return null;
	}
}

function write(key: string, value: string): void {
	try {
		localStorage.setItem(key, value);
	} catch {
		/* not remembered; the page still works */
	}
}

function readScoped<T extends string>(
	scoped: string,
	legacy: string,
	valid: (v: unknown) => v is T
): T | null {
	const own = read(scoped);
	if (valid(own)) return own;
	const old = read(legacy);
	if (valid(old)) {
		write(scoped, old);
		return old;
	}
	return null;
}

export function loadViewMode(ws: string, coll: string, fallback: ViewMode): ViewMode {
	return readScoped(viewModeKey(ws, coll), legacyViewModeKey(coll), isViewMode) ?? fallback;
}

export function storeViewMode(ws: string, coll: string, mode: ViewMode): void {
	write(viewModeKey(ws, coll), mode);
}

export function loadSortMode(ws: string, coll: string): SortMode {
	return readScoped(sortModeKey(ws, coll), legacySortModeKey(coll), isSortMode) ?? 'manual';
}

export function storeSortMode(ws: string, coll: string, mode: SortMode): void {
	write(sortModeKey(ws, coll), mode);
}
