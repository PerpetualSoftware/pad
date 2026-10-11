// TASK-3601: the plan-limit prompt's actions, on a mount.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup, fireEvent, screen } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({ goto: vi.fn(), commerce: true, billing: true }));
vi.mock('$app/navigation', () => ({ goto: mocks.goto }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get commerceAllowed() {
			return mocks.commerce;
		},
		get billingAvailable() {
			return mocks.billing;
		},
		onIdentityChange: () => () => {},
	},
}));

import PlanLimitPrompt from './PlanLimitPrompt.svelte';
import { planLimitPrompt } from '$lib/billing/planLimitPrompt.svelte';
import { readUpgradeResume, clearUpgradeResume } from '$lib/billing/upgradeResume';
import { PadApiError } from '$lib/api/client';

const MSG = 'This workspace has reached its 3-member limit on Free.';
const err = (details: Record<string, unknown>) => new PadApiError({ code: 'plan_limit_exceeded', message: MSG, details });

beforeEach(() => {
	cleanup();
	mocks.goto.mockReset();
	mocks.commerce = true;
	mocks.billing = true;
	planLimitPrompt.dismiss();
	clearUpgradeResume();
	window.history.replaceState({}, '', '/ada/ws/settings?tab=members');
});

describe('PlanLimitPrompt (TASK-3601)', () => {
	it('the owner: Upgrade remembers the blocked flow and goes to billing', async () => {
		render(PlanLimitPrompt, { scope: 'invite' });
		planLimitPrompt.raise(err({ payer_is_you: true }), { scope: 'invite', blocked: 'invite a member' });
		await tick();
		expect(screen.getByRole('alert').textContent).toContain(MSG);
		await fireEvent.click(screen.getByRole('button', { name: 'Upgrade' }));
		expect(mocks.goto).toHaveBeenCalledWith('/console/billing');
		expect(readUpgradeResume()).toMatchObject({ path: '/ada/ws/settings?tab=members', action: 'invite a member' });
		expect(planLimitPrompt.current).toBeNull();
	});

	it('a member: told who to ask, a copy button, no Upgrade', async () => {
		render(PlanLimitPrompt, { scope: 'invite' });
		planLimitPrompt.raise(err({ payer_is_you: false, payer_name: 'Dana' }), { scope: 'invite' });
		await tick();
		expect(screen.getByRole('alert').textContent).toContain('Ask Dana, the workspace owner, to upgrade.');
		expect(screen.queryByRole('button', { name: 'Upgrade' })).toBeNull();
		expect(screen.getByRole('button', { name: 'Copy note' })).toBeTruthy();
	});

	it('billing off: the statement, Dismiss, and nothing to buy', async () => {
		mocks.billing = false;
		render(PlanLimitPrompt);
		planLimitPrompt.raise(err({ payer_is_you: true }));
		await tick();
		expect(screen.queryByRole('button', { name: 'Upgrade' })).toBeNull();
		expect(screen.queryByRole('button', { name: 'Copy note' })).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
		expect(screen.queryByRole('alert')).toBeNull();
	});

	it('the global host does not show a refusal that belongs to a mounted inline host', async () => {
		render(PlanLimitPrompt);
		render(PlanLimitPrompt, { scope: 'invite' });
		planLimitPrompt.raise(err({ payer_is_you: true }), { scope: 'invite' });
		await tick();
		expect(screen.getAllByRole('alert')).toHaveLength(1);
	});
});
