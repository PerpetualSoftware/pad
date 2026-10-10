import { describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import { CollabProvider } from './wsProvider.svelte';

// BUG-3542: a GUARDED applier_request is refused, not applied, while this tab
// holds edits the server may not have stored (editsMayBeMissing). An unguarded
// one (an explicit overwrite, or a server that predates the flag) applies as
// before, and a tab with nothing unconfirmed applies a guarded one.

class FakeSocket extends EventTarget {
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

function setup() {
	const doc = new Y.Doc();
	const apply = vi.fn(() => true);
	const provider = new CollabProvider('item-3542', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-3542',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
		onApplierRequest: apply,
	});
	const sock = FakeSocket.last!;
	sock.dispatchEvent(new Event('open'));
	sock.control({ type: 'op_log_cursor', op_log_id: 0 });
	return { provider, sock, doc, apply };
}

const controls = (sock: FakeSocket) =>
	sock.sent.filter((d): d is string => typeof d === 'string').map((d) => JSON.parse(d) as { type: string; reason?: string });
const request = (guarded?: boolean) => ({
	type: 'applier_request',
	request_id: 'r1',
	markdown: 'API body',
	expires_at_millis: Date.now() + 60_000,
	...(guarded === undefined ? {} : { guarded }),
});

describe('applier_request vs unconfirmed edits (BUG-3542)', () => {
	it('refuses a guarded request while a local edit is unconfirmed, and applies nothing', () => {
		const { provider, sock, doc, apply } = setup();
		doc.getText('t').insert(0, 'typed'); // sent, no barrier_ack yet
		expect(provider.editsMayBeMissing).toBe(true);
		sock.control(request(true));
		expect(apply).not.toHaveBeenCalled();
		const types = controls(sock).map((c) => c.type);
		expect(types).toContain('applier_refuse');
		expect(types).not.toContain('applier_apply_start');
		expect(controls(sock).find((c) => c.type === 'applier_refuse')?.reason).toBe('unconfirmed_edits');
		provider.destroy();
	});

	it('applies a guarded request when nothing is unconfirmed', () => {
		const { provider, sock, apply } = setup();
		expect(provider.editsMayBeMissing).toBe(false);
		sock.control(request(true));
		expect(apply).toHaveBeenCalledTimes(1);
		expect(controls(sock).map((c) => c.type)).toEqual(expect.arrayContaining(['applier_apply_start', 'applier_ack']));
		provider.destroy();
	});

	it('applies an unguarded request (an explicit overwrite, or an older server) even with unconfirmed edits', () => {
		const { provider, sock, doc, apply } = setup();
		doc.getText('t').insert(0, 'typed');
		sock.control(request()); // no `guarded`
		expect(apply).toHaveBeenCalledTimes(1);
		expect(controls(sock).map((c) => c.type)).not.toContain('applier_refuse');
		provider.destroy();
	});
});
