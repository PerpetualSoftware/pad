// The billing portal's error codes (BUG-3898). pad-cloud's GET /billing/portal
// is reached by a full-page navigation, so on failure it sends the browser
// back to /console/billing?portal_error=<code> instead of answering JSON. The
// names are pinned in pad-cloud's stripe.go (portalErr*) and must match.
export const PORTAL_ERROR_MESSAGES: Record<string, string> = {
	not_configured: 'Billing management is not available right now. Please try again later.',
	unauthenticated: 'Your session has ended. Sign in again, then try once more.',
	origin: 'We could not open the billing portal from that link. Open it from the button on this page.',
	no_billing_account: 'There is no billing account for your plan, so there is nothing to manage.',
	stripe_error: 'The billing portal could not be opened. Please try again in a moment.'
};

export const PORTAL_ERROR_FALLBACK = 'The billing portal could not be opened. Please try again, or contact support.';

// portalErrorMessage is the message for a ?portal_error= code: its own for a
// known code, the generic one for anything else. The code is never shown, so
// a crafted link cannot put its text on the page.
export function portalErrorMessage(code: string): string {
	return Object.hasOwn(PORTAL_ERROR_MESSAGES, code) ? PORTAL_ERROR_MESSAGES[code] : PORTAL_ERROR_FALLBACK;
}
