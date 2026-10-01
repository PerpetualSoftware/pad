import { describe, expect, it } from 'vitest';
import { readChallengeFragment } from './challengeFragment';

describe('readChallengeFragment (BUG-3322)', () => {
	it('reads the challenge and keeps the query, dropping the fragment', () => {
		const got = readChallengeFragment('https://app.getpad.dev/login?redirect=%2Fconsole#challenge=abc.def');
		expect(got).toEqual({ challenge: 'abc.def', cleaned: '/login?redirect=%2Fconsole' });
	});

	it('returns null with no fragment, or a fragment without a challenge', () => {
		expect(readChallengeFragment('https://app.getpad.dev/login')).toBeNull();
		expect(readChallengeFragment('https://app.getpad.dev/login#other=1')).toBeNull();
		expect(readChallengeFragment('https://app.getpad.dev/login#challenge=')).toBeNull();
	});

	it('never reads a challenge from the query string', () => {
		expect(readChallengeFragment('https://app.getpad.dev/login?challenge=abc')).toBeNull();
	});
});
