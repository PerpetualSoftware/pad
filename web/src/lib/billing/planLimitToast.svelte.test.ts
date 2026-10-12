import { describe, it, expect, vi, beforeEach } from 'vitest';

// TASK-3601: every plan-limit refusal raises the persistent prompt (it was a
// 6-second toast). The upgrade is offered only to the payer, and only where
// commerce is allowed and billing is available (PLAN-3291 DR-3 still holds).

const state = vi.hoisted(() => ({ commerce: true, billing: true }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get commerceAllowed() {
			return state.commerce;
		},
		get billingAvailable() {
			return state.billing;
		},
		onIdentityChange: () => () => {},
	},
}));

import { showPlanLimitToast, promptOnPlanLimit, PLAN_LIMIT_PROMPTED } from './planLimitToast';
import { planLimitPrompt } from './planLimitPrompt.svelte';
import { PadApiError } from '$lib/api/client';

const MSG = 'This workspace has reached its 3-member limit on Free.';
const err = (details: Record<string, unknown> = { payer_is_you: true }) =>
	new PadApiError({ code: 'plan_limit_exceeded', message: MSG, details });

describe('showPlanLimitToast raises the plan-limit prompt (TASK-3601)', () => {
	beforeEach(() => {
		state.commerce = true;
		state.billing = true;
		planLimitPrompt.dismiss();
	});

	it('the payer, where commerce is allowed: Upgrade', () => {
		showPlanLimitToast(err());
		expect(planLimitPrompt.current).toMatchObject({ message: MSG, action: 'upgrade', scope: null });
	});

	it('no commerce in the app: the statement alone, no upgrade anywhere', () => {
		state.commerce = false;
		showPlanLimitToast(err());
		const p = planLimitPrompt.current!;
		expect(p.action).toBe('none');
		expect([p.message, p.hint, p.note].join(' ')).not.toMatch(/upgrade|billing|pro\b/i);
	});

	it('stays until dismissed: no timer takes it away', () => {
		vi.useFakeTimers();
		showPlanLimitToast(err());
		vi.advanceTimersByTime(60_000);
		expect(planLimitPrompt.current).not.toBeNull();
		planLimitPrompt.dismiss();
		expect(planLimitPrompt.current).toBeNull();
		vi.useRealTimers();
	});

	it('a newer refusal from another flow replaces the prompt and shows in its own host (codex r1)', () => {
		const offInvite = planLimitPrompt.registerInlineHost('invite');
		showPlanLimitToast(err(), { scope: 'invite' });
		expect(planLimitPrompt.current?.scope).toBe('invite');
		// No inline host for this one: it goes to the global host, not nowhere.
		showPlanLimitToast(err(), { scope: 'new-item-sheet' });
		expect(planLimitPrompt.current?.scope).toBeNull();
		offInvite();
		expect(planLimitPrompt.current, 'the invite host closing leaves the global prompt').not.toBeNull();
	});

	it('a scoped refusal goes to its inline host when one is mounted, else to the global one', () => {
		showPlanLimitToast(err(), { scope: 'invite', blocked: 'invite a member' });
		expect(planLimitPrompt.current?.scope).toBeNull();
		const off = planLimitPrompt.registerInlineHost('invite');
		showPlanLimitToast(err(), { scope: 'invite', blocked: 'invite a member' });
		expect(planLimitPrompt.current).toMatchObject({ scope: 'invite', blocked: 'invite a member' });
		off();
		expect(planLimitPrompt.current, 'the host closing takes its prompt with it').toBeNull();
	});
});

// TASK-3619: an editor's upload or image edit past the storage limit raises
// the prompt, and its failure callback gets a marker so it adds no toast.
describe('promptOnPlanLimit', () => {
	beforeEach(() => planLimitPrompt.dismiss());

	it('a plan-limit refusal raises the prompt and comes back as the marker', () => {
		const out = promptOnPlanLimit(err({ plan: 'pro', payer_is_you: true }));
		expect(out).toBeInstanceOf(Error);
		expect((out as Error).message).toBe(PLAN_LIMIT_PROMPTED);
		expect(planLimitPrompt.current?.message).toBe(MSG);
	});

	it('any other error passes through untouched and raises nothing', () => {
		const other = new PadApiError({ code: 'file_too_large', message: 'too big' });
		expect(promptOnPlanLimit(other)).toBe(other);
		expect(planLimitPrompt.current).toBeNull();
	});
});
