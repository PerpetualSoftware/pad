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
	/**
	 * A lower bound on the feed, or '' / absent (TASK-3615). Set from the
	 * catch-up card's "See everything" link, which carries its window's exact
	 * RFC3339 start; the server also takes a bare YYYY-MM-DD.
	 */
	since?: string;
}

const SINCE_FORM =
	/^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:Z|([+-])(\d{2}):(\d{2})))?$/;

function daysIn(year: number, month: number): number {
	if (month === 2) return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0 ? 29 : 28;
	return [4, 6, 9, 11].includes(month) ? 30 : 31;
}

/**
 * The `since` a URL may set, or ''. Anything the server would refuse with
 * 400 invalid_since is dropped here, so a hand-edited link shows the
 * unfiltered feed instead of a load error.
 *
 * The ranges are Go's time.Parse ones (go1.26), checked by hand and never
 * through Date.parse: V8 rolls 2026-02-30 over to March 2 and accepts
 * T24:00:00, where Go refuses both. Go does accept offsets up to ±24:60
 * (TASK-3615 PR B r1). The server also refuses an instant whose UTC year
 * passes 9999, which an offset can reach from 9999-12-30 or -31 (PR B r5).
 */
export function sinceFromUrl(raw: string | null): string {
	const m = raw ? SINCE_FORM.exec(raw) : null;
	if (!raw || !m) return '';
	const [year, month, day] = [Number(m[1]), Number(m[2]), Number(m[3])];
	if (month < 1 || month > 12 || day < 1 || day > daysIn(year, month)) return '';
	if (m[4] !== undefined) {
		if (Number(m[4]) > 23 || Number(m[5]) > 59 || Number(m[6]) > 59) return '';
		if (m[7] !== undefined) {
			const [oh, om] = [Number(m[8]), Number(m[9])];
			if (oh > 24 || om > 60) return '';
			// UTC = local − offset, so a negative offset moves the instant later.
			const utcMinutes = Number(m[4]) * 60 + Number(m[5]) - (m[7] === '+' ? 1 : -1) * (oh * 60 + om);
			if (year === 9999 && month === 12 && day + Math.floor(utcMinutes / 1440) > 31) return '';
		}
	}
	return raw;
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
	if (f.since) params.since = f.since;
	return params;
}

export function hasActivityFilters(f: ActivityFilters): boolean {
	return !!(f.action || f.source || f.actor || f.collection || f.since);
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
