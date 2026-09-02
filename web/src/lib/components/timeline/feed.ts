import type { TimelineEntry } from '$lib/types';

/**
 * What a SECOND, host-rendered view of the item timeline needs (IDEA-2843).
 *
 * Comments render under the item content while changes and versions render in
 * the pane's tabs, and one component cannot be in two DOM locations. So
 * `ItemTimeline` stays the single owner of fetching, the SSE subscription,
 * pagination and every mutation, and mirrors these values out for the host to
 * pass to a second `TimelineEntryList`.
 *
 * It lives in its own module rather than being exported from the component's
 * instance script so both sides can import the type without importing the
 * component.
 */
export interface TimelineFeed {
	entries: TimelineEntry[];
	loading: boolean;
	hasMore: boolean;
	loadingMore: boolean;
	loadMore: () => void;
}

/**
 * Which entry kinds each view renders (IDEA-2843).
 *
 * These are constants rather than literals at the two mount sites because the
 * partition carries an invariant that literals cannot state: EVERY kind must
 * appear in at least one view. A kind omitted from all of them renders
 * NOWHERE, silently — which is exactly how `note` / `decision` shipped
 * invisible the first time (BUG-2301). `ALL_TIMELINE_KINDS` plus the
 * exhaustiveness check below turn that into a build error when a new kind is
 * added to `TimelineEntry`, and a unit test asserts the union covers it.
 */
export const COMMENT_KINDS = ['comment'] as const;
export const CHANGE_KINDS = ['activity', 'note', 'decision'] as const;
export const VERSION_KINDS = ['version'] as const;

/**
 * Every kind `TimelineEntry` admits. Adding a kind to the type without adding
 * it here is a COMPILE error (the assignment below is checked both ways), and
 * adding it here without routing it to a view fails the coverage test.
 */
export const ALL_TIMELINE_KINDS = [
	'comment',
	'activity',
	'version',
	'note',
	'decision'
] as const;

// Both directions: a missing kind fails the first, an invented one the second.
const _kindsAreComplete: TimelineEntry['kind'] = null as unknown as (typeof ALL_TIMELINE_KINDS)[number];
const _kindsAreValid: (typeof ALL_TIMELINE_KINDS)[number] = null as unknown as TimelineEntry['kind'];
void _kindsAreComplete;
void _kindsAreValid;
