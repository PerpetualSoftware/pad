import { describe, expect, it, vi } from 'vitest';
import { createWatermarkStamper } from './watermarkStamper';
import { sha256Hex } from '$lib/utils/sha256';

// BUG-3124 unit B: the page-side rules for the content-free watermark stamp.
function make(cursor: number, opts: { recovering?: boolean; fail?: boolean } = {}) {
	let current = cursor;
	const send = vi.fn(async () => {
		if (opts.fail) throw new Error('network');
		return { advanced: true };
	});
	const stamp = createWatermarkStamper({
		cursorFor: () => current,
		isRecovering: () => !!opts.recovering,
		send,
	});
	return { stamp, send, setCursor: (c: number) => (current = c) };
}
const input = { ws: 'ws', itemId: 'item-1', content: 'stored body', keepalive: false };

describe('createWatermarkStamper', () => {
	it('sends the cursor and the sha256 of the content the server holds', () => {
		const { stamp, send } = make(42);
		stamp({ ...input, keepalive: true });
		expect(send).toHaveBeenCalledWith('ws', 'item-1', 42, sha256Hex('stored body'), true);
	});

	it('sends synchronously, so a pagehide keepalive request starts before unload', () => {
		const { stamp, send } = make(42);
		stamp(input);
		expect(send).toHaveBeenCalledTimes(1);
	});

	it('does not re-send a cursor it already sent, and does send a newer one', () => {
		const { stamp, send, setCursor } = make(42);
		stamp(input);
		stamp(input);
		expect(send).toHaveBeenCalledTimes(1);
		setCursor(43);
		stamp(input);
		expect(send).toHaveBeenCalledTimes(2);
	});

	it('sends nothing without a cursor the server issued', () => {
		const { stamp, send } = make(0);
		stamp(input);
		expect(send).not.toHaveBeenCalled();
	});

	it('sends nothing while force-refresh recovery is in flight', () => {
		const { stamp, send } = make(42, { recovering: true });
		stamp(input);
		expect(send).not.toHaveBeenCalled();
	});

	it('forgets a cursor whose send failed, so a later settle can retry it', async () => {
		const { stamp, send } = make(42, { fail: true });
		stamp(input);
		await Promise.resolve();
		await Promise.resolve();
		stamp(input);
		expect(send).toHaveBeenCalledTimes(2);
	});
});

describe('createWatermarkStamper — a send that throws synchronously', () => {
	it('does not throw into its caller (the flusher dedupe arm), and can retry', () => {
		const send = vi.fn(() => {
			throw new TypeError('not a function');
		});
		const stamp = createWatermarkStamper({ cursorFor: () => 7, isRecovering: () => false, send });
		expect(() => stamp(input)).not.toThrow();
		stamp(input);
		expect(send).toHaveBeenCalledTimes(2);
	});
});

describe('createWatermarkStamper — codex round 1', () => {
	it('the keepalive (pagehide) path sends even for a cursor already sent', () => {
		const { stamp, send } = make(42);
		stamp(input);
		stamp({ ...input, keepalive: true });
		expect(send).toHaveBeenCalledTimes(2);
		expect(send).toHaveBeenLastCalledWith('ws', 'item-1', 42, sha256Hex('stored body'), true);
	});

	it('a 200 {advanced:false} forgets the cursor so a later flush retries it', async () => {
		const send = vi.fn(async () => ({ advanced: false }));
		const stamp = createWatermarkStamper({ cursorFor: () => 9, isRecovering: () => false, send });
		stamp(input);
		await Promise.resolve();
		await Promise.resolve();
		stamp(input);
		expect(send).toHaveBeenCalledTimes(2);
	});

	it('CONTROL: a 200 {advanced:true} keeps the cursor, so it is not re-sent', async () => {
		const send = vi.fn(async () => ({ advanced: true }));
		const stamp = createWatermarkStamper({ cursorFor: () => 9, isRecovering: () => false, send });
		stamp(input);
		await Promise.resolve();
		await Promise.resolve();
		stamp(input);
		expect(send).toHaveBeenCalledTimes(1);
	});
});
