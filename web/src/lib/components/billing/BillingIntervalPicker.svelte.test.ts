// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';

type Offer = { intervals: { interval: 'monthly' | 'annual'; lookup_key: string; unit_amount: number; currency: string }[]; trial_days: number } | null;
const served = (): Offer => ({
	intervals: [
		{ interval: 'monthly', lookup_key: 'pad_pro_monthly', unit_amount: 800, currency: 'usd' },
		{ interval: 'annual', lookup_key: 'pad_pro_annual', unit_amount: 8000, currency: 'usd' },
	],
	trial_days: 14,
});
const auth = vi.hoisted(() => ({ commerceAllowed: true, billingOffer: null as unknown }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import BillingIntervalPicker from './BillingIntervalPicker.svelte';

auth.billingOffer = served();

afterEach(() => {
	cleanup();
	auth.commerceAllowed = true;
	auth.billingOffer = served();
});

describe('BillingIntervalPicker (TASK-3468)', () => {
	it('offers monthly and annual with prices, monthly selected by default', () => {
		render(BillingIntervalPicker, { props: { name: 'i' } });
		const monthly = screen.getByRole('radio', { name: /Monthly/ }) as HTMLInputElement;
		const annual = screen.getByRole('radio', { name: /Annual/ }) as HTMLInputElement;
		expect(monthly.checked).toBe(true);
		expect(annual.checked).toBe(false);
		expect(screen.getByText('$8 / month')).toBeTruthy();
		expect(screen.getByText('$80 / year')).toBeTruthy();
		expect(screen.getByRole('group', { name: 'Billing interval' })).toBeTruthy();
	});

	it('disables an interval reported unavailable', () => {
		render(BillingIntervalPicker, { props: { name: 'i', unavailable: ['annual'] } });
		expect((screen.getByRole('radio', { name: /Annual/ }) as HTMLInputElement).disabled).toBe(true);
		expect((screen.getByRole('radio', { name: /Monthly/ }) as HTMLInputElement).disabled).toBe(false);
	});

	it('renders nothing outside the commerce gate (the mobile apps show no prices)', () => {
		auth.commerceAllowed = false;
		const { container } = render(BillingIntervalPicker, { props: { name: 'i' } });
		expect(container.querySelector('fieldset')).toBeNull();
		expect(container.textContent).not.toContain('$8');
	});

	// TASK-3577: the prices are the sidecar's, not constants.
	it('renders the served price: a different price in the offer renders differently (counterfactual)', () => {
		auth.billingOffer = {
			intervals: [{ interval: 'monthly', lookup_key: 'pad_pro_monthly', unit_amount: 1250, currency: 'eur' }],
			trial_days: 0,
		};
		const { container } = render(BillingIntervalPicker, { props: { name: 'i' } });
		expect(screen.getByText('€12.50 / month')).toBeTruthy();
		expect(container.textContent).not.toContain('$8');
		expect(container.textContent).toContain('Prices in EUR.');
	});

	it('does not show an interval the offer does not sell', () => {
		auth.billingOffer = {
			intervals: [{ interval: 'annual', lookup_key: 'pad_pro_annual', unit_amount: 9600, currency: 'usd' }],
			trial_days: 0,
		};
		render(BillingIntervalPicker, { props: { name: 'i', value: 'annual' } });
		expect(screen.queryByRole('radio', { name: /Monthly/ })).toBeNull();
		expect(screen.getByText('$96 / year')).toBeTruthy();
	});

	it('renders nothing without an offer', () => {
		auth.billingOffer = null;
		const { container } = render(BillingIntervalPicker, { props: { name: 'i' } });
		expect(container.querySelector('fieldset')).toBeNull();
	});
});
