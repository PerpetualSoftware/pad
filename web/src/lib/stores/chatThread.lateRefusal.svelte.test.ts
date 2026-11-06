// BUG-4341 (TASK-4315 PR 3b): a definite refusal that arrives after the composer
// that sent it is gone. Every path that unmounts the Composer must either
// invalidate the thread guard (the draft is written) or find the live composer
// (the text is restored into it). None may drop the text, and a late refusal
// never lands in another conversation's draft.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import type { ChatMessage } from '$lib/types';

const h = vi.hoisted(() => ({
	getConversation: vi.fn(),
	listMessages: vi.fn(),
	markRead: vi.fn(),
	sendMessage: vi.fn(),
	deleteMessage: vi.fn(),
	state: { epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

class NotFound extends Error {
	code = 'not_found';
}

vi.mock('$lib/api/client', () => ({
	api: {
		chat: {
			getConversation: h.getConversation,
			listMessages: h.listMessages,
			markRead: h.markRead,
			sendMessage: h.sendMessage,
			deleteMessage: h.deleteMessage
		}
	},
	isNotFoundError: (e: unknown) => e instanceof NotFound,
	isReplyTargetDeletedError: (e: unknown) => (e as { code?: string })?.code === 'reply_target_deleted',
	isReplyTargetNotFoundError: (e: unknown) => (e as { code?: string })?.code === 'reply_target_not_found'
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'u1',
		authenticated: true,
		identityFence() {
			const at = h.state.epoch;
			return () => h.state.epoch === at;
		},
		onIdentityChange(fn: (prev: string) => void) {
			h.state.listeners.push(fn);
			return () => {
				const i = h.state.listeners.indexOf(fn);
				if (i >= 0) h.state.listeners.splice(i, 1);
			};
		}
	}
}));

import Composer from '$lib/components/chat/Composer.svelte';
import { getDraft, setDraft, savePending, listPending, purgeAll, unsealChatRecords } from '$lib/chat/chatRecords';
import { chatStore } from './chat.svelte';
import { ChatThread, type SendOutcome } from './chatThread.svelte';

function msg(seq: number): ChatMessage {
	return {
		id: `m${seq}`, conversation_id: 'c1', seq, author_agent_key: 'agent-1', author_is_human: false, mine: true,
		source: 'cli', body: `body ${seq}`, created_at: '2026-10-26T12:00:00.000Z', deleted: false
	};
}
const summary = (number: number) => ({
	id: `c${number}`, number, kind: 'direct', title: '', agent_key: 'agent-1', last_seq: 0, created_at: '', updated_at: '',
	read_seq: 0, unread: 0
});
const emptyPage = { messages: [], oldest_seq: 0, newest_seq: 0, last_seq: 0, read_seq: 0, has_more: false, refs: [], refs_truncated: false };

async function flush() {
	for (let i = 0; i < 12; i++) {
		await Promise.resolve();
		flushSync();
	}
}

let threads: ChatThread[] = [];
function newThread(): ChatThread {
	const t = new ChatThread();
	threads.push(t);
	return t;
}
let resync: ReturnType<typeof vi.spyOn>;
let reject!: (reason: unknown) => void;
let answer!: (res: object) => void;

/** Mount a composer wired to `thread` for conversation `n`, as ChatThreadView does. */
function mountComposer(thread: ChatThread, n: number) {
	const r = render(Composer, {
		props: { n, onClearReply: () => {}, onSend: (b: string, reply?: number) => thread.send(b, reply) } as never
	});
	return {
		...r,
		box: r.container.querySelector('textarea')!,
		send: r.container.querySelector<HTMLButtonElement>('button.send')!
	};
}
type Mounted = ReturnType<typeof mountComposer>;
async function typeAndSend(m: Mounted, text: string) {
	await fireEvent.input(m.box, { target: { value: text } });
	flushSync();
	await fireEvent.click(m.send);
	await flush();
}

beforeEach(() => {
	h.getConversation.mockReset();
	h.listMessages.mockReset();
	h.markRead.mockReset();
	h.markRead.mockResolvedValue({ read_seq: 0 });
	h.sendMessage.mockReset();
	h.deleteMessage.mockReset();
	h.state.epoch = 0;
	purgeAll();
	unsealChatRecords();
	sessionStorage.clear();
	h.getConversation.mockImplementation(async (n: number) => summary(n));
	h.listMessages.mockImplementation(async () => ({ ...emptyPage }));
	h.sendMessage.mockReturnValue(new Promise((yes, no) => { answer = yes; reject = no; }));
	resync = vi.spyOn(chatStore, 'resync').mockImplementation(() => {});
	threads = [];
});
afterEach(() => {
	cleanup();
	for (const t of threads) t.dispose();
	resync.mockRestore();
});

/** Start a send of `body` in conversation 1 from a mounted composer; the POST stays open. */
async function sendInFlight(thread: ChatThread, body = 'refused body') {
	await thread.open(1);
	const m = mountComposer(thread, 1);
	await typeAndSend(m, body);
	expect(h.sendMessage).toHaveBeenCalledTimes(1);
	return m;
}

describe('a late refusal after the composer is unmounted', () => {
	// The ChatThreadView of a closed surface runs onDestroy -> thread.dispose().
	it.each(['dock close', 'drawer close', 'shallow pop'])('%s then refusal writes the draft', async () => {
		const thread = newThread();
		const m = await sendInFlight(thread);
		m.unmount();
		thread.dispose();
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(getDraft('u1', 1)).toMatchObject({ text: 'refused body' });
		expect(listPending('u1', 1)).toEqual([]);
	});

	it('reopen before refusal restores into the live composer, teardown save keeps it', async () => {
		const first = newThread();
		const m1 = await sendInFlight(first);
		m1.unmount();
		first.dispose();
		const second = newThread();
		await second.open(1);
		const m2 = mountComposer(second, 1);
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(m2.box.value).toBe('refused body');
		m2.unmount(); // the composer's own destroy save must not lose it
		expect(getDraft('u1', 1)).toMatchObject({ text: 'refused body' });
	});

	it('a refusal merges ahead of text typed in the live composer', async () => {
		const first = newThread();
		const m1 = await sendInFlight(first);
		m1.unmount();
		first.dispose();
		const second = newThread();
		await second.open(1);
		const m2 = mountComposer(second, 1);
		await fireEvent.input(m2.box, { target: { value: 'typed since' } });
		flushSync();
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(m2.box.value).toBe('refused body\ntyped since');
	});

	it('n change A->B->A in one view restores into the remounted composer', async () => {
		const thread = newThread();
		const a1 = await sendInFlight(thread);
		a1.unmount();
		await thread.open(2);
		const b = mountComposer(thread, 2);
		b.unmount();
		await thread.open(1);
		const a2 = mountComposer(thread, 1);
		reject({ code: 'reply_target_not_found' });
		await flush();
		expect(a2.box.value).toBe('refused body');
		a2.unmount();
		expect(getDraft('u1', 1)).toMatchObject({ text: 'refused body' });
		expect(getDraft('u1', 2)).toBeNull();
	});

	it('never merges a late refusal into another conversation draft or composer', async () => {
		const thread = newThread();
		const a = await sendInFlight(thread);
		a.unmount();
		await thread.open(2);
		const b = mountComposer(thread, 2);
		await fireEvent.input(b.box, { target: { value: 'B typing' } });
		flushSync();
		setDraft('u1', 2, 'B private draft', 8);
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(b.box.value).toBe('B typing');
		expect(getDraft('u1', 2)?.text).toBe('B private draft');
		expect(getDraft('u1', 1)).toMatchObject({ text: 'refused body' });
	});

	it('direct entry / reload lists the stored attempt as not confirmed', async () => {
		savePending('u1', 1, 'stored-key', 'stored body', 4);
		const thread = newThread();
		await thread.open(1);
		expect(thread.items).toEqual([
			{ kind: 'pending', pending: { op_key: 'stored-key', body: 'stored body', reply_to_seq: 4, status: 'unconfirmed' } }
		]);
	});

	it('sign-out writes nothing', async () => {
		const thread = newThread();
		const m = await sendInFlight(thread);
		m.unmount();
		h.state.epoch++;
		for (const fn of [...h.state.listeners]) fn('u1');
		purgeAll();
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(getDraft('u1', 1)).toBeNull();
		expect(listPending('u1', 1)).toEqual([]);
	});

	it('notFound unmount keeps refused body', async () => {
		const thread = newThread();
		const m = await sendInFlight(thread);
		h.deleteMessage.mockRejectedValue(new NotFound('gone'));
		expect(await thread.remove(1)).toMatchObject({ ok: false });
		expect(thread.status).toBe('notFound');
		m.unmount(); // the shell shows "Conversation not found" and drops the composer
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(getDraft('u1', 1)).toMatchObject({ text: 'refused body' });
	});

	it('control: a refusal in the still-mounted composer is returned to it, not written', async () => {
		const thread = newThread();
		const m = await sendInFlight(thread);
		reject({ code: 'reply_target_deleted' });
		await flush();
		expect(m.box.value).toBe('refused body');
	});
});

describe('a send still in flight when the reader comes back', () => {
	it('A->B->A shows no Send-not-confirmed bubble, and none after success', async () => {
		const thread = newThread();
		await thread.open(1);
		const sending: Promise<SendOutcome> = thread.send('in flight');
		await thread.open(2);
		await thread.open(1);
		expect(thread.items.filter((i) => i.kind === 'pending')).toEqual([]);
		answer({ message: msg(1), replayed: false });
		expect(await sending).toBe('stale');
		expect(thread.items.filter((i) => i.kind === 'pending')).toEqual([]);
		expect(listPending('u1', 1)).toEqual([]);
	});

	it('a settled attempt is listed again by the next open (the in-flight mark is released)', async () => {
		const thread = newThread();
		await thread.open(1);
		const sending = thread.send('network down');
		reject(new Error('network'));
		expect(await sending).toBe('failed');
		const again = newThread();
		await again.open(1);
		expect(again.items.filter((i) => i.kind === 'pending')).toHaveLength(1);
	});
});
