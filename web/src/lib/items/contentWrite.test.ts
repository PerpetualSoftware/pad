// BUG-3050 U1: a browser editor sends the body only when it changed, and then
// always with the row's token.
import { describe, expect, it } from 'vitest';
import { PadApiError } from '$lib/api/client';
import {
	contentOutcomeNotice,
	contentWriteFor,
	isContentNotAppliedUnconfirmed,
	isContentPendingFlush,
	isEditsNotStoredRefusal,
	pendingEditsReason,
	prunedEditsNotice,
} from './contentWrite';

const row = { content: 'stored body', seq: 7, updated_at: '2026-09-26T05:00:00Z' };

describe('contentWriteFor', () => {
	it('sends nothing when the body is unchanged (a title-only save must not re-send it)', () => {
		expect(contentWriteFor('stored body', row)).toEqual({});
		expect(contentWriteFor('', { ...row, content: undefined as unknown as string })).toEqual({});
	});

	it('a changed body always carries the row token, so the server can refuse over unflushed edits', () => {
		expect(contentWriteFor('new body', row)).toEqual({ content: 'new body', expected_seq: 7 });
	});

	it('a row with no seq (an old cached row) falls back to updated_at, never to no token', () => {
		expect(contentWriteFor('new body', { ...row, seq: undefined as unknown as number })).toEqual({
			content: 'new body',
			expected_updated_at: '2026-09-26T05:00:00Z',
		});
	});
});

describe('isContentPendingFlush', () => {
	it('recognises only the pending-flush refusal', () => {
		const make = (code: string) => Object.assign(Object.create(PadApiError.prototype), { code, message: code });
		expect(isContentPendingFlush(make('content_pending_flush'))).toBe(true);
		expect(isContentPendingFlush(make('update_conflict'))).toBe(false);
		expect(isContentPendingFlush(new Error('content_pending_flush'))).toBe(false);
	});
});

// TASK-3548: an open tab refusing the apply (BUG-3542) is the same situation,
// for the person saving, as the pending-flush refusal.
describe('isEditsNotStoredRefusal', () => {
	const make = (code: string, details?: Record<string, unknown>) =>
		Object.assign(Object.create(PadApiError.prototype), { code, message: code, details });

	it('recognises content_not_applied only with apply_reason unconfirmed_edits', () => {
		expect(isContentNotAppliedUnconfirmed(make('content_not_applied', { apply_reason: 'unconfirmed_edits' }))).toBe(true);
		// Other content_not_applied arms (the apply failed or its outcome is
		// unknown) are not "another tab has edits": they keep the generic path.
		expect(isContentNotAppliedUnconfirmed(make('content_not_applied', { content_landed: false }))).toBe(false);
		expect(isContentNotAppliedUnconfirmed(make('content_not_applied'))).toBe(false);
		expect(isContentNotAppliedUnconfirmed(make('content_pending_flush'))).toBe(false);
	});

	it('is either refusal, and nothing else', () => {
		expect(isEditsNotStoredRefusal(make('content_pending_flush'))).toBe(true);
		expect(isEditsNotStoredRefusal(make('content_not_applied', { apply_reason: 'unconfirmed_edits' }))).toBe(true);
		expect(isEditsNotStoredRefusal(make('content_not_applied'))).toBe(false);
		expect(isEditsNotStoredRefusal(make('update_conflict'))).toBe(false);
		expect(isEditsNotStoredRefusal(new Error('content_not_applied'))).toBe(false);
	});

	it('the dialog reads an unconfirmed-edits refusal as pending edits, not set-aside ones', () => {
		expect(pendingEditsReason(make('content_not_applied', { apply_reason: 'unconfirmed_edits' }))).toBe('pending');
	});
});

// BUG-3230 U2: the sentence an overwrite owes when it deleted another tab's edits.
describe('prunedEditsNotice', () => {
	it('is null when nothing was deleted', () => {
		expect(prunedEditsNotice(null)).toBeNull();
		expect(prunedEditsNotice({})).toBeNull();
		expect(prunedEditsNotice({ warnings: {} })).toBeNull();
		expect(prunedEditsNotice({ warnings: { pruned_pending_edits: 0 } })).toBeNull();
	});
	it('names the count, singular and plural', () => {
		expect(prunedEditsNotice({ warnings: { pruned_pending_edits: 1 } })).toBe('1 unsaved change from another tab was discarded.');
		expect(prunedEditsNotice({ warnings: { pruned_pending_edits: 3 } })).toBe('3 unsaved changes from another tab were discarded.');
	});
});

// BUG-3230 U3: a body applied to an open tab's live document, reported outside the pane.
describe('contentOutcomeNotice', () => {
	it('is null unless the body went to a live document', () => {
		expect(contentOutcomeNotice(null)).toBeNull();
		expect(contentOutcomeNotice({})).toBeNull();
		expect(contentOutcomeNotice({ warnings: { pruned_pending_edits: 2 } })).toBeNull();
	});
	it('says where the body went and when it shows here', () => {
		expect(contentOutcomeNotice({ warnings: { content_outcome: 'applied_pending_flush' } })).toMatch(/open in another tab.*Until that tab saves it, this page may still show the old body/);
		// BUG-3000: the flush is not guaranteed, so nothing promises it will appear.
		expect(contentOutcomeNotice({ warnings: { content_outcome: 'applied_pending_flush' } })).not.toMatch(/appears|once it saves/);
	});
});
