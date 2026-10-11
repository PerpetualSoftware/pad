import { describe, expect, it, vi } from 'vitest';
import * as Y from 'yjs';
import { CollabProvider, type ApplierPatchHandler } from './wsProvider.svelte';

// PLAN-3578 step 2 at the provider: a patch-capable tab announces itself, and
// a patch request is answered with a refusal (no bracket), or with
// apply_start → (the handler's synchronous apply) → applier_ack carrying the
// canonical before/after. Unsent typing does not refuse a patch.

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

function setup(handler?: ApplierPatchHandler) {
	const doc = new Y.Doc();
	const provider = new CollabProvider('item-3578', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-3578',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
		onApplierRequest: vi.fn(() => true),
		onApplierPatch: handler,
	});
	const sock = FakeSocket.last!;
	sock.dispatchEvent(new Event('open'));
	sock.control({ type: 'op_log_cursor', op_log_id: 0 });
	return { provider, sock, doc };
}
const controls = (sock: FakeSocket) =>
	sock.sent.filter((d): d is string => typeof d === 'string').map((d) => JSON.parse(d) as Record<string, unknown>);
const patchRequest = () => ({
	type: 'applier_request',
	request_id: 'p1',
	patch_id: 'patch-1',
	edits: [{ old: 'a', new: 'b' }],
	expires_at_millis: Date.now() + 60_000,
});

describe('patch applier (PLAN-3578 step 2)', () => {
	it('announces patch_capable only with a patch handler', () => {
		setup(() => null).provider.destroy();
		expect(FakeSocket.last!.url).toContain('patch_capable=1');
		setup(undefined).provider.destroy();
		expect(FakeSocket.last!.url).not.toContain('patch_capable=1');
	});

	it('brackets an applied patch and acks with before/after, even with unsent typing', () => {
		const order: string[] = [];
		const { provider, sock, doc } = setup((edits, _id, _exp, openBracket) => {
			expect(edits).toEqual([{ old: 'a', new: 'b' }]);
			openBracket();
			order.push('applied');
			return { ok: true, before: 'a', after: 'b' };
		});
		doc.getText('t').insert(0, 'typed'); // unconfirmed local edit
		expect(provider.editsMayBeMissing).toBe(true);
		sock.control(patchRequest());
		const sent = controls(sock).filter((c) => c.request_id === 'p1');
		expect(sent.map((c) => c.type)).toEqual(['applier_apply_start', 'applier_ack']);
		expect(sent[1]).toMatchObject({ before: 'a', after: 'b' });
		expect(order).toEqual(['applied']);
		provider.destroy();
	});

	it('answers a refusal instead of a bracket', () => {
		const { provider, sock } = setup(() => ({ ok: false, reason: 'unstable_roundtrip', line: 7 }));
		sock.control(patchRequest());
		const sent = controls(sock).filter((c) => c.request_id === 'p1');
		expect(sent).toEqual([{ type: 'applier_refuse', request_id: 'p1', reason: 'unstable_roundtrip', line: 7 }]);
		provider.destroy();
	});

	it('never sends a refusal after the bracket opened', () => {
		const { provider, sock } = setup((_e, _i, _x, openBracket) => {
			openBracket();
			return { ok: false, reason: 'unstable_roundtrip' };
		});
		sock.control(patchRequest());
		expect(controls(sock).filter((c) => c.request_id === 'p1').map((c) => c.type)).toEqual(['applier_apply_start']);
		provider.destroy();
	});

	it('drops an expired patch request untouched', () => {
		const handler = vi.fn(() => null);
		const { provider, sock } = setup(handler);
		sock.control({ ...patchRequest(), expires_at_millis: Date.now() - 1 });
		expect(handler).not.toHaveBeenCalled();
		provider.destroy();
	});
});

describe('patch re-send (TASK-3585)', () => {
	it('re-acks a patch it already applied, without applying it again', () => {
		let applies = 0;
		const { provider, sock } = setup((_e, _i, _x, openBracket) => {
			openBracket();
			applies++;
			return { ok: true, before: 'a', after: 'b' };
		});
		sock.control(patchRequest());
		sock.control({ ...patchRequest(), request_id: 'p2' }); // same patch_id, a retry
		expect(applies).toBe(1);
		const second = controls(sock).filter((c) => c.request_id === 'p2');
		expect(second.map((c) => c.type)).toEqual(['applier_apply_start', 'applier_ack']);
		expect(second[1]).toMatchObject({ before: 'a', after: 'b' });
		provider.destroy();
	});

	it('remembers a patch it applied too late to ack, so the retry is re-acked', () => {
		let applies = 0;
		const now = Date.now();
		let late = false;
		const spy = vi.spyOn(Date, 'now').mockImplementation(() => (late ? now + 120_000 : now));
		const { provider, sock } = setup((_e, _i, _x, openBracket) => {
			openBracket();
			applies++;
			late = true; // the deadline passes during the apply
			return { ok: true, before: 'a', after: 'b' };
		});
		sock.control({ ...patchRequest(), expires_at_millis: now + 60_000 });
		expect(controls(sock).filter((c) => c.request_id === 'p1').map((c) => c.type)).toEqual(['applier_apply_start']);
		late = false;
		sock.control({ ...patchRequest(), request_id: 'p2', expires_at_millis: now + 60_000 });
		spy.mockRestore();
		expect(applies).toBe(1);
		expect(controls(sock).filter((c) => c.request_id === 'p2').map((c) => c.type)).toEqual(['applier_apply_start', 'applier_ack']);
		provider.destroy();
	});
});
