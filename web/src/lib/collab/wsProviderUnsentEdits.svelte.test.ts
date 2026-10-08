import { describe, expect, it } from 'vitest';
import * as Y from 'yjs';
import { CollabProvider } from './wsProvider.svelte';

// TASK-2199: the provider says when this tab holds local edits the server has
// not been sent, so the page can warn before the tab closes. Set when a local
// update cannot be written (closed socket, or buffered before the anchor);
// cleared only when the local state is written to an open socket.

class FakeSocket extends EventTarget {
	// The provider compares readyState with WebSocketImpl.OPEN.
	static readonly CONNECTING = 0;
	static readonly OPEN = 1;
	static readonly CLOSING = 2;
	static readonly CLOSED = 3;
	static last: FakeSocket | null = null;
	binaryType = 'arraybuffer';
	readyState = 1;
	sent: unknown[] = [];
	constructor(public url: string) {
		super();
		FakeSocket.last = this;
	}
	send(data: unknown) {
		this.sent.push(data);
	}
	close() {
		this.readyState = 3;
	}
	control(msg: object) {
		this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(msg) }));
	}
}

function makeProvider() {
	const doc = new Y.Doc();
	const provider = new CollabProvider('item-2199', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-2199',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
	});
	const sock = FakeSocket.last!;
	sock.dispatchEvent(new Event('open'));
	return { provider, sock, doc };
}

const type = (doc: Y.Doc, text: string) => doc.getText('t').insert(0, text);

describe('CollabProvider unsentLocalEdits', () => {
	it('stays clear while edits go out on an open, anchored socket', () => {
		const { provider, sock, doc } = makeProvider();
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		type(doc, 'a');
		expect(provider.unsentLocalEdits).toBe(false);
		provider.destroy();
	});

	it('sets on a local edit while the socket is closed, and clears on the catch-up after it reopens', () => {
		const { provider, sock, doc } = makeProvider();
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		sock.readyState = 3; // the connection dropped
		type(doc, 'offline');
		expect(provider.unsentLocalEdits).toBe(true);
		type(doc, ' more');
		expect(provider.unsentLocalEdits).toBe(true);
		const before = sock.sent.length;
		sock.readyState = 1;
		sock.dispatchEvent(new Event('open')); // reconnected: the catch-up frame goes out
		expect(sock.sent.length).toBeGreaterThan(before);
		expect(provider.unsentLocalEdits).toBe(false);
		provider.destroy();
	});

	it('sets while buffered before the anchor, and clears when the anchor flushes the buffer', () => {
		const { provider, sock, doc } = makeProvider();
		type(doc, 'early');
		expect(provider.unsentLocalEdits).toBe(true);
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		expect(provider.unsentLocalEdits).toBe(false);
		provider.destroy();
	});

	it('a remote update is not a local edit', () => {
		const { provider, sock, doc } = makeProvider();
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		sock.readyState = 3;
		const other = new Y.Doc();
		other.getText('t').insert(0, 'from a peer');
		Y.applyUpdate(doc, Y.encodeStateAsUpdate(other), provider);
		expect(provider.unsentLocalEdits).toBe(false);
		provider.destroy();
	});
});
