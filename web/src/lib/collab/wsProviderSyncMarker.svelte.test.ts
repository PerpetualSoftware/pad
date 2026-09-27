import { afterEach, describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import { CollabProvider } from './wsProvider.svelte';

// BUG-3240: the connection's first op_log_cursor frame (written by the relay
// after every replay frame) completes the sync and carries the seeder grant; a
// 10s safety net covers a frame that never comes and reports itself.

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
(FakeSocket as unknown as { OPEN: number }).OPEN = 1;

function open() {
	const provider = new CollabProvider('item-3240', new Y.Doc(), {
		url: 'ws://test.invalid/api/v1/collab/item-3240',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
	});
	const sock = FakeSocket.last!;
	sock.dispatchEvent(new Event('open'));
	return { provider, sock };
}

afterEach(() => {
	vi.useRealTimers();
});

describe('CollabProvider sync completion (BUG-3240)', () => {
	it('completes on the first cursor frame, with no timer', () => {
		vi.useFakeTimers();
		const { provider, sock } = open();
		expect(provider.replayComplete).toBe(false);
		expect(provider.synced).toBe(false);
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		expect(provider.replayComplete).toBe(true);
		expect(provider.synced).toBe(true);
		expect(provider.seedGranted).toBe(false);
		expect(provider.seedByElection).toBe(false);
		provider.destroy();
	});

	it('does NOT complete on a timer shorter than the safety net', () => {
		vi.useFakeTimers();
		const { provider } = open();
		vi.advanceTimersByTime(9_000);
		expect(provider.replayComplete).toBe(false);
		expect(provider.synced).toBe(false);
		provider.destroy();
	});

	it('takes the grant from the initial cursor frame', () => {
		const { provider, sock } = open();
		sock.control({ type: 'op_log_cursor', op_log_id: 4, seed: true });
		expect(provider.seedGranted).toBe(true);
		provider.destroy();
	});

	it('only the FIRST cursor frame can grant', () => {
		const { provider, sock } = open();
		sock.control({ type: 'op_log_cursor', op_log_id: 4 });
		sock.control({ type: 'op_log_cursor', op_log_id: 5, seed: true });
		expect(provider.seedGranted).toBe(false);
		provider.destroy();
	});

	it('takes a later grant from seed_grant', () => {
		const { provider, sock } = open();
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		sock.control({ type: 'seed_grant' });
		expect(provider.seedGranted).toBe(true);
		provider.destroy();
	});

	it('the safety net completes, falls back to election, and reports to the server', () => {
		vi.useFakeTimers();
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const { provider, sock } = open();
		vi.advanceTimersByTime(10_000);
		expect(provider.replayComplete).toBe(true);
		expect(provider.synced).toBe(true);
		expect(provider.seedByElection).toBe(true);
		expect(provider.seedGranted).toBe(false);
		expect(sock.sent.filter((d) => typeof d === 'string' && JSON.parse(d).type === 'sync_safety_net')).toHaveLength(1);
		expect(warn).toHaveBeenCalled();
		provider.destroy();
		warn.mockRestore();
	});

	it('a cursor frame cancels the safety net', () => {
		vi.useFakeTimers();
		const { provider, sock } = open();
		sock.control({ type: 'op_log_cursor', op_log_id: 0 });
		vi.advanceTimersByTime(20_000);
		expect(provider.seedByElection).toBe(false);
		expect(sock.sent.some((d) => typeof d === 'string' && d.includes('sync_safety_net'))).toBe(false);
		provider.destroy();
	});
});
