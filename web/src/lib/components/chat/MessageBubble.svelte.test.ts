// TASK-4044 PR 3a: one confirmed message. The quote chip shows the excerpt only,
// the "sent from" label shows only when the server sent a context_workspace.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import MessageBubble from './MessageBubble.svelte';
import UnreadDivider from './UnreadDivider.svelte';
import type { ChatMessage } from '$lib/types';

const base: ChatMessage = {
	id: 'm5',
	conversation_id: 'c1',
	seq: 5,
	author_agent_key: 'codex',
	author_is_human: false,
	mine: false,
	source: 'cli',
	body: 'the full body, longer than the excerpt',
	created_at: '2026-10-26T12:00:00.000000000Z',
	deleted: false
};

afterEach(cleanup);

describe('MessageBubble', () => {
	it('names the agent and keeps a human row as You', () => {
		const a = render(MessageBubble, { props: { message: base } });
		expect(a.container.querySelector('.author')!.textContent).toBe('codex');
		a.unmount();
		const b = render(MessageBubble, {
			props: { message: { ...base, mine: true, author_agent_key: '', author_is_human: true } }
		});
		expect(b.container.querySelector('.author')!.textContent).toBe('You');
		expect(b.container.querySelector('.bubble')!.classList.contains('mine')).toBe(true);
	});

	// U11
	it('shows "sent from" only with a context_workspace', () => {
		const a = render(MessageBubble, { props: { message: base } });
		expect(a.container.textContent).not.toContain('sent from');
		a.unmount();
		const b = render(MessageBubble, {
			props: { message: { ...base, context_workspace: { slug: 'docapp', name: 'DocApp' } } }
		});
		expect(b.container.textContent).toContain('sent from DocApp');
	});

	// U10
	it('quote chip shows the excerpt only, and a tap reports the quoted seq', () => {
		const onJump = vi.fn();
		const message: ChatMessage = {
			...base,
			reply_to_seq: 2,
			reply_to: {
				seq: 2,
				author_agent_key: 'codex',
				author_is_human: false,
				excerpt: 'short excerpt',
				deleted: false
			}
		};
		const { container } = render(MessageBubble, { props: { message, onJumpToQuote: onJump } });
		const chip = container.querySelector('.quote') as HTMLButtonElement;
		expect(chip.textContent).toContain('short excerpt');
		expect(chip.textContent).not.toContain('the full body');
		chip.click();
		expect(onJump).toHaveBeenCalledWith(2);
	});

	it('a deleted quote reads "Deleted message"', () => {
		const message: ChatMessage = {
			...base,
			reply_to_seq: 2,
			reply_to: { seq: 2, author_agent_key: 'codex', author_is_human: false, excerpt: '', deleted: true }
		};
		const { container } = render(MessageBubble, { props: { message } });
		expect(container.querySelector('.quote')!.textContent).toContain('Deleted message');
	});

	it('a tombstone shows "Message deleted" and no body or quote', () => {
		const message: ChatMessage = { ...base, deleted: true, body: '', reply_to_seq: 2 };
		const { container } = render(MessageBubble, { props: { message } });
		expect(container.textContent).toContain('Message deleted');
		expect(container.querySelector('.quote')).toBeNull();
	});

	it('renders the body as text, never as markup', () => {
		const { container } = render(MessageBubble, {
			props: { message: { ...base, body: '<b>bold</b> [[ws::TASK-1]]' } }
		});
		expect(container.querySelector('.body b')).toBeNull();
		expect(container.querySelector('.body')!.textContent).toBe('<b>bold</b> [[ws::TASK-1]]');
	});
});

describe('UnreadDivider', () => {
	it('says N new, and N+ new when rows beyond the window are unread too', () => {
		const a = render(UnreadDivider, { props: { count: 2 } });
		expect(a.container.textContent).toContain('2 new');
		a.unmount();
		const b = render(UnreadDivider, { props: { count: 200, more: true } });
		expect(b.container.textContent).toContain('200+ new');
	});
});
