// Where the PAGE is, as opposed to where the address bar is (TASK-4315 PR 3b).
//
// Under an open Chat (a shallow navigation) the address bar reads /messages/N
// while `page.url` stays the route beneath. Anything that wants the page's own
// address (a heal rebuilding the URL, a print footer, a saved return target)
// must read it from page.url then. With no shallow navigation it reads
// window.location, as before: raw `history.replaceState` writes do not update
// Kit's page.url, so a bare page.url read would go stale after those.
import { page } from '$app/state';

function source(): Pick<URL, 'href' | 'pathname' | 'search'> | null {
	if (page.shallow) return page.url;
	return typeof window === 'undefined' ? null : window.location;
}

/** The page's full URL, or '' outside a browser. */
export function currentHref(): string {
	return source()?.href ?? '';
}

/** The page's `?query` (with its leading ?), or '' when there is none. */
export function currentSearch(): string {
	return source()?.search ?? '';
}

/** The page's pathname + search, for a return-to target. */
export function currentPathSearch(): string {
	const s = source();
	return s ? s.pathname + s.search : '';
}
