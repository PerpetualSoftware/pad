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
