import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-3923 (IDEA-3906 A6): the "Needs you" page. The API client is stubbed;
 * what is pinned is what the page does with the server's answers.
 */
const list = vi.hoisted(() => vi.fn());
const snooze = vi.hoisted(() => vi.fn());
const restore = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		needs: {
			list: (p: unknown) => list(p),
			snooze: (ws: string, ref: string, body: unknown) => snooze(ws, ref, body),
			restore: (ws: string, ref: string) => restore(ws, ref)
		}
	}
}));

const { default: Page } = await import('./+page.svelte');

const marker = (n: number) => ({
	seen_comment_at: `2026-10-0${n}T10:00:00Z`,
	seen_comment_ids: [`c${n}`],
	seen_transition_seq: n
});

function row(ws: string, ref: string, extra: Record<string, unknown> = {}) {
	return {
		workspace: { slug: ws, name: `WS ${ws}`, owner_username: 'dave' },
		item: {
			ref,
			title: `Title ${ref}`,
			collection: 'Tasks',
			collection_slug: 'tasks',
			created_at: '2026-10-01T00:00:00Z',
			updated_at: '2026-10-01T00:00:00Z',
			path: `/dave/${ws}/tasks/${ref.toLowerCase()}`
		},
		why: { rule: 'assigned', text: 'It is assigned to you.' },
		because: [],
		waiting_since: '2026-10-01T00:00:00Z',
		marker: marker(Number(ref.split('-')[1]) % 9),
		...extra
	};
}

function resp(rows: ReturnType<typeof row>[], extra: Record<string, unknown> = {}) {
	const counts = new Map<string, number>();
	for (const r of rows) counts.set(r.workspace.slug, (counts.get(r.workspace.slug) ?? 0) + 1);
	return {
		rows,
		total: rows.length,
		hidden: 0,
		workspaces: [...counts].map(([slug, count]) => ({
			slug,
			name: `WS ${slug}`,
			owner_username: 'dave',
			count
		})),
		...extra
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	flushSync();
	for (let i = 0; i < 6; i++) await Promise.resolve();
	await tick();
	flushSync();
}

async function mountWith(body: unknown) {
	list.mockResolvedValue(body);
	app = mount(Page, { target: host, props: {} }) as Record<string, unknown>;
	await settle();
}

function btn(label: string | RegExp, scope: ParentNode = host): HTMLButtonElement {
	const found = [...scope.querySelectorAll('button')].find((b) =>
		typeof label === 'string' ? b.textContent?.trim() === label : label.test(b.textContent ?? '')
	);
	if (!found) throw new Error(`no button ${label}`);
	return found as HTMLButtonElement;
}

async function click(el: HTMLElement) {
	el.click();
	await settle();
}

const refs = () => [...host.querySelectorAll('[data-testid="needs-row"]')].map((r) => r.getAttribute('data-ref'));
const groupCounts = () =>
	[...host.querySelectorAll('[data-testid="needs-group"]')].map((g) => [
		g.querySelector('h2')?.firstChild?.textContent?.trim(),
		g.querySelector('[data-testid="needs-group-count"]')?.textContent?.trim()
	]);

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	list.mockReset();
	snooze.mockReset();
	restore.mockReset();
});

afterEach(() => {
	vi.useRealTimers();
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('needs page: grouping', () => {
	it('groups by first appearance, takes header counts from workspaces[], and shows a hint chip only where there is one', async () => {
		const rows = [
			row('beta', 'TASK-1', { importance: { value: 80, hint: 'due soon' } }),
			row('alpha', 'TASK-2'),
			row('beta', 'TASK-3')
		];
		const body = resp(rows);
		body.workspaces = [
			{ slug: 'alpha', name: 'WS alpha', owner_username: 'dave', count: 1 },
			{ slug: 'beta', name: 'WS beta', owner_username: 'dave', count: 7 }
		];
		body.total = 8;
		await mountWith(body);
		// beta appears first in rows although alpha sorts first in workspaces[].
		expect(groupCounts()).toEqual([
			['WS beta', '7'],
			['WS alpha', '1']
		]);
		expect(refs()).toEqual(['TASK-1', 'TASK-3', 'TASK-2']);
		const hints = [...host.querySelectorAll('[data-testid="needs-hint"]')];
		expect(hints.map((h) => h.textContent?.trim())).toEqual(['due soon']);
		expect(hints[0].closest('[data-testid="needs-row"]')?.getAttribute('data-ref')).toBe('TASK-1');
	});
});

describe('needs page: hints', () => {
	it('renders a row whose importance and simplicity carry the same hint', async () => {
		await mountWith(
			resp([row('a', 'TASK-1', { importance: { value: 1, hint: 'same' }, simplicity: { value: 2, hint: 'same' } })])
		);
		expect(refs()).toEqual(['TASK-1']);
		expect(host.querySelectorAll('[data-testid="needs-hint"]').length).toBe(2);
	});
});

describe('needs page: sort', () => {
	it('defaults to age and sends the toggle server-side', async () => {
		await mountWith(resp([row('a', 'TASK-1')]));
		expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'active' });
		await click(btn('Importance'));
		expect(list).toHaveBeenLastCalledWith({ sort: 'importance', view: 'active' });
		await click(btn('Simplicity'));
		expect(list).toHaveBeenLastCalledWith({ sort: 'simplicity', view: 'active' });
	});

	it('ignores a stale response that lands after a newer sort', async () => {
		let resolveAge!: (v: unknown) => void;
		list.mockImplementationOnce(() => new Promise((r) => (resolveAge = r)));
		app = mount(Page, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		list.mockResolvedValueOnce(resp([row('a', 'TASK-9')]));
		await click(btn('Importance'));
		expect(refs()).toEqual(['TASK-9']);
		resolveAge(resp([row('a', 'TASK-1')]));
		await settle();
		expect(refs()).toEqual(['TASK-9']);
	});

	it('ignores a stale response that lands after a view toggle', async () => {
		let resolveActive!: (v: unknown) => void;
		list.mockImplementationOnce(() => new Promise((r) => (resolveActive = r)));
		app = mount(Page, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		list.mockResolvedValueOnce(resp([row('a', 'TASK-5', { snooze: { kind: 'dismiss' } })], { hidden: 1 }));
		await click(btn(/^Hidden/));
		resolveActive(resp([row('a', 'TASK-1')]));
		await settle();
		expect(refs()).toEqual(['TASK-5']);
		expect(host.textContent).toContain('Back to needs you');
	});
});

describe('needs page: dismiss', () => {
	it('sends the marker verbatim, removes the row, adjusts counts and drops an empty group', async () => {
		const a = row('alpha', 'TASK-1');
		const b = row('beta', 'TASK-2');
		const c = row('beta', 'TASK-3');
		await mountWith(resp([a, b, c], { hidden: 2 }));
		snooze.mockResolvedValue({ ref: 'TASK-1', snooze: {} });
		await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
		expect(snooze).toHaveBeenCalledTimes(1);
		const [ws, ref, body] = snooze.mock.calls[0];
		expect(ws).toBe('alpha');
		expect(ref).toBe('TASK-1');
		expect(body).toStrictEqual({ kind: 'dismiss', marker: a.marker });
		expect(refs()).toEqual(['TASK-2', 'TASK-3']);
		// alpha had one row: its group is gone, beta's count is untouched.
		expect(groupCounts()).toEqual([['WS beta', '2']]);
		expect(host.querySelector('h1')?.textContent).toContain('2');
		expect(btn(/^Hidden/).textContent).toContain('(3)');

		await click(btn('Dismiss', host.querySelector('[data-ref="TASK-2"]')!));
		expect(groupCounts()).toEqual([['WS beta', '1']]);
		expect(btn(/^Hidden/).textContent).toContain('(4)');
	});

	it('keeps the row and shows the error when the PUT is rejected', async () => {
		await mountWith(resp([row('alpha', 'TASK-1')], { hidden: 0 }));
		snooze.mockRejectedValue(new Error('boom'));
		await click(btn('Dismiss'));
		expect(refs()).toEqual(['TASK-1']);
		expect(host.querySelector('[data-testid="needs-action-error"]')?.textContent).toContain('boom');
		expect(btn(/^Hidden/).textContent).toContain('(0)');
		expect(groupCounts()).toEqual([['WS alpha', '1']]);
	});
});

describe('needs page: defer menu', () => {
	// Thursday 2026-10-22, 14:30 local.
	const NOW = new Date(2026, 9, 22, 14, 30, 0);

	async function openMenu() {
		vi.useFakeTimers({ toFake: ['Date'] });
		vi.setSystemTime(NOW);
		await mountWith(resp([row('alpha', 'TASK-1')]));
		await click(btn('Defer'));
	}
	const deferCall = () => snooze.mock.calls[0];

	it('tomorrow is 09:00 local the next day', async () => {
		await openMenu();
		snooze.mockResolvedValue({});
		await click(btn('Tomorrow 09:00'));
		const [ws, ref, body] = deferCall();
		expect([ws, ref]).toEqual(['alpha', 'TASK-1']);
		expect(body.kind).toBe('defer');
		expect(body.defer_until).toBe(new Date(2026, 9, 23, 9, 0, 0).toISOString());
		expect(body.marker).toStrictEqual(resp([row('alpha', 'TASK-1')]).rows[0].marker);
		expect(refs()).toEqual([]);
	});

	it('next week is the following Monday 09:00 local', async () => {
		await openMenu();
		snooze.mockResolvedValue({});
		await click(btn(/^Next week/));
		expect(deferCall()[2].defer_until).toBe(new Date(2026, 9, 26, 9, 0, 0).toISOString());
	});

	it('a picked date is that day at 09:00 local', async () => {
		await openMenu();
		snooze.mockResolvedValue({});
		const input = host.querySelector<HTMLInputElement>('input[type="date"]')!;
		input.value = '2026-11-05';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		await click(btn('Defer', host.querySelector('.defer-menu')!));
		expect(deferCall()[2].defer_until).toBe(new Date(2026, 10, 5, 9, 0, 0).toISOString());
	});

	it('blocks a past date and a blank one', async () => {
		await openMenu();
		const menu = host.querySelector('.defer-menu')!;
		// Blank: the button is disabled and nothing is sent.
		expect(btn('Defer', menu).disabled).toBe(true);
		await click(btn('Defer', menu));
		expect(snooze).not.toHaveBeenCalled();
		// Past: refused with a message.
		const input = host.querySelector<HTMLInputElement>('input[type="date"]')!;
		input.value = '2026-10-20';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		await click(btn('Defer', menu));
		expect(snooze).not.toHaveBeenCalled();
		expect(menu.querySelector('[role="alert"]')?.textContent).toContain('future');
		// Today at 09:00 has already passed at 14:30.
		input.value = '2026-10-22';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		await click(btn('Defer', menu));
		expect(snooze).not.toHaveBeenCalled();
	});

	it('Escape closes the menu without deferring', async () => {
		await openMenu();
		const menu = host.querySelector('.defer-menu')!;
		menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
		await settle();
		expect(host.querySelector('.defer-menu')).toBeNull();
		expect(snooze).not.toHaveBeenCalled();
		expect(refs()).toEqual(['TASK-1']);
	});
});

describe('needs page: hidden view', () => {
	it('asks for view=hidden, offers Restore only, and restore sends no body and updates counts', async () => {
		await mountWith(resp([row('alpha', 'TASK-1')], { hidden: 1 }));
		list.mockResolvedValue(
			resp([row('alpha', 'TASK-7', { snooze: { kind: 'dismiss', holds: true } }), row('alpha', 'TASK-8')], {
				hidden: 2
			})
		);
		await click(btn(/^Hidden/));
		expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'hidden' });
		const labels = [...host.querySelectorAll('button')].map((b) => b.textContent?.trim());
		expect(labels).toContain('Restore');
		expect(labels).not.toContain('Dismiss');
		expect(labels).not.toContain('Defer');
		restore.mockResolvedValue({ ref: 'TASK-7', snooze: null });
		await click(btn('Restore', host.querySelector('[data-ref="TASK-7"]')!));
		expect(restore).toHaveBeenCalledWith('alpha', 'TASK-7');
		expect(snooze).not.toHaveBeenCalled();
		expect(refs()).toEqual(['TASK-8']);
		expect(groupCounts()).toEqual([['WS alpha', '1']]);
		expect(host.querySelector('h1')?.textContent).toContain('1');
		expect(btn(/^Back to needs you/)).toBeTruthy();
	});
});

describe('needs page: degraded, empty, paging', () => {
	it('names the degraded workspaces without hiding the rows', async () => {
		await mountWith(resp([row('alpha', 'TASK-1')], { degraded: ['alpha'] }));
		expect(host.querySelector('[data-testid="needs-degraded"]')?.textContent).toContain('alpha');
		expect(refs()).toEqual(['TASK-1']);
	});

	it('shows the empty state, and none of the degraded banner when there is none', async () => {
		await mountWith(resp([]));
		expect(host.querySelector('[data-testid="needs-empty"]')?.textContent).toContain('Nothing needs you');
		expect(host.querySelector('[data-testid="needs-degraded"]')).toBeNull();
	});

	it('Load more sends the cursor with the current sort and appends', async () => {
		await mountWith(resp([row('alpha', 'TASK-1')], { next_cursor: 'cur-1' }));
		list.mockResolvedValue(resp([row('alpha', 'TASK-2')]));
		await click(btn('Load more'));
		expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'active', cursor: 'cur-1' });
		expect(refs()).toEqual(['TASK-1', 'TASK-2']);
		expect(host.textContent).not.toContain('Load more');
	});

	it('shows a load failure', async () => {
		list.mockRejectedValue(new Error('nope'));
		app = mount(Page, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		expect(host.querySelector('[role="alert"]')?.textContent).toContain('nope');
	});
});

describe('needs page: r1 fixes', () => {
	const deferred = () => {
		let resolve!: (v: unknown) => void;
		const promise = new Promise((r) => (resolve = r));
		return { promise, resolve };
	};

	it('hidden-view copy says deferrals hold until their time and only dismissals return on news', async () => {
		await mountWith(resp([row('a', 'TASK-1')], { hidden: 1 }));
		list.mockResolvedValue(resp([row('a', 'TASK-7', { snooze: { kind: 'dismiss' } })], { hidden: 1 }));
		await click(btn(/^Hidden/));
		const text = host.textContent ?? '';
		expect(text).not.toContain('on their own');
		expect(text).toMatch(/Dismissed items come back when something new happens/);
		expect(text).toMatch(/Deferred items come back at the time you chose/);
	});

	it('a dismissed row is not shown by a list started before the PUT finished, whichever lands first', async () => {
		await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
		const put = deferred();
		snooze.mockImplementationOnce(() => put.promise);
		await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
		// Sort change while the PUT is pending; the new list still holds the row.
		list.mockResolvedValueOnce(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
		await click(btn('Importance'));
		expect(refs()).toEqual(['TASK-1', 'TASK-3']);
		put.resolve({});
		await settle();
		expect(refs()).toEqual(['TASK-3']);
	});

	it('restoring stops the filter, and dismissing does not hide the row from the hidden view', async () => {
		await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { hidden: 0 }));
		snooze.mockResolvedValue({});
		await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
		list.mockResolvedValue(resp([row('a', 'TASK-1', { snooze: { kind: 'dismiss' } })], { hidden: 1 }));
		await click(btn(/^Hidden/));
		expect(refs()).toEqual(['TASK-1']);
		restore.mockResolvedValue({});
		await click(btn('Restore'));
		expect(refs()).toEqual([]);
		list.mockResolvedValue(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
		await click(btn(/^Back to needs you/));
		expect(refs()).toEqual(['TASK-1', 'TASK-3']);
	});

	describe('stale responses (class fix): one fresh page-1 load of the current view', () => {
		const heading = () => host.querySelector('h1')?.textContent ?? '';
		const hiddenN = () => btn(/^Hidden/).textContent ?? '';
		const lastCall = () => list.mock.calls[list.mock.calls.length - 1][0];
		const never = () => new Promise(() => {});

		it('row 1: a stale active response shows a dismissed row; the refetch drops it and fixes every count', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			list.mockImplementationOnce(() => Promise.resolve(resp([row('a', 'TASK-3')], { hidden: 1 })));
			reload.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { hidden: 0 }));
			await settle();
			expect(list).toHaveBeenCalledTimes(3);
			expect(lastCall()).toEqual({ sort: 'importance', view: 'active' });
			expect(refs()).toEqual(['TASK-3']);
			expect(groupCounts()).toEqual([['WS a', '1']]);
			expect(heading()).toContain('1');
			expect(heading()).not.toContain('2');
			expect(hiddenN()).toContain('(1)');
		});

		it('row 2: a stale hidden response shows a restored row; the refetch drops it', async () => {
			await mountWith(resp([]));
			list.mockResolvedValueOnce(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { hidden: 2 }));
			await click(btn(/^Hidden/));
			const put = deferred();
			restore.mockImplementationOnce(() => put.promise);
			await click(btn('Restore', host.querySelector('[data-ref="TASK-1"]')!));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			list.mockImplementationOnce(() => Promise.resolve(resp([row('a', 'TASK-3')], { hidden: 1 })));
			reload.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { hidden: 2 }));
			await settle();
			expect(lastCall()).toEqual({ sort: 'importance', view: 'hidden' });
			expect(refs()).toEqual(['TASK-3']);
			expect(groupCounts()).toEqual([['WS a', '1']]);
			expect(heading()).toContain('1');
			expect(heading()).not.toContain('2');
			list.mockResolvedValueOnce(resp([row('a', 'TASK-1')], { hidden: 1 }));
			await click(btn(/^Back to needs you/));
			expect(hiddenN()).toContain('(1)');
		});

		it('row 3: a stale first-page response whose counts include a filtered row (two workspaces; one group empties)', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('b', 'TASK-2')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			list.mockImplementationOnce(() => Promise.resolve(resp([row('b', 'TASK-2')], { hidden: 1 })));
			reload.resolve(resp([row('a', 'TASK-1'), row('b', 'TASK-2')], { hidden: 0 }));
			await settle();
			expect(refs()).toEqual(['TASK-2']);
			expect(groupCounts()).toEqual([['WS b', '1']]);
			expect(heading()).toContain('1');
			expect(heading()).not.toContain('2');
			expect(hiddenN()).toContain('(1)');
		});

		it('row 4: the acted row is on page 2 of the stale response; the refetch corrects the counts', async () => {
			await mountWith(
				resp([row('a', 'TASK-1'), row('a', 'TASK-3')], {
					total: 4,
					workspaces: [{ slug: 'a', name: 'WS a', owner_username: 'dave', count: 4 }]
				})
			);
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			list.mockImplementationOnce(() =>
				Promise.resolve(resp([row('a', 'TASK-3'), row('a', 'TASK-4'), row('a', 'TASK-5')], { hidden: 1 }))
			);
			// Row 1 sits on page 2 of this stale response, so nothing is filtered.
			reload.resolve(
				resp([row('a', 'TASK-3'), row('a', 'TASK-4'), row('a', 'TASK-5')], {
					total: 4,
					hidden: 0,
					next_cursor: 'p2',
					workspaces: [{ slug: 'a', name: 'WS a', owner_username: 'dave', count: 4 }]
				})
			);
			await settle();
			expect(list).toHaveBeenCalledTimes(3);
			expect(refs()).toEqual(['TASK-3', 'TASK-4', 'TASK-5']);
			expect(groupCounts()).toEqual([['WS a', '3']]);
			expect(heading()).toContain('3');
			expect(heading()).not.toContain('4');
			expect(hiddenN()).toContain('(1)');
		});

		it('row 5: a stale DESTINATION response omits the acted row; the refetch shows it and Hidden (N) is right', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			const toHidden = deferred();
			list.mockImplementationOnce(() => toHidden.promise);
			await click(btn(/^Hidden/));
			// The PUT completing while the destination load is in flight takes the
			// H-F2 refetch (the stale load is then superseded and writes nothing).
			list.mockImplementationOnce(() =>
				Promise.resolve(resp([row('a', 'TASK-1', { snooze: { kind: 'dismiss' } })], { hidden: 1 }))
			);
			put.resolve({});
			await settle();
			toHidden.resolve(resp([], { hidden: 0 }));
			await settle();
			expect(list).toHaveBeenCalledTimes(3);
			expect(lastCall()).toEqual({ sort: 'age', view: 'hidden' });
			expect(refs()).toEqual(['TASK-1']);
			expect(groupCounts()).toEqual([['WS a', '1']]);
			expect(heading()).toContain('1');
			list.mockResolvedValueOnce(resp([row('a', 'TASK-3')], { hidden: 1 }));
			await click(btn(/^Back to needs you/));
			expect(refs()).toEqual(['TASK-3']);
			expect(hiddenN()).toContain('(1)');
		});

		it('row 6: a stale Load more page brings an acted row back; it stays filtered and page 1 is reloaded', async () => {
			await mountWith(
				resp([row('a', 'TASK-1'), row('a', 'TASK-3')], {
					total: 3,
					next_cursor: 'c1',
					workspaces: [{ slug: 'a', name: 'WS a', owner_username: 'dave', count: 3 }]
				})
			);
			const more = deferred();
			list.mockImplementationOnce(() => more.promise);
			await click(btn('Load more'));
			snooze.mockResolvedValue({});
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			expect(refs()).toEqual(['TASK-3']);
			list.mockImplementationOnce(() =>
				Promise.resolve(resp([row('a', 'TASK-3'), row('a', 'TASK-2')], { hidden: 1 }))
			);
			more.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-2')], { total: 3, hidden: 0 }));
			await settle();
			expect(list).toHaveBeenCalledTimes(3);
			// The refetch is page 1: no cursor.
			expect(lastCall()).toEqual({ sort: 'age', view: 'active' });
			expect(refs()).toEqual(['TASK-3', 'TASK-2']);
			expect(groupCounts()).toEqual([['WS a', '2']]);
			expect(heading()).toContain('2');
			expect(hiddenN()).toContain('(1)');
		});

		it('row 7: a stale Load more in the destination view omits the row; the refetch shows it', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			// The hidden view loads before the PUT commits: it lacks TASK-1.
			list.mockResolvedValueOnce(
				resp([row('a', 'TASK-7', { snooze: { kind: 'dismiss' } })], { hidden: 1, next_cursor: 'h1' })
			);
			await click(btn(/^Hidden/));
			expect(refs()).toEqual(['TASK-7']);
			const more = deferred();
			list.mockImplementationOnce(() => more.promise);
			await click(btn('Load more'));
			list.mockImplementationOnce(() =>
				Promise.resolve(
					resp(
						[row('a', 'TASK-1', { snooze: { kind: 'dismiss' } }), row('a', 'TASK-7', { snooze: { kind: 'dismiss' } })],
						{ hidden: 2 }
					)
				)
			);
			put.resolve({});
			await settle();
			more.resolve(resp([row('a', 'TASK-8', { snooze: { kind: 'dismiss' } })], { hidden: 1 }));
			await settle();
			expect(lastCall()).toEqual({ sort: 'age', view: 'hidden' });
			expect(refs()).toEqual(['TASK-1', 'TASK-7']);
			expect(groupCounts()).toEqual([['WS a', '2']]);
			expect(heading()).toContain('2');
			list.mockResolvedValueOnce(resp([row('a', 'TASK-3')], { hidden: 2 }));
			await click(btn(/^Back to needs you/));
			expect(hiddenN()).toContain('(2)');
		});

		it('row 8: the error path clears degraded, rows, total and workspaces (Hidden stays)', async () => {
			await mountWith(resp([row('alpha', 'TASK-1')], { degraded: ['alpha'], hidden: 4 }));
			expect(host.querySelector('[data-testid="needs-degraded"]')).not.toBeNull();
			list.mockRejectedValueOnce(new Error('nope'));
			await click(btn('Importance'));
			expect(host.querySelector('[role="alert"]')?.textContent).toContain('nope');
			expect(host.querySelector('[data-testid="needs-degraded"]')).toBeNull();
			expect(refs()).toEqual([]);
			expect(groupCounts()).toEqual([]);
			expect(heading()).not.toContain('1');
			expect(hiddenN()).toContain('(4)');
		});

		async function staleSetup() {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			return reload;
		}

		it('(a) no loop: one stale response leads to exactly one refetch', async () => {
			const reload = await staleSetup();
			expect(list).toHaveBeenCalledTimes(2);
			let after = 0;
			// A loop would keep asking; cap it so a regression fails instead of hanging.
			list.mockImplementation(() => (++after > 5 ? never() : Promise.resolve(resp([row('a', 'TASK-3')], { hidden: 1 }))));
			reload.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { hidden: 0 }));
			for (let i = 0; i < 5; i++) await settle();
			expect(list).toHaveBeenCalledTimes(3);
			expect(refs()).toEqual(['TASK-3']);
		});

		it('(c) a sort toggle during the refetch supersedes it, and the superseded refetch writes no state', async () => {
			const reload = await staleSetup();
			const refetch = deferred();
			list.mockImplementationOnce(() => refetch.promise);
			reload.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { hidden: 0 }));
			await settle();
			expect(list).toHaveBeenCalledTimes(3);
			list.mockImplementationOnce(() => Promise.resolve(resp([row('a', 'TASK-9')], { hidden: 1 })));
			await click(btn('Simplicity'));
			expect(lastCall()).toEqual({ sort: 'simplicity', view: 'active' });
			expect(refs()).toEqual(['TASK-9']);
			refetch.resolve(resp([row('a', 'TASK-3')], { total: 5, hidden: 7 }));
			await settle();
			expect(refs()).toEqual(['TASK-9']);
			expect(heading()).not.toContain('5');
			expect(hiddenN()).toContain('(1)');
			expect(list).toHaveBeenCalledTimes(4);
		});

		it('no double count: settle adjusted the row, and a reload started after the PUT is used as-is, with no refetch', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			snooze.mockResolvedValue({});
			await click(btn('Dismiss', host.querySelector('[data-ref="TASK-1"]')!));
			expect(groupCounts()).toEqual([['WS a', '1']]);
			list.mockResolvedValueOnce(resp([row('a', 'TASK-3')], { hidden: 1 }));
			await click(btn('Importance'));
			expect(list).toHaveBeenCalledTimes(2);
			expect(groupCounts()).toEqual([['WS a', '1']]);
			expect(heading()).toContain('1');
			expect(hiddenN()).toContain('(1)');
		});
	});

	it('a failed reload clears the previous total next to the error', async () => {
		await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-2'), row('a', 'TASK-3')]));
		expect(host.querySelector('h1')?.textContent).toContain('3');
		list.mockRejectedValueOnce(new Error('nope'));
		await click(btn('Importance'));
		expect(host.querySelector('[role="alert"]')?.textContent).toContain('nope');
		expect(host.querySelector('h1')?.textContent).not.toContain('3');
	});

	describe('defer menus', () => {
		async function twoRows() {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-2')]));
		}
		const deferBtn = (ref: string) => btn(/^Defer$/, host.querySelector(`[data-ref="${ref}"]`)!);

		it('a press outside the menu closes it, a press inside does not', async () => {
			await twoRows();
			await click(deferBtn('TASK-1'));
			const menu = host.querySelector('.defer-menu')!;
			menu.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
			await settle();
			expect(host.querySelector('.defer-menu')).not.toBeNull();
			host.querySelector('h2')!.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
			await settle();
			expect(host.querySelector('.defer-menu')).toBeNull();
		});

		it('opening a second row menu closes the first, and one Escape leaves nothing open', async () => {
			await twoRows();
			await click(deferBtn('TASK-1'));
			await click(deferBtn('TASK-2'));
			const menus = host.querySelectorAll('.defer-menu');
			expect(menus.length).toBe(1);
			expect(host.querySelector('[data-ref="TASK-2"] .defer-menu')).not.toBeNull();
			menus[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
			await settle();
			expect(host.querySelectorAll('.defer-menu').length).toBe(0);
		});
	});
});

describe('needs page: r4 (empty state vs the cursor; a settle that did not apply)', () => {
	const deferred = () => {
		let resolve!: (v: unknown) => void;
		const promise = new Promise((r) => (resolve = r));
		return { promise, resolve };
	};
	const heading = () => host.querySelector('h1')?.textContent ?? '';
	const hiddenN = () => btn(/^Hidden/).textContent ?? '';
	const empty = () => host.querySelector('[data-testid="needs-empty"]');
	const never = () => new Promise(() => {});
	const inRow = (ref: string) => host.querySelector(`[data-ref="${ref}"]`)! as HTMLElement;
	const held = (extra: Record<string, unknown> = {}) => ({ snooze: { kind: 'dismiss' }, ...extra });
	// A mock that fails instead of hanging if something keeps asking.
	const bounded = (after: number, make: () => unknown) => {
		let n = 0;
		list.mockImplementation(() => (++n > after ? never() : Promise.resolve(make())));
	};
	async function drain() {
		for (let i = 0; i < 5; i++) await settle();
	}

	describe('H-F1: the empty state needs no rows AND no cursor', () => {
		it('dismissing every loaded row with a cursor set reloads page 1; the empty text never shows', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { total: 4, next_cursor: 'c1' }));
			snooze.mockResolvedValue({});
			await click(btn('Dismiss', inRow('TASK-1')));
			expect(empty()).toBeNull();
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Dismiss', inRow('TASK-3')));
			// In flight: a loading state, not the empty text.
			expect(empty()).toBeNull();
			expect(host.textContent).toContain('Loading');
			expect(list).toHaveBeenCalledTimes(2);
			expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'active' });
			reload.resolve(resp([row('a', 'TASK-5'), row('a', 'TASK-6')], { total: 2, hidden: 2 }));
			await drain();
			expect(list).toHaveBeenCalledTimes(2);
			expect(empty()).toBeNull();
			expect(refs()).toEqual(['TASK-5', 'TASK-6']);
			expect(groupCounts()).toEqual([['WS a', '2']]);
			expect(heading()).toContain('2');
			expect(hiddenN()).toContain('(2)');
		});

		it('with NO cursor, dismissing the last row shows the empty text and issues no reload', async () => {
			await mountWith(resp([row('a', 'TASK-1')]));
			snooze.mockResolvedValue({});
			await click(btn('Dismiss', inRow('TASK-1')));
			await drain();
			expect(list).toHaveBeenCalledTimes(1);
			expect(empty()?.textContent).toContain('Nothing needs you');
		});

		it('hidden view mirror: restoring every loaded row with a cursor set reloads page 1, Load more stays reachable', async () => {
			await mountWith(resp([row('a', 'TASK-3')], { hidden: 2 }));
			list.mockResolvedValueOnce(
				resp([row('a', 'TASK-7', held()), row('a', 'TASK-8', held())], { hidden: 3, next_cursor: 'h1' })
			);
			await click(btn(/^Hidden/));
			restore.mockResolvedValue({});
			await click(btn('Restore', inRow('TASK-7')));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Restore', inRow('TASK-8')));
			expect(empty()).toBeNull();
			expect(list).toHaveBeenCalledTimes(3);
			expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'hidden' });
			reload.resolve(resp([row('a', 'TASK-9', held())], { hidden: 1 }));
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			expect(empty()).toBeNull();
			expect(refs()).toEqual(['TASK-9']);
			expect(groupCounts()).toEqual([['WS a', '1']]);
			expect(heading()).toContain('1');
		});
	});

	describe('H-F2 (population row 9): a list that started and landed while the PUT was in flight', () => {
		it('dismiss X, toggle to Hidden, the hidden list resolves BEFORE the PUT (lacking X): the refetch shows X and Hidden (1) is right', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', inRow('TASK-1')));
			list.mockResolvedValueOnce(resp([], { hidden: 0 }));
			await click(btn(/^Hidden/));
			expect(refs()).toEqual([]);
			list.mockResolvedValueOnce(resp([row('a', 'TASK-1', held())], { hidden: 1 }));
			put.resolve({});
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'hidden' });
			expect(refs()).toEqual(['TASK-1']);
			expect(groupCounts()).toEqual([['WS a', '1']]);
			expect(heading()).toContain('1');
			list.mockResolvedValueOnce(resp([row('a', 'TASK-3')], { hidden: 1 }));
			await click(btn(/^Back to needs you/));
			expect(refs()).toEqual(['TASK-3']);
			expect(hiddenN()).toContain('(1)');
		});

		it('Restore mirror: the active list resolves BEFORE the restore PUT (lacking the row): the refetch shows it', async () => {
			await mountWith(resp([row('a', 'TASK-3')], { hidden: 2 }));
			list.mockResolvedValueOnce(resp([row('a', 'TASK-7', held()), row('a', 'TASK-8', held())], { hidden: 2 }));
			await click(btn(/^Hidden/));
			const put = deferred();
			restore.mockImplementationOnce(() => put.promise);
			await click(btn('Restore', inRow('TASK-7')));
			list.mockResolvedValueOnce(resp([row('a', 'TASK-3')], { hidden: 2 }));
			await click(btn(/^Back to needs you/));
			expect(refs()).toEqual(['TASK-3']);
			list.mockResolvedValueOnce(resp([row('a', 'TASK-3'), row('a', 'TASK-7')], { hidden: 1 }));
			put.resolve({});
			await drain();
			expect(list).toHaveBeenCalledTimes(4);
			expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'active' });
			expect(refs()).toEqual(['TASK-3', 'TASK-7']);
			expect(groupCounts()).toEqual([['WS a', '2']]);
			expect(heading()).toContain('2');
			expect(hiddenN()).toContain('(1)');
		});
	});

	describe('(a) extended: one action completion yields at most one refetch', () => {
		it('(i) the r3 accept-time path alone', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', inRow('TASK-1')));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			expect(list).toHaveBeenCalledTimes(2);
			bounded(5, () => resp([row('a', 'TASK-3')], { hidden: 1 }));
			reload.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			expect(refs()).toEqual(['TASK-3']);
		});

		it('(ii) the H-F2 path alone', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', inRow('TASK-1')));
			list.mockResolvedValueOnce(resp([], { hidden: 0 }));
			await click(btn(/^Hidden/));
			bounded(5, () => resp([row('a', 'TASK-1', held())], { hidden: 1 }));
			put.resolve({});
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			expect(refs()).toEqual(['TASK-1']);
		});

		it('(iii) both: a stale response lands while the H-F2 refetch is pending; still one refetch', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', inRow('TASK-1')));
			const toHidden = deferred();
			list.mockImplementationOnce(() => toHidden.promise);
			await click(btn(/^Hidden/));
			const refetch = deferred();
			list.mockImplementationOnce(() => refetch.promise);
			put.resolve({});
			await settle();
			expect(list).toHaveBeenCalledTimes(3);
			bounded(0, () => resp([]));
			toHidden.resolve(resp([], { hidden: 0 }));
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			refetch.resolve(resp([row('a', 'TASK-1', held())], { hidden: 1 }));
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			expect(refs()).toEqual(['TASK-1']);
		});

		it('(iv) H-F1 together with the r3 path: a stale page filtered to nothing with a cursor refetches once', async () => {
			await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')], { total: 3, next_cursor: 'c1' }));
			const put = deferred();
			snooze.mockImplementationOnce(() => put.promise);
			await click(btn('Dismiss', inRow('TASK-1')));
			const reload = deferred();
			list.mockImplementationOnce(() => reload.promise);
			await click(btn('Importance'));
			put.resolve({});
			await settle();
			// Settled with TASK-3 left: no refetch yet.
			expect(list).toHaveBeenCalledTimes(2);
			bounded(5, () => resp([row('a', 'TASK-5')], { total: 1, hidden: 1 }));
			// The stale page holds only the dismissed row and a cursor: the display
			// filter empties it, so both H-F1 and r3 want a refetch.
			reload.resolve(resp([row('a', 'TASK-1')], { total: 3, next_cursor: 'c2' }));
			await drain();
			expect(list).toHaveBeenCalledTimes(3);
			expect(refs()).toEqual(['TASK-5']);
			expect(empty()).toBeNull();
		});
	});
});

describe('needs page: r5 (a quiet automatic refetch)', () => {
	const deferred = () => {
		let resolve!: (v: unknown) => void;
		let reject!: (e: unknown) => void;
		const promise = new Promise((res, rej) => {
			resolve = res;
			reject = rej;
		});
		return { promise, resolve, reject };
	};
	const inRow = (ref: string) => host.querySelector(`[data-ref="${ref}"]`)! as HTMLElement;
	const actionErr = () => host.querySelector('[data-testid="needs-action-error"]');
	const heading = () => host.querySelector('h1')?.textContent ?? '';
	const alerts = () => [...host.querySelectorAll('[role="alert"]')].map((a) => a.textContent?.trim());

	// Rows 1, 3, 5 with a cursor. A Load more is in flight when TASK-1's dismiss
	// completes, so the Load more lands stale and requests ONE quiet refetch,
	// which stays pending (`refetch`). Returns it, with the PUTs for further use.
	async function pendingRefetch() {
		await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3'), row('a', 'TASK-5')], { total: 5, hidden: 0, next_cursor: 'c1' }));
		const more = deferred();
		list.mockImplementationOnce(() => more.promise);
		await click(btn('Load more'));
		snooze.mockResolvedValueOnce({});
		await click(btn('Dismiss', inRow('TASK-1')));
		const refetch = deferred();
		list.mockImplementationOnce(() => refetch.promise);
		more.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-7')], { total: 5, next_cursor: 'c2' }));
		await settle();
		expect(list).toHaveBeenCalledTimes(3);
		expect(list).toHaveBeenLastCalledWith({ sort: 'age', view: 'active' });
		return refetch;
	}

	it('a quiet refetch shows no Loading over the list and does not close an open defer menu', async () => {
		const refetch = await pendingRefetch();
		expect(host.textContent).not.toContain('Loading');
		expect(refs()).toEqual(['TASK-3', 'TASK-5', 'TASK-7']);
		await click(btn('Defer', inRow('TASK-3')));
		expect(btn('Defer', inRow('TASK-3')).getAttribute('aria-expanded')).toBe('true');
		refetch.resolve(resp([row('a', 'TASK-3'), row('a', 'TASK-5'), row('a', 'TASK-7')], { total: 3, hidden: 1 }));
		await settle();
		expect(refs()).toEqual(['TASK-3', 'TASK-5', 'TASK-7']);
		expect(heading()).toContain('3');
		expect(host.textContent).not.toContain('Loading');
		// Still the same open menu after the response landed.
		expect(btn('Defer', inRow('TASK-3')).getAttribute('aria-expanded')).toBe('true');
	});

	it('a quiet refetch does not clear an action error', async () => {
		await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3'), row('a', 'TASK-5')], { total: 5, next_cursor: 'c1' }));
		const more = deferred();
		list.mockImplementationOnce(() => more.promise);
		await click(btn('Load more'));
		const putA = deferred();
		snooze.mockImplementationOnce(() => putA.promise);
		await click(btn('Dismiss', inRow('TASK-1')));
		snooze.mockRejectedValueOnce(new Error('B failed'));
		await click(btn('Dismiss', inRow('TASK-3')));
		expect(actionErr()?.textContent).toContain('B failed');
		putA.resolve({});
		await settle();
		const refetch = deferred();
		list.mockImplementationOnce(() => refetch.promise);
		more.resolve(resp([row('a', 'TASK-1'), row('a', 'TASK-7')], { total: 5 }));
		await settle();
		expect(list).toHaveBeenCalledTimes(3);
		expect(actionErr()?.textContent).toContain('B failed');
		refetch.resolve(resp([row('a', 'TASK-3'), row('a', 'TASK-5')], { total: 2, hidden: 1 }));
		await settle();
		expect(refs()).toEqual(['TASK-3', 'TASK-5']);
		expect(actionErr()?.textContent).toContain('B failed');
	});

	it('a FAILED quiet refetch keeps the list and counts and sets neither loadError nor actionError', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const refetch = await pendingRefetch();
		const before = { refs: refs(), counts: groupCounts(), head: heading(), hid: btn(/^Hidden/).textContent, cursor: !!btn('Load more') };
		refetch.reject(new Error('boom'));
		await settle();
		expect(refs()).toEqual(before.refs);
		expect(groupCounts()).toEqual(before.counts);
		expect(heading()).toBe(before.head);
		expect(before.cursor).toBe(true);
		expect(btn('Load more')).toBeTruthy();
		expect(btn(/^Hidden/).textContent).toBe(before.hid);
		expect(alerts()).toEqual([]);
		expect(actionErr()).toBeNull();
		expect(host.textContent).not.toContain('boom');
		expect(host.textContent).not.toContain('Loading');
		expect(list).toHaveBeenCalledTimes(3);
		expect(warn).toHaveBeenCalledTimes(1);
		warn.mockRestore();
	});

	it('a user-initiated toggle still shows Loading and clears the errors', async () => {
		await mountWith(resp([row('a', 'TASK-1'), row('a', 'TASK-3')]));
		snooze.mockRejectedValueOnce(new Error('nope'));
		await click(btn('Dismiss', inRow('TASK-1')));
		expect(actionErr()?.textContent).toContain('nope');
		const next = deferred();
		list.mockImplementationOnce(() => next.promise);
		await click(btn('Importance'));
		expect(host.textContent).toContain('Loading');
		expect(refs()).toEqual([]);
		expect(actionErr()).toBeNull();
		next.resolve(resp([row('a', 'TASK-3')]));
		await settle();
		expect(refs()).toEqual(['TASK-3']);
		expect(host.textContent).not.toContain('Loading');
	});
});
