import { describe, expect, it } from 'vitest';
import { getActiveKey, getPrimaryDestinations } from './destinations';

// TASK-2258 (audit C43): the library is a primary destination, hidden on a
// guest's shared workspace, and its pages mark it active.
describe('the library destination', () => {
	it('is listed, after Tags, and guest-hidden', () => {
		const dests = getPrimaryDestinations('/u/ws');
		const lib = dests.find((d) => d.key === 'library');
		expect(lib).toMatchObject({ href: '/u/ws/library', label: 'Library', guestHidden: true });
		expect(dests.findIndex((d) => d.key === 'library')).toBe(dests.findIndex((d) => d.key === 'tags') + 1);
	});

	it('is active on the library and its tabs, and not a collection', () => {
		expect(getActiveKey('/u/ws/library', '/u/ws')).toBe('library');
		expect(getActiveKey('/u/ws/library/anything', '/u/ws')).toBe('library');
		expect(getActiveKey('/u/ws/librarian', '/u/ws')).not.toBe('library');
	});
});
