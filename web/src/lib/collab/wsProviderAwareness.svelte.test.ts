import { afterEach, describe, expect, it } from 'vitest';
import * as Y from 'yjs';
import * as awarenessProtocol from 'y-protocols/awareness';
import * as encoding from 'lib0/encoding';
import * as decoding from 'lib0/decoding';
import { CollabProvider } from './wsProvider.svelte';

// TASK-2206: the provider sends only its OWN client's awareness, so the server
// can tell whose presence a connection carries and remove it when the
// connection drops; and it re-announces on connect at a NEW clock, so after the
// server has removed it (at its last clock + 1) peers accept it straight away.

class FakeSocket extends EventTarget {
	static OPEN = 1;
	static last: FakeSocket | null = null;
	binaryType = 'arraybuffer';
	readyState = 1;
	sent: Uint8Array[] = [];
	constructor(public url: string) {
		super();
		FakeSocket.last = this;
	}
	send(data: Uint8Array) {
		this.sent.push(data);
	}
	close() {
		this.readyState = 3;
	}
}

const cleanups: Array<() => void> = [];
afterEach(() => {
	while (cleanups.length) cleanups.pop()!();
});

function makeProvider() {
	const doc = new Y.Doc();
	const provider = new CollabProvider('item-2206', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-2206',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
	});
	cleanups.push(() => provider.destroy());
	return { provider, doc, sock: FakeSocket.last! };
}

/** The client IDs and clocks in every awareness frame sent. */
function awarenessSent(sock: FakeSocket): Array<{ clientID: number; clock: number }> {
	const out: Array<{ clientID: number; clock: number }> = [];
	for (const frame of sock.sent) {
		const d = decoding.createDecoder(frame);
		if (decoding.readVarUint(d) !== 1) continue;
		const u = decoding.createDecoder(decoding.readVarUint8Array(d));
		const n = decoding.readVarUint(u);
		for (let i = 0; i < n; i++) {
			out.push({ clientID: decoding.readVarUint(u), clock: decoding.readVarUint(u) });
			decoding.readVarString(u);
		}
	}
	return out;
}

describe('CollabProvider awareness (TASK-2206)', () => {
	it('announces itself on connect at a new clock', () => {
		const { provider, doc, sock } = makeProvider();
		provider.awareness.setLocalState({ user: { name: 'Ada' } });
		const before = provider.awareness.meta.get(doc.clientID)!.clock;
		sock.sent.length = 0;
		sock.dispatchEvent(new Event('open'));
		const mine = awarenessSent(sock).filter((e) => e.clientID === doc.clientID);
		expect(mine.length).toBeGreaterThan(0);
		expect(mine[mine.length - 1]!.clock).toBeGreaterThan(before);
	});

	it("never re-sends a peer's awareness it received", () => {
		const { doc, sock } = makeProvider();
		sock.dispatchEvent(new Event('open'));
		const peerDoc = new Y.Doc();
		peerDoc.clientID = 4242;
		const peer = new awarenessProtocol.Awareness(peerDoc);
		cleanups.push(() => peer.destroy());
		peer.setLocalState({ user: { name: 'Peer' } });
		const enc = encoding.createEncoder();
		encoding.writeVarUint(enc, 1);
		encoding.writeVarUint8Array(enc, awarenessProtocol.encodeAwarenessUpdate(peer, [4242]));
		sock.sent.length = 0;
		sock.dispatchEvent(new MessageEvent('message', { data: encoding.toUint8Array(enc).buffer }));

		const ids = awarenessSent(sock).map((e) => e.clientID);
		expect(ids).not.toContain(4242);
		expect(ids.every((id) => id === doc.clientID)).toBe(true);
	});
});
