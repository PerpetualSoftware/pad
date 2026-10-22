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
