// The password rule, stated where people choose a password (TASK-2260).
//
// The server (internal/server/password_strength.go) takes 8-128 characters and
// a zxcvbn score of at least 2, scored against the account's own email, name
// and username. The five pages that set a password checked only the length,
// and never said there was more: `password123` passed every client check and
// was refused after the round trip, at the top of the form. Every page now
// states the rule beside the field, and a refusal for the password, whether
// caught here or by the server, shows on the field itself.
//
// Deliberately NOT a live strength meter (lead ruling, day 90): scoring as
// someone types would need an unauthenticated endpoint that receives the
// password on every pause. The server's own check on submit stays the judge.

export const PASSWORD_RULE = 'At least 8 characters, and not easy to guess. A few unrelated words work well.';

/** The length half of the rule, checked before the round trip, in the server's own words. */
export function localPasswordProblem(password: string): string | null {
	if (password.length < 8) return 'Password must be at least 8 characters.';
	if (password.length > 128) return 'Password must be at most 128 characters.';
	return null;
}

/**
 * Whether a server refusal is about the password itself (length or strength),
 * so the page shows it on the password field rather than at the top of the
 * form. The server's messages are fixed sentences with no reflected input,
 * and all of them come back with the generic validation_error code, so this
 * matches their text. TestValidatePasswordStrength_MessagesTheWebFormsRoute
 * (internal/server/password_strength_test.go) pins the same prefixes.
 */
export function isPasswordRuleError(message: string | null | undefined): boolean {
	if (!message) return false;
	return /^Password must be (at least|at most) \d+ characters/i.test(message) || /^Password is too weak/i.test(message);
}

/** aria-describedby for a new-password input next to a PasswordRuleHint with the same id. */
export function passwordDescribedBy(id: string, error: string | null | undefined): string {
	return error ? `${id}-rule ${id}-error` : `${id}-rule`;
}
