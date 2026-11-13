// TASK-4044: the inbox rows. Unread pills come from chatStore only; the
// `unread` field on a summary is ignored.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	opened: [] as string[],
	unreadByNumber: {} as Record<number, number>,
	presence: {} as Record<string, string>
}));
vi.mock('$lib/stores/chatInbox.svelte', () => ({
	chatInbox: {
		presenceFor: (k: string) => h.presence[k] ?? 'unknown',
		armedCount: (k: string) => (k === 'agent-2' ? 2 : 0),
		checkedAt: null
	}
}));
vi.mock('$lib/stores/chatSurface.svelte', () => ({
	chatSurface: {
		handleLinkClick: (e: MouseEvent, href: string) => {
			if (e.ctrlKey) return;
			e.preventDefault();
			h.opened.push(href);
		}
	}
}));
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
	h.presence = {};
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

	// BUG-4321: one agent, one card, carrying the preview AND the presence (section 7 wording).
	it('a direct conversation carries its agent presence as words beside the name, with the preview', () => {
		h.presence = { 'agent-7': 'online' };
		const { container } = render(ConversationList, {
			props: {
				conversations: [
					conv(7, {
						last_message: { seq: 1, author_agent_key: 'agent-7', author_is_human: false, excerpt: 'hello there', deleted: false, created_at: '' }
					})
				]
			}
		});
		expect(container.querySelectorAll('a')).toHaveLength(1);
		const a = container.querySelector('a')!;
		expect(a.querySelector('.presence-label')?.textContent).toBe('Live · armed');
		expect(a.textContent).toContain('hello there');
	});

	it('an unread presence read shows Status unknown, never Offline', () => {
		const { container } = render(ConversationList, { props: { conversations: [conv(1)] } });
		expect(container.querySelector('.presence-label')?.textContent).toBe('Status unknown');
	});

	it('names several armed sessions in the tooltip', () => {
		h.presence = { 'agent-2': 'online' };
		const { container } = render(ConversationList, { props: { conversations: [conv(2)] } });
		expect(container.querySelector('.presence-wrap')?.getAttribute('title')).toContain('2 armed sessions');
	});

	it('a group conversation shows no presence: its key is not an agent to chat with', () => {
		const { container } = render(ConversationList, {
			props: { conversations: [conv(3, { kind: 'group' as ChatConversationSummary['kind'], title: 'Room' })] }
		});
		expect(container.querySelector('.presence-dot')).toBeNull();
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

describe('ConversationList rows (TASK-4515)', () => {
	const when = new Date(Date.now() - 60_000).toISOString();
	const last = (over: Record<string, unknown> = {}) => ({
		seq: 1, author_agent_key: 'agent-7', author_is_human: false, excerpt: 'hello there', deleted: false,
		created_at: when, ...over
	});

	it('shows the last message time in a <time>', () => {
		const { container } = render(ConversationList, { props: { conversations: [conv(7, { last_message: last() })] } });
		const t = container.querySelector('a.row time')!;
		expect(t).not.toBeNull();
		expect(t.getAttribute('datetime')).toBe(when);
		expect(t.textContent!.trim()).not.toBe('');
	});

	it('shows no time without a last message', () => {
		const { container } = render(ConversationList, { props: { conversations: [conv(7)] } });
		expect(container.querySelector('a.row time')).toBeNull();
	});

	it('the name carries the full title, and the preview is on its own line, not in the name line', () => {
		const { container } = render(ConversationList, {
			props: { conversations: [conv(7, { title: 'A very long release conversation title', last_message: last() })] }
		});
		const name = container.querySelector('a.row .name')!;
		expect(name.getAttribute('title')).toBe('A very long release conversation title');
		const line = name.parentElement!;
		expect(line.querySelector('.preview')).toBeNull();
		expect(container.querySelector('a.row .preview')!.textContent).toBe('hello there');
		expect(container.querySelector('a.row .avatar')).not.toBeNull();
	});

	it('an unread row is marked so its preview reads stronger', () => {
		h.unreadByNumber = { 7: 2 };
		const { container } = render(ConversationList, { props: { conversations: [conv(7, { last_message: last() })] } });
		flushSync();
		expect(container.querySelector('a.row')!.classList.contains('unread')).toBe(true);
	});
});

describe('opening a conversation (TASK-4315 PR 3b)', () => {
	beforeEach(() => {
		h.opened = [];
	});

	it('a plain click goes through the Chat surface and keeps the href for everything else', () => {
		const { container } = render(ConversationList, { conversations: [conv(5)] });
		const a = container.querySelector('a.row') as HTMLAnchorElement;
		expect(a.getAttribute('href')).toBe('/messages/5');
		const plain = new MouseEvent('click', { bubbles: true, cancelable: true });
		a.dispatchEvent(plain);
		expect(h.opened).toEqual(['/messages/5']);
		expect(plain.defaultPrevented).toBe(true);
	});

	it('a modified click is left to the browser (direct entry)', () => {
		const { container } = render(ConversationList, { conversations: [conv(5)] });
		const a = container.querySelector('a.row') as HTMLAnchorElement;
		const modified = new MouseEvent('click', { bubbles: true, cancelable: true, ctrlKey: true });
		a.dispatchEvent(modified);
		expect(h.opened).toEqual([]);
		expect(modified.defaultPrevented).toBe(false);
	});
});
