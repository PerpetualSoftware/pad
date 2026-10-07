import { describe, it, expect } from 'vitest';
import { decideCheckout } from './checkoutDecision';

// TASK-3468: redirect only to a session priced at the interval the user
// picked. The lead's three echo cases, plus the plain match.
describe('decideCheckout', () => {
	const url = 'https://checkout.stripe.com/c/pay/cs_test_1';

	it('redirects when the echo matches the pick', () => {
		expect(decideCheckout('annual', { url, interval: 'annual' })).toEqual({ kind: 'redirect', url });
		expect(decideCheckout('monthly', { url, interval: 'monthly' })).toEqual({ kind: 'redirect', url });
	});

	it('1. a MISSING echo with a MONTHLY pick redirects: an old sidecar sells monthly, which is what was picked', () => {
		expect(decideCheckout('monthly', { url })).toEqual({ kind: 'redirect', url });
	});

	it('2. a MISSING echo with an ANNUAL pick refuses: an old sidecar would charge monthly', () => {
		expect(decideCheckout('annual', { url })).toEqual({ kind: 'refuse', interval: 'annual' });
	});

	it('3. an echo that DIFFERS from the pick refuses, either way round', () => {
		expect(decideCheckout('annual', { url, interval: 'monthly' })).toEqual({ kind: 'refuse', interval: 'annual' });
		expect(decideCheckout('monthly', { url, interval: 'annual' })).toEqual({ kind: 'refuse', interval: 'monthly' });
	});
});
