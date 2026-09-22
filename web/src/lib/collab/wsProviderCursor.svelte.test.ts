import { describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import { CollabProvider } from './wsProvider.svelte';

// BUG-3124 unit B: the provider tells the page when the op-log cursor ADVANCES,
// because a cursor advance with no document change (a reconnect replaying ops
// this tab already has) fires no editor update and so would never reach the
// flusher. This pins the provider half of that wiring against the real frame
// handler, through a fake socket.

class FakeSocket extends EventTarget {
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

function makeProvider(onOpLogCursor: (id: number) => void) {
	const provider = new CollabProvider('item-3124', new Y.Doc(), {
		url: 'ws://test.invalid/api/v1/collab/item-3124',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
		onOpLogCursor,
	});
	const sock = FakeSocket.last!;
	sock.dispatchEvent(new Event('open'));
	return { provider, sock };
}

describe('CollabProvider onOpLogCursor', () => {
	it('fires on every cursor ADVANCE and not on a frame that does not advance', () => {
		const seen: number[] = [];
		const { provider, sock } = makeProvider((id) => seen.push(id));
		sock.control({ type: 'op_log_cursor', op_log_id: 5 });
		sock.control({ type: 'op_log_cursor', op_log_id: 9 });
		sock.control({ type: 'op_log_cursor', op_log_id: 9 }); // repeat: no advance
		sock.control({ type: 'op_log_cursor', op_log_id: 7 }); // regression: never adopted
		expect(seen).toEqual([5, 9]);
		expect(provider.lastOpLogID).toBe(9);
		provider.destroy();
	});

	it('a throwing handler does not break the frame loop', () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const handler = vi.fn(() => {
			throw new Error('page bug');
		});
		const { provider, sock } = makeProvider(handler);
		sock.control({ type: 'op_log_cursor', op_log_id: 3 });
		sock.control({ type: 'op_log_cursor', op_log_id: 4 });
		expect(handler).toHaveBeenCalledTimes(2);
		expect(provider.lastOpLogID).toBe(4);
		provider.destroy();
		warn.mockRestore();
	});
});
