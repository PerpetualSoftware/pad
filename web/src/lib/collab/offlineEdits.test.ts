import { describe, expect, it } from 'vitest';
import { leaveQuestion, offlineRecoveryOnForceRefresh, unloadLosesEdits } from './offlineEdits';

// TASK-2199: edits that have not reached the server, and a recovered offline
// version that has not been copied, are protected on every way out.

const recovery = { itemId: 'i1', text: 'my offline version' };

describe('offlineRecoveryOnForceRefresh', () => {
	it('hands back unsent edits whose text differs from the server', () => {
		expect(
			offlineRecoveryOnForceRefresh({ unsentLocalEdits: true, liveMarkdown: 'mine', storedContent: 'theirs', itemId: 'i1' })
		).toEqual({ itemId: 'i1', text: 'mine' });
	});
	it('hands back nothing when nothing was unsent, nothing is readable, or the text matches', () => {
		expect(offlineRecoveryOnForceRefresh({ unsentLocalEdits: false, liveMarkdown: 'mine', storedContent: 'x', itemId: 'i1' })).toBeNull();
		expect(offlineRecoveryOnForceRefresh({ unsentLocalEdits: true, liveMarkdown: null, storedContent: 'x', itemId: 'i1' })).toBeNull();
		expect(offlineRecoveryOnForceRefresh({ unsentLocalEdits: true, liveMarkdown: 'same', storedContent: 'same', itemId: 'i1' })).toBeNull();
	});
});

describe('unloadLosesEdits', () => {
	it('prompts for unsent collab edits, an uncopied recovery, or a dirty raw editor', () => {
		expect(unloadLosesEdits({ rawDirty: false, unsentLocalEdits: true, recovery: null })).toBe(true);
		expect(unloadLosesEdits({ rawDirty: false, unsentLocalEdits: false, recovery })).toBe(true);
		expect(unloadLosesEdits({ rawDirty: true, unsentLocalEdits: false, recovery: null })).toBe(true);
	});
	it('does not prompt when nothing would be lost', () => {
		expect(unloadLosesEdits({ rawDirty: false, unsentLocalEdits: false, recovery: null })).toBe(false);
	});
});

describe('leaveQuestion', () => {
	it('asks about an uncopied recovered version first, since it is the last copy', () => {
		expect(leaveQuestion({ unsentLocalEdits: true, recovery })).toMatch(/offline version of this item hasn't been copied/);
	});
	it('asks about unsent edits', () => {
		expect(leaveQuestion({ unsentLocalEdits: true, recovery: null })).toMatch(/haven't reached the server/);
	});
	it('asks nothing when leaving loses nothing', () => {
		expect(leaveQuestion({ unsentLocalEdits: false, recovery: null })).toBeNull();
	});
});
