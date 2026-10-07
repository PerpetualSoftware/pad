// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3468, through the real billing page: the picked interval is what is
// bought, a 503 for it shows a plain message and disables that option, and a
// session priced at another interval is never opened.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, screen, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const m = vi.hoisted(() => ({
	create: vi.fn(),
	toast: vi.fn(),
	BillingCheckoutError: class extends Error {
		status: number;
		constructor(msg: string, status: number) {
			super(msg);
			this.status = status;
		}
	}
}));

vi.mock('$lib/api/client', () => ({
	api: { billing: { createCheckoutSession: m.create } },
	withRequestDeadline: (fn: (signal: AbortSignal) => unknown) => fn(new AbortController().signal),
	BillingCheckoutError: m.BillingCheckoutError
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		user: { plan: 'free' },
		cloudMode: true,
		billingAvailable: true,
		commerceAllowed: true,
		load: async () => {}
	}
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: m.toast } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/console/billing') } }));

import BillingPage from './+page.svelte';

let href = '';
beforeEach(() => {
	m.create.mockReset();
	m.toast.mockReset();
	href = '';
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => new Response(JSON.stringify({ free: {}, pro: {} }), { status: 200 }))
	);
	Object.defineProperty(window, 'location', {
		configurable: true,
		value: {
			...window.location,
			set href(v: string) {
				href = v;
			},
			get href() {
				return href;
			}
		}
	});
});
afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

async function settle() {
	for (let i = 0; i < 5; i++) {
		await Promise.resolve();
		await tick();
	}
}

function annualRadios() {
	return screen.getAllByRole('radio', { name: /Annual/ }) as HTMLInputElement[];
}

async function pickAnnualAndUpgrade() {
	await fireEvent.click(annualRadios()[0]);
	await settle();
	await fireEvent.click(screen.getAllByRole('button', { name: 'Upgrade to Pro' })[0]);
	await settle();
}

describe('billing page interval (TASK-3468)', () => {
	it('monthly is the default, and the picked interval is the one bought', async () => {
		render(BillingPage);
		await settle();
		expect((screen.getAllByRole('radio', { name: /Monthly/ })[0] as HTMLInputElement).checked).toBe(true);

		m.create.mockResolvedValue({ url: 'https://checkout.stripe.com/annual', interval: 'annual' });
		await pickAnnualAndUpgrade();
		expect(m.create).toHaveBeenCalledWith('annual');
		expect(href).toBe('https://checkout.stripe.com/annual');
		// Both pickers on the page show the same choice.
		expect(annualRadios().every((r) => r.checked)).toBe(true);
	});

	it('a 503 for the interval shows a plain message, disables it, and does not redirect', async () => {
		render(BillingPage);
		await settle();
		m.create.mockRejectedValue(new m.BillingCheckoutError('This billing interval is not available', 503));
		await pickAnnualAndUpgrade();

		expect(href).toBe('');
		expect(m.toast).not.toHaveBeenCalled();
		// ONE live region, so a screen reader announces it once (codex r1).
		const statuses = screen.getAllByRole('status');
		expect(statuses).toHaveLength(1);
		expect(statuses[0].textContent).toContain("Annual billing isn't available right now");
		expect(annualRadios().every((r) => r.disabled)).toBe(true);
		// Selection moves to the interval that can still be bought.
		expect((screen.getAllByRole('radio', { name: /Monthly/ })[0] as HTMLInputElement).checked).toBe(true);
	});

	it('a session not priced at the picked interval is never opened (missing echo, annual pick)', async () => {
		render(BillingPage);
		await settle();
		m.create.mockResolvedValue({ url: 'https://checkout.stripe.com/old-sidecar' });
		await pickAnnualAndUpgrade();

		expect(href).toBe('');
		expect(screen.getAllByRole('status')[0].textContent).toContain("Annual billing isn't available right now");
	});

	it('another failure stays a toast and leaves both options available', async () => {
		render(BillingPage);
		await settle();
		m.create.mockRejectedValue(new m.BillingCheckoutError('Billing is temporarily unavailable', 502));
		await pickAnnualAndUpgrade();
		expect(m.toast).toHaveBeenCalledWith('Billing is temporarily unavailable', 'error');
		expect(annualRadios().every((r) => !r.disabled)).toBe(true);
	});
});
