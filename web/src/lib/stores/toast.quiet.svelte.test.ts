// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`). jsdom's
// own localStorage is non-functional in this setup, so each case stubs
// `globalThis.localStorage` explicitly — which is also exactly the contract
// under test: the helper must never throw, whatever storage does (BUG-2334).
import { describe, it, expect, afterEach } from 'vitest';
import { quietExternalToasts } from './toast.svelte';

const original = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');

function stubStorage(value: Pick<Storage, 'getItem'> | undefined) {
	Object.defineProperty(globalThis, 'localStorage', {
		configurable: true,
		value,
	});
}

afterEach(() => {
	if (original) Object.defineProperty(globalThis, 'localStorage', original);
	else delete (globalThis as Record<string, unknown>).localStorage;
});

describe('quietExternalToasts (BUG-2334 e2e kill switch)', () => {
	it('is true only when the flag is the literal "1"', () => {
		stubStorage({ getItem: (k) => (k === 'pad:e2e-quiet-external-toasts' ? '1' : null) });
		expect(quietExternalToasts()).toBe(true);
	});

	it('is false when the flag is absent — the production default', () => {
		stubStorage({ getItem: () => null });
		expect(quietExternalToasts()).toBe(false);
	});

	it('is false, not a throw, when storage itself is unavailable or hostile', () => {
		stubStorage(undefined);
		expect(quietExternalToasts()).toBe(false);
		stubStorage({
			getItem: () => {
				throw new Error('denied');
			},
		});
		expect(quietExternalToasts()).toBe(false);
	});
});
