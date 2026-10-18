// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3897, through the real billing page: a Pro user's "Manage Billing" link
// goes to /billing/portal, a pad-cloud sidecar path with no SPA route. Without
// data-sveltekit-reload the client router took the click and showed the
// catch-all; the request never reached nginx's /billing/ route, and the
// portal's origin check needs the Referer a full navigation sends.
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';

vi.mock('$lib/api/client', () => ({
	api: { billing: { createCheckoutSession: vi.fn() } },
	withRequestDeadline: (fn: (signal: AbortSignal) => unknown) => fn(new AbortController().signal),
	BillingCheckoutError: class extends Error {}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get user() {
			return { plan: 'pro' };
		},
		cloudMode: true,
		billingAvailable: true,
		billingOffer: {
			intervals: [{ interval: 'monthly', lookup_key: 'pad_pro_monthly', unit_amount: 800, currency: 'usd' }],
			trial_days: 14
		},
		commerceAllowed: true,
		load: async () => {}
	}
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: vi.fn() } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/console/billing') } }));

import BillingPage from './+page.svelte';

beforeEach(() => {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => new Response(JSON.stringify({ free: {}, pro: {} }), { status: 200 }))
	);
});
afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

describe('BUG-3897: Manage Billing', () => {
	it('is a full navigation to the sidecar portal, never a client-router route', () => {
		render(BillingPage);
		const link = screen.getByRole('link', { name: 'Manage Billing' });
		expect(link.getAttribute('href')).toBe('/billing/portal');
		// /billing/ is served by pad-cloud, not the SPA: the client router must
		// not take the click.
		expect(link.hasAttribute('data-sveltekit-reload')).toBe(true);
	});
});
