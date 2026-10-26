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

import Composer from './Composer.svelte';
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

beforeEach(() => {
	h.userId = 'u1';
	sessionStorage.clear();
});
afterEach(() => {
	cleanup();
	Reflect.deleteProperty(window, 'visualViewport');
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

	it('shows no count for a short body', async () => {
		const { box, container } = mount();
		await type(box, 'hello');
		expect(container.querySelector('.count')).toBeNull();
	});
});

describe('sending', () => {
	it('sends the text with the reply target, clears the box and the reply on success', async () => {
		const { box, send, onSend, onClearReply } = mount({ replyTo: target });
		await type(box, 'hello');
		await fireEvent.click(send);
		await tick();
		expect(onSend).toHaveBeenCalledWith('hello', 4);
		expect(box.value).toBe('');
		expect(onClearReply).toHaveBeenCalledTimes(1);
	});

	it('Ctrl+Enter sends', async () => {
		const { box, onSend } = mount();
		await type(box, 'hi');
		await fireEvent.keyDown(box, { key: 'Enter', ctrlKey: true });
		expect(onSend).toHaveBeenCalledWith('hi', undefined);
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
describe('keyboard offset', () => {
	function fakeViewport(height: number, offsetTop = 0) {
		const vv = Object.assign(new EventTarget(), { height, offsetTop });
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		return vv;
	}

	it('lifts the composer by the covered height on a viewport resize', async () => {
		const vv = fakeViewport(window.innerHeight);
		const { container } = mount();
		const form = container.querySelector<HTMLElement>('form.composer')!;
		expect(form.style.marginBottom).toBe('');
		vv.height = window.innerHeight - 300;
		vv.dispatchEvent(new Event('resize'));
		flushSync();
		expect(form.style.marginBottom).toBe('300px');
		vv.height = window.innerHeight;
		vv.dispatchEvent(new Event('resize'));
		flushSync();
		expect(form.style.marginBottom).toBe('');
	});

	// Red if the handler is not removed on destroy (B6).
	it('removes its viewport listeners when destroyed', () => {
		const vv = fakeViewport(window.innerHeight);
		const add = vi.spyOn(vv, 'addEventListener');
		const remove = vi.spyOn(vv, 'removeEventListener');
		const { unmount } = mount();
		expect(add.mock.calls.map((c) => c[0]).sort()).toEqual(['resize', 'scroll']);
		unmount();
		expect(remove.mock.calls.map((c) => c[0]).sort()).toEqual(['resize', 'scroll']);
		for (const [i, c] of add.mock.calls.entries()) expect(remove.mock.calls[i][1]).toBe(c[1]);
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
		expect(sessionStorage.length).toBe(0);
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

	it('stops keeping once the composer is gone', async () => {
		const first = mount();
		await type(first.box, 'x');
		first.unmount();
		runSessionExpiryKeepers();
		expect(sessionStorage.length).toBe(0);
	});
});
