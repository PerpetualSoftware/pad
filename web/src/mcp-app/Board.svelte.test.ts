// Component tests for the MCP Apps board. The host bridge is replaced by a fake
// `connect`, so no SDK and no host are involved.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import Board from './Board.svelte';
import source from './Board.svelte?raw';
import type { Bridge, HostContext } from './bridge';
import type { ToolResultLike } from './model';
import type { BoardItem } from './boardModel';

type Handlers = {
	onToolInput(args: Record<string, unknown>): void;
	onToolResult(result: ToolResultLike): void;
	onHostContext?(ctx: HostContext): void;
};
type Responder = (tool: string, args: Record<string, unknown>) => ToolResultLike | Promise<ToolResultLike>;

const card = (id: string, status: string, over: Partial<BoardItem> = {}): BoardItem => ({
	id, ref: `TASK-${id}`, title: `Title ${id}`, status, priority: '', assignee: '', parent_ref: '', seq: 7,
	updated_at: '2026-11-12T10:00:00Z', ...over
});
const snapshot = (over: Record<string, unknown> = {}) => ({
	workspace: 'demo', collection: 'tasks', truncated: false, unplaced: 0, cursor: 'c1', access_epoch: 'e1',
	columns: [
		{ status: 'open', terminal: false, abandoned: false, total: 2, items: [
			card('1', 'open', { priority: 'high', assignee: 'Ada' }), card('2', 'open')
		] },
		{ status: 'in-progress', terminal: false, abandoned: false, total: 1, items: [card('3', 'in-progress')] },
		{ status: 'done', terminal: true, abandoned: false, total: 0, items: [] },
		{ status: 'cancelled', terminal: true, abandoned: true, total: 0, items: [] }
	],
	...over
});
const ok = (structuredContent: unknown): ToolResultLike => ({ structuredContent });
const fail = (code: string, message: string): ToolResultLike => ({ isError: true, structuredContent: { error: { code, message } } });
const noChanges = (over: Record<string, unknown> = {}) =>
	ok({ upserts: [], removed_ids: [], cursor: 'c2', access_epoch: 'e1', resync: false, ...over });
// What pad_item answers with: the full item, status inside `fields`.
const fullItem = (id: string, status: string, seq: number, over: Record<string, unknown> = {}) => ({
	id, ref: `TASK-${id}`, title: `Title ${id}`, seq, collection_slug: 'tasks', updated_at: '2026-11-13T09:00:00Z',
	content: 'BODY-MUST-NOT-SHOW', fields: { status }, ...over
});
const SHOW_ARGS = { action: 'show', workspace: 'demo', collection: 'tasks' };

function setup(respond: Responder = () => noChanges()) {
	let handlers!: Handlers;
	const call = vi.fn(async (tool: string, args: Record<string, unknown>) => respond(tool, args));
	const connect = async (h: Handlers): Promise<Bridge> => { handlers = h; return { call }; };
	const view = render(Board, { props: { connect } });
	const callsTo = (tool: string, action: string) =>
		call.mock.calls.filter(([t, a]) => t === tool && a.action === action).map(([, a]) => a);
	return { ...view, call, callsTo, handlers: () => handlers };
}
type Ctx = ReturnType<typeof setup>;

/** Lets resolved bridge calls and the renders they cause settle, under real or fake timers. */
async function settle() {
	for (let i = 0; i < 8; i++) await Promise.resolve();
	if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(0);
	for (let i = 0; i < 8; i++) await Promise.resolve();
}
async function show(ctx: Ctx, result: ToolResultLike = ok(snapshot()), args: Record<string, unknown> = SHOW_ARGS, host: HostContext = {}) {
	await settle();
	ctx.handlers().onHostContext?.(host);
	ctx.handlers().onToolInput(args);
	ctx.handlers().onToolResult(result);
	await settle();
}
const column = (name: string) => screen.getByRole('region', { name: new RegExp(`^${name},`) });
const titlesIn = (name: string) => within(column(name)).queryAllByRole('listitem').map(li => li.querySelector('.item-title')?.textContent);
const columnLabels = (ctx: Ctx) => Array.from(ctx.container.querySelectorAll('.columns > section')).map(el => el.getAttribute('aria-label'));
const statusOf = (ref: string) => screen.getByLabelText(`Status of ${ref}`) as HTMLSelectElement;

function setVisibility(state: 'visible' | 'hidden') {
	Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state });
	document.dispatchEvent(new Event('visibilitychange'));
}

afterEach(() => {
	vi.useRealTimers();
	setVisibility('visible');
});

describe('Board', () => {
	it('renders the columns in order with totals and each item summary', async () => {
		const ctx = setup();
		await show(ctx);
		expect(screen.getByRole('region', { name: 'Tasks board' })).toBeInTheDocument();
		expect(columnLabels(ctx)).toEqual(['open, 2 items', 'in-progress, 1 item', 'done, 0 items, closed', 'cancelled, 0 items, closed without delivering']);
		expect(screen.getAllByRole('heading', { level: 3 }).map(h => h.textContent)).toEqual(['open', 'in-progress', 'done', 'cancelled']);
		const open = within(column('open'));
		expect(open.getByText('TASK-1')).toBeInTheDocument();
		expect(open.getByText('Title 1')).toBeInTheDocument();
		expect(open.getByText('high')).toBeInTheDocument();
		expect(open.getByText('Ada')).toBeInTheDocument();
		expect(column('done').classList.contains('terminal')).toBe(true);
		expect(column('done').classList.contains('abandoned')).toBe(false);
		expect(column('cancelled').classList.contains('abandoned')).toBe(true);
		expect(column('open').classList.contains('terminal')).toBe(false);
		// The status control offers exactly the column statuses.
		expect(Array.from(statusOf('TASK-1').options).map(o => o.value)).toEqual(['open', 'in-progress', 'done', 'cancelled']);
		expect(statusOf('TASK-3').value).toBe('in-progress');
		expect(ctx.call).not.toHaveBeenCalled();
	});

	it('says "showing N of M" for a column holding fewer items than its total, and notes unplaced items', async () => {
		const ctx = setup();
		const board = snapshot({ truncated: true, unplaced: 1 });
		(board.columns as { total: number }[])[0].total = 57;
		await show(ctx, ok(board));
		expect(within(column('open')).getByText('showing 2 of 57')).toBeInTheDocument();
		expect(within(column('in-progress')).queryByText(/showing/)).toBeNull();
		expect(screen.getByText(/1 item has a status that is not a column/)).toBeInTheDocument();
	});

	it('renders titles as text, never as markup, and survives repeated columns and ids', async () => {
		const ctx = setup();
		const board = snapshot();
		board.columns[0].items = [card('1', 'open', { title: '<img src=x onerror=alert(1)>' }), card('1', 'open'), card('2', 'open')];
		board.columns.push({ ...board.columns[0], items: [card('9', 'open')] });
		await show(ctx, ok(board));
		expect(screen.getByText('<img src=x onerror=alert(1)>')).toBeInTheDocument();
		expect(ctx.container.querySelector('img')).toBeNull();
		expect(columnLabels(ctx)).toHaveLength(4);
		expect(within(column('open')).getAllByRole('listitem')).toHaveLength(2);
	});

	it('renders nothing before any tool data, and nothing for a changes result', async () => {
		const ctx = setup();
		await settle();
		expect(ctx.container.querySelector('*')).toBeNull();
		await show(ctx, noChanges(), { action: 'changes', workspace: 'demo', collection: 'tasks', since: 'c1', access_epoch: 'e1' });
		expect(ctx.container.querySelector('*')).toBeNull();
		expect(ctx.call).not.toHaveBeenCalled();
	});

	it('shows the message of an error result, and "Collection not found" for not_found', async () => {
		const ctx = setup();
		await show(ctx, fail('validation_failed', 'Collection has no status select field'));
		expect(screen.getByRole('alert')).toHaveTextContent('Collection has no status select field');
		expect(screen.queryByRole('region')).toBeNull();
		await show(ctx, fail('not_found', 'pad board show: 404'));
		expect(screen.getByRole('alert')).toHaveTextContent('Collection not found');
		expect(screen.queryByText(/404/)).toBeNull();
	});

	it('does not poll inline; Refresh re-runs show with the same arguments', async () => {
		vi.useFakeTimers();
		const fresh = snapshot({ cursor: 'c9' });
		fresh.columns[1].items = [card('3', 'in-progress', { title: 'Renamed' })];
		const ctx = setup((_tool, args) => args.action === 'show' ? ok(fresh) : noChanges());
		await show(ctx, ok(snapshot()), { ...SHOW_ARGS, parent: 'PLAN-4' }, { displayMode: 'inline' });
		await vi.advanceTimersByTimeAsync(300_000);
		expect(ctx.call).not.toHaveBeenCalled();

		await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
		await settle();
		expect(ctx.call.mock.calls).toEqual([['pad_board', { action: 'show', workspace: 'demo', collection: 'tasks', parent: 'PLAN-4' }]]);
		expect(titlesIn('in-progress')).toEqual(['Renamed']);
	});

	it('polls only while fullscreen and visible, and stops when hidden or destroyed', async () => {
		vi.useFakeTimers();
		const ctx = setup();
		await show(ctx, ok(snapshot()), { ...SHOW_ARGS, parent: 'PLAN-4' }, { displayMode: 'inline' });
		await vi.advanceTimersByTimeAsync(5_000);
		expect(ctx.call).not.toHaveBeenCalled();

		ctx.handlers().onHostContext?.({ displayMode: 'fullscreen' });
		await vi.advanceTimersByTimeAsync(4_999);
		expect(ctx.call).not.toHaveBeenCalled();
		await vi.advanceTimersByTimeAsync(1);
		// Same workspace, collection and parent as show, plus the snapshot cursor and epoch.
		expect(ctx.call.mock.calls).toEqual([['pad_board', { action: 'changes', workspace: 'demo', collection: 'tasks', parent: 'PLAN-4', since: 'c1', access_epoch: 'e1' }]]);
		expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();

		await vi.advanceTimersByTimeAsync(10_000);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(2);
		// The cursor of the previous answer is the next `since`.
		expect(ctx.callsTo('pad_board', 'changes')[1]).toMatchObject({ since: 'c2', access_epoch: 'e1' });

		setVisibility('hidden');
		await vi.advanceTimersByTimeAsync(120_000);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(2);

		// Back in view after a long gap: one catch-up poll at once, then the usual pace.
		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(3);
		await vi.advanceTimersByTimeAsync(9_999);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(3);

		ctx.handlers().onHostContext?.({ displayMode: 'inline' });
		await vi.advanceTimersByTimeAsync(60_000);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(3);

		ctx.handlers().onHostContext?.({ displayMode: 'fullscreen' });
		await vi.advanceTimersByTimeAsync(0);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(4);
		ctx.unmount();
		await vi.advanceTimersByTimeAsync(120_000);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(4);
		expect(vi.getTimerCount()).toBe(0);
	});

	it('applies deltas from a poll', async () => {
		vi.useFakeTimers();
		const ctx = setup(() => noChanges({ upserts: [card('1', 'done', { seq: 8, updated_at: '2026-11-13T09:00:00Z' }), card('4', 'open')], removed_ids: ['2'] }));
		await show(ctx, ok(snapshot()), SHOW_ARGS, { displayMode: 'fullscreen' });
		await vi.advanceTimersByTimeAsync(10_000);
		expect(titlesIn('open')).toEqual(['Title 4']);
		expect(titlesIn('done')).toEqual(['Title 1']);
		expect(column('done').getAttribute('aria-label')).toBe('done, 1 item, closed');
		expect(ctx.callsTo('pad_board', 'show')).toEqual([]);
	});

	it('backs off after a rate-limit error and returns to the usual pace after a success', async () => {
		vi.useFakeTimers();
		let limited = true;
		const ctx = setup(() => limited ? fail('rate_limited', 'pad board changes rate-limited (HTTP 429)') : noChanges());
		await show(ctx, ok(snapshot()), SHOW_ARGS, { displayMode: 'fullscreen' });
		const polls = () => ctx.callsTo('pad_board', 'changes').length;
		await vi.advanceTimersByTimeAsync(10_000);
		expect(polls()).toBe(1);
		await vi.advanceTimersByTimeAsync(19_999);
		expect(polls()).toBe(1);
		await vi.advanceTimersByTimeAsync(1);
		expect(polls()).toBe(2);
		// The board stays up, and a rate limit never triggers a re-show.
		expect(titlesIn('open')).toEqual(['Title 1', 'Title 2']);
		expect(ctx.callsTo('pad_board', 'show')).toEqual([]);
		limited = false;
		await vi.advanceTimersByTimeAsync(40_000);
		expect(polls()).toBe(3);
		await vi.advanceTimersByTimeAsync(10_000);
		expect(polls()).toBe(4);
	});

	it('re-runs show on resync and replaces the board', async () => {
		vi.useFakeTimers();
		const fresh = snapshot({ cursor: 'c7', access_epoch: 'e2' });
		fresh.columns[0] = { ...fresh.columns[0], total: 1, items: [card('8', 'open')] };
		const ctx = setup((_tool, args) => {
			if (args.action === 'show') return ok(fresh);
			// The delta that comes with a resync is not applied: show is the truth.
			return args.since === 'c1' ? noChanges({ resync: true, access_epoch: 'e2', upserts: [card('5', 'open')] }) : noChanges({ cursor: 'c8', access_epoch: 'e2' });
		});
		await show(ctx, ok(snapshot()), SHOW_ARGS, { displayMode: 'fullscreen' });
		await vi.advanceTimersByTimeAsync(10_000);
		expect(ctx.callsTo('pad_board', 'show')).toEqual([SHOW_ARGS]);
		expect(titlesIn('open')).toEqual(['Title 8']);
		await vi.advanceTimersByTimeAsync(10_000);
		expect(ctx.callsTo('pad_board', 'changes')[1]).toMatchObject({ since: 'c7', access_epoch: 'e2' });
		expect(ctx.callsTo('pad_board', 'show')).toHaveLength(1);
	});

	it('takes the board down when a live reload finds the collection gone', async () => {
		vi.useFakeTimers();
		const ctx = setup((_tool, args) => args.action === 'show' ? fail('not_found', 'Collection not found') : noChanges({ resync: true }));
		await show(ctx, ok(snapshot()), SHOW_ARGS, { displayMode: 'fullscreen' });
		await vi.advanceTimersByTimeAsync(10_000);
		expect(screen.getByRole('alert')).toHaveTextContent('Collection not found');
		expect(screen.queryByRole('region')).toBeNull();
		await vi.advanceTimersByTimeAsync(120_000);
		expect(ctx.callsTo('pad_board', 'changes')).toHaveLength(1);
	});

	it('sends a status change as exactly one pad_item update with expected_seq and no content', async () => {
		let release!: (result: ToolResultLike) => void;
		const ctx = setup(() => new Promise<ToolResultLike>(resolve => { release = resolve; }));
		await show(ctx);
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		await settle();

		expect(ctx.call.mock.calls).toEqual([['pad_item', { action: 'update', ref: 'TASK-1', expected_seq: 7, workspace: 'demo', field: ['status=done'] }]]);
		expect(ctx.call.mock.calls[0][1]).not.toHaveProperty('content');
		expect(ctx.call.mock.calls[0][1]).not.toHaveProperty('title');
		// Optimistic move, saving state, and that item's control locked while others stay usable.
		expect(titlesIn('open')).toEqual(['Title 2']);
		expect(titlesIn('done')).toEqual(['Title 1']);
		expect(column('open').getAttribute('aria-label')).toBe('open, 1 item');
		expect(screen.getByText('Saving…')).toBeInTheDocument();
		expect(statusOf('TASK-1')).toBeDisabled();
		expect(statusOf('TASK-1').value).toBe('done');
		expect(statusOf('TASK-2')).not.toBeDisabled();

		release(ok(fullItem('1', 'done', 8)));
		await waitFor(() => expect(screen.queryByText('Saving…')).toBeNull());
		expect(titlesIn('done')).toEqual(['Title 1']);
		expect(statusOf('TASK-1')).not.toBeDisabled();
		expect(ctx.container.textContent).not.toContain('BODY-MUST-NOT-SHOW');

		// The next write carries the seq the server returned.
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'open' } });
		await settle();
		expect(ctx.callsTo('pad_item', 'update')[1]).toEqual({ action: 'update', ref: 'TASK-1', expected_seq: 8, workspace: 'demo', field: ['status=open'] });
		expect(ctx.call).toHaveBeenCalledTimes(2);
	});

	it('reverts on update_conflict, reloads the item and says so', async () => {
		const ctx = setup((_tool, args) => args.action === 'update'
			? fail('update_conflict', 'stale')
			: ok(fullItem('1', 'in-progress', 9, { title: 'Renamed elsewhere' })));
		await show(ctx);
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });

		expect(await screen.findByText('Changed elsewhere; reloaded')).toBeInTheDocument();
		expect(ctx.callsTo('pad_item', 'update')).toHaveLength(1);
		expect(ctx.callsTo('pad_item', 'get')).toEqual([{ action: 'get', ref: 'TASK-1', workspace: 'demo' }]);
		expect(titlesIn('done')).toEqual([]);
		expect(titlesIn('open')).toEqual(['Title 2']);
		expect(titlesIn('in-progress')).toEqual(['Renamed elsewhere', 'Title 3']);
		expect(statusOf('TASK-1').value).toBe('in-progress');
		expect(statusOf('TASK-1')).not.toBeDisabled();
		expect(ctx.container.textContent).not.toContain('BODY-MUST-NOT-SHOW');

		// The reloaded seq is what the next attempt sends.
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		await settle();
		expect(ctx.callsTo('pad_item', 'update')[1]).toMatchObject({ expected_seq: 9 });
	});

	it('re-runs show when a conflicted item cannot be re-read', async () => {
		const fresh = snapshot();
		fresh.columns[0] = { ...fresh.columns[0], total: 1, items: [card('2', 'open')] };
		const ctx = setup((tool, args) => tool === 'pad_board' ? ok(fresh)
			: args.action === 'update' ? fail('update_conflict', 'stale') : fail('item_not_found', 'gone'));
		await show(ctx);
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		expect(await screen.findByText('Changed elsewhere; reloaded')).toBeInTheDocument();
		expect(ctx.callsTo('pad_board', 'show')).toEqual([SHOW_ARGS]);
		expect(titlesIn('open')).toEqual(['Title 2']);
		expect(titlesIn('done')).toEqual([]);
	});

	it('reverts and shows the message on any other error, and when the bridge rejects', async () => {
		let mode: 'refuse' | 'reject' = 'refuse';
		const ctx = setup(() => mode === 'refuse'
			? fail('permission_denied', 'You have view-only access to this workspace')
			: Promise.reject(new Error('host went away')));
		await show(ctx);
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		expect(await screen.findByText('You have view-only access to this workspace')).toBeInTheDocument();
		expect(titlesIn('open')).toEqual(['Title 1', 'Title 2']);
		expect(statusOf('TASK-1').value).toBe('open');
		expect(statusOf('TASK-1')).not.toBeDisabled();
		expect(ctx.callsTo('pad_item', 'get')).toEqual([]);

		mode = 'reject';
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		expect(await screen.findByText('host went away')).toBeInTheDocument();
		expect(statusOf('TASK-1').value).toBe('open');
	});

	it('keeps a newer saved item when an older delta for it arrives', async () => {
		vi.useFakeTimers();
		const ctx = setup((tool) => tool === 'pad_item'
			? ok(fullItem('1', 'done', 9))
			: noChanges({ upserts: [card('1', 'in-progress', { seq: 8 })] }));
		await show(ctx, ok(snapshot()), SHOW_ARGS, { displayMode: 'fullscreen' });
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		await settle();
		await vi.advanceTimersByTimeAsync(10_000);
		expect(titlesIn('done')).toEqual(['Title 1']);
		expect(titlesIn('in-progress')).toEqual(['Title 3']);
	});

	it('only ever calls the board reads and a status-only item update', async () => {
		vi.useFakeTimers();
		const ctx = setup((tool, args) => tool === 'pad_board'
			? (args.action === 'show' ? ok(snapshot()) : noChanges({ resync: args.since === 'c1' }))
			: args.action === 'update' ? fail('update_conflict', 'stale') : ok(fullItem('1', 'open', 9)));
		await show(ctx, ok(snapshot()), SHOW_ARGS, { displayMode: 'fullscreen' });
		await fireEvent.change(statusOf('TASK-1'), { target: { value: 'done' } });
		await vi.advanceTimersByTimeAsync(30_000);
		const pairs = new Set(ctx.call.mock.calls.map(([tool, args]) => `${tool}:${args.action}`));
		expect([...pairs].sort()).toEqual(['pad_board:changes', 'pad_board:show', 'pad_item:get', 'pad_item:update']);
		for (const args of ctx.callsTo('pad_item', 'update')) {
			expect(Object.keys(args).sort()).toEqual(['action', 'expected_seq', 'field', 'ref', 'workspace']);
			expect(args.field).toEqual(['status=done']);
		}
	});

	it('keeps the view source inside its safety bounds', () => {
		// Tokens are split so the directory-wide guard in bridge.test.ts can scan this file too.
		for (const token of ['{@' + 'html', 'open' + 'Link', 'send' + 'Message', 'update' + 'ModelContext', 'ui/' + 'message', 'inner' + 'HTML', '@modelcontextprotocol', 'callServer' + 'Tool']) {
			expect(source).not.toContain(token);
		}
		// No item body: the view never reads a content property.
		expect(source).not.toMatch(/\.content\b/);
		// Every server call is the local `call`, which only forwards to bridge.call, and names one of four pairs.
		expect(source.match(/\(await bridge\)\.call\(/g)).toHaveLength(1);
		const calls = [...source.matchAll(/\bcall\('([a-z_]+)',\s*(?:buildUpdateArgs\(|\{\s*action:\s*'([a-z]+)')/g)].map(m => `${m[1]}:${m[2] ?? 'update'}`);
		expect(calls.sort()).toEqual(['pad_board:changes', 'pad_board:show', 'pad_item:get', 'pad_item:update']);
		expect(source.match(/\bcall\('/g)).toHaveLength(4);
		expect(source.match(/buildUpdateArgs\(/g)).toHaveLength(1);
		expect(source).toMatch(/buildUpdateArgs\(\{[^}]*field:\s*\{\s*key:\s*'status'/);
	});
});
