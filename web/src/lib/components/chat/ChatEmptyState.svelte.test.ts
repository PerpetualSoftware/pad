// TASK-4186 PR B: the empty /messages help. Unknown is not none: only two good
// reads that found nothing may say "No one to message yet".
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';

vi.mock('$lib/api/client', () => ({ api: { chat: {}, sessions: { list: vi.fn() } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		authenticated: true,
		identityFence: () => () => true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { version: 0, unreadByNumber: {} } }));

import ChatEmptyState from './ChatEmptyState.svelte';
import { chatInbox } from '$lib/stores/chatInbox.svelte';
import type { LiveSession } from '$lib/types';

const NONE = 'No one to message yet';

function set(over: {
	loaded?: boolean;
	failed?: boolean;
	sessions?: LiveSession[] | 'unavailable' | null;
}) {
	chatInbox.conversations = [];
	chatInbox.loaded = over.loaded ?? true;
	chatInbox.failed = over.failed ?? false;
	chatInbox.sessions = over.sessions === undefined ? [] : over.sessions;
	flushSync();
}

const session = (agent: string) =>
	({ id: `s-${agent}`, agent, label: '', accepting_work: true }) as unknown as LiveSession;

beforeEach(() => set({}));
afterEach(cleanup);

describe('ChatEmptyState', () => {
	it('an empty roster and no conversations shows the heading, body and both snippets', () => {
		const { container } = render(ChatEmptyState);
		const text = container.textContent ?? '';
		expect(text).toContain(NONE);
		expect(text).toContain('Chats are with your agents.');
		expect(text).toContain('Name an agent with PAD_AGENT or agent_name in .pad.toml.');
		const codes = [...container.querySelectorAll('code')].map((c) => c.textContent);
		expect(codes).toEqual(['/pad:connect', 'PAD_AGENT=<name> pad watch --stream --for-session']);
		expect(container.querySelectorAll('button').length).toBe(2);
	});

	it('a non-empty roster hides the help and says Start a conversation', () => {
		set({ sessions: [session('rook')] });
		const { container } = render(ChatEmptyState);
		const text = container.textContent ?? '';
		expect(text).toContain('Start a conversation');
		expect(text).not.toContain(NONE);
		expect(container.querySelector('code')).toBeNull();
	});

	it.each([
		['loading', { loaded: false }],
		['failed', { failed: true }],
		['sessions unavailable', { sessions: 'unavailable' as const }],
		['sessions not read yet', { sessions: null }]
	])('%s does not claim no one to message', (_n, over) => {
		set(over);
		const { container } = render(ChatEmptyState);
		expect(container.textContent).not.toContain(NONE);
		expect(container.querySelector('code')).toBeNull();
	});
});
