import { describe, it, expect } from 'vitest';
import { planLimitPromptModel } from './planLimitPrompt';

const MSG = 'This workspace has reached its 3-member limit on Free.';
const on = { commerceAllowed: true, billingAvailable: true };

describe('planLimitPromptModel (TASK-3601)', () => {
	it('the payer gets Upgrade', () => {
		const m = planLimitPromptModel(MSG, { payer_is_you: true }, on);
		expect(m).toMatchObject({ message: MSG, action: 'upgrade' });
	});

	it('anyone else is told who to ask, with a copyable note, and no checkout', () => {
		const m = planLimitPromptModel(MSG, { payer_is_you: false, payer_name: 'Dana' }, on);
		expect(m.action).toBe('ask');
		expect(m.hint).toBe('Ask Dana, the workspace owner, to upgrade.');
		expect(m.note).toContain(MSG);
	});

	it('a nameless payer still gets an ask, without a name', () => {
		expect(planLimitPromptModel(MSG, { payer_is_you: false }, on).hint).toBe('Ask the workspace owner to upgrade.');
	});

	it('billing off: the statement alone, for payer and member alike', () => {
		for (const d of [{ payer_is_you: true }, { payer_is_you: false, payer_name: 'Dana' }]) {
			expect(planLimitPromptModel(MSG, d, { commerceAllowed: true, billingAvailable: false })).toEqual({ message: MSG, action: 'none', hint: '', note: '' });
		}
	});

	it('no commerce here (self-host, the mobile apps): the statement alone', () => {
		expect(planLimitPromptModel(MSG, { payer_is_you: true }, { commerceAllowed: false, billingAvailable: true }).action).toBe('none');
	});

	it('an unknown payer gets the statement alone: checkout would sell the wrong account Pro', () => {
		expect(planLimitPromptModel(MSG, {}, on).action).toBe('none');
		expect(planLimitPromptModel(MSG, undefined, on).action).toBe('none');
	});
});

describe('planLimitPromptModel on Pro (TASK-3619)', () => {
	const PRO = 'This file (2 MB) would take this workspace past its 1 GB storage limit on the Pro plan (1 GB used).';

	it('the payer is offered contact, never an upgrade to the plan they have', () => {
		const m = planLimitPromptModel(PRO, { plan: 'pro', payer_is_you: true }, on);
		expect(m).toMatchObject({ message: PRO, action: 'contact', hint: 'Need more? Contact us.' });
	});

	it('anyone else is asked to raise it with the payer, not to upgrade', () => {
		const m = planLimitPromptModel(PRO, { plan: 'pro', payer_is_you: false, payer_name: 'Dana' }, on);
		expect(m.action).toBe('ask');
		expect(m.hint).toBe('Ask Dana, the workspace owner, about more room.');
		expect(m.hint + m.note).not.toMatch(/upgrade/i);
	});

	it('no commerce here: the statement alone, on Pro too', () => {
		expect(planLimitPromptModel(PRO, { plan: 'pro', payer_is_you: true }, { commerceAllowed: false, billingAvailable: true }).action).toBe('none');
	});

	it('a free-plan limit still offers Upgrade', () => {
		expect(planLimitPromptModel(MSG, { plan: 'free', payer_is_you: true }, on).action).toBe('upgrade');
	});
});
