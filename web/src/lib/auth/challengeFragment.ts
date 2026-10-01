// pad-cloud's OAuth callbacks send a 2FA account to /login with the challenge
// in the URL FRAGMENT (BUG-3322): /login?redirect=…#challenge=<token>. A
// fragment never reaches a server log or a Referer header, which a query
// string would. The caller strips it from the address bar once read.

export interface ChallengeFragment {
	challenge: string;
	/** The same URL without its fragment, for history.replaceState. */
	cleaned: string;
}

export function readChallengeFragment(href: string): ChallengeFragment | null {
	const url = new URL(href);
	if (!url.hash) return null;
	const challenge = new URLSearchParams(url.hash.slice(1)).get('challenge');
	if (!challenge) return null;
	url.hash = '';
	return { challenge, cleaned: url.pathname + url.search };
}
