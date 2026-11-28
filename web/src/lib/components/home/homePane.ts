// URL state for the item pane on home (TASK-4902). Home is outside any
// workspace route, so beside `?item=<ref>` the URL carries the opened item's
// owner, workspace and collection in `item_in=<owner>/<workspace>/<collection>`,
// each segment encoded on its own.

export const ITEM_IN_PARAM = 'item_in';

export interface ItemIn {
	owner: string;
	workspace: string;
	collection: string;
}

/** What a home surface hands up to open an item in the pane. */
export interface HomePaneTarget extends ItemIn {
	ref: string;
}

export function formatItemIn(t: ItemIn): string {
	return [t.owner, t.workspace, t.collection].map(encodeURIComponent).join('/');
}

/** The parsed value, or null for a missing, empty or malformed one. */
export function parseItemIn(raw: string | null | undefined): ItemIn | null {
	if (!raw) return null;
	const parts = raw.split('/');
	if (parts.length !== 3) return null;
	let decoded: string[];
	try {
		decoded = parts.map(decodeURIComponent);
	} catch {
		return null;
	}
	if (decoded.some((p) => !p)) return null;
	const [owner, workspace, collection] = decoded;
	return { owner, workspace, collection };
}

/** The `data-pane-key` a home ref link carries, for focus return. */
export function homePaneKey(owner: string, workspace: string, ref: string): string {
	return `${owner}/${workspace}/${ref}`;
}
