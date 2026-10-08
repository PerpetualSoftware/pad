import { describe, it, expect } from 'vitest';
import { builtinActive, builtinOfferLabel, builtinFieldChanges } from './builtinOffers';
import type { BuiltinListEntry } from '$lib/types';

const row = (key: string, state: BuiltinListEntry['state'], slug = key): BuiltinListEntry => ({
	item_id: slug,
	slug,
	title: slug,
	collection_slug: 'playbooks',
	key,
	state
});

describe('builtinActive (TASK-3462 U3b)', () => {
	it('matches by key, so a renamed item still reads active', () => {
		expect(builtinActive([row('playbook/ship', 'current', 'renamed')], { key: 'playbook/ship', title: 'Ship tasks' }, new Set())).toBe(true);
	});
	it('falls back to the title for an item with no recorded origin (legacy, before U4)', () => {
		expect(builtinActive([], { key: 'playbook/ship', title: 'Ship tasks' }, new Set(['Ship tasks']))).toBe(true);
	});
	it('is false when neither the key nor the title is present', () => {
		expect(builtinActive([row('playbook/plan', 'current')], { key: 'playbook/ship', title: 'Ship tasks' }, new Set(['Other']))).toBe(false);
	});
});

describe('builtinOfferLabel (TASK-3462 U3b)', () => {
	it('offers nothing for a current item, an unknown origin, or no key', () => {
		expect(builtinOfferLabel([row('k', 'current')], 'k')).toBeNull();
		expect(builtinOfferLabel([row('k', 'unknown_origin')], 'k')).toBeNull();
		expect(builtinOfferLabel([row('k', 'update_available')], undefined)).toBeNull();
	});
	it('says "Update available" for an unedited copy', () => {
		expect(builtinOfferLabel([row('k', 'update_available')], 'k')?.label).toBe('Update available');
	});
	it('says "Library changed" for an edited one', () => {
		expect(builtinOfferLabel([row('k', 'diverged')], 'k')?.label).toBe('Library changed');
	});
	it('prefers the unedited copy when two items share the key', () => {
		const offer = builtinOfferLabel([row('k', 'diverged', 'a'), row('k', 'update_available', 'b')], 'k');
		expect(offer?.entry.slug).toBe('b');
	});
});

describe('builtinFieldChanges (TASK-3462 U3b)', () => {
	it('lists only fields whose value the update changes, never status', () => {
		const changes = builtinFieldChanges(
			{ trigger: 'on-release', scope: 'all', status: 'deprecated' },
			{ trigger: 'manual', scope: 'all', status: 'active' }
		);
		expect(changes).toEqual([{ key: 'trigger', current: 'on-release', library: 'manual' }]);
	});
	it('ignores key order inside an object value', () => {
		expect(builtinFieldChanges({ convention: { a: 1, b: 2 } }, { convention: { b: 2, a: 1 } })).toEqual([]);
	});
	it('lists a field the seed had and the library dropped as removed', () => {
		expect(builtinFieldChanges({ legacy_note: 'x' }, {}, { legacy_note: 'x' })).toEqual([
			{ key: 'legacy_note', current: 'x', library: undefined }
		]);
	});
	it('lists a field the library adds', () => {
		expect(builtinFieldChanges({}, { invocation_slug: 'plan' })).toEqual([
			{ key: 'invocation_slug', current: undefined, library: 'plan' }
		]);
	});
});
