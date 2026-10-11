import { describe, it, expect } from 'vitest';
import { DEFAULT_REDIRECT, postSignupTarget } from './redirect';

// TASK-3595: a first signup lands in its new workspace; an explicit
// destination (a ?redirect=, an accepted invitation) is unchanged.
describe('postSignupTarget', () => {
	it('lands a plain first signup in the workspace the server made', () => {
		expect(postSignupTarget(DEFAULT_REDIRECT, { workspace_path: '/ada/adas-workspace' })).toBe('/ada/adas-workspace');
	});

	it('keeps an explicit redirect (OAuth authorize, CLI/MCP auth, a deep link)', () => {
		const authorize = '/oauth/authorize?client_id=x&state=y';
		expect(postSignupTarget(authorize, { workspace_path: '/ada/adas-workspace' })).toBe(authorize);
		expect(postSignupTarget('/join/abc123', { workspace_path: '/ada/adas-workspace' })).toBe('/join/abc123');
	});

	it('keeps the default when the signup accepted an invitation', () => {
		expect(
			postSignupTarget(DEFAULT_REDIRECT, { workspace_path: '/ada/adas-workspace', accepted_invitation: { workspace_slug: 'team' } }),
		).toBe(DEFAULT_REDIRECT);
	});

	it('keeps the default without a usable workspace (self-host, or the auto-create failed)', () => {
		expect(postSignupTarget(DEFAULT_REDIRECT, {})).toBe(DEFAULT_REDIRECT);
		expect(postSignupTarget(DEFAULT_REDIRECT, null)).toBe(DEFAULT_REDIRECT);
	});

	it('ignores a workspace_path that would not pass as a redirect', () => {
		expect(postSignupTarget(DEFAULT_REDIRECT, { workspace_path: '//evil.example' })).toBe(DEFAULT_REDIRECT);
		expect(postSignupTarget(DEFAULT_REDIRECT, { workspace_path: 'https://evil.example' })).toBe(DEFAULT_REDIRECT);
	});
});
