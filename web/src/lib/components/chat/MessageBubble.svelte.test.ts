// TASK-4044 PR 3a: one confirmed message. The quote chip shows the excerpt only,
// the "sent from" label shows only when the server sent a context_workspace.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import MessageBubble from './MessageBubble.svelte';
import PendingBubble from './PendingBubble.svelte';
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
