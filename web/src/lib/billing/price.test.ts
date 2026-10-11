import { describe, it, expect } from 'vitest';
import { formatPrice, perInterval } from './price';

describe('formatPrice (TASK-3577)', () => {
	it('drops zero cents and keeps real ones', () => {
		expect(formatPrice(800, 'usd')).toBe('$8');
		expect(formatPrice(8000, 'USD')).toBe('$80');
		expect(formatPrice(750, 'usd')).toBe('$7.50');
	});
	it('reads minor units by the currency, not a fixed 100', () => {
		expect(formatPrice(800, 'jpy')).toBe('¥800');
		expect(formatPrice(1200, 'eur')).toBe('€12');
	});
	it('an unknown code is shown, not guessed', () => {
		expect(formatPrice(800, 'zzzz')).toBe('8.00 ZZZZ');
	});
	it('names the interval', () => {
		expect(perInterval('monthly')).toBe('/ month');
		expect(perInterval('annual')).toBe('/ year');
	});
});
