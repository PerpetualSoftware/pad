// BUG-3244 ruling 1: edits an editor upgrade SET ASIDE are marked like any
// stale body, but no copy may tell the user that opening the item restores
// them. Each describe pairs the set-aside leg with the pending leg it must
// differ from, so a branch that never fires cannot pass.
import { describe, expect, it } from 'vitest';
import { PadApiError } from '$lib/api/client';
import type { ItemCopyResult } from '$lib/types';
import { pendingEditsReason } from './contentWrite';
import { copyResultToast } from './copyResultToast';
import { rawSeedDecision } from './rawSeed';
import { isBodyStale, isSetAside } from './staleBody';

const refusal = (details?: Record<string, unknown>) =>
	Object.assign(Object.create(PadApiError.prototype), { code: 'content_pending_flush', message: 'x', details });

function copyResult(state: string, archived = false): ItemCopyResult {
	return {
		source: { archived },
		destination: { workspace_name: 'Other', ref: 'TASK-9', slug: 'x' },
		archive_source: archived,
		warnings: {
			dropped_fields: [],
			dropped_assignee: false,
			dropped_agent_role: false,
			attachment_count: 0,
			attachment_bytes: 0,
			unresolvable_ref_count: 0,
			source_content_state: state,
		},
	} as unknown as ItemCopyResult;
}

describe('the stale-body mark covers both values', () => {
	it('marks pending and set-aside, not a current row', () => {
		expect(isBodyStale({ content_state: 'applied_pending_flush' })).toBe(true);
		expect(isBodyStale({ content_state: 'superseded_set_aside' })).toBe(true);
		expect(isBodyStale({})).toBe(false);
		expect(isSetAside('superseded_set_aside')).toBe(true);
		expect(isSetAside('applied_pending_flush')).toBe(false);
	});
});

describe('pendingEditsReason', () => {
	it('reads set_aside_rows from the refusal', () => {
		expect(pendingEditsReason(refusal({ ref: 'TASK-1', pending_rows: 0, set_aside_rows: 2 }))).toBe('set_aside');
	});
	it('a refusal about unflushed edits only, or from an older server, is pending', () => {
		expect(pendingEditsReason(refusal({ ref: 'TASK-1', pending_rows: 3 }))).toBe('pending');
		expect(pendingEditsReason(refusal())).toBe('pending');
		expect(pendingEditsReason(new Error('x'))).toBe('pending');
	});
});

describe('copy and move toast', () => {
	for (const archived of [false, true]) {
		it(`${archived ? 'move' : 'copy'}: a set-aside source never says to open it or wait for a save`, () => {
			const setAside = copyResultToast(copyResult('superseded_set_aside', archived));
			const pending = copyResultToast(copyResult('applied_pending_flush', archived));
			expect(setAside.type).toBe('info');
			expect(setAside.message).toMatch(/earlier editor version/);
			expect(setAside.message).not.toMatch(/open it|once they are saved|open tab/);
			expect(pending.message).toMatch(/open tab/);
		});
	}
});

describe('raw editor switch', () => {
	it('is refused on a set-aside item even when the live read equals the seed', () => {
		const inputs = { liveNow: 'same', lastFlushed: 'same', stored: 'same' };
		expect(rawSeedDecision({ ...inputs, contentState: 'superseded_set_aside' }).refuse).toBe(true);
		expect(rawSeedDecision({ ...inputs, contentState: 'applied_pending_flush' }).refuse).toBe(false);
	});
});
