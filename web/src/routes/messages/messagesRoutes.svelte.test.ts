// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4044: the /messages routes. The layout redirects the signed-out and does
// not redirect a guest; the list page renders the inbox; the /messages/[n]
// placeholder says "not found" for a bad number or a 404, never marks read, and
// never loads messages.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	getConversation: vi.fn(),
	markRead: vi.fn(),
	listMessages: vi.fn(),
	listConversations: vi.fn(),
	load: vi.fn(),
	goto: vi.fn(),
	chat: { unreadByNumber: {} as Record<number, number> }
}));

class NotFound extends Error {
	code = 'not_found';
}

vi.mock('$lib/api/client', () => ({
	api: {
		chat: {
			getConversation: h.getConversation,
			markRead: h.markRead,
			listMessages: h.listMessages,
			listConversations: h.listConversations
		}
	},
	isNotFoundError: (e: unknown) => e instanceof NotFound
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		load: h.load,
		authenticated: true,
		identityFence: () => () => true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({
	chatStore: {
		version: 0,
		get unreadByNumber() {
			return h.chat.unreadByNumber;
		}
	}
}));
vi.mock('$app/navigation', () => ({ goto: h.goto }));

import MessagesLayout from './+layout.svelte';
import MessagesPage from './+page.svelte';
import ThreadPage from './[n]/+page.svelte';
import { page } from '$app/state';
import { chatInbox } from '$lib/stores/chatInbox.svelte';

const child = createRawSnippet(() => ({ render: () => `<div data-testid="child">child</div>` }));

async function settle() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
		flushSync();
	}
}

const conv = (n: number) => ({
	id: `c${n}`,
	number: n,
	kind: 'dm',
	title: '',
	agent_key: `agent-${n}`,
	last_seq: 0,
	created_at: '',
	updated_at: '',
	read_seq: 0,
	unread: 0
});

beforeEach(() => {
	for (const f of Object.values(h)) if (typeof f === 'function' && 'mockReset' in f) (f as ReturnType<typeof vi.fn>).mockReset();
	h.load.mockResolvedValue({ authenticated: true });
	h.listConversations.mockResolvedValue({ conversations: [conv(3)], unread_total: 0 });
	h.getConversation.mockResolvedValue(conv(3));
	page.params = {};
	// The inbox is a singleton: start each test from a cold one.
	chatInbox.conversations = [];
	chatInbox.loaded = false;
	chatInbox.failed = false;
});

afterEach(() => {
	cleanup();
	page.params = {};
});

describe('/messages layout (TASK-4044)', () => {
	it('renders its children once signed in', async () => {
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(queryByTestId('child')).not.toBeNull();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('sends a signed-out visitor to /login and renders nothing', async () => {
		h.load.mockResolvedValue({ authenticated: false });
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.goto).toHaveBeenCalledWith('/login', { replaceState: true });
		expect(queryByTestId('child')).toBeNull();
	});

	it('sends the visitor to /login when the session probe fails', async () => {
		h.load.mockRejectedValue(new Error('down'));
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.goto).toHaveBeenCalledWith('/login', { replaceState: true });
		expect(queryByTestId('child')).toBeNull();
	});

	it('does NOT redirect a guest: only the signed-out are sent away', async () => {
		h.load.mockResolvedValue({ authenticated: true, user: { is_guest: true } });
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.goto).not.toHaveBeenCalled();
		expect(queryByTestId('child')).not.toBeNull();
	});
});

describe('/messages page (TASK-4044)', () => {
	it('lists the conversations', async () => {
		const { container, getByText } = render(MessagesPage);
		await settle();
		expect(getByText('Chat')).toBeTruthy();
		expect(container.querySelector('a[href="/messages/3"]')).not.toBeNull();
	});

	it('says so when the first load fails', async () => {
		h.listConversations.mockRejectedValue(new Error('boom'));
		const { getByText } = render(MessagesPage);
		await settle();
		expect(getByText('Could not load your conversations.')).toBeTruthy();
	});

	// Nothing else retries a failed list while the unread fetch is fine (r1).
	it('Try again reloads the list once and shows it', async () => {
		h.listConversations.mockRejectedValueOnce(new Error('boom'));
		const { container, getByText } = render(MessagesPage);
		await settle();
		expect(h.listConversations).toHaveBeenCalledTimes(1);
		getByText('Try again').click();
		await settle();
		expect(h.listConversations).toHaveBeenCalledTimes(2);
		expect(container.querySelector('a[href="/messages/3"]')).not.toBeNull();
		expect(container.textContent).not.toContain('Could not load');
	});
});

describe('/messages/[n] placeholder (TASK-4044)', () => {
	it('shows the notice and the agent for a real conversation', async () => {
		page.params = { n: '3' };
		const { container } = render(ThreadPage);
		await settle();
		expect(h.getConversation).toHaveBeenCalledWith(3);
		expect(container.textContent).toContain('agent-3');
		expect(container.textContent).toContain('Messages open here in the next update.');
		expect(container.textContent).toContain('pad chat show C3');
	});

	it.each(['abc', '0', '-1', '1.5', '3x', ''])('"%s" is not a conversation number: not found, no request', async (raw) => {
		page.params = { n: raw };
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Conversation not found');
		expect(h.getConversation).not.toHaveBeenCalled();
	});

	it('a 404 shows not found', async () => {
		page.params = { n: '99' };
		h.getConversation.mockRejectedValue(new NotFound('nope'));
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Conversation not found');
		expect(container.textContent).not.toContain('Messages open here');
	});

	it('another failure is not reported as not found', async () => {
		page.params = { n: '3' };
		h.getConversation.mockRejectedValue(new Error('500'));
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Could not load this conversation');
		expect(container.textContent).not.toContain('Conversation not found');
	});

	it('never marks anything read and never loads messages', async () => {
		page.params = { n: '3' };
		render(ThreadPage);
		await settle();
		expect(h.markRead).not.toHaveBeenCalled();
		expect(h.listMessages).not.toHaveBeenCalled();
	});
});
