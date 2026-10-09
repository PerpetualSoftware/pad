import { describe, it, expect } from 'vitest';
import { TERMS, termTitle, enforcementLabel } from './agentTerms';

describe('agent terms (TASK-2257, C72)', () => {
	it('every tooltip names the term, the value, and what the term means', () => {
		expect(termTitle('trigger', 'on-commit')).toBe(`Trigger: on-commit. ${TERMS.trigger}`);
		expect(termTitle('surface', 'all')).toBe(`Surface: all. ${TERMS.surface}`);
		expect(termTitle('enforcement', 'must')).toBe(`Enforcement: must. ${TERMS.enforcement}`);
	});

	it('explains each enforcement level the pages show', () => {
		for (const level of ['must', 'should', 'nice-to-have']) expect(TERMS.enforcement).toContain(`"${level}"`);
	});

	it('labels enforcement as words', () => {
		expect(enforcementLabel('nice-to-have')).toBe('nice to have');
		expect(enforcementLabel('must')).toBe('must');
	});
});
