/**
 * The mailbox-only proof an invitation EMAIL's join link carries
 * (TASK-3352): `/join/<code>#proof=<32 hex>`. It is in the fragment so it
 * never reaches the server's request logs or a Referer. Sending it with the
 * accept or the signup is what verifies the invitee's address; the code
 * alone never does (BUG-3348).
 *
 * The join page may send the user away to sign in (including an OAuth round
 * trip) before it accepts, which loses the fragment, so the proof is kept in
 * sessionStorage (this tab only) keyed by the code, and stripped from the
 * address bar so it is not bookmarked or shared by accident.
 */

const PROOF_RE = /^[0-9a-f]{32}$/;
const key = (code: string) => `pad.invite-proof.${code}`;

/** Reads `#proof=` from a fragment, '' when absent or malformed. */
export function proofFromHash(hash: string): string {
	const m = /(?:^#|&)proof=([^&]*)/.exec(hash);
	if (!m) return '';
	let v = '';
	try {
		v = decodeURIComponent(m[1]);
	} catch {
		return ''; // a malformed escape (`#proof=%`) is no proof, not a crash
	}
	return PROOF_RE.test(v) ? v : '';
}

/** Stashes the proof for this code (if the fragment has one) and returns it,
 *  or whatever an earlier visit stashed. */
export function captureInvitationProof(code: string, hash: string): string {
	const fromHash = proofFromHash(hash);
	try {
		if (fromHash) {
			sessionStorage.setItem(key(code), fromHash);
			return fromHash;
		}
		const kept = sessionStorage.getItem(key(code)) ?? '';
		return PROOF_RE.test(kept) ? kept : '';
	} catch {
		return fromHash;
	}
}

/** Forgets the proof once it has been sent with a successful accept/signup. */
export function clearInvitationProof(code: string): void {
	try {
		sessionStorage.removeItem(key(code));
	} catch {
		// storage unavailable: nothing was kept
	}
}
