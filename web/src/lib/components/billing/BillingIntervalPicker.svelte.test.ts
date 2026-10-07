// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';

const auth = vi.hoisted(() => ({ commerceAllowed: true }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import BillingIntervalPicker from './BillingIntervalPicker.svelte';

afterEach(() => {
	cleanup();
	auth.commerceAllowed = true;
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
});
