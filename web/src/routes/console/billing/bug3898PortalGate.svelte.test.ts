// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3898, through the real billing page:
// - Manage Billing shows only for a Pro account with a Stripe customer
//   (stripe_customer_id, from GET /auth/me). A plan granted by an admin has
//   none, and gets a one-line note instead of a button the portal can only
//   refuse.
// - A customer with no live subscription still gets the portal (R2): the
//   gate is the customer, not the subscription.
// - A refused portal request comes back as ?portal_error=<code>, which shows
//   its message (a generic one for an unknown code) and is cleared from the URL.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { PORTAL_ERROR_FALLBACK, PORTAL_ERROR_MESSAGES } from '$lib/billing/portalError';

const m = vi.hoisted(() => ({
	me: vi.fn(),
	commerce: true,
	url: 'http://localhost/console/billing'
}));

// Reactive, so a plan that changes during the visit reaches the page's
// $derived isPro the way authStore.load() does.
const plan = $state({ value: 'pro' });

vi.mock('$lib/api/client', () => ({
	api: { auth: { me: m.me }, billing: { createCheckoutSession: vi.fn() } },
	withRequestDeadline: (fn: (signal: AbortSignal) => unknown) => fn(new AbortController().signal),
	BillingCheckoutError: class extends Error {}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get user() {
			return { plan: plan.value };
		},
		cloudMode: true,
		billingAvailable: true,
		billingOffer: {
			intervals: [{ interval: 'monthly', lookup_key: 'pad_pro_monthly', unit_amount: 800, currency: 'usd' }],
			trial_days: 14
		},
		get commerceAllowed() {
			return m.commerce;
		},
		load: async () => {}
	}
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: vi.fn() } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return new URL(m.url);
		}
	}
}));

import BillingPage from './+page.svelte';

let replaced: string[] = [];
beforeEach(() => {
	m.me.mockReset();
	m.commerce = true;
	m.url = 'http://localhost/console/billing';
	plan.value = 'pro';
	replaced = [];
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => new Response(JSON.stringify({ free: {}, pro: {} }), { status: 200 }))
	);
	// Recorded AND applied, so the URL really changes, as in a browser
	// (hunter r1 L4: a recording-only mock left it unchanged).
	const realReplace = history.replaceState.bind(history);
	vi.spyOn(history, 'replaceState').mockImplementation((s, t, url) => {
		replaced.push(String(url));
		realReplace(s, t, url);
	});
});

// In a browser page.url and window.location agree; put the query on both.
function openAt(url: string) {
	m.url = url;
	window.history.pushState({}, '', new URL(url).pathname + new URL(url).search);
}
afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

async function settle() {
	for (let i = 0; i < 5; i++) {
		await Promise.resolve();
		await tick();
	}
}

const GRANTED = /plan was granted to your account, so there is no billing to manage/;

describe('BUG-3898: Manage Billing is gated on a Stripe customer', () => {
	it('a Pro account with a customer gets the portal link', async () => {
		m.me.mockResolvedValue({ id: 'u', stripe_customer_id: 'cus_1' });
		render(BillingPage);
		await settle();
		const link = screen.getByRole('link', { name: 'Manage Billing' });
		expect(link.getAttribute('href')).toBe('/billing/portal');
		expect(link.hasAttribute('data-sveltekit-reload')).toBe(true);
		expect(screen.queryByText(GRANTED)).toBeNull();
	});

	it('a granted Pro plan with no customer gets the note, not a link the portal would refuse', async () => {
		m.me.mockResolvedValue({ id: 'u' });
		render(BillingPage);
		await settle();
		expect(screen.queryByRole('link', { name: 'Manage Billing' })).toBeNull();
		expect(screen.getByText(GRANTED)).toBeTruthy();
	});

	it('when /auth/me fails the link stays: the portal answers a refusal with portal_error', async () => {
		m.me.mockRejectedValue(new Error('network'));
		render(BillingPage);
		await settle();
		expect(screen.getByRole('link', { name: 'Manage Billing' })).toBeTruthy();
		expect(screen.queryByText(GRANTED)).toBeNull();
	});
});

describe('BUG-3898: ?portal_error= from pad-cloud', () => {
	it('a known code shows its own message and is cleared from the URL', async () => {
		m.me.mockResolvedValue({ id: 'u', stripe_customer_id: 'cus_1' });
		openAt('http://localhost/console/billing?portal_error=stripe_error');
		render(BillingPage);
		await settle();
		expect(screen.getByRole('alert').textContent?.trim()).toBe(PORTAL_ERROR_MESSAGES.stripe_error);
		expect(replaced.length).toBeGreaterThan(0);
		expect(window.location.search).toBe('');
	});

	it('an unknown code shows the generic message, never the code', async () => {
		m.me.mockResolvedValue({ id: 'u', stripe_customer_id: 'cus_1' });
		openAt('http://localhost/console/billing?portal_error=%3Cb%3Eforged%3C%2Fb%3E');
		render(BillingPage);
		await settle();
		expect(screen.getByRole('alert').textContent?.trim()).toBe(PORTAL_ERROR_FALLBACK);
		expect(document.body.innerHTML).not.toContain('forged');
	});

	it('in the apps it is dropped unread, like a checkout return', async () => {
		m.commerce = false;
		openAt('http://localhost/console/billing?portal_error=no_billing_account');
		render(BillingPage);
		await settle();
		expect(screen.queryByRole('alert')).toBeNull();
		expect(window.location.search).toBe('');
	});

	it('a portal_error alongside a checkout return: both are read, both are cleared', async () => {
		m.me.mockResolvedValue({ id: 'u', stripe_customer_id: 'cus_1' });
		openAt('http://localhost/console/billing?checkout=cancelled&portal_error=origin');
		render(BillingPage);
		await settle();
		expect(screen.getByRole('alert').textContent?.trim()).toBe(PORTAL_ERROR_MESSAGES.origin);
		expect(screen.getByText(/without completing checkout/)).toBeTruthy();
		expect(window.location.search).toBe('');
	});
});

describe('BUG-3898 r1: a plan that becomes Pro during the visit', () => {
	it('is read for a customer then, not only at mount', async () => {
		plan.value = 'free';
		m.me.mockResolvedValue({ id: 'u' });
		render(BillingPage);
		await settle();
		expect(m.me).not.toHaveBeenCalled();
		plan.value = 'pro';
		await settle();
		expect(m.me).toHaveBeenCalledTimes(1);
		expect(screen.getByText(GRANTED)).toBeTruthy();
		expect(screen.queryByRole('link', { name: 'Manage Billing' })).toBeNull();
	});
});
