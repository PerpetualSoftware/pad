// What a plan-limit refusal says and offers (TASK-3601). A refusal used to be
// a 6-second toast with a generic billing link, shown to everyone alike. It
// now names the limit and offers what the viewer can actually do:
//   - the payer, where Pro can be bought: Upgrade (then back to the flow);
//   - anyone else, where Pro can be bought: ask the payer, by name;
//   - billing off, or no commerce here (self-host, the mobile apps): the
//     statement alone.
// A limit on Pro (TASK-3619: the interim storage cap) has no plan to upgrade
// to: the payer gets "Need more? Contact us", anyone else is asked to raise
// it with the payer.
// Copy is plain and was shown to Dave before merge (TASK-3601 ruling).

/** Where Upgrade goes: the plan picker, which starts checkout. */
export const BILLING_PATH = '/console/billing';

export interface PlanLimitDetails {
	feature?: string;
	limit?: number;
	current?: number;
	plan?: string;
	payer_is_you?: boolean;
	payer_name?: string;
}

export type PlanLimitAction = 'upgrade' | 'contact' | 'ask' | 'none';

export interface PlanLimitPromptModel {
	/** The server's statement of fact, e.g. "This workspace has reached its 3-member limit on Free." */
	message: string;
	action: PlanLimitAction;
	/** The line under the statement; empty when there is nothing to offer. */
	hint: string;
	/** For 'ask': a note the viewer can copy and send the payer. */
	note: string;
}

export function planLimitPromptModel(
	message: string,
	details: PlanLimitDetails | undefined,
	ctx: { commerceAllowed: boolean; billingAvailable: boolean },
): PlanLimitPromptModel {
	const statement = message || 'Plan limit reached.';
	if (!ctx.commerceAllowed || !ctx.billingAvailable) {
		return { message: statement, action: 'none', hint: '', note: '' };
	}
	// Unknown payer: say what happened and offer nothing (codex r1). Checkout
	// sells Pro to the CURRENT user, so offering it to someone who is not the
	// payer would sell them a plan that does not lift this limit. The server
	// that serves this bundle always says (TASK-3601), so this is a fallback.
	if (details?.payer_is_you === undefined) {
		return { message: statement, action: 'none', hint: '', note: '' };
	}
	const onPro = details.plan === 'pro';
	if (details.payer_is_you) {
		if (onPro) return { message: statement, action: 'contact', hint: 'Need more? Contact us.', note: '' };
		return { message: statement, action: 'upgrade', hint: 'Upgrade to Pro to add more.', note: '' };
	}
	const who = details.payer_name?.trim();
	if (onPro) {
		return {
			message: statement,
			action: 'ask',
			hint: who ? `Ask ${who}, the workspace owner, about more room.` : 'Ask the workspace owner about more room.',
			note: `${statement} Could you free some space, or ask Pad for more?`,
		};
	}
	return {
		message: statement,
		action: 'ask',
		hint: who ? `Ask ${who}, the workspace owner, to upgrade.` : 'Ask the workspace owner to upgrade.',
		note: `${statement} Could you upgrade the workspace to Pro so we can keep going?`,
	};
}
