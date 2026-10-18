import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import { bindReactiveEpoch, bindReactiveUserId, isEpochReactive } from '../../../test/identityEpochMock.svelte';

// TASK-3615 PR B: the catch-up card. Every load is HELD until the test
// resolves it, so a leg can move the identity or the workspace in between.
// Each refusal leg has a control, because "nothing rendered" is also what a
// card that never loaded looks like.

const auth = vi.hoisted(() => {
	const hook = {
		read: null as null | (() => number),
		write: null as null | ((n: number) => void),
		readUser: null as null | (() => string),
		writeUser: null as null | ((id: string) => void),
	};
	return {
		__hook: hook,
		get identityEpoch() { return hook.read ? hook.read() : 0; },
		get userId() { return hook.readUser ? hook.readUser() : 'u1'; },
		bumpEpoch() { hook.write?.((hook.read ? hook.read() : 0) + 1); },
		setUserId(id: string) { hook.writeUser?.(id); },
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

type Held = { ws: string; resolve: (v: unknown) => void; reject: (e: unknown) => void };
const gets = vi.hoisted(() => [] as Held[]);
const marks = vi.hoisted(() => [] as string[]);

vi.mock('$lib/api/client', () => ({
	api: {
		catchUp: {
			get: vi.fn((ws: string) => new Promise((resolve, reject) => {
				gets.push({ ws, resolve: resolve as (v: unknown) => void, reject });
			})),
			markCaughtUp: vi.fn(async (ws: string) => {
				marks.push(ws);
				return { seen_at: '2026-10-18T12:00:00Z' };
			}),
		},
	},
}));

import CatchUpCard from './CatchUpCard.svelte';

bindReactiveEpoch(auth.__hook);
bindReactiveUserId(auth.__hook);

const empty = () => ({ total: 0, lines: [] });
const line = (ref: string, title: string, what: string) => ({ ref, title, collection: 'tasks', what, actor: 'Rook', at: '2026-10-18T10:00:00Z' });

function digest(over: Record<string, unknown> = {}) {
	return {
		window: { since: '2026-10-17T18:40:00Z', until: '2026-10-18T12:00:00Z', has_cursor: true, capped_at_ceiling: false },
		needs_you: empty(),
		finished: empty(),
		new_and_reopened: empty(),
		comments: empty(),
		agents: { total: 0, groups: [] },
		...over,
	};
}

const busy = () =>
	digest({
		needs_you: { total: 1, lines: [line('BUG-1', 'Settings export', 'needs a decision')] },
		finished: {
			total: 7,
			lines: ['TASK-1', 'TASK-2', 'TASK-3', 'TASK-4', 'TASK-5'].map((r) => line(r, `title ${r}`, 'finished')),
		},
		agents: {
			total: 14,
			groups: [{ agent: 'claude-code', changes: 14, items: 5, lines: [line('TASK-9', 'agent work', 'updated')] }],
		},
	});

async function flush() {
	for (let i = 0; i < 5; i++) {
		await Promise.resolve();
		await tick();
	}
}

async function nextGet(n: number): Promise<Held> {
	for (let i = 0; i < 50 && gets.length <= n; i++) await flush();
	if (gets.length <= n) throw new Error(`load ${n} never started`);
	return gets[n];
}

beforeEach(() => {
	gets.length = 0;
	marks.length = 0;
	auth.setUserId('u1');
});
afterEach(() => cleanup());

describe('CatchUpCard (TASK-3615)', () => {
	it('the identity mock is reactive, or the fence legs below measure nothing', () => {
		expect(isEpochReactive(() => auth.identityEpoch, () => auth.bumpEpoch())).toBe(true);
	});

	it('renders the non-empty sections in order, with totals, "and N more", and See everything to the exact window', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(busy());
		await flush();

		const ids = [...r.container.querySelectorAll('[data-testid^="catch-up-section-"]')].map((el) => el.getAttribute('data-testid'));
		expect(ids).toEqual(['catch-up-section-needs_you', 'catch-up-section-finished', 'catch-up-section-agents']);
		expect(r.getByTestId('catch-up-section-finished').textContent).toContain('Finished (7)');
		const more = r.getByTestId('catch-up-section-finished').querySelector('a.more');
		expect(more?.textContent).toContain('and 2 more');
		const want = '/dave/pad/activity?since=2026-10-17T18%3A40%3A00Z&view=audit';
		expect(more?.getAttribute('href')).toBe(want);
		expect(r.getByTestId('catch-up-see-everything').getAttribute('href')).toBe(want);
		// The item line links to the item by ref.
		const ref = r.getByTestId('catch-up-section-needs_you').querySelector('a.line-ref');
		expect(ref?.getAttribute('href')).toBe('/dave/pad/tasks/BUG-1');
		// Agent groups start collapsed (DOC-3616: collapsed on mobile).
		const group = r.getByTestId('catch-up-section-agents').querySelector('details');
		expect(group?.open).toBe(false);
		expect(group?.textContent).toContain('14 changes across 5 items');
	});

	it('an empty digest is one caught-up line, and the no-cursor form names the 14-day window', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(digest());
		await flush();
		expect(r.getByTestId('catch-up-caught-up').textContent).toMatch(/^You're caught up · nothing new since /);
		expect(r.queryByTestId('catch-up-mark')).toBeNull();
		cleanup();

		const r2 = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(1)).resolve(digest({ window: { since: '2026-10-04T12:00:00Z', until: '2026-10-18T12:00:00Z', has_cursor: false, capped_at_ceiling: true } }));
		await flush();
		expect(r2.getByTestId('catch-up-caught-up').textContent).toBe("You're caught up · nothing new in the last 14 days");
	});

	it('a degraded needs_you is said, not shown as caught up', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(digest({ degraded: ['needs_you'] }));
		await flush();
		expect(r.queryByTestId('catch-up-caught-up')).toBeNull();
		expect(r.getByTestId('catch-up-degraded')).toBeTruthy();
	});

	it('Mark caught up PUTs once, then refetches and collapses', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(busy());
		await flush();
		await fireEvent.click(r.getByTestId('catch-up-mark'));
		const refetch = await nextGet(1);
		expect(marks).toEqual(['pad']);
		expect(refetch.ws).toBe('pad');
		refetch.resolve(digest());
		await flush();
		expect(r.getByTestId('catch-up-caught-up')).toBeTruthy();
	});

	it('a load that lands after the identity moved does not paint (control: the same load without the move does)', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		const held = await nextGet(0);
		auth.bumpEpoch();
		held.resolve(busy());
		await flush();
		expect(r.queryByTestId('catch-up-card')).toBeNull();
		cleanup();

		const c = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(1)).resolve(busy());
		await flush();
		expect(c.getByTestId('catch-up-card')).toBeTruthy();
	});

	it('a sign-in as someone else drops the card and reloads for them', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(busy());
		await flush();
		expect(r.getByTestId('catch-up-card')).toBeTruthy();
		auth.setUserId('u2');
		auth.bumpEpoch();
		await flush();
		expect(r.queryByTestId('catch-up-card')).toBeNull();
		(await nextGet(1)).resolve(digest());
		await flush();
		expect(r.getByTestId('catch-up-caught-up')).toBeTruthy();
	});

	it("a workspace switch drops the old digest, and the old workspace's late answer never paints", async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		const old = await nextGet(0);
		await r.rerender({ username: 'dave', wsSlug: 'other' });
		const fresh = await nextGet(1);
		expect(fresh.ws).toBe('other');
		old.resolve(busy());
		await flush();
		expect(r.queryByTestId('catch-up-card')).toBeNull();
		fresh.resolve(digest());
		await flush();
		expect(r.getByTestId('catch-up-caught-up')).toBeTruthy();
	});

	it('A→B→A: the first A answer, landing last, does not paint over the second (PR B r1)', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		const firstA = await nextGet(0);
		await r.rerender({ username: 'dave', wsSlug: 'other' });
		await nextGet(1);
		await r.rerender({ username: 'dave', wsSlug: 'pad' });
		const secondA = await nextGet(2);
		secondA.resolve(digest());
		await flush();
		// Control for this leg is the caught-up line here: the second A did
		// paint, so a busy card below would be the first A's stale answer.
		expect(r.getByTestId('catch-up-caught-up')).toBeTruthy();
		firstA.resolve(busy());
		await flush();
		expect(r.getByTestId('catch-up-caught-up')).toBeTruthy();
		expect(r.queryByTestId('catch-up-section-finished')).toBeNull();
	});

	it('a mark that lands but whose refetch fails keeps the card and says so (PR B r1)', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(busy());
		await flush();
		await fireEvent.click(r.getByTestId('catch-up-mark'));
		(await nextGet(1)).reject(new Error('refetch failed'));
		await flush();
		expect(marks).toEqual(['pad']);
		expect(r.getByTestId('catch-up-section-finished')).toBeTruthy();
		expect(r.getByTestId('catch-up-refresh-error').textContent).toContain('Marked caught up');
		expect(r.queryByText("Couldn't mark this workspace caught up. Try again.")).toBeNull();
	});

	it('a section that repeats a ref renders both lines instead of throwing (BUG-3538 class)', async () => {
		const r = render(CatchUpCard, { props: { username: 'dave', wsSlug: 'pad' } });
		(await nextGet(0)).resolve(
			digest({ needs_you: { total: 2, lines: [line('BUG-1', 'x', 'needs a decision'), line('BUG-1', 'x', 'blocked')] } })
		);
		await flush();
		expect(r.getByTestId('catch-up-section-needs_you').querySelectorAll('li.line')).toHaveLength(2);
	});
});
