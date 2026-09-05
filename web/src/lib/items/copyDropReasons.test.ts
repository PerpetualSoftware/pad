import { describe, it, expect } from 'vitest';
import {
	COPY_DROP_REASONS,
	copyDropReasonMessage,
	type CopyDropReason,
} from './copyDropReasons';

// The two tests IDEA-2894 exists to make possible. Neither could be written
// while the mapper was inline in CopyItemDialog.svelte and unexported, which
// is why two review-round fixes to it shipped unpinned.

describe('copyDropReasonMessage', () => {
	it('has a sentence for every reason the server can emit', () => {
		// This is round 12's OTHER finding as a test: BUG-2674 added
		// `referent_not_portable` server-side, nothing here learned it, and it
		// rendered through the fallback as the raw enum string in front of a
		// user. A reason that maps to itself is that defect.
		const unmapped = COPY_DROP_REASONS.filter((r) => copyDropReasonMessage(r) === r);
		expect(unmapped, 'reasons rendering as their own raw enum string').toEqual([]);
	});

	it('never claims a target exists, or says where it is', () => {
		// Round 12 (`not_found` asserted non-existence) and round 18
		// (`referent_not_portable` asserted existence AND location) were the
		// same defect twice. The server collapses a HIDDEN target to
		// `not_found`, and emits `referent_not_portable` without resolving the
		// target at all, so any sentence that settles the question is a claim
		// the response does not support.
		//
		// Asserted over the two reasons that carry the hazard rather than all
		// ten: `wrong_collection` legitimately says the target is outside the
		// field's collection, and it may — the server only emits it to a
		// caller who can SEE the target.
		const mustStayNeutral: CopyDropReason[] = ['not_found', 'referent_not_portable'];
		const forbidden = [
			/does not exist/i,
			/no such/i,
			/deleted/i,
			/in the source workspace/i,
			/points at/i,
		];
		for (const reason of mustStayNeutral) {
			const message = copyDropReasonMessage(reason);
			for (const pattern of forbidden) {
				expect(
					pattern.test(message),
					`"${reason}" says "${message}", which asserts something the response does not`
				).toBe(false);
			}
		}
	});

	it('shows an unknown reason verbatim rather than inventing a sentence', () => {
		// A reason this build has never heard of means the server is ahead of
		// the client. Showing the enum is more honest than a made-up sentence
		// or a hidden row — and it is what makes the completeness test above
		// meaningful rather than vacuous.
		expect(copyDropReasonMessage('a_reason_from_a_newer_server')).toBe(
			'a_reason_from_a_newer_server'
		);
	});
});
