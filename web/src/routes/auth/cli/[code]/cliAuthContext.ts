// What the CLI sign-in approval page says about who asked (TASK-2253).
//
// The approval mints a 30-day session, and the threat it guards against is
// device-code phishing: someone sends a victim a sign-in link they started
// themselves. So the page says when the request was made and from where,
// and labels the agent string as the requester's own claim, since anything
// in it is text the requester chose.

/** "just now", "1 minute ago", "N minutes ago": a request lives at most 20 minutes. */
export function requestedAgo(createdAt: string, now: Date = new Date()): string {
	const at = Date.parse(createdAt);
	if (Number.isNaN(at)) return '';
	const minutes = Math.floor((now.getTime() - at) / 60_000);
	if (minutes < 1) return 'just now';
	if (minutes === 1) return '1 minute ago';
	if (minutes < 60) return `${minutes} minutes ago`;
	const hours = Math.floor(minutes / 60);
	return hours === 1 ? '1 hour ago' : `${hours} hours ago`;
}

/** The sentence under the heading, from what the server reported. Empty when it reported nothing. */
export function requestedLine(createdAt: string | undefined, ip: string | undefined, now: Date = new Date()): string {
	const ago = createdAt ? requestedAgo(createdAt, now) : '';
	if (ago && ip) return `Requested ${ago} from ${ip}.`;
	if (ago) return `Requested ${ago}.`;
	if (ip) return `Requested from ${ip}.`;
	return '';
}
