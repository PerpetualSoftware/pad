import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import * as decoding from 'lib0/decoding';
import { BARRIER_IDLE_MS, CollabProvider } from './wsProvider.svelte';

/**
 * BUG-3556: closing or leaving the page asks first when typing may not have
 * reached the server. The prompts read `editsAtRiskOnClose`, which covers two
 * cases the old `unsentLocalEdits` read missed: typing sent on a socket that
 * closed before the server confirmed it, and a resend after the server
 * reported a failed op-log append. An ordinary send awaiting its barrier is
 * not at risk (asking then would fire on every keystroke).
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
	/** The connection dropped: closed, and the close event fired. */
	drop() {
		this.readyState = 3;
		this.dispatchEvent(new CloseEvent('close'));
	}
}

/** Sync frames this socket sent, by y-protocols subtype (0 step1, 1 step2, 2 update). */
function syncSubtypes(sock: FakeSocket): number[] {
	return sock.sent.flatMap((frame) => {
		if (typeof frame === 'string') return [];
		const d = decoding.createDecoder(frame);
		if (decoding.readVarUint(d) !== 0) return [];
		return [decoding.readVarUint(d)];
	});
}
const catchUps = (sock: FakeSocket) => syncSubtypes(sock).filter((t) => t === 2).length;
/** The `barrier` control frames this socket sent, by n. */
function barriers(sock: FakeSocket): number[] {
	return sock.sent.flatMap((frame) => {
		if (typeof frame !== 'string') return [];
		const msg = JSON.parse(frame);
		return msg.type === 'barrier' ? [msg.n] : [];
	});
}

let provider: CollabProvider;
let doc: Y.Doc;

function connect() {
	doc = new Y.Doc();
	provider = new CollabProvider('item-3523', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-3523',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
	});
	const sock = FakeSocket.all.at(-1)!;
	sock.dispatchEvent(new Event('open'));
	sock.control({ type: 'op_log_cursor', op_log_id: 1 }); // anchored
	return sock;
}

/** Drop the socket and let the provider reconnect; the new socket is open. */
async function reconnect(prev: FakeSocket): Promise<FakeSocket> {
	const before = FakeSocket.all.length;
	prev.drop();
	await vi.advanceTimersByTimeAsync(60_000);
	const next = FakeSocket.all.at(-1)!;
	expect(FakeSocket.all.length, 'the provider reconnected').toBeGreaterThan(before);
	next.dispatchEvent(new Event('open'));
	return next;
}

beforeEach(() => {
	vi.useFakeTimers();
	FakeSocket.all = [];
});
afterEach(() => {
	provider?.destroy();
	vi.useRealTimers();
});


/** The barrier the provider sent last on this socket. */
function lastBarrier(sock: FakeSocket): number {
	const ns = barriers(sock);
	return ns[ns.length - 1];
}

describe('BUG-3556: what closing the page would lose', () => {
	it('an ordinary send awaiting its barrier is not at risk, and the barrier clears it', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed');
		expect(provider.editsMayBeMissing, 'unconfirmed, as always for a moment').toBe(true);
		expect(provider.editsAtRiskOnClose, 'but not worth a prompt').toBe(false);
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		sock.control({ type: 'barrier_ack', n: lastBarrier(sock), ok: true });
		expect(provider.editsMayBeMissing).toBe(false);
		expect(provider.editsAtRiskOnClose).toBe(false);
	});

	it('typing sent on a socket that closed before the server confirmed it is at risk', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed, then the socket died');
		sock.drop();
		expect(provider.unsentLocalEdits, 'the old prompt read missed it').toBe(false);
		expect(provider.editsAtRiskOnClose).toBe(true);
	});

	it('a failed op-log append is at risk until the resend is confirmed', async () => {
		const sock = connect();
		doc.getText('t').insert(0, 'typed');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		sock.control({ type: 'barrier_ack', n: lastBarrier(sock), ok: false });
		expect(provider.unsentLocalEdits, 'the resend cleared the old flag').toBe(false);
		expect(provider.editsAtRiskOnClose, 'the resend is not confirmed').toBe(true);
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		sock.control({ type: 'barrier_ack', n: lastBarrier(sock), ok: true });
		expect(provider.editsAtRiskOnClose, 'confirmed').toBe(false);
	});

	it('typing while no socket was open is at risk, as before', () => {
		const sock = connect();
		sock.readyState = 3;
		doc.getText('t').insert(0, 'typed offline');
		expect(provider.editsAtRiskOnClose).toBe(true);
	});
});
