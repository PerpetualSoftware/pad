import { describe, it, expect, vi } from 'vitest';
import { registerSessionExpiryKeeper, runSessionExpiryKeepers } from './sessionExpiryKeep';

describe('BUG-3571 part 2: session-expiry keepers', () => {
	it('runs each registered keeper once, survives a throw, and honours unregister', () => {
		const a = vi.fn();
		const b = vi.fn(() => {
			throw new Error('x');
		});
		const c = vi.fn();
		const offA = registerSessionExpiryKeeper(a);
		const offB = registerSessionExpiryKeeper(b);
		const offC = registerSessionExpiryKeeper(c);
		expect(() => runSessionExpiryKeepers()).not.toThrow();
		expect(a).toHaveBeenCalledOnce();
		expect(c).toHaveBeenCalledOnce();
		offA();
		offB();
		runSessionExpiryKeepers();
		expect(a).toHaveBeenCalledOnce();
		expect(c).toHaveBeenCalledTimes(2);
		offC();
	});
});
