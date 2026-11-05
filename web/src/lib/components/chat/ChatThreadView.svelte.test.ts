import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { ChatConversation, ChatMessage } from '$lib/types';

const h = vi.hoisted(() => ({
	userId: 'user-1', emailVerified: true, request: vi.fn(), remove: vi.fn(), toast: vi.fn(),
	thread: null as unknown as { conversation: ChatConversation | null }
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
	get userId() { return h.userId; }, get emailVerified() { return h.emailVerified; },
	onIdentityChange: () => () => {}
} }));
vi.mock('$lib/stores/confirmDialog.svelte', () => ({ confirmDialog: { request: h.request } }));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: h.toast } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { workspaces: [] } }));
vi.mock('$lib/stores/chatInbox.svelte', () => ({ chatInbox: {
	startPresence() {}, stopPresence() {}, presenceFor: () => 'unknown', armedCount: () => 0
} }));
vi.mock('$lib/stores/chatThread.svelte', () => ({ ChatThread: class {
	conversation = $state<ChatConversation | null>({ id: 'c1', number: 1, kind: 'direct', title: '', agent_key: 'agent', last_seq: 5, created_at: '', updated_at: '' });
	status = 'ready';
	items = [{ kind: 'message', message: { id: 'm5', conversation_id: 'c1', seq: 5, author_agent_key: 'agent', author_is_human: false, mine: false, source: 'cli', body: 'keep this message', created_at: '2026-11-01T12:00:00Z', deleted: false } satisfies ChatMessage }];
	detached = []; refs = new Map(); newestSeq = 5; ownSendTick = 0; dividerIndex = -1; atBottom = false;
	constructor() { h.thread = this; }
	open() {} dispose() {} tryMarkRead() {}
	remove = h.remove;
} }));
import ChatThreadView from './ChatThreadView.svelte';

beforeEach(() => {
	h.userId = 'user-1'; h.emailVerified = true;
	h.request.mockReset(); h.remove.mockReset(); h.toast.mockReset();
	h.remove.mockResolvedValue({ ok: true });
});
afterEach(cleanup);
async function ask() {
	await fireEvent.click(screen.getByRole('button', { name: /More/ }));
	await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
}
describe('frozen delete confirmation (TASK-4315)', () => {
	it('asks with Cancel focused; cancel sends no DELETE', async () => {
		h.request.mockResolvedValue(false);
		render(ChatThreadView, { n: 1 });
		await ask();
		expect(h.request).toHaveBeenCalledWith({ title: 'Delete this message?', message: 'Delete this message? It will be removed from the conversation. This cannot undo anything an agent has already received or done.', confirmLabel: 'Delete', danger: true, focusCancel: true });
		expect(h.remove).not.toHaveBeenCalled();
	});
	it.each(['user', 'selected conversation', 'loaded conversation'])('changing %s while awaiting confirmation sends no DELETE', async (change) => {
		let answer!: (ok: boolean) => void;
		h.request.mockImplementation(() => new Promise(resolve => { answer = resolve; }));
		const r = render(ChatThreadView, { n: 1 });
		await ask();
		expect(h.remove).not.toHaveBeenCalled();
		if (change === 'user') h.userId = 'user-2';
		else if (change === 'selected conversation') await r.rerender({ n: 2 });
		else h.thread.conversation = { ...h.thread.conversation!, id: 'c2' };
		answer(true);
		await tick();
		expect(h.remove).not.toHaveBeenCalled();
	});
	it('confirmed refusal keeps the message and shows the server text', async () => {
		h.request.mockResolvedValue(true);
		h.remove.mockResolvedValue({ ok: false, message: 'The author no longer permits deletion' });
		render(ChatThreadView, { n: 1 });
		await ask();
		await tick();
		expect(h.remove).toHaveBeenCalledWith(5);
		expect(screen.getByText('keep this message')).toBeInTheDocument();
		expect(h.toast).toHaveBeenCalledWith('The author no longer permits deletion', 'error');
	});
	it('Reply focuses the composer', async () => {
		render(ChatThreadView, { n: 1 });
		await fireEvent.click(screen.getByRole('button', { name: /More/ }));
		await fireEvent.click(screen.getByRole('menuitem', { name: 'Reply' }));
		await tick();
		expect(screen.getByRole('textbox', { name: 'Message' })).toHaveFocus();
	});
	it.each(['group', 'unverified', 'not loaded'])('hides Delete when %s', async (kind) => {
		if (kind === 'unverified') h.emailVerified = false;
		render(ChatThreadView, { n: 1 });
		if (kind === 'group') h.thread.conversation = { ...h.thread.conversation!, kind: 'group' };
		if (kind === 'not loaded') h.thread.conversation = null;
		await tick();
		await fireEvent.click(screen.getByRole('button', { name: /More/ }));
		expect(screen.queryByRole('menuitem', { name: 'Delete' })).toBeNull();
	});
});
