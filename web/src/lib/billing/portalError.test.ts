import { describe, it, expect } from 'vitest';
import { PORTAL_ERROR_FALLBACK, PORTAL_ERROR_MESSAGES, portalErrorMessage } from './portalError';

// BUG-3898: the codes pad-cloud's portal sends back, pinned in its stripe.go.
describe('portalErrorMessage', () => {
	it('knows exactly the five codes pad-cloud sends', () => {
		expect(Object.keys(PORTAL_ERROR_MESSAGES).sort()).toEqual(
			['no_billing_account', 'not_configured', 'origin', 'stripe_error', 'unauthenticated'].sort()
		);
		for (const [code, msg] of Object.entries(PORTAL_ERROR_MESSAGES)) {
			expect(portalErrorMessage(code)).toBe(msg);
			expect(msg).not.toBe(PORTAL_ERROR_FALLBACK);
		}
	});

	it('gives any other code the generic message, never its own text', () => {
		for (const code of ['', 'bogus', 'toString', '__proto__', '<script>alert(1)</script>']) {
			expect(portalErrorMessage(code)).toBe(PORTAL_ERROR_FALLBACK);
		}
	});
});
