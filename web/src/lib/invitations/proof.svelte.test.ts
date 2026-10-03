import { describe, it, expect, beforeEach } from 'vitest';
import { proofFromHash, captureInvitationProof, clearInvitationProof } from './proof';

// TASK-3352: the invitation email's #proof= is read, kept per code for this
// tab across a sign-in round trip, and dropped once used.
const P = '0123456789abcdef0123456789abcdef';

describe('invitation proof', () => {
	beforeEach(() => sessionStorage.clear());

	it('reads a well-formed proof from the fragment and nothing else', () => {
		expect(proofFromHash(`#proof=${P}`)).toBe(P);
		expect(proofFromHash(`#x=1&proof=${P}`)).toBe(P);
		expect(proofFromHash('')).toBe('');
		expect(proofFromHash('#proof=short')).toBe('');
		expect(proofFromHash(`#proof=${P.toUpperCase()}`)).toBe('');
	});

	it('keeps the proof per code across a visit without the fragment', () => {
		expect(captureInvitationProof('code-a', `#proof=${P}`)).toBe(P);
		// Back from signing in: the fragment is gone, the stash is not.
		expect(captureInvitationProof('code-a', '')).toBe(P);
		// Another invitation's page never picks it up.
		expect(captureInvitationProof('code-b', '')).toBe('');
	});

	it('is forgotten once used', () => {
		captureInvitationProof('code-a', `#proof=${P}`);
		clearInvitationProof('code-a');
		expect(captureInvitationProof('code-a', '')).toBe('');
	});
});
