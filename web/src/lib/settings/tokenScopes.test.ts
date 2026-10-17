import { describe, expect, it } from 'vitest';
import fixture from '../../../../internal/server/testdata/token_scope_levels.json';
import { describeTokenScopes } from './tokenScopes';

interface Row {
	scopes: string;
	level: string;
	legacy?: boolean;
	unknown?: string[];
}

// The same rows TestTokenScopeLevelsFixture holds tokenScopeAllows to.
describe('describeTokenScopes agrees with the server (TASK-3866)', () => {
	const rows = (fixture as { rows: Row[] }).rows;

	it('has rows', () => {
		expect(rows.length).toBeGreaterThan(0);
	});

	for (const row of rows) {
		it(`${JSON.stringify(row.scopes)} → ${row.level}`, () => {
			const got = describeTokenScopes(row.scopes);
			expect(got.level).toBe(row.level);
			expect(got.legacy).toBe(row.legacy ?? false);
			expect(got.unknown).toEqual(row.unknown ?? []);
		});
	}

	it('never labels a refused key as having access', () => {
		for (const raw of ['null', 'not-json', '["docs"]']) {
			expect(describeTokenScopes(raw).label).toBe('No access');
		}
	});
});
