import { describe, expect, it } from 'vitest';
import { DEFAULT_ROLE_ICON, roleIcon } from './roleIcon';

describe('roleIcon (BUG-3161)', () => {
	it('falls back to the robot CHARACTER, never an HTML entity', () => {
		// Built from the code point so this test cannot be satisfied by an
		// escape that some layer decoded into the same bytes.
		expect(DEFAULT_ROLE_ICON).toBe(String.fromCodePoint(0x1f916));
		expect(roleIcon('')).toBe(String.fromCodePoint(0x1f916));
		expect(roleIcon(undefined)).not.toContain('&#');
		expect(roleIcon(null)).toBe(DEFAULT_ROLE_ICON);
	});

	it('keeps a role\u2019s own icon', () => {
		expect(roleIcon(String.fromCodePoint(0x1f528))).toBe(String.fromCodePoint(0x1f528));
	});
});
