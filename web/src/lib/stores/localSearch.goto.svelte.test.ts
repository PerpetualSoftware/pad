import { describe, it, expect } from 'vitest';
import { parseGoToTarget } from './localSearch.svelte';

// BUG-2128: the palette's Enter fast-path must treat a full ref
// (`TASK-1345`) as a direct go-to, same as a bare number (BUG-910).
// parseGoToTarget is the pure core of that routing decision.
describe('parseGoToTarget', () => {
	it('parses a bare number with no ref constraint', () => {
		expect(parseGoToTarget('1345')).toEqual({ num: 1345, ref: null });
	});

	it('trims whitespace around either form', () => {
		expect(parseGoToTarget('  42 ')).toEqual({ num: 42, ref: null });
		expect(parseGoToTarget(' task-7 ')).toEqual({ num: 7, ref: 'TASK-7' });
	});

	it('parses a full ref and normalizes the prefix to upper-case', () => {
		expect(parseGoToTarget('TASK-1345')).toEqual({ num: 1345, ref: 'TASK-1345' });
		expect(parseGoToTarget('task-1345')).toEqual({ num: 1345, ref: 'TASK-1345' });
		expect(parseGoToTarget('Bug-8')).toEqual({ num: 8, ref: 'BUG-8' });
	});

	it('preserves the typed digits in the ref while parsing the number', () => {
		// Leading zeros: the number is 7, the ref keeps the typed form —
		// the caller matches on formatItemRef output, which never carries
		// leading zeros, so `TASK-007` deliberately matches nothing
		// rather than jumping to TASK-7 on a guess.
		expect(parseGoToTarget('TASK-007')).toEqual({ num: 7, ref: 'TASK-007' });
	});

	it('rejects everything that is not go-to-shaped', () => {
		expect(parseGoToTarget('')).toBeNull();
		expect(parseGoToTarget('fix the parser')).toBeNull();
		expect(parseGoToTarget('TASK-1345 extra words')).toBeNull();
		expect(parseGoToTarget('TASK-')).toBeNull();
		expect(parseGoToTarget('-1345')).toBeNull();
		expect(parseGoToTarget('TASK-13a5')).toBeNull();
		expect(parseGoToTarget('1345x')).toBeNull();
		expect(parseGoToTarget('#1345')).toBeNull(); // item:/# prefixes are search syntax, not go-to
		expect(parseGoToTarget('TA SK-5')).toBeNull();
	});
});
