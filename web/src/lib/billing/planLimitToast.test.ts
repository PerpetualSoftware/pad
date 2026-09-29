import { describe, it, expect, vi, beforeEach } from 'vitest';

// PLAN-3291 DR-3 (TASK-3293): one toast for every plan-limit refusal, with the
// upgrade copy and link only where commerce is allowed.

const state = vi.hoisted(() => ({ commerce: true }));
const show = vi.hoisted(() => vi.fn());

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get commerceAllowed() {
			return state.commerce;
		}
	}
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show } }));

import { showPlanLimitToast } from './planLimitToast';
import { PadApiError } from '$lib/api/client';

const err = () =>
	new PadApiError({ code: 'plan_limit_exceeded', message: "You've reached the 3-member limit on the free plan." });

describe('showPlanLimitToast', () => {
	beforeEach(() => show.mockReset());

	it('offers the upgrade where commerce is allowed', () => {
		state.commerce = true;
		showPlanLimitToast(err());
		expect(show).toHaveBeenCalledWith(
			"You've reached the 3-member limit on the free plan. Upgrade to Pro",
			'error',
			6000,
			'/console/billing'
		);
	});

	it('states the limit and nothing else in the app', () => {
		state.commerce = false;
		showPlanLimitToast(err());
		expect(show).toHaveBeenCalledTimes(1);
		const args = show.mock.calls[0];
		expect(args[0]).toBe("You've reached the 3-member limit on the free plan.");
		expect(args[3]).toBeUndefined();
		expect(args.join(' ')).not.toMatch(/upgrade|billing|pro\b/i);
	});
});
