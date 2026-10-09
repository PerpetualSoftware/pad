// The activity page's filters as the request carries them (TASK-2219).
//
// Every filter is now applied by the SERVER, in the store query before its
// LIMIT. The collection filter used to run client-side over the loaded page:
// a collection whose rows were not in the newest 30 read as empty, and Load
// more was hidden while it was set. The page's first load, its Load more and
// its live head re-read all build their query here, so the three cannot ask
// for different feeds.

export interface ActivityFilters {
	action: string;
	source: string;
	/** '' (anyone), 'agent' or 'user', the server's `actor` values. */
	actor: string;
	/** A collection slug, or ''. */
	collection: string;
}

export const ACTOR_OPTIONS: { value: string; label: string }[] = [
	{ value: '', label: 'Anyone' },
	{ value: 'user', label: 'Humans' },
	{ value: 'agent', label: 'Agents' }
];

export function activityFilterParams(f: ActivityFilters): Record<string, string> {
	const params: Record<string, string> = {};
	if (f.action) params.action = f.action;
	if (f.source) params.source = f.source;
	if (f.actor) params.actor = f.actor;
	if (f.collection) params.collection = f.collection;
	return params;
}

export function hasActivityFilters(f: ActivityFilters): boolean {
	return !!(f.action || f.source || f.actor || f.collection);
}

/**
 * The tooltip for an actor badge (TASK-2219, audit C117). The badge says web
 * versus CLI by colour alone, which a colour-blind reader, a screen reader and
 * a printout all lose; the tooltip and accessible name say it in words.
 */
export function actorBadgeTitle(kind: string, label: string, named: boolean): string {
	if (kind === 'agent') return named ? `${label} (agent)` : 'An agent';
	// `cli` is a CLI write; `user` (a named web write) and `web` are the web.
	const cli = kind === 'cli';
	if (named) return `${label}, ${cli ? 'via CLI' : 'via web'}`;
	return cli ? 'Via CLI' : 'Via web';
}
