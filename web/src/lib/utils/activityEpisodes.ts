import type { Activity } from '$lib/types';
import { agentNameFromMetadata } from '$lib/utils/agentActor';

/**
 * The episode fold behind the activity page's Live view (IDEA-2755).
 *
 * An EPISODE is a run of consecutive activities by one actor on one item,
 * split when the gap between neighbours exceeds `gapMinutes`. The fold turns
 * an audit-grain feed (one row per event) into work-grain cards (one per
 * episode) — the collapse operator is the whole feature; rendering is just
 * the page's existing vocabulary.
 *
 * Identity today is (actor kind + name, item). The server stamps
 * `metadata.agent` from the client's X-Pad-Agent header
 * (handlers_documents.go agentMeta), and this fold renders whatever is
 * there verbatim — see `agentActor.ts` for where the name comes from and
 * what it does not claim. An agent that names itself gets its own label
 * and its own fold key; one that sends a generic client id gets that id;
 * one that sends nothing collapses into the shared "agent" label, and its
 * writes are then distinguishable only by the items they touch.
 *
 * LIVENESS is claimed only from evidence: an episode is `live` when its
 * newest event is younger than `liveMinutes`. The view never says "active
 * now" — it says how long ago the last event arrived, which is all the feed
 * actually knows (the delivery-honesty rule, inherited).
 */

export interface Episode {
	/** Stable key: actorKey + item key of the run. */
	key: string;
	itemRef?: string;
	itemTitle?: string;
	itemSlug?: string;
	collectionSlug?: string;
	/** Display label for the actor ("Dave", an agent's stamped name, or "agent"). */
	actorLabel: string;
	/** "agent" | "user" — drives the row's border treatment like the audit view. */
	actorKind: string;
	/** Newest activity in the episode (activities arrive newest-first). */
	latest: Activity;
	/** Oldest activity in the episode. */
	first: Activity;
	/** Event count folded into this card. */
	count: number;
	/** Distinct actions seen, newest first, deduped (e.g. ["updated","created"]). */
	actions: string[];
	/** Milliseconds between first and latest event. */
	spanMs: number;
	/** Newest event is younger than liveMinutes. */
	live: boolean;
}

export interface FoldOptions {
	/** Gap that splits two runs on the same (actor, item) into episodes. */
	gapMinutes?: number;
	/** Age of the newest event below which an episode renders as live. */
	liveMinutes?: number;
	/** Clock injection for tests. */
	now?: () => number;
}

function actorKeyOf(a: Activity): { key: string; label: string; kind: string } {
	if (a.actor === 'agent') {
		const label = agentNameFromMetadata(a.metadata) ?? 'agent';
		return { key: `agent:${label}`, label, kind: 'agent' };
	}
	const label = a.actor_name ?? (a.source === 'cli' ? 'cli' : 'web');
	return { key: `user:${label}`, label, kind: 'user' };
}

/**
 * Fold a newest-first activity list into newest-first episodes.
 *
 * Input order is the API's contract (created_at descending); the fold walks
 * it once and keeps that order for the output, so the newest episode — the
 * one whose newest event is youngest — is element 0. Splitting compares each
 * event against the OLDEST event already in the open run for its key, since
 * walking newest-first means the next same-key event is always older.
 */
export function foldEpisodes(activities: Activity[], opts: FoldOptions = {}): Episode[] {
	const gapMs = (opts.gapMinutes ?? 30) * 60_000;
	const liveMs = (opts.liveMinutes ?? 10) * 60_000;
	const now = opts.now ?? Date.now;

	const episodes: Episode[] = [];
	// Open run per fold key; closed when a same-key event falls past the gap.
	const open = new Map<string, Episode>();

	for (const a of activities) {
		const actor = actorKeyOf(a);
		const itemKey = a.document_id ?? a.item_ref ?? 'workspace';
		const key = `${actor.key}|${itemKey}`;
		const t = new Date(a.created_at).getTime();

		const run = open.get(key);
		if (run && new Date(run.first.created_at).getTime() - t <= gapMs) {
			run.first = a;
			run.count += 1;
			if (!run.actions.includes(a.action)) run.actions.push(a.action);
			run.spanMs = new Date(run.latest.created_at).getTime() - t;
			// Enrichment fields can be absent on some rows (e.g. the newest
			// row lacks item_title); keep the first non-empty value seen.
			run.itemRef ??= a.item_ref;
			run.itemTitle ??= a.item_title;
			run.itemSlug ??= a.item_slug;
			run.collectionSlug ??= a.collection_slug;
			continue;
		}

		// Past the gap: the old run stays closed in `episodes` (it is already
		// there); this event opens a fresh run under the same key.
		const ep: Episode = {
			key: `${key}@${a.id}`,
			itemRef: a.item_ref,
			itemTitle: a.item_title,
			itemSlug: a.item_slug,
			collectionSlug: a.collection_slug,
			actorLabel: actor.label,
			actorKind: actor.kind,
			latest: a,
			first: a,
			count: 1,
			actions: [a.action],
			spanMs: 0,
			live: now() - t <= liveMs
		};
		open.set(key, ep);
		episodes.push(ep);
	}

	return episodes;
}
