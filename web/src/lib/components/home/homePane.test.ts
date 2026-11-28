import { describe, expect, it } from 'vitest';
import { formatItemIn, parseItemIn, homePaneKey, ITEM_IN_PARAM } from './homePane';

// The home item pane carries the opened item's workspace in `item_in`
// beside `?item=` (TASK-4902).
describe('item_in', () => {
	it('round-trips owner, workspace and collection', () => {
		const t = { owner: 'mara', workspace: 'pad', collection: 'tasks' };
		expect(formatItemIn(t)).toBe('mara/pad/tasks');
		expect(parseItemIn(formatItemIn(t))).toEqual(t);
	});

	it('encodes each segment, so a slash or percent inside one survives', () => {
		const t = { owner: 'a/b', workspace: 'w%1', collection: 'c d' };
		const raw = formatItemIn(t);
		expect(raw.split('/')).toHaveLength(3);
		expect(parseItemIn(raw)).toEqual(t);
		// Through a real query string too.
		const url = new URL('http://pad.invalid/');
		url.searchParams.set(ITEM_IN_PARAM, raw);
		expect(parseItemIn(new URL(url.href).searchParams.get(ITEM_IN_PARAM))).toEqual(t);
	});

	it('is null for a missing, empty or malformed value', () => {
		expect(parseItemIn(null)).toBeNull();
		expect(parseItemIn(undefined)).toBeNull();
		expect(parseItemIn('')).toBeNull();
		expect(parseItemIn('mara/pad')).toBeNull();
		expect(parseItemIn('mara/pad/tasks/extra')).toBeNull();
		expect(parseItemIn('mara//tasks')).toBeNull();
		expect(parseItemIn('/pad/tasks')).toBeNull();
		expect(parseItemIn('mara/pad/%E0%A4%A')).toBeNull();
	});

	it('keys a pane target by owner, workspace and ref', () => {
		expect(homePaneKey('mara', 'pad', 'TASK-1')).toBe('mara/pad/TASK-1');
	});
});
