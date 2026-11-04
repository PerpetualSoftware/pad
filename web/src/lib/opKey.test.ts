// BUG-4312: crypto.randomUUID exists only in a secure context. A self-hosted
// Pad on a LAN IP over plain http is not one, so every op_key comes from here.
import { afterEach, describe, expect, it } from 'vitest';
import { newOpKey } from './opKey';

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function withoutRandomUUID(): void {
	Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true });
}
afterEach(() => {
	Reflect.deleteProperty(crypto, 'randomUUID');
});

describe('newOpKey', () => {
	it('is a UUID when crypto.randomUUID exists', () => {
		expect(newOpKey()).toMatch(UUID_V4);
	});

	// Red while there is no fallback: the call throws "crypto.randomUUID is not a function".
	it('is still a v4 UUID, and different each time, without crypto.randomUUID', () => {
		withoutRandomUUID();
		expect(typeof crypto.randomUUID).toBe('undefined');
		const keys = new Set(Array.from({ length: 200 }, () => newOpKey()));
		expect(keys.size).toBe(200);
		for (const k of keys) expect(k).toMatch(UUID_V4);
	});

	it('fits the server op_key limit (128 bytes) either way', () => {
		expect(newOpKey().length).toBeLessThanOrEqual(128);
		withoutRandomUUID();
		expect(newOpKey().length).toBeLessThanOrEqual(128);
	});
});
