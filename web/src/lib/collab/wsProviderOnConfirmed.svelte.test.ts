import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import { BARRIER_IDLE_MS, CollabProvider } from './wsProvider.svelte';

/**
 * BUG-3850: onConfirmed fires the moment a barrier ack confirms every local
 * edit, so the crash draft's base is taken before any later keystroke joins
 * the document. Never while a later send is still unconfirmed, and never on a
 * failed append.
 */
class FakeSocket extends EventTarget {
	static readonly CONNECTING = 0;
	static readonly OPEN = 1;
	static readonly CLOSING = 2;
	static readonly CLOSED = 3;
	static all: FakeSocket[] = [];
	binaryType = 'arraybuffer';
	readyState = 1;
	sent: (Uint8Array | string)[] = [];
	constructor(public url: string) {
		super();
		FakeSocket.all.push(this);
	}
	send(data: Uint8Array | string) {
		this.sent.push(data);
	}
	close() {
		this.readyState = 3;
	}
	control(msg: object) {
		this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(msg) }));
	}
}

function lastBarrier(sock: FakeSocket): number {
	const ns = sock.sent.flatMap((f) => (typeof f === 'string' && JSON.parse(f).type === 'barrier' ? [JSON.parse(f).n as number] : []));
	return ns[ns.length - 1];
}

let provider: CollabProvider;
let doc: Y.Doc;
let calls: boolean[];

function connect(onConfirmed?: () => void) {
	doc = new Y.Doc();
	calls = [];
	provider = new CollabProvider('item-3850', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-3850',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
		// Records what the provider reports at the call: the base is read then.
		onConfirmed: onConfirmed ?? (() => calls.push(provider.editsMayBeMissing)),
	});
	const sock = FakeSocket.all.at(-1)!;
	sock.dispatchEvent(new Event('open'));
	sock.control({ type: 'op_log_cursor', op_log_id: 1 });
	return sock;
}

beforeEach(() => {
	vi.useFakeTimers();
	FakeSocket.all = [];
});
afterEach(() => {
	provider?.destroy();
	vi.useRealTimers();
});

describe('BUG-3850: onConfirmed', () => {
	it('fires on the ack that confirms every local edit, with nothing missing', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		expect(calls).toEqual([]);
		sock.control({ type: 'barrier_ack', n: lastBarrier(sock), ok: true });
		expect(calls).toEqual([false]);
	});

	it('does not fire when a later send is still awaiting its own barrier', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const n = lastBarrier(sock);
		doc.getText('t').insert(5, ' more');
		sock.control({ type: 'barrier_ack', n, ok: true });
		expect(calls, 'the doc holds unconfirmed text: no base now').toEqual([]);
	});

	it('does not fire when an edit made while the socket was closing never went out', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const n = lastBarrier(sock);
		// CLOSING: the edit is kept as unsent, but a message already received
		// on this socket (the ack) can still be dispatched.
		sock.readyState = 2;
		doc.getText('t').insert(5, ' unsent');
		sock.control({ type: 'barrier_ack', n, ok: true });
		expect(provider.editsMayBeMissing, 'premise: the unsent edit is still missing').toBe(true);
		expect(calls).toEqual([]);
	});

	it('does not fire on a failed append', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		sock.control({ type: 'barrier_ack', n: lastBarrier(sock), ok: false });
		expect(calls).toEqual([]);
	});

	it('a throwing handler does not break the ack', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const sock = connect(() => {
			throw new Error('boom');
		});
		doc.getText('t').insert(0, 'typed');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		sock.control({ type: 'barrier_ack', n: lastBarrier(sock), ok: true });
		expect(provider.editsMayBeMissing).toBe(false);
		warn.mockRestore();
	});
});

// Lead ruling baa63dc3: remote updates alone move remoteDocVersion, so a crash
// draft can tell its base predates a collaborator's change.
describe('BUG-3850: remoteDocVersion', () => {
	it('moves on an update applied from a server message (origin = the provider), never on a local edit', () => {
		connect();
		const before = { all: provider.docVersion, remote: provider.remoteDocVersion };
		doc.getText('t').insert(0, 'local');
		expect(provider.docVersion).toBe(before.all + 1);
		expect(provider.remoteDocVersion, 'a local edit').toBe(before.remote);
		const peer = new Y.Doc();
		peer.getText('t').insert(0, 'theirs');
		Y.applyUpdate(doc, Y.encodeStateAsUpdate(peer), provider);
		expect(provider.docVersion).toBe(before.all + 2);
		expect(provider.remoteDocVersion, 'a remote apply').toBe(before.remote + 1);
	});
});

