// What a share page says when the link can't be loaded (TASK-2249, audit C85).
//
// The server answers an invalid, expired, revoked, deleted or used-up link with
// the same 404 on purpose, so a stranger can't tell which tokens exist. The page
// used to echo that as "Unable to load / Not found", indistinguishable from a
// typo, and a dropped connection showed the browser's raw "Failed to fetch".
// This keeps the server's single answer and says, in plain words, the things
// the reader can act on.

export interface ShareLoadFailure {
	title: string;
	message: string;
	/** Worth a Retry button: the link may well be fine and the request was not. */
	retryable: boolean;
}

export const LINK_GONE: ShareLoadFailure = {
	title: "This link isn't available",
	message:
		'It may have expired, reached its view limit, or been revoked, or the address is incomplete. Ask the person who shared it for a new link.',
	retryable: false
};

export function shareLoadFailure(err: unknown): ShareLoadFailure {
	const code = typeof err === 'object' && err !== null ? (err as { code?: unknown }).code : undefined;
	if (code === 'not_found') return LINK_GONE;
	if (code === 'rate_limited') {
		return {
			title: 'Too many requests',
			message: 'Pad is limiting requests from your network for a moment. Try again shortly.',
			retryable: true
		};
	}
	if (typeof code === 'string' && code !== '') {
		// Another structured refusal: the server's message is written for people.
		const message = err instanceof Error && err.message ? err.message : 'The server refused this link.';
		return { title: "Couldn't open this link", message, retryable: false };
	}
	// No structured answer: the request never got one (offline, a timeout, a
	// proxy error page). The link itself may be fine.
	return {
		title: "Couldn't reach Pad",
		message: 'Check your connection, then try again.',
		retryable: true
	};
}
