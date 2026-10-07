import type { BillingInterval } from '$lib/api/client';

/**
 * Whether to send the user to the Stripe session the sidecar created
 * (TASK-3468). The sidecar echoes the interval it PRICED. Charging an
 * interval other than the one picked is the outcome to prevent, above all an
 * annual pick quietly charged monthly.
 *
 * - echo equals the pick: redirect.
 * - echo missing: a sidecar from before TASK-3367, which sells monthly
 *   whatever it is sent. A monthly pick gets exactly that, so it redirects;
 *   an annual pick would be charged monthly, so it is refused.
 * - echo differs: refused.
 */
export type CheckoutDecision =
	| { kind: 'redirect'; url: string }
	| { kind: 'refuse'; interval: BillingInterval };

export function decideCheckout(
	picked: BillingInterval,
	result: { url: string; interval?: string }
): CheckoutDecision {
	const priced = result.interval ?? 'monthly';
	if (priced === picked && result.url) return { kind: 'redirect', url: result.url };
	return { kind: 'refuse', interval: picked };
}

/** What the user sees when an interval cannot be bought right now. */
export function unavailableMessage(interval: BillingInterval): string {
	return interval === 'annual'
		? "Annual billing isn't available right now. Choose Monthly, or try again later."
		: "Monthly billing isn't available right now. Choose Annual, or try again later.";
}
