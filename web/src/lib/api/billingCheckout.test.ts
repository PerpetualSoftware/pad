import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, BillingCheckoutError } from './client';

// TASK-3468: the client sends the chosen interval and surfaces a refusal's
// HTTP status, so the page can tell "this interval is not sold" (503) apart.
afterEach(() => vi.unstubAllGlobals());

function stubFetch(status: number, body: unknown) {
	const fn = vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
	vi.stubGlobal('fetch', fn);
	return fn;
}

function sentBody(fn: ReturnType<typeof stubFetch>) {
	const init = (fn.mock.calls[0] as unknown as [string, RequestInit])[1];
	return JSON.parse(String(init.body));
}

describe('api.billing.createCheckoutSession', () => {
	it('sends the chosen interval and returns the echo', async () => {
		const fn = stubFetch(200, { url: 'https://checkout.stripe.com/x', interval: 'annual' });
		await expect(api.billing.createCheckoutSession('annual')).resolves.toEqual({
			url: 'https://checkout.stripe.com/x',
			interval: 'annual'
		});
		expect(sentBody(fn)).toEqual({ interval: 'annual' });
	});

	it('defaults to monthly', async () => {
		const fn = stubFetch(200, { url: 'https://checkout.stripe.com/x', interval: 'monthly' });
		await api.billing.createCheckoutSession();
		expect(sentBody(fn)).toEqual({ interval: 'monthly' });
	});

	it('a 503 throws BillingCheckoutError carrying the status and the sidecar message', async () => {
		stubFetch(503, { error: 'This billing interval is not available' });
		const err = await api.billing.createCheckoutSession('annual').catch((e) => e);
		expect(err).toBeInstanceOf(BillingCheckoutError);
		expect(err.status).toBe(503);
		expect(err.message).toBe('This billing interval is not available');
	});
});
