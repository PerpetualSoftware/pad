// TASK-4044 PR 3a: one confirmed message. The quote chip shows the excerpt only,
// the "sent from" label shows only when the server sent a context_workspace.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { toastStore } from '$lib/stores/toast.svelte';
import MessageBubble from './MessageBubble.svelte';
import PendingBubble from './PendingBubble.svelte';
import UnreadDivider from './UnreadDivider.svelte';
import { refKey } from '$lib/chat/refs';
import type { ChatMessage, ChatRef } from '$lib/types';

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

	it('renders markup in the body as text, never as elements', () => {
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

// TASK-4315: native button activation (Tab/Enter/Space) and layout are also
// exercised in Playwright; jsdom does not synthesize native keyboard clicks.
describe('MessageBubble row actions', () => {
	const mount = (message = base, canDelete = true) => {
		const onReply = vi.fn();
		const onDelete = vi.fn();
		return { ...render(MessageBubble, { message, canDelete, onReply, onDelete }), onReply, onDelete };
	};
	const more = () => screen.getByRole('button', { name: /More/ });
	const open = async () => { await fireEvent.click(more()); await tick(); };
	afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

	it('shows Agent, full timestamp and a persistent, tabbable More without opening on hover or focus', async () => {
		const r = mount();
		expect(screen.getByText('Agent')).toBeInTheDocument();
		expect(r.container.querySelector('time')).toHaveAttribute('title', new Date(base.created_at).toLocaleString());
		expect(more().tabIndex).toBe(0);
		more().focus();
		await fireEvent.pointerEnter(r.container.querySelector('article')!, { pointerType: 'mouse' });
		expect(screen.queryByRole('menu')).toBeNull();
		expect(screen.queryByText('Delete')).toBeNull();
	});
	it('shows Human and You on an own human row', () => {
		mount({ ...base, mine: true, author_is_human: true, author_agent_key: '' });
		expect(screen.getByText('Human')).toBeInTheDocument();
		expect(screen.getByText('You')).toBeInTheDocument();
	});
	it.each([true, false])('Delete is only in More when permitted=%s', async (allowed) => {
		const r = mount(base, allowed);
		expect(screen.queryByText('Delete')).toBeNull();
		await open();
		const menu = screen.getByRole('menu', { name: /codex.*5/ });
		expect(r.container.contains(menu)).toBe(false); // portal escapes the scroller
		if (allowed) {
			await fireEvent.click(within(menu).getByRole('menuitem', { name: 'Delete' }));
			expect(r.onDelete).toHaveBeenCalledWith(base);
		} else expect(within(menu).queryByRole('menuitem', { name: 'Delete' })).toBeNull();
	});
	it('arrow keys navigate and Escape restores focus to More', async () => {
		mount();
		await open();
		const reply = screen.getByRole('menuitem', { name: 'Reply' });
		expect(reply).toHaveFocus();
		await fireEvent.keyDown(reply, { key: 'ArrowDown' });
		expect(screen.getByRole('menuitem', { name: 'Copy' })).toHaveFocus();
		await fireEvent.keyDown(document.activeElement!, { key: 'ArrowUp' });
		expect(reply).toHaveFocus();
		await fireEvent.keyDown(reply, { key: 'Escape' });
		expect(screen.queryByRole('menu')).toBeNull();
		expect(more()).toHaveFocus();
	});
	it('Reply calls onReply with the message and closes', async () => {
		const r = mount();
		await open();
		await fireEvent.click(screen.getByRole('menuitem', { name: 'Reply' }));
		expect(r.onReply).toHaveBeenCalledWith(base);
		expect(screen.queryByRole('menu')).toBeNull();
	});
	it('Copy writes the original markdown source', async () => {
		const writeText = vi.fn().mockResolvedValue(undefined);
		Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
		const message = { ...base, body: '**bold** [[TASK-1]]' };
		mount(message);
		await open();
		await fireEvent.click(screen.getByRole('menuitem', { name: 'Copy' }));
		expect(writeText).toHaveBeenCalledWith(message.body);
	});
	it.each(['unavailable', 'denied'])('Copy offers selected text when clipboard is %s, without claiming success', async (kind) => {
		Object.defineProperty(navigator, 'clipboard', { configurable: true, value: kind === 'unavailable' ? undefined : { writeText: vi.fn().mockRejectedValue(new Error('denied')) } });
		const toast = vi.spyOn(toastStore, 'show');
		mount();
		await open();
		await fireEvent.click(screen.getByRole('menuitem', { name: 'Copy' }));
		await tick();
		const dialog = screen.getByRole('dialog', { name: 'Select and copy this message.' });
		const text = within(dialog).getByRole('textbox', { name: 'Select and copy this message.' }) as HTMLTextAreaElement;
		expect(text).toHaveValue(base.body);
		expect(text.readOnly).toBe(true);
		expect(document.querySelectorAll('textarea')).toHaveLength(1); // no leaked legacy copy control
		expect(text).toHaveFocus();
		expect([text.selectionStart, text.selectionEnd]).toEqual([0, base.body.length]);
		expect(toast).not.toHaveBeenCalledWith(expect.anything(), 'success');
	});
	it('a long press opens the same menu', async () => {
		vi.useFakeTimers();
		const r = mount();
		await fireEvent.pointerDown(r.container.querySelector('article')!, { pointerType: 'touch' });
		vi.advanceTimersByTime(500);
		await tick();
		expect(screen.getByRole('menu', { name: /codex.*5/ })).toBeInTheDocument();
	});
	it('preserves row focus when its focused More becomes a tombstone', async () => {
		const r = mount();
		more().focus();
		await r.rerender({ message: { ...base, deleted: true, body: '' }, canDelete: false, onReply: r.onReply, onDelete: r.onDelete });
		expect(r.container.querySelector('article')).toHaveFocus();
	});
	it('tombstones have no actions and remain a programmatic focus target', () => {
		const r = mount({ ...base, deleted: true, body: '' });
		expect(r.container.querySelector('article')).toHaveAttribute('tabindex', '-1');
		expect(screen.queryByRole('button')).toBeNull();
	});
});

describe('PendingBubble', () => {
	it('shows Sending…, then Retry and Discard once failed, with no message menu', async () => {
		const onRetry = vi.fn();
		const onDiscard = vi.fn();
		const pending = { op_key: 'k1', body: 'hello', status: 'sending' as const };
		const r = render(PendingBubble, { props: { pending, onRetry, onDiscard } });
		expect(r.container.textContent).toContain('Sending…');
		expect(r.getByText('Human')).toBeInTheDocument();
		expect(r.getByText('You')).toBeInTheDocument();
		expect(r.container.querySelectorAll('button')).toHaveLength(0);
		await r.rerender({ pending: { ...pending, status: 'failed' as const }, onRetry, onDiscard });
		const [retry, discard] = [...r.container.querySelectorAll('button')];
		await fireEvent.click(retry);
		await fireEvent.click(discard);
		expect(onRetry).toHaveBeenCalledWith('k1');
		expect(onDiscard).toHaveBeenCalledWith('k1');
	});

	// Red if the live region is inserted with its text, or replaced when the state changes.
	it('keeps one live region mounted and changes only its text', async () => {
		const pending = { op_key: 'k1', body: 'hello', status: 'sending' as const };
		const props = { pending, onRetry: vi.fn(), onDiscard: vi.fn() };
		const r = render(PendingBubble, { props });
		const region = r.container.querySelector('[role=status]');
		expect(region!.textContent).toContain('Sending');
		await r.rerender({ ...props, pending: { ...pending, status: 'failed' as const } });
		expect(r.container.querySelector('[role=status]')).toBe(region);
		expect(region!.textContent).toContain('Failed to send');
	});
});

// TASK-4044 3c (U29, corrected by N9): how a body renders its refs.
describe('MessageBubble refs', () => {
	const entry = (r: ChatRef) => [refKey(r.workspace, r.ref), r] as const;
	const map = (...rs: ChatRef[]) => new Map(rs.map(entry));
	const withBody = (body: string, over: Partial<ChatMessage> = {}): ChatMessage => ({ ...base, body, ...over });
	const body = (c: HTMLElement) => c.querySelector('.body') as HTMLElement;

	it('a visible ref is a link to /-/r with the title, in a new tab', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Ship it', collection_slug: 'tasks' });
		const r = render(MessageBubble, { props: { message: withBody('see [[docapp::TASK-1]] now'), refs } });
		const a = body(r.container).querySelector('a')!;
		expect(a.getAttribute('href')).toBe('/-/r/docapp/TASK-1');
		expect(a.getAttribute('title')).toBe('Ship it');
		// TASK-4315: a body link opens in a new tab (which also keeps the SvelteKit router off the server redirect).
		expect(a.getAttribute('target')).toBe('_blank');
		expect(a.getAttribute('rel')).toBe('noopener noreferrer');
		expect(a.textContent).toBe('TASK-1');
		expect(body(r.container).textContent).toBe('see TASK-1 now');
	});

	it('a bare visible ref links through the context workspace', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Ship it' });
		const message = withBody('[[task-1]]', { context_workspace: { slug: 'docapp', name: 'DocApp' } });
		const r = render(MessageBubble, { props: { message, refs } });
		expect(body(r.container).querySelector('a')!.getAttribute('href')).toBe('/-/r/docapp/TASK-1');
	});

	it('a restricted ref is a span with no href and no title attribute', () => {
		const refs = map({ workspace: 'other', ref: 'BUG-2', state: 'restricted' });
		const r = render(MessageBubble, { props: { message: withBody('x [[other::BUG-2]] y'), refs } });
		const b = body(r.container);
		expect(b.querySelector('a')).toBeNull();
		const span = b.querySelector('span.ref-restricted')!;
		expect(span.textContent).toBe('restricted item');
		expect(span.hasAttribute('href')).toBe(false);
		expect(span.hasAttribute('title')).toBe(false);
		expect(span.getAttributeNames()).toEqual(['class']);
		expect(b.textContent).toBe('x restricted item y');
		expect(b.innerHTML).not.toContain('BUG-2');
	});

	it('unresolved and missing refs stay the raw text, brackets kept, with no link', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-3', state: 'unresolved' });
		const text = 'a [[docapp::TASK-3]] b [[docapp::TASK-99]] c [[TASK-3]]';
		const r = render(MessageBubble, { props: { message: withBody(text), refs } });
		const b = body(r.container);
		expect(b.textContent).toBe(text);
		expect(b.querySelector('a, span')).toBeNull();
	});

	it('with no refs at all the body is the raw text', () => {
		const r = render(MessageBubble, { props: { message: withBody('[[docapp::TASK-1]]') } });
		expect(body(r.container).textContent).toBe('[[docapp::TASK-1]]');
		expect(body(r.container).querySelector('a')).toBeNull();
	});

	it('keeps line breaks, and renders markup in the body as text', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: '<b>t</b>' });
		const text = 'line one\n  <b>bold</b> [[docapp::TASK-1]]\nend';
		const r = render(MessageBubble, { props: { message: withBody(text), refs } });
		const b = body(r.container);
		expect(b.querySelectorAll('br')).toHaveLength(2);
		expect(b.textContent).toBe('line one<b>bold</b> TASK-1end');
		expect(b.querySelector('b')).toBeNull();
		expect(b.querySelector('a')!.getAttribute('title')).toBe('<b>t</b>');
	});

	it('a tombstone renders no body and no ref', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Ship it' });
		const message = withBody('', { deleted: true });
		const r = render(MessageBubble, { props: { message, refs } });
		expect(r.container.textContent).toContain('Message deleted');
		expect(r.container.querySelector('a')).toBeNull();
	});

	it('PendingBubble and the quote chip keep the raw text', () => {
		const p = render(PendingBubble, {
			props: { pending: { op_key: 'k', body: 'see [[docapp::TASK-1]]', status: 'sending' as const }, onRetry: vi.fn(), onDiscard: vi.fn() }
		});
		expect(p.container.querySelector('a')).toBeNull();
		expect(p.container.textContent).toContain('[[docapp::TASK-1]]');
		p.unmount();
		const message = withBody('x', {
			reply_to_seq: 2,
			reply_to: { seq: 2, author_agent_key: 'codex', author_is_human: false, excerpt: 'see [[docapp::TASK-1]]', deleted: false }
		});
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Ship it' });
		const q = render(MessageBubble, { props: { message, refs } });
		expect(q.container.querySelector('.quote a')).toBeNull();
		expect(q.container.querySelector('.quote-text')!.textContent).toBe('see [[docapp::TASK-1]]');
	});
});
