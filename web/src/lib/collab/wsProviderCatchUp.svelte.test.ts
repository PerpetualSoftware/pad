import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import * as decoding from 'lib0/decoding';
import { BARRIER_IDLE_MS, CollabProvider } from './wsProvider.svelte';

// BUG-3523: the reconnect catch-up (the WHOLE document as one update) is sent
// only when something of this tab's can be missing on the server: an edit made
// while no socket was open, or a local update sent on a socket that then
// closed. It used to be sent on every reconnect, appending a full copy of the
// document to the op-log on every laptop wake and network blip.

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

describe('the reconnect catch-up is sent only when something can be missing (BUG-3523)', () => {
	it('a reconnect after a socket that carried nothing of ours sends no catch-up', async () => {
		const first = connect();
		const second = await reconnect(first);
		expect(syncSubtypes(second), 'the sync step 1 still goes out').toContain(0);
		expect(catchUps(second)).toBe(0);
	});

	it('a local update sent on the socket that died owes the next one the catch-up', async () => {
		const first = connect();
		doc.getText('t').insert(0, 'typed while connected');
		expect(catchUps(first), 'the live edit went out as an update').toBe(1);
		const second = await reconnect(first);
		expect(catchUps(second)).toBe(1);
	});

	it('an edit made while no socket was open is caught up', async () => {
		const first = connect();
		first.readyState = 3;
		doc.getText('t').insert(0, 'typed offline');
		expect(provider.unsentLocalEdits).toBe(true);
		const second = await reconnect(first);
		expect(catchUps(second)).toBe(1);
		expect(provider.unsentLocalEdits).toBe(false);
	});

	it('with no barrier answer (an older server) a catch-up sent on a socket that also died is owed again, every time', async () => {
		const first = connect();
		doc.getText('t').insert(0, 'x');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		expect(barriers(first), 'the barrier went out once the sends went quiet').toHaveLength(1);
		const second = await reconnect(first);
		expect(catchUps(second)).toBe(1);
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const third = await reconnect(second);
		expect(catchUps(third), 'unanswered, the catch-up on second is still owed').toBe(1);
		const fourth = await reconnect(third);
		expect(catchUps(fourth)).toBe(1);
	});
});

describe('the barrier clears what the socket carried (BUG-3523)', () => {
	it('one barrier per burst, sent only once the local sends go quiet', async () => {
		const first = connect();
		for (let i = 0; i < 10; i++) {
			doc.getText('t').insert(0, String(i));
			await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS / 4);
		}
		expect(barriers(first), 'no barrier while typing').toEqual([]);
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		expect(barriers(first)).toEqual([1]);
	});

	it('an OK ack covering every local send breaks the chain: the next reconnect sends no catch-up', async () => {
		const first = connect();
		doc.getText('t').insert(0, 'x');
		const second = await reconnect(first);
		expect(catchUps(second)).toBe(1);
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const [n] = barriers(second);
		second.control({ type: 'barrier_ack', n, ok: true });
		const third = await reconnect(second);
		expect(catchUps(third)).toBe(0);
	});

	it('ok=false resends the document on the live socket at once, and names the item', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const first = connect();
		doc.getText('t').insert(0, 'x');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const [n] = barriers(first);
		const before = catchUps(first);
		first.control({ type: 'barrier_ack', n, ok: false });
		expect(warn.mock.calls.some((c) => String(c[0]).includes('item-3523'))).toBe(true);
		expect(catchUps(first), 'the catch-up went out on the same socket').toBe(before + 1);
		// The resend gets its own barrier; until that is answered OK the tab
		// still owes the next connection.
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const again = barriers(first);
		expect(again).toHaveLength(2);
		const second = await reconnect(first);
		expect(catchUps(second), 'unanswered, the resend is still owed').toBe(1);
		warn.mockRestore();
	});

	it('after an ok=false resend is acknowledged, nothing is owed', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const first = connect();
		doc.getText('t').insert(0, 'x');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		first.control({ type: 'barrier_ack', n: barriers(first)[0], ok: false });
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		first.control({ type: 'barrier_ack', n: barriers(first)[1], ok: true });
		const second = await reconnect(first);
		expect(catchUps(second)).toBe(0);
		warn.mockRestore();
	});

	it('an ack for a barrier that a later local send outran does not clear it', async () => {
		const first = connect();
		doc.getText('t').insert(0, 'x');
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		const [n] = barriers(first);
		doc.getText('t').insert(0, 'y'); // sent after the barrier
		first.control({ type: 'barrier_ack', n, ok: true });
		const second = await reconnect(first);
		expect(catchUps(second)).toBe(1);
	});

	it('an ack arriving on a socket that is no longer current is ignored', async () => {
		const first = connect();
		doc.getText('t').insert(0, 'x');
		const second = await reconnect(first);
		await vi.advanceTimersByTimeAsync(BARRIER_IDLE_MS);
		// Only second's barrier is pending; an ack for its n arriving on the
		// old socket says nothing about what second carried.
		const [n] = barriers(second);
		first.control({ type: 'barrier_ack', n, ok: true });
		const third = await reconnect(second);
		expect(catchUps(third), 'second carried the catch-up and nothing acked it').toBe(1);
	});
});
