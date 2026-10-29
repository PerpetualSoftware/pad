// TASK-4044 PR 3a: one confirmed message. The quote chip shows the excerpt only,
// the "sent from" label shows only when the server sent a context_workspace.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
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

// ── PR 3b: the message menu (U24) ────────────────────────────────────────────
describe('MessageBubble menu', () => {
	const fire = (el: Element, type: string, init: Record<string, unknown> = {}) =>
		el.dispatchEvent(new PointerEvent(type, { bubbles: true, ...init }));
	const menu = (c: HTMLElement) => c.querySelector('.menu');
	const mount = (message: ChatMessage = base) => {
		const onReply = vi.fn();
		const onDelete = vi.fn();
		const r = render(MessageBubble, { props: { message, onReply, onDelete } });
		return { ...r, onReply, onDelete, bubble: r.container.querySelector('article')! };
	};

	afterEach(() => vi.useRealTimers());

	it('has no menu until hovered, opens on a mouse hover and closes on leave', async () => {
		const { container, bubble } = mount();
		expect(menu(container)).toBeNull();
		fire(bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		expect(menu(container)).not.toBeNull();
		fire(bubble, 'pointerleave', { pointerType: 'mouse' });
		await tick();
		expect(menu(container)).toBeNull();
	});

	// jsdom has no focus-visible heuristic, so the match is stubbed: true is what a
	// browser reports after keyboard focus, false after a tap or click (r5 H1).
	const focusVisible = (on: boolean) => {
		const real = Element.prototype.matches;
		return vi.spyOn(Element.prototype, 'matches').mockImplementation(function (
			this: Element,
			sel: string
		) {
			return sel === ':focus-visible' ? on : real.call(this, sel);
		});
	};

	it('opens on keyboard focus (:focus-visible)', async () => {
		const spy = focusVisible(true);
		const { container, bubble } = mount();
		bubble.focus();
		await tick();
		expect(menu(container)).not.toBeNull();
		spy.mockRestore();
	});

	// Red if a plain tap, which focuses the bubble but is not focus-visible, opens
	// the menu (r5 H1: Delete would be one tap away).
	it('a focus that is not focus-visible (a tap) opens nothing', async () => {
		const spy = focusVisible(false);
		const { container, bubble } = mount();
		bubble.focus();
		await tick();
		expect(menu(container)).toBeNull();
		spy.mockRestore();
	});

	// Red if only a press outside the whole bubble unpins (r5 Codex M).
	it('a pinned menu closes on a press inside the bubble but outside the menu', async () => {
		vi.useFakeTimers();
		const { container, bubble } = mount();
		fire(bubble, 'pointerdown', { pointerType: 'touch' });
		vi.advanceTimersByTime(500);
		await tick();
		fire(container.querySelector('.menu button')!, 'pointerdown', { pointerType: 'touch' });
		await tick();
		expect(menu(container)).not.toBeNull();
		fire(container.querySelector('.body')!, 'pointerdown', { pointerType: 'touch' });
		await tick();
		expect(menu(container)).toBeNull();
	});

	// Red if a short tap opens it (B4).
	it('a short tap on touch opens nothing; a 500 ms press opens it', async () => {
		vi.useFakeTimers();
		const { container, bubble } = mount();
		fire(bubble, 'pointerdown', { pointerType: 'touch' });
		vi.advanceTimersByTime(300);
		fire(bubble, 'pointerup', { pointerType: 'touch' });
		vi.advanceTimersByTime(1000);
		await tick();
		expect(menu(container)).toBeNull();
		fire(bubble, 'pointerdown', { pointerType: 'touch' });
		vi.advanceTimersByTime(499);
		await tick();
		expect(menu(container)).toBeNull();
		vi.advanceTimersByTime(1);
		await tick();
		expect(menu(container)).not.toBeNull();
	});

	// Red if the native callout / context menu can open while a touch press is armed (r5 H4).
	it('suppresses the native context menu while a touch press is armed, not for a mouse', async () => {
		vi.useFakeTimers();
		const { bubble } = mount();
		const ctx = () => {
			const e = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
			bubble.dispatchEvent(e);
			return e.defaultPrevented;
		};
		expect(ctx()).toBe(false);
		fire(bubble, 'pointerdown', { pointerType: 'touch' });
		vi.advanceTimersByTime(200);
		expect(ctx()).toBe(true);
		fire(bubble, 'pointerup', { pointerType: 'touch' });
		expect(ctx()).toBe(false);
	});

	// Red if focus drops to the body when the menu unmounts (r5 H3). Returning it to the
	// bubble must not reopen the menu, even when the browser calls that focus visible.
	it.each([['Copy', 1], ['Delete', 2]])('after %s focus returns to the bubble and the menu stays shut', async (_name, idx) => {
		const writeText = vi.fn().mockResolvedValue(undefined);
		Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
		const spy = focusVisible(true);
		const { container, bubble } = mount();
		fire(bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		await fireEvent.click(container.querySelectorAll<HTMLButtonElement>('.menu button')[idx]);
		await tick();
		expect(document.activeElement).toBe(bubble);
		expect(menu(container)).toBeNull();
		spy.mockRestore();
	});

	it('a press that moves is a scroll, not a long-press', async () => {
		vi.useFakeTimers();
		const { container, bubble } = mount();
		fire(bubble, 'pointerdown', { pointerType: 'touch', clientX: 10, clientY: 10 });
		fire(bubble, 'pointermove', { pointerType: 'touch', clientX: 10, clientY: 60 });
		vi.advanceTimersByTime(800);
		await tick();
		expect(menu(container)).toBeNull();
	});

	it('a long-pressed menu closes on a press elsewhere', async () => {
		vi.useFakeTimers();
		const { container, bubble } = mount();
		fire(bubble, 'pointerdown', { pointerType: 'touch' });
		vi.advanceTimersByTime(500);
		await tick();
		expect(menu(container)).not.toBeNull();
		fire(document.body, 'pointerdown', { pointerType: 'touch' });
		await tick();
		expect(menu(container)).toBeNull();
	});

	it('Reply and Delete pass the message through, and the menu closes', async () => {
		const { container, bubble, onReply, onDelete } = mount();
		fire(bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		const [reply] = [...container.querySelectorAll<HTMLButtonElement>('.menu button')];
		await fireEvent.click(reply);
		expect(onReply).toHaveBeenCalledWith(base);
		fire(bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		await fireEvent.click(container.querySelectorAll<HTMLButtonElement>('.menu button')[2]);
		expect(onDelete).toHaveBeenCalledWith(base);
	});

	it('Copy writes the plain body', async () => {
		const writeText = vi.fn().mockResolvedValue(undefined);
		Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
		const { container, bubble } = mount();
		fire(bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		await fireEvent.click(container.querySelectorAll<HTMLButtonElement>('.menu button')[1]);
		expect(writeText).toHaveBeenCalledWith('the full body, longer than the excerpt');
	});

	it('offers Delete on an agent row (the server allows it) and no menu on a tombstone', async () => {
		const live = mount();
		fire(live.bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		expect([...live.container.querySelectorAll('.menu button')].map((b) => b.textContent)).toEqual([
			'Reply',
			'Copy',
			'Delete'
		]);
		live.unmount();
		const gone = mount({ ...base, deleted: true, body: '' });
		fire(gone.bubble, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		expect(menu(gone.container)).toBeNull();
	});

	it('is read-only without the hooks', async () => {
		const { container } = render(MessageBubble, { props: { message: base } });
		fire(container.querySelector('article')!, 'pointerenter', { pointerType: 'mouse' });
		await tick();
		expect(menu(container)).toBeNull();
	});
});

describe('PendingBubble', () => {
	it('shows Sending…, then Retry and Discard once failed, with no message menu', async () => {
		const onRetry = vi.fn();
		const onDiscard = vi.fn();
		const pending = { op_key: 'k1', body: 'hello', status: 'sending' as const };
		const r = render(PendingBubble, { props: { pending, onRetry, onDiscard } });
		expect(r.container.textContent).toContain('Sending…');
		expect(r.container.querySelectorAll('button')).toHaveLength(0);
		await r.rerender({ pending: { ...pending, status: 'failed' as const }, onRetry, onDiscard });
		const [retry, discard] = [...r.container.querySelectorAll('button')];
		await fireEvent.click(retry);
		await fireEvent.click(discard);
		expect(onRetry).toHaveBeenCalledWith('k1');
		expect(onDiscard).toHaveBeenCalledWith('k1');
	});

	// Red if the live region is inserted with its text, or replaced when the state changes (r5 H5).
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

	it('a visible ref is a link to /-/r with the title and a hard reload', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Ship it', collection_slug: 'tasks' });
		const r = render(MessageBubble, { props: { message: withBody('see [[docapp::TASK-1]] now'), refs } });
		const a = body(r.container).querySelector('a')!;
		expect(a.getAttribute('href')).toBe('/-/r/docapp/TASK-1');
		expect(a.getAttribute('title')).toBe('Ship it');
		expect(a.hasAttribute('data-sveltekit-reload')).toBe(true);
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

	it('keeps newlines and spacing as text, and renders markup in the body as text', () => {
		const refs = map({ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: '<b>t</b>' });
		const text = 'line one\n  <b>bold</b> [[docapp::TASK-1]]\nend';
		const r = render(MessageBubble, { props: { message: withBody(text), refs } });
		const b = body(r.container);
		expect(b.textContent).toBe('line one\n  <b>bold</b> TASK-1\nend');
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
