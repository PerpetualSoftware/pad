// Component tests for the MCP Apps item card. The host bridge is replaced by a
// fake `connect`, so no SDK and no host are involved.
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import ItemCard from './ItemCard.svelte';
import type { Bridge, HostContext } from './bridge';
import type { ToolResultLike } from './model';

type Handlers = {
	onToolInput(args: Record<string, unknown>): void;
	onToolResult(result: ToolResultLike): void;
	onHostContext?(ctx: HostContext): void;
};
type Responder = (tool: string, args: Record<string, unknown>) => ToolResultLike | Promise<ToolResultLike>;

const item = (over: Record<string, unknown> = {}) => ({
	id: 'i1', ref: 'TASK-5', title: 'Fix the login redirect', seq: 7, collection_slug: 'tasks',
	collection_name: 'Tasks', content: 'Body text', updated_at: '2026-11-12T10:00:00Z',
	assigned_user_name: 'Ada', fields: { status: 'open', priority: 'high' }, ...over
});

const collections = {
	structuredContent: { items: [{ slug: 'tasks', schema: JSON.stringify({ fields: [
		{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in-progress', 'done'] },
		{ key: 'priority', label: 'Priority', type: 'select', options: ['low', 'high'] },
		{ key: 'health', label: 'Health', type: 'select', options: ['ok', 'bad'], computed: true },
		{ key: 'notes', label: 'Notes', type: 'text' }
	] }) }] }
};

const conflict = { isError: true, structuredContent: { error: { code: 'update_conflict', message: 'stale' } } };

function setup(respond: Responder = () => ({ structuredContent: item() })) {
	let handlers!: Handlers;
	const call = vi.fn(async (tool: string, args: Record<string, unknown>) =>
		tool === 'pad_collection' ? collections : respond(tool, args));
	const connect = async (h: Handlers): Promise<Bridge> => { handlers = h; return { call }; };
	const view = render(ItemCard, { props: { connect } });
	const callsTo = (tool: string, action: string) =>
		call.mock.calls.filter(([t, a]) => t === tool && a.action === action).map(([, a]) => a);
	return { ...view, call, callsTo, handlers: () => handlers };
}

async function show(ctx: ReturnType<typeof setup>, args: Record<string, unknown>, result: ToolResultLike) {
	await waitFor(() => expect(ctx.handlers()).toBeTruthy());
	ctx.handlers().onToolInput(args);
	ctx.handlers().onToolResult(result);
}

async function shownWithSchema(ctx: ReturnType<typeof setup>, args: Record<string, unknown> = { action: 'get', ref: 'TASK-5' }) {
	await show(ctx, args, { structuredContent: item() });
	return await screen.findByLabelText('Status') as HTMLSelectElement;
}

describe('ItemCard', () => {
	it('renders item content as text, never as markup', async () => {
		const ctx = setup();
		const payload = '<img src=x onerror=alert(1)>';
		await show(ctx, { action: 'get', ref: 'TASK-5' }, { structuredContent: item({ content: payload, title: '<b>bold</b>' }) });
		expect(await screen.findByText(payload)).toBeInTheDocument();
		expect(screen.getByText('<b>bold</b>')).toBeInTheDocument();
		expect(ctx.container.querySelector('img')).toBeNull();
		expect(ctx.container.querySelector('b')).toBeNull();
	});

	it('renders nothing for a list result', async () => {
		const ctx = setup();
		await show(ctx, { action: 'list' }, { structuredContent: { items: [item()] } });
		await new Promise(resolve => setTimeout(resolve, 20));
		expect(ctx.container.textContent).toBe('');
		expect(ctx.container.querySelector('*')).toBeNull();
		expect(ctx.call).not.toHaveBeenCalled();
	});

	it('renders nothing before any tool data arrives', async () => {
		const ctx = setup();
		await waitFor(() => expect(ctx.handlers()).toBeTruthy());
		expect(ctx.container.querySelector('*')).toBeNull();
	});

	it('shows the item summary', async () => {
		const ctx = setup();
		await show(ctx, { action: 'get', ref: 'TASK-5' }, { structuredContent: item({ content_state: 'applied_pending_flush' }) });
		expect(await screen.findByText('TASK-5')).toBeInTheDocument();
		expect(screen.getByRole('heading', { name: 'Fix the login redirect' })).toBeInTheDocument();
		expect(screen.getByText('Tasks')).toBeInTheDocument();
		expect(screen.getByText('Ada')).toBeInTheDocument();
		expect(ctx.container.querySelector('time')?.getAttribute('datetime')).toBe('2026-11-12T10:00:00Z');
		expect(screen.getByText(/preview may be behind/i)).toBeInTheDocument();
		// Computed selects are shown but not editable; non-select fields are not listed.
		expect(await screen.findByLabelText('Priority')).toBeInstanceOf(HTMLSelectElement);
		expect(screen.queryByLabelText('Health')).toBeNull();
		expect(screen.queryByText('Notes')).toBeNull();
		expect(ctx.callsTo('pad_collection', 'list')).toEqual([{ action: 'list' }]);
	});

	it('fetches the item when the result has no usable structured content', async () => {
		const ctx = setup(() => ({ structuredContent: item({ title: 'Fetched title' }) }));
		await show(ctx, { action: 'update', ref: 'TASK-5', workspace: 'demo' }, { content: [{ type: 'text', text: 'ok' }] });
		expect(await screen.findByRole('heading', { name: 'Fetched title' })).toBeInTheDocument();
		expect(ctx.callsTo('pad_item', 'get')).toEqual([{ action: 'get', ref: 'TASK-5', workspace: 'demo' }]);
	});

	it('falls back to the ref when that fetch fails', async () => {
		const ctx = setup(() => ({ isError: true, content: [{ type: 'text', text: 'nope' }] }));
		await show(ctx, { action: 'get', ref: 'TASK-5' }, {});
		expect(await screen.findByText('TASK-5')).toBeInTheDocument();
		expect(ctx.container.querySelector('select, button, input')).toBeNull();
	});

	it('sends a select change as an update with expected_seq and no content', async () => {
		let release!: (result: ToolResultLike) => void;
		const ctx = setup(() => new Promise<ToolResultLike>(resolve => { release = resolve; }));
		const status = await shownWithSchema(ctx, { action: 'get', ref: 'TASK-5', workspace: 'demo' });
		await fireEvent.change(status, { target: { value: 'done' } });

		const updates = ctx.callsTo('pad_item', 'update');
		expect(updates).toEqual([{ action: 'update', ref: 'TASK-5', expected_seq: 7, workspace: 'demo', field: ['status=done'] }]);
		expect(updates[0]).not.toHaveProperty('content');
		// Optimistic value, saving indicator, every control locked.
		expect(status.value).toBe('done');
		expect(screen.getByText('Saving…')).toBeInTheDocument();
		for (const control of ctx.container.querySelectorAll('select, button')) expect(control).toBeDisabled();

		release({ structuredContent: item({ seq: 8, fields: { status: 'done', priority: 'low' } }) });
		await waitFor(() => expect(screen.queryByText('Saving…')).toBeNull());
		expect(status.value).toBe('done');
		expect((screen.getByLabelText('Priority') as HTMLSelectElement).value).toBe('low');
		expect(status).not.toBeDisabled();

		// The next write carries the seq the server just returned, and still no workspace-less drift.
		await fireEvent.change(screen.getByLabelText('Priority'), { target: { value: 'high' } });
		expect(ctx.callsTo('pad_item', 'update')[1]).toMatchObject({ expected_seq: 8, field: ['priority=high'] });
		expect(ctx.callsTo('pad_collection', 'list')).toHaveLength(1);
	});

	it('omits workspace from the update when the tool input had none', async () => {
		const ctx = setup();
		const status = await shownWithSchema(ctx);
		await fireEvent.change(status, { target: { value: 'done' } });
		expect(ctx.callsTo('pad_item', 'update')[0]).not.toHaveProperty('workspace');
	});

	it('reloads on update_conflict and shows the server value', async () => {
		const ctx = setup((_tool, args) => args.action === 'update'
			? conflict
			: { structuredContent: item({ seq: 9, title: 'Renamed elsewhere', fields: { status: 'in-progress', priority: 'high' } }) });
		const status = await shownWithSchema(ctx);
		await fireEvent.change(status, { target: { value: 'done' } });

		expect(await screen.findByText('Changed elsewhere; reloaded')).toBeInTheDocument();
		expect(ctx.callsTo('pad_item', 'get')).toEqual([{ action: 'get', ref: 'TASK-5' }]);
		expect(status.value).toBe('in-progress');
		expect(screen.getByRole('heading', { name: 'Renamed elsewhere' })).toBeInTheDocument();
		expect(status).not.toBeDisabled();
	});

	it('reverts and shows the message on any other error', async () => {
		const ctx = setup(() => ({ isError: true, structuredContent: { error: { code: 'forbidden', message: 'You cannot edit this item' } } }));
		const status = await shownWithSchema(ctx);
		await fireEvent.change(status, { target: { value: 'done' } });

		expect(await screen.findByText('You cannot edit this item')).toBeInTheDocument();
		expect(status.value).toBe('open');
		expect(ctx.callsTo('pad_item', 'get')).toEqual([]);
		expect(status).not.toBeDisabled();
	});

	it('reverts when the bridge call rejects', async () => {
		const ctx = setup(() => Promise.reject(new Error('host went away')));
		const status = await shownWithSchema(ctx);
		await fireEvent.change(status, { target: { value: 'done' } });
		expect(await screen.findByText('host went away')).toBeInTheDocument();
		expect(status.value).toBe('open');
	});

	it('saves the title on Enter and cancels on Escape', async () => {
		const ctx = setup((_tool, args) => ({ structuredContent: item({ seq: 8, title: String(args.title) }) }));
		await shownWithSchema(ctx);

		await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }));
		let input = screen.getByLabelText('Title') as HTMLInputElement;
		expect(input.value).toBe('Fix the login redirect');
		await fireEvent.input(input, { target: { value: 'Abandoned draft' } });
		await fireEvent.keyDown(input, { key: 'Escape' });
		expect(screen.queryByLabelText('Title')).toBeNull();
		expect(screen.getByRole('heading', { name: 'Fix the login redirect' })).toBeInTheDocument();
		expect(ctx.callsTo('pad_item', 'update')).toEqual([]);

		await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }));
		input = screen.getByLabelText('Title') as HTMLInputElement;
		expect(input.value).toBe('Fix the login redirect');
		await fireEvent.input(input, { target: { value: '  New title  ' } });
		await fireEvent.keyDown(input, { key: 'Enter' });

		expect(ctx.callsTo('pad_item', 'update')).toEqual([{ action: 'update', ref: 'TASK-5', expected_seq: 7, title: 'New title' }]);
		expect(await screen.findByRole('heading', { name: 'New title' })).toBeInTheDocument();
		expect(screen.queryByLabelText('Title')).toBeNull();
	});
});
