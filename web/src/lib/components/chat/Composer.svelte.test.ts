// TASK-4044 PR 3b: the composer. The empty / byte-count rules are the server's
// (N1, N2); the keyboard offset is read from visualViewport (U25); the draft
// keeper stashes for the signed-in user on this thread only (U27, N8).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';

const h = vi.hoisted(() => ({ userId: 'u1' }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get userId() {
			return h.userId;
		},
		// signOutGuard (pulled in by the composer) subscribes at import.
		onIdentityChange: () => () => {}
	}
}));

import { kb } from '$lib/chat/uiKeyboard.fixture.svelte';
// A real soft keyboard cannot be raised in jsdom: keyboardVisible is driven by hand.
vi.mock('$lib/stores/ui.svelte', async (orig) => {
	const actual = await orig<typeof import('$lib/stores/ui.svelte')>();
	return {
		...actual,
		uiStore: new Proxy(actual.uiStore, {
			get: (t, k) => (k === 'keyboardVisible' ? kb.visible : Reflect.get(t, k))
		})
	};
});

import Composer from './Composer.svelte';
import { getDraft, purgeAll, unsealChatRecords } from '$lib/chat/chatRecords';
import { confirmSignOut, signOutFailed } from '$lib/stores/signOutGuard.svelte';
import { inputMode } from '$lib/chat/inputMode.svelte';
import { runSessionExpiryKeepers } from '$lib/stores/sessionExpiryKeep';
import type { SendOutcome } from '$lib/stores/chatThread.svelte';
import type { ChatMessage } from '$lib/types';

const target: ChatMessage = {
	id: 'm4',
	conversation_id: 'c1',
	seq: 4,
	author_agent_key: 'codex',
	author_is_human: false,
	mine: false,
	source: 'cli',
	body: 'the target',
	created_at: '2026-10-26T12:00:00.000000000Z',
	deleted: false
};

type Props = Parameters<typeof render<typeof Composer>>[1] extends { props?: infer P } ? P : never;
function mount(over: Record<string, unknown> = {}) {
	const onSend = vi.fn<(b: string, r?: number) => Promise<SendOutcome>>().mockResolvedValue('sent');
	const onClearReply = vi.fn();
	const r = render(Composer, { props: { n: 3, onSend, onClearReply, ...over } as Props });
	const box = r.container.querySelector('textarea')!;
	const send = r.container.querySelector<HTMLButtonElement>('button.send')!;
	return { ...r, onSend, onClearReply, box, send };
}
const type = async (box: HTMLTextAreaElement, v: string) => {
	await fireEvent.input(box, { target: { value: v } });
	flushSync();
};

// The primary pointer decides the default key mode: fine means a hardware keyboard.
function setPointer(fine: boolean) {
	Object.defineProperty(window, 'matchMedia', {
		configurable: true,
		writable: true,
		value: (q: string) => ({
			matches: q.includes('pointer: fine') ? fine : false,
			media: q,
			addEventListener() {},
			removeEventListener() {}
		})
	});
}

beforeEach(() => {
	h.userId = 'u1';
	purgeAll();
	unsealChatRecords();
	signOutFailed();
	sessionStorage.clear();
	localStorage.clear();
	inputMode.reset();
	setPointer(true);
});
afterEach(() => {
	cleanup();
	kb.visible = false;
	Reflect.deleteProperty(window, 'visualViewport');
	Reflect.deleteProperty(window, 'matchMedia');
});

describe('Send enable', () => {
	// N1: red if the empty check is JS trim() (B9): U+0085 would enable, U+FEFF would block.
	it.each([
		['', true],
		['   \n\t', true],
		['\u0085', true],
		['﻿', false],
		['hi', false]
	])('body %j disabled=%s', async (v, disabled) => {
		const { box, send } = mount();
		await type(box, v);
		expect(send.disabled).toBe(disabled);
	});

	// N2: red if the cap counts UTF-16 units (B10).
	it('blocks a body over 64 KiB of UTF-8 bytes and says so', async () => {
		const { box, send, container } = mount();
		await type(box, '😀'.repeat(20_000));
		expect(send.disabled).toBe(true);
		expect(container.querySelector('[role=status]')!.textContent).toContain('longer than 65,536 bytes');
		expect(container.querySelector('.count')!.textContent).toContain('80,000 / 65,536 bytes');
	});

	// N3: red if the encoded-size check is dropped (B13).
	it('blocks a body whose escaped JSON passes the request cap, with its own copy', async () => {
		const { box, send, container } = mount();
		await type(box, '"'.repeat(65_536));
		expect(send.disabled).toBe(true);
		expect(container.querySelector('[role=status]')!.textContent).toContain('too large to send');
	});

	// Red if the live region is inserted together with its text (r5 H5): some screen
	// readers announce only a change to a region that was already in the page.
	it('keeps its live region mounted, empty at first, and only changes its text', async () => {
		const { box, container } = mount();
		const region = container.querySelector('[role=status]');
		expect(region).not.toBeNull();
		expect(region!.textContent!.trim()).toBe('');
		await type(box, '"'.repeat(65_536));
		expect(container.querySelector('[role=status]')).toBe(region);
		expect(region!.textContent).toContain('too large to send');
		await type(box, 'ok');
		expect(container.querySelector('[role=status]')).toBe(region);
		expect(region!.textContent!.trim()).toBe('');
	});

	it('shows no count for a short body', async () => {
		const { box, container } = mount();
		await type(box, 'hello');
		expect(container.querySelector('.count')).toBeNull();
	});
});

describe('sending', () => {
	it.each(['pointerdown', 'mousedown'])('prevents %s on Send from taking focus', async (event) => {
		setPointer(false);
		const { box, send } = mount();
		await type(box, 'hello');
		box.focus();
		const press = event === 'pointerdown'
			? new PointerEvent(event, { pointerType: 'touch', bubbles: true, cancelable: true })
			: new MouseEvent(event, { bubbles: true, cancelable: true });
		expect(send.dispatchEvent(press)).toBe(false);
		expect(document.activeElement).toBe(box);
	});

	it('focuses the box synchronously when Send submits, before the send answers', async () => {
		setPointer(false);
		const { box, send, onSend } = mount();
		onSend.mockImplementation(() => {
			expect(document.activeElement).toBe(box);
			return new Promise(() => {});
		});
		await type(box, 'hello');
		send.focus();
		expect(document.activeElement).toBe(send);
		await fireEvent.click(send);
		expect(onSend).toHaveBeenCalledTimes(1);
		expect(document.activeElement).toBe(box);
	});

	it('leaves focus alone when it was outside the box (a closed touch keyboard stays closed)', async () => {
		setPointer(false);
		const { box, send, onSend } = mount();
		await type(box, 'hello');
		box.blur();
		expect(document.activeElement).toBe(document.body);
		await fireEvent.click(send);
		expect(onSend).toHaveBeenCalledTimes(1);
		expect(document.activeElement).toBe(document.body);
	});

	it('sends the text with the reply target, clears the box and the reply on success', async () => {
		const { box, send, onSend, onClearReply } = mount({ replyTo: target });
		await type(box, 'hello');
		await fireEvent.click(send);
		await tick();
		expect(onSend).toHaveBeenCalledWith('hello', 4);
		expect(box.value).toBe('');
		expect(onClearReply).toHaveBeenCalledTimes(1);
	});

	// U23: red if a deleted reply target stays set (B3). The draft comes back.
	it('on a deleted reply target: drops the target, keeps the draft, says why', async () => {
		const { box, send, onSend, onClearReply, container } = mount({ replyTo: target });
		onSend.mockResolvedValue('reply_deleted');
		await type(box, 'my draft');
		await fireEvent.click(send);
		await tick();
		expect(onClearReply).toHaveBeenCalledTimes(1);
		expect(box.value).toBe('my draft');
		expect(container.querySelector('[role=status]')!.textContent).toContain('was deleted');
	});

	it('on an over-size refusal: the text comes back and the reply target stays', async () => {
		const onSend = vi.fn().mockResolvedValue('too_large');
		const onClearReply = vi.fn();
		const r = render(Composer, { props: { n: 3, replyTo: target, onSend, onClearReply } });
		const b = r.container.querySelector('textarea')!;
		await type(b, 'my draft');
		await fireEvent.click(r.container.querySelector('button.send')!);
		await tick();
		expect(b.value).toBe('my draft');
		expect(onClearReply).not.toHaveBeenCalled();
	});

	it('a failed send leaves the text to its bubble', async () => {
		const { box, send, onSend } = mount();
		onSend.mockResolvedValue('failed');
		await type(box, 'hello');
		await fireEvent.click(send);
		await tick();
		expect(box.value).toBe('');
	});

	it('the reply bar names the target and clears on ×', async () => {
		const { container, onClearReply } = mount({ replyTo: target });
		expect(container.querySelector('.reply')!.textContent).toContain('codex');
		expect(container.querySelector('.reply')!.textContent).toContain('the target');
		await fireEvent.click(container.querySelector('.reply button')!);
		expect(onClearReply).toHaveBeenCalled();
	});
});

// U25
describe('keyboard', () => {
	// The surface that contains the composer fits the visual viewport (viewportOwner);
	// the composer itself no longer lifts, or the lift would be applied twice.
	it('never lifts itself and holds no viewport listeners', () => {
		const vv = Object.assign(new EventTarget(), { height: window.innerHeight, offsetTop: 0 });
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		const add = vi.spyOn(vv, 'addEventListener');
		const { container } = mount();
		const form = container.querySelector<HTMLElement>('form.composer')!;
		vv.height = window.innerHeight - 300;
		vv.dispatchEvent(new Event('resize'));
		flushSync();
		expect(form.style.marginBottom).toBe('');
		expect(add).not.toHaveBeenCalled();
	});

	it('a soft keyboard coming up enters touch mode once, on the false to true edge', async () => {
		setPointer(true);
		const spy = vi.spyOn(inputMode, 'softKeyboardUp');
		mount();
		expect(spy).not.toHaveBeenCalled();
		kb.visible = true;
		flushSync();
		expect(spy).toHaveBeenCalledTimes(1);
		flushSync();
		expect(spy).toHaveBeenCalledTimes(1);
		kb.visible = false;
		flushSync();
		expect(spy).toHaveBeenCalledTimes(1);
		kb.visible = true;
		flushSync();
		expect(spy).toHaveBeenCalledTimes(2);
	});

	it('a keyboard already up when the composer mounts counts as touch', () => {
		kb.visible = true;
		const spy = vi.spyOn(inputMode, 'softKeyboardUp');
		mount();
		flushSync();
		expect(spy).toHaveBeenCalledTimes(1);
	});
});

// U27, N8
describe('draft keeper', () => {
	// Red if the keeper is not registered (B8).
	it('stashes the typed draft when the session expires, and restores it on the same thread', async () => {
		const first = mount();
		await type(first.box, 'half typed');
		runSessionExpiryKeepers();
		expect(sessionStorage.length).toBe(1);
		first.unmount();
		const again = mount();
		await tick();
		expect(again.box.value).toBe('half typed');
		expect(sessionStorage.length).toBe(1);
	});

	// Red if the stash ignores identity (B11).
	it('never offers a stashed draft to another user', async () => {
		const first = mount();
		await type(first.box, 'private');
		runSessionExpiryKeepers();
		first.unmount();
		h.userId = 'u2';
		const other = mount();
		await tick();
		expect(other.box.value).toBe('');
	});

	it('does not offer it on another thread', async () => {
		const first = mount();
		await type(first.box, 'for thread 3');
		runSessionExpiryKeepers();
		first.unmount();
		const other = mount({ n: 4 });
		await tick();
		expect(other.box.value).toBe('');
	});

	it('keeps nothing for an empty box or a signed-out tab', async () => {
		const first = mount();
		runSessionExpiryKeepers();
		expect(sessionStorage.length).toBe(0);
		await type(first.box, 'x');
		h.userId = '';
		runSessionExpiryKeepers();
		expect(sessionStorage.length).toBe(0);
	});

	// An unanswered body is already a pending-attempt record (written by the thread before the POST),
	// so the keeper stashes only what was typed since. Stashing both offered the same text twice
	// after re-login, as a draft and as a 'Send not confirmed' bubble.
	it('stashes only what was typed since an unanswered send, not the in-flight body', async () => {
		const first = mount();
		first.onSend.mockReturnValue(new Promise(() => {})); // a 401 is on its way: the send never answers
		await type(first.box, 'in flight');
		await fireEvent.click(first.send);
		flushSync();
		expect(first.box.value).toBe('');
		await type(first.box, 'typed since');
		runSessionExpiryKeepers();
		first.unmount();
		const again = mount();
		await tick();
		expect(again.box.value).toBe('typed since');
	});

	it('stops stashing a body once its send has answered', async () => {
		const first = mount();
		await type(first.box, 'delivered');
		await fireEvent.click(first.send);
		await tick();
		runSessionExpiryKeepers();
		expect(sessionStorage.length).toBe(0);
	});

	it('stops keeping once the composer is gone', async () => {
		const first = mount();
		await type(first.box, 'x');
		first.unmount();
		const kept = sessionStorage.getItem('pad:chat-draft:u1:3');
		runSessionExpiryKeepers();
		expect(sessionStorage.getItem('pad:chat-draft:u1:3')).toBe(kept);
		expect(getDraft('u1', 3)?.text).toBe('x');
	});
});

// BUG-4312: send() empties the box before it awaits onSend. A send that throws,
// or that never started, used to leave the person with an empty box, no bubble
// and no word: the message was lost without a trace.
describe('a send that does not happen', () => {
	it('puts the text back with a notice when onSend throws', async () => {
		const onSend = vi.fn<(b: string, r?: number) => Promise<SendOutcome>>().mockRejectedValue(new Error('boom'));
		const { box, send, container } = mount({ onSend });
		await type(box, 'do not lose me');
		await fireEvent.click(send);
		await tick();
		expect(onSend).toHaveBeenCalledTimes(1);
		expect(box.value).toBe('do not lose me');
		expect(container.querySelector('[role=status]')!.textContent).toContain('could not be sent');
	});

	it('puts the text back with a notice when the send never started', async () => {
		const onSend = vi.fn<(b: string, r?: number) => Promise<SendOutcome>>().mockResolvedValue('not_started');
		const { box, send, container } = mount({ onSend });
		await type(box, 'still here');
		await fireEvent.click(send);
		await tick();
		expect(box.value).toBe('still here');
		expect(container.querySelector('[role=status]')!.textContent).toContain('not ready');
	});

	it('keeps text typed after the failed send, ahead of the restored one', async () => {
		let reject!: (e: Error) => void;
		const onSend = vi.fn<(b: string, r?: number) => Promise<SendOutcome>>().mockReturnValue(new Promise((_, rej) => (reject = rej)));
		const { box, send } = mount({ onSend });
		await type(box, 'first');
		await fireEvent.click(send);
		await type(box, 'second');
		reject(new Error('boom'));
		await tick();
		await tick();
		expect(box.value).toBe('first\nsecond');
	});
});

// TASK-4099 part 0 (DOC-4316 section 5): which keys send. Product 11-04: the default follows
// the PRIMARY pointer, (pointer: fine) means Enter sends, (pointer: coarse) means Enter adds a line.
describe('Enter sends (TASK-4099)', () => {
	const key = (box: HTMLTextAreaElement, init: KeyboardEventInit & { keyCode?: number }) => {
		const ev = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true, ...init });
		if (init.keyCode !== undefined) Object.defineProperty(ev, 'keyCode', { value: init.keyCode });
		box.dispatchEvent(ev);
		flushSync();
		return ev;
	};

	it('with a fine pointer, Enter sends once and is not left to insert a newline', async () => {
		const { box, onSend } = mount();
		await type(box, 'hello');
		box.focus();
		const ev = key(box, {});
		expect(document.activeElement).toBe(box);
		expect(ev.defaultPrevented).toBe(true);
		expect(onSend).toHaveBeenCalledTimes(1);
		expect(onSend.mock.calls[0][0]).toBe('hello');
		expect(box.value).toBe('');
	});

	it('Shift+Enter inserts a newline: no send, default left alone', async () => {
		const { box, onSend } = mount();
		await type(box, 'hello');
		const ev = key(box, { shiftKey: true });
		expect(ev.defaultPrevented).toBe(false);
		expect(onSend).not.toHaveBeenCalled();
	});

	it.each([['ctrlKey'], ['metaKey'], ['altKey']])('%s+Enter is not a send shortcut', async (mod) => {
		const { box, onSend } = mount();
		await type(box, 'hello');
		key(box, { [mod]: true });
		expect(onSend).not.toHaveBeenCalled();
	});

	it('never sends while an IME is composing, whichever signal the browser gives', async () => {
		const { box, onSend } = mount();
		await type(box, 'こんにちは');
		key(box, { isComposing: true });
		key(box, { keyCode: 229 });
		box.dispatchEvent(new CompositionEvent('compositionstart'));
		key(box, {});
		expect(onSend).not.toHaveBeenCalled();
	});

	it('the Enter that commits a composition does not send, even after compositionend (Safari order)', async () => {
		const { box, onSend } = mount();
		await type(box, 'こんにちは');
		box.dispatchEvent(new CompositionEvent('compositionstart'));
		box.dispatchEvent(new CompositionEvent('compositionend'));
		key(box, {});
		expect(onSend).not.toHaveBeenCalled();
		await new Promise((r) => setTimeout(r, 0));
		key(box, {});
		expect(onSend).toHaveBeenCalledTimes(1);
	});

	it('a held key sends once and its repeats neither send nor insert lines', async () => {
		const { box, onSend } = mount();
		await type(box, 'hello');
		key(box, {});
		await type(box, 'again');
		const rep = key(box, { repeat: true });
		expect(rep.defaultPrevented).toBe(true);
		expect(onSend).toHaveBeenCalledTimes(1);
	});

	it('Enter on an empty box sends nothing and adds no newline', async () => {
		const { box, onSend } = mount();
		const ev = key(box, {});
		expect(ev.defaultPrevented).toBe(true);
		expect(onSend).not.toHaveBeenCalled();
	});

	it('pasting and blurring never send', async () => {
		const { box, onSend } = mount();
		await type(box, 'hello');
		await fireEvent.paste(box);
		await fireEvent.blur(box);
		expect(onSend).not.toHaveBeenCalled();
	});

	it('with a coarse pointer, Enter inserts a newline and the Send button sends', async () => {
		setPointer(false);
		const { box, send, onSend } = mount();
		await type(box, 'hello');
		const ev = key(box, {});
		expect(ev.defaultPrevented).toBe(false);
		expect(onSend).not.toHaveBeenCalled();
		await fireEvent.click(send);
		expect(onSend).toHaveBeenCalledTimes(1);
	});

	it('no matchMedia at all is the safe default: Enter does not send', async () => {
		Reflect.deleteProperty(window, 'matchMedia');
		const { box, onSend } = mount();
		await type(box, 'hello');
		key(box, {});
		expect(onSend).not.toHaveBeenCalled();
	});

	it('shows the key hint for the mode in force', async () => {
		const { container } = mount();
		expect(container.querySelector('.key-hint')!.textContent).toContain('Enter sends');
		cleanup();
		setPointer(false);
		const touch = mount();
		expect(touch.container.querySelector('.key-hint')!.textContent).toContain('Enter adds a line');
	});

	describe('per-device Enter-sends choice', () => {
		it('a stored-on choice on a coarse pointer makes Enter send', async () => {
			setPointer(false);
			const { box, onSend } = mount();
			await type(box, 'hello');
			inputMode.setHardware(true);
			flushSync();
			key(box, {});
			expect(onSend).toHaveBeenCalledTimes(1);
		});

		it('has no hardware checkbox in the composer on a coarse pointer', () => {
			setPointer(false);
			const { queryByLabelText, container } = mount();
			expect(queryByLabelText('Hardware keyboard: Enter sends')).toBeNull();
			expect(container.querySelector('label.hardware')).toBeNull();
		});

		it('a fresh composer uses the stored-on choice', async () => {
			setPointer(false);
			mount();
			inputMode.setHardware(true);
			cleanup();
			const second = mount();
			await type(second.box, 'hello');
			key(second.box, {});
			expect(second.onSend).toHaveBeenCalledTimes(1);
		});

		it('with nothing stored, a touch on the box resets a fine pointer to touch mode', async () => {
			const { box, onSend } = mount();
			await type(box, 'hello');
			box.dispatchEvent(new PointerEvent('pointerdown', { pointerType: 'touch', bubbles: true }));
			flushSync();
			key(box, {});
			expect(onSend).not.toHaveBeenCalled();
		});

		it('a mouse or pen press does not change the mode', async () => {
			const { box, onSend } = mount();
			await type(box, 'hello');
			box.dispatchEvent(new PointerEvent('pointerdown', { pointerType: 'mouse', bubbles: true }));
			flushSync();
			key(box, {});
			expect(onSend).toHaveBeenCalledTimes(1);
		});

		it('a touch on the box preserves a stored-on choice', async () => {
			setPointer(false);
			const { box, onSend } = mount();
			inputMode.setHardware(true);
			box.dispatchEvent(new PointerEvent('pointerdown', { pointerType: 'touch', bubbles: true }));
			flushSync();
			await type(box, 'hello');
			key(box, {});
			expect(onSend).toHaveBeenCalledTimes(1);
			expect(inputMode.hardware).toBe(true);
			expect(localStorage.getItem('pad-chat-enter-sends')).toBe('on');
		});

		it('a soft keyboard drops a stored-on choice to touch while it is up and keeps storage', async () => {
			setPointer(false);
			inputMode.setHardware(true);
			const { box, onSend } = mount();
			await type(box, 'hello');
			kb.visible = true;
			flushSync();
			await tick();
			key(box, {});
			expect(onSend).not.toHaveBeenCalled();
			expect(localStorage.getItem('pad-chat-enter-sends')).toBe('on');
			kb.visible = false;
			flushSync();
			await tick();
			key(box, {});
			expect(onSend).toHaveBeenCalledTimes(1);
		});

		it('a composer unmounted while the keyboard was up leaves no stuck touch mode for the next one', async () => {
			setPointer(false);
			inputMode.setHardware(true);
			mount();
			kb.visible = true;
			flushSync();
			cleanup();
			kb.visible = false;
			flushSync();
			const { box, onSend } = mount();
			await type(box, 'hello');
			key(box, {});
			expect(onSend).toHaveBeenCalledTimes(1);
		});

		it('with nothing stored, a soft keyboard going down leaves touch mode in force', async () => {
			setPointer(true);
			const { box, onSend } = mount();
			await type(box, 'hello');
			kb.visible = true;
			flushSync();
			kb.visible = false;
			flushSync();
			await tick();
			key(box, {});
			expect(onSend).not.toHaveBeenCalled();
		});
	});
});

describe('target and delivery wording (DOC-4316 sections 5 and 7)', () => {
	it('names the destination above the box: agent, number and title', () => {
		const { container } = mount({ agentKey: 'build-agent', title: 'Review', presence: 'online' });
		const t = container.querySelector('.target')!;
		expect(t.textContent).toContain('To build-agent');
		expect(t.textContent).toContain('C3');
		expect(t.textContent).toContain('Review');
	});

	it('the Send button names the agent and conversation', () => {
		const { send } = mount({ agentKey: 'build-agent', presence: 'online' });
		expect(send.getAttribute('aria-label')).toBe('Send to build-agent, conversation 3');
	});

	it.each([['offline'], ['connected_not_accepting']])('warns before sending to a %s agent', (presence) => {
		const { container } = mount({ agentKey: 'a', presence });
		expect(container.querySelector('.presence-note')!.textContent).toContain(
			'No live armed session. Saving a message does not confirm receipt.'
		);
	});

	it('says the status is unavailable rather than offline when presence is unknown', () => {
		const { container } = mount({ agentKey: 'a', presence: 'unknown' });
		expect(container.querySelector('.presence-note')!.textContent).toBe('Session status unavailable.');
	});

	it.each([['online'], ['busy_uncertain']])('has no warning for a %s agent', (presence) => {
		const { container } = mount({ agentKey: 'a', presence });
		expect(container.querySelector('.presence-note')!.textContent).toBe('');
	});

	it('after a send to a live agent says Saved to conversation; to any other, receipt unconfirmed', async () => {
		const live = mount({ agentKey: 'a', presence: 'online' });
		await type(live.box, 'hi');
		await fireEvent.click(live.send);
		await tick();
		expect(live.container.querySelector('[role=status]')!.textContent).toContain('Saved to conversation');
		expect(live.container.querySelector('[role=status]')!.textContent).not.toContain('receipt unconfirmed');
		cleanup();
		const off = mount({ agentKey: 'a', presence: 'offline' });
		await type(off.box, 'hi');
		await fireEvent.click(off.send);
		await tick();
		expect(off.container.querySelector('[role=status]')!.textContent).toContain(
			'Saved to conversation · receipt unconfirmed'
		);
	});

	it('never says delivered, read, accepted or will execute', async () => {
		const { container, box, send } = mount({ agentKey: 'a', presence: 'offline' });
		await type(box, 'hi');
		await fireEvent.click(send);
		await tick();
		expect(container.textContent).not.toMatch(/delivered|read by|accepted|will execute/i);
	});
});

describe('persistent drafts (TASK-4315)', () => {
	beforeEach(() => vi.useFakeTimers());
	afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });
	async function save() { await vi.advanceTimersByTimeAsync(500); flushSync(); }
	it('keeps text on A to B to A navigation, including a close before the debounce', async () => {
		const a = mount({ n: 1 }); await type(a.box, 'draft A'); a.unmount();
		const b = mount({ n: 2 }); expect(b.box.value).toBe(''); await type(b.box, 'draft B'); b.unmount();
		const again = mount({ n: 1 }); await tick();
		expect(again.box.value).toBe('draft A'); expect(again.onSend).not.toHaveBeenCalled();
		expect(getDraft('u1', 2)?.text).toBe('draft B');
	});
	it('saves text and reply before teardown so they survive reload; restoration never sends', async () => {
		const first = mount({ replyTo: target }); await type(first.box, 'reload me'); await save();
		const stored = sessionStorage.getItem('pad:chat-draft:u1:3');
		expect(JSON.parse(stored!)).toEqual({ userId: 'u1', n: 3, text: 'reload me', replySeq: 4 });
		first.unmount();
		const again = mount(); await tick(); await save();
		expect(again.box.value).toBe('reload me'); expect(again.onSend).not.toHaveBeenCalled();
		expect(getDraft('u1', 3)?.replySeq).toBe(4);
		await fireEvent.click(again.send);
		expect(again.onSend).toHaveBeenCalledWith('reload me', 4);
	});
	it('clears the record when text empties, including after send', async () => {
		const first = mount(); await type(first.box, 'erase'); await save();
		expect(getDraft('u1', 3)?.text).toBe('erase');
		await type(first.box, ''); expect(getDraft('u1', 3)).toBeNull();
		await type(first.box, 'send'); await save(); await fireEvent.click(first.send);
		expect(getDraft('u1', 3)).toBeNull();
	});
	it('teardown and debounce after sign-out cannot write the draft back', async () => {
		const first = mount(); await type(first.box, 'private'); await save();
		expect(getDraft('u1', 3)?.text).toBe('private');
		await confirmSignOut(); first.unmount(); await save();
		expect(sessionStorage.length).toBe(0); expect(getDraft('u1', 3)).toBeNull();
	});
	it('refuses a stored conversation-id mismatch', async () => {
		sessionStorage.setItem('pad:chat-draft:u1:3', JSON.stringify({ userId: 'u1', n: 99, text: 'wrong conversation', replySeq: null }));
		const r = mount(); await tick(); expect(r.box.value).toBe(''); expect(r.onSend).not.toHaveBeenCalled();
	});
	it('shows the persisted notice', async () => {
		const r = mount(); await type(r.box, 'keep'); await save();
		expect(r.container.textContent).toContain('Draft kept in this tab');
	});
	it('shows the memory fallback notice and restores the memory draft', async () => {
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw Error('blocked'); });
		const r = mount(); await type(r.box, 'memory'); await save();
		expect(r.container.textContent).toContain('Draft kept until this page reloads');
		r.unmount(); const again = mount(); await tick(); expect(again.box.value).toBe('memory');
	});
	it('persists a reply change even when the text is unchanged', async () => {
		const r = mount(); await type(r.box, 'reply later'); await save();
		await r.rerender({ n: 3, onSend: r.onSend, onClearReply: r.onClearReply, replyTo: target });
		await save(); expect(getDraft('u1', 3)?.replySeq).toBe(4);
	});
});

it('refreshes a restored reply preview from authorized messages and allows cancelling it', async () => {
	sessionStorage.setItem('pad:chat-draft:u1:3', JSON.stringify({ userId: 'u1', n: 3, text: 'reply draft', replySeq: 4 }));
	const r = mount({ messages: [target] }); await tick();
	expect(r.container.querySelector('.reply')?.textContent).toContain('the target');
	await fireEvent.click(r.getByRole('button', { name: 'Cancel reply' }));
	await fireEvent.click(r.send);
	expect(r.onSend).toHaveBeenCalledWith('reply draft', undefined);
});
