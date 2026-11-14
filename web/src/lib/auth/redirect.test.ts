import { describe, it, expect } from 'vitest';
import { DEFAULT_REDIRECT, validateRedirect, redirectQueryFragment, postSignupTarget } from './redirect';

// Regression pins for the open-redirect guard in validateRedirect (BUG-1929
// follow-up). A bare `startsWith('/')` check is not enough — these
// are the concrete bypass shapes a browser or an upstream redirect handler
// can end up treating as cross-origin, so each MUST fall through to
// DEFAULT_REDIRECT rather than being echoed back as a safe target.
describe('validateRedirect — hostile input rejection', () => {
	const hostileValues = [
		'//host', // protocol-relative — browsers treat this as same-scheme, different host
		'%2F%2Fhost', // percent-encoded protocol-relative (also fails the leading-'/' check outright)
		'/%2Fhost', // single-encoded second slash — decodes to //host
		'/\\host', // backslash form — some browsers/redirect handlers normalize \\ to /
		'/%5Chost' // percent-encoded backslash form — decodes to /\host
	];

	for (const value of hostileValues) {
		it(`rejects ${JSON.stringify(value)}`, () => {
			expect(validateRedirect(value)).toBe(DEFAULT_REDIRECT);
		});
	}

	it('still accepts a legitimate same-origin path', () => {
		expect(validateRedirect('/join/abc123')).toBe('/join/abc123');
	});
});

// Default destinations must not be sent to the hosted signup service.
it.each(['/', '/console', '/console/'])('omits the default %s and lands signup in its new workspace', (target) => {
 expect(redirectQueryFragment(target)).toBe('');
 expect(redirectQueryFragment(target, '&')).toBe('');
 expect(postSignupTarget(target, { workspace_path: '/ada/work' })).toBe('/ada/work');
 expect(postSignupTarget('/', { workspace_path: target })).toBe('/');
});
it('uses root as the default and forwards a real destination', () => {
 expect(DEFAULT_REDIRECT).toBe('/');
 expect(redirectQueryFragment('/settings/account')).toBe('?redirect=%2Fsettings%2Faccount');
});
