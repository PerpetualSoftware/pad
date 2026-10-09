import { describe, it, expect } from 'vitest';
import { isPasswordRuleError, localPasswordProblem, passwordDescribedBy } from './passwordRule';

describe('password rule (TASK-2260)', () => {
	it('checks the length half of the rule at the server bounds', () => {
		expect(localPasswordProblem('')).toBe('Password must be at least 8 characters.');
		expect(localPasswordProblem('1234567')).toBe('Password must be at least 8 characters.');
		expect(localPasswordProblem('12345678')).toBeNull();
		expect(localPasswordProblem('x'.repeat(128))).toBeNull();
		expect(localPasswordProblem('x'.repeat(129))).toBe('Password must be at most 128 characters.');
	});

	it("recognises the server's three password refusals, in its own words", () => {
		// internal/server/password_strength.go::validatePasswordStrength
		expect(isPasswordRuleError('Password must be at least 8 characters')).toBe(true);
		expect(isPasswordRuleError('Password must be at most 128 characters')).toBe(true);
		expect(isPasswordRuleError('Password is too weak — try a longer passphrase or add unusual characters')).toBe(true);
	});

	it('leaves every other refusal for the top of the form', () => {
		expect(isPasswordRuleError(undefined)).toBe(false);
		expect(isPasswordRuleError('')).toBe(false);
		expect(isPasswordRuleError('Invalid or expired reset link')).toBe(false);
		expect(isPasswordRuleError('An account with this email already exists')).toBe(false);
		expect(isPasswordRuleError('Current password is incorrect')).toBe(false);
		expect(isPasswordRuleError('Passwords do not match')).toBe(false);
	});

	it('points the input at the rule, and at the error when there is one', () => {
		expect(passwordDescribedBy('setup-password', '')).toBe('setup-password-rule');
		expect(passwordDescribedBy('setup-password', null)).toBe('setup-password-rule');
		expect(passwordDescribedBy('setup-password', 'Password is too weak')).toBe('setup-password-rule setup-password-error');
	});
});
