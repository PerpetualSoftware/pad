import type { ClientWriteStamp } from '$lib/types';

/**
 * This page load's content-write sequence (BUG-3080).
 *
 * Every content PATCH the item pane sends carries the next stamp, and the
 * server refuses one whose `n` is below a write it already applied for the same
 * (item, tab). That orders this tab's writes where both requests actually meet:
 * the teardown flushes are fired from a page that may be gone before either
 * answers, so nothing on the client can order them, and both would carry the
 * same version token.
 *
 * Module scope is the point. A module is evaluated once per page load, so the
 * id is per LOAD — a reload, or a second tab of the same user, starts a new
 * sequence, and two tabs never share one. It is deliberately not a session or
 * user id. `n` is shared across items; the server keys its mark by item too.
 */
function mintTabId(): string {
	if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
		return crypto.randomUUID();
	}
	return `t-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

const tab = mintTabId();
let n = 0;

export function nextClientWrite(): ClientWriteStamp {
	n += 1;
	return { tab, n };
}
