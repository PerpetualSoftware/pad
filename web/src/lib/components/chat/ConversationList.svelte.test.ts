// TASK-4044: the inbox rows. Unread pills come from chatStore only; the
// `unread` field on a summary is ignored.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';

const h = vi.hoisted(() => ({ unreadByNumber: {} as Record<number, number> }));
vi.mock('$lib/stores/chat.svelte', () => ({
	chatStore: {
		get unreadByNumber() {
			return h.unreadByNumber;
		}
	}
}));

import ConversationList from './ConversationList.svelte';
import type { ChatConversationSummary } from '$lib/types';

function conv(n: number, over: Partial<ChatConversationSummary> = {}): ChatConversationSummary {
	return {
		id: `c${n}`,
		number: n,
		kind: 'direct',
		title: '',
		agent_key: `agent-${n}`,
		last_seq: 1,
		created_at: '',
		updated_at: '',
		read_seq: 0,
		unread: 0,
		...over
	};
}

beforeEach(() => {
	h.unreadByNumber = {};
});
afterEach(cleanup);

describe('ConversationList (TASK-4044)', () => {
	it('shows the empty state for no conversations', () => {
		const { getByText } = render(ConversationList, { props: { conversations: [] } });
		expect(getByText('No conversations yet.')).toBeTruthy();
	});

	it('links each row to /messages/{number} and shows the agent and the excerpt', () => {
		const { container } = render(ConversationList, {
			props: {
				conversations: [
					conv(7, {
						last_message: {
							seq: 1,
							author_agent_key: 'agent-7',
							author_is_human: false,
							excerpt: 'hello there',
							deleted: false,
							created_at: ''
						}
					})
				]
			}
		});
		const a = container.querySelector('a')!;
		expect(a.getAttribute('href')).toBe('/messages/7');
		expect(a.textContent).toContain('agent-7');
		expect(a.textContent).toContain('hello there');
	});

	it('prefers the title and falls back to the agent key', () => {
		const { container } = render(ConversationList, {
			props: { conversations: [conv(1, { title: 'Release talk' }), conv(2)] }
		});
		const rows = container.querySelectorAll('a');
		expect(rows[0].textContent).toContain('Release talk');
		expect(rows[1].textContent).toContain('agent-2');
	});

	it('does not show a deleted message excerpt', () => {
		const { container } = render(ConversationList, {
			props: {
				conversations: [
					conv(1, {
						last_message: {
							seq: 2,
							author_agent_key: 'agent-1',
							author_is_human: false,
							excerpt: 'secret words',
							deleted: true,
							created_at: ''
						}
					})
				]
			}
		});
		expect(container.textContent).toContain('Message deleted');
		expect(container.textContent).not.toContain('secret words');
	});

	it('says so for a conversation with no messages', () => {
		const { container } = render(ConversationList, { props: { conversations: [conv(1)] } });
		expect(container.textContent).toContain('No messages yet');
	});

	it('takes the pill from chatStore.unreadByNumber, not from the summary', () => {
		h.unreadByNumber = { 1: 4 };
		const { getByLabelText, queryByLabelText } = render(ConversationList, {
			props: { conversations: [conv(1, { unread: 9 }), conv(2, { unread: 5 })] }
		});
		flushSync();
		expect(getByLabelText('4 unread')).toBeTruthy();
		expect(queryByLabelText('9 unread')).toBeNull();
		expect(queryByLabelText('5 unread')).toBeNull();
	});
});
