import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Activity, Comment } from '$lib/types';

/**
 * EpisodeFeed renders WORK-grain cards (one per episode from foldEpisodes),
 * not audit-grain rows — the collapse is the feature, so the first assertion
 * here is card count vs activity count. The other claims: live vs earlier
 * sectioning follows `episode.live`, and the checkpoint enrichment (newest
 * comment's first line) is fetched for LIVE episodes only.
 */

const commentsListMock = vi.hoisted(() =>
	vi.fn<(ws: string, slug: string) => Promise<Comment[]>>()
);

vi.mock('$lib/api/client', () => ({
	api: {
		comments: {
			list: (ws: string, slug: string) => commentsListMock(ws, slug),
		},
	},
}));

const { default: EpisodeFeed } = await import('./EpisodeFeed.svelte');

let seq = 0;
function act(minutesAgo: number, over: Partial<Activity> = {}): Activity {
	seq += 1;
	return {
		id: `a${seq}`,
		workspace_id: 'ws',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		metadata: '{}',
		created_at: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
		document_id: 'item-live',
		item_ref: 'TASK-1',
		item_title: 'Live thing',
		item_slug: 'live-thing',
		collection_slug: 'tasks',
		...over,
	};
}

function comment(body: string, minutesAgo: number): Comment {
	return {
		id: `c${++seq}`,
		item_id: 'item-live',
		workspace_id: 'ws',
		author: 'agent',
		body,
		created_by: 'agent',
		source: 'cli',
		created_at: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
		updated_at: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
	};
}

/** 5 activities folding into 2 episodes: one live (agent, minutes old) and
 *  one stale (user, hours old). Newest-first, as the API delivers. */
function fiveActivitiesTwoEpisodes(): Activity[] {
	const stale = {
		actor: 'user',
		actor_name: 'Dave',
		source: 'web',
		document_id: 'item-stale',
		item_ref: 'DOC-9',
		item_title: 'Stale thing',
		item_slug: 'stale-thing',
		collection_slug: 'docs',
	};
	return [act(1), act(3), act(5), act(120, stale), act(125, stale)];
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

function mountFeed(activities: Activity[]) {
	app = mount(EpisodeFeed, {
		target: host,
		props: { activities, wsSlug: 'ws', username: 'alice' },
	}) as Record<string, unknown>;
	flushSync();
}

/** Let the mount effect fire, the comments fetch resolve, and the checkpoint
 *  state re-render. */
async function settle() {
	flushSync();
	await Promise.resolve();
	await Promise.resolve();
	await tick();
	flushSync();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	commentsListMock.mockReset();
	commentsListMock.mockResolvedValue([]);
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('EpisodeFeed', () => {
	it('renders one card per episode, not per activity', async () => {
		mountFeed(fiveActivitiesTwoEpisodes());
		await settle();

		expect(host.querySelectorAll('.episode-card')).toHaveLength(2);
	});

	it('sections live episodes under "happening now" and stale ones under "earlier"', async () => {
		mountFeed(fiveActivitiesTwoEpisodes());
		await settle();

		const sections = Array.from(host.querySelectorAll('.feed-section'));
		const bySection = (label: string) =>
			sections.find((s) =>
				s.querySelector('.section-label')?.textContent?.toLowerCase().includes(label)
			);

		const happeningNow = bySection('happening now');
		const earlier = bySection('earlier');
		expect(happeningNow).toBeDefined();
		expect(earlier).toBeDefined();
		expect(happeningNow!.textContent).toContain('Live thing');
		expect(happeningNow!.textContent).not.toContain('Stale thing');
		expect(earlier!.textContent).toContain('Stale thing');
		expect(earlier!.textContent).not.toContain('Live thing');
	});

	it('fetches the checkpoint line only for live episodes and shows the newest comment first line', async () => {
		commentsListMock.mockResolvedValue([
			comment('older note\nnot this line', 30),
			comment('Checkpoint: wiring the fold\nsecond line stays hidden', 2),
		]);

		mountFeed(fiveActivitiesTwoEpisodes());
		await settle();

		// Enrichment ran only for the live episode's item, never the stale one.
		expect(commentsListMock).toHaveBeenCalledTimes(1);
		expect(commentsListMock).toHaveBeenCalledWith('ws', 'live-thing');

		const checkpoint = host.querySelector('.ep-checkpoint');
		expect(checkpoint).not.toBeNull();
		expect(checkpoint!.textContent).toContain('latest: Checkpoint: wiring the fold');
		expect(checkpoint!.textContent).not.toContain('second line stays hidden');
	});

	// TASK-2759. foldEpisodes computes the label; this asserts the FEED renders
	// it (CONVE-19 — a correct fold the card never reads would pass every test
	// in activityEpisodes.test.ts). Two named agents on separate items also
	// prove the fold key follows the name: a filtered or blanked label would
	// merge them into one card, so the count is the counterfactual.
	it('renders each agent under its own stamped name', async () => {
		const onOtherItem = {
			document_id: 'item-other',
			item_ref: 'BUG-7',
			item_title: 'Other thing',
			item_slug: 'other-thing',
			collection_slug: 'bugs',
		};
		mountFeed([
			act(1, { metadata: '{"agent":"wren"}' }),
			act(2, { metadata: '{"agent":"rook"}', ...onOtherItem }),
		]);
		await settle();

		const labels = [...host.querySelectorAll('.ep-actor')].map((el) => el.textContent);
		expect(labels.sort()).toEqual(['rook', 'wren']);
	});

	it('renders the generic label for an agent that stamped no name', async () => {
		// `act`'s default metadata is '{}' — the pre-BUG-2542 shape, and the
		// shape any agent that never sends the header still produces.
		mountFeed([act(1)]);
		await settle();

		expect(host.querySelector('.ep-actor')!.textContent).toBe('agent');
	});
});
