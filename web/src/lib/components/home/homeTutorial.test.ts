import { describe, expect, it } from 'vitest';
import { showHomeTutorial } from './homeTutorial';

// TASK-4571: Home's Workspaces section restores the old console's one-time
// tutorial for a user who owns no workspace, once the list has loaded.
describe('showHomeTutorial', () => {
	it('is false before the workspace list has loaded', () => {
		expect(showHomeTutorial(false, [], 'u')).toBe(false);
	});
	it('is true once loaded with no workspaces', () => {
		expect(showHomeTutorial(true, [], 'u')).toBe(true);
	});
	it('is true when every workspace is a guest or another owner', () => {
		expect(showHomeTutorial(true, [{ owner_id: 'other' }, { owner_id: undefined }], 'u')).toBe(true);
	});
	it('is false once the user owns a workspace', () => {
		expect(showHomeTutorial(true, [{ owner_id: 'other' }, { owner_id: 'u' }], 'u')).toBe(false);
	});
	it.each(['', null, undefined])('is false with no user id (%s)', userId => {
		expect(showHomeTutorial(true, [], userId)).toBe(false);
	});
});
