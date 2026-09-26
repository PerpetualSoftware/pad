// BUG-3050 U1: a browser editor sends the body only when it changed, and then
// always with the row's token.
import { describe, expect, it } from 'vitest';
import { PadApiError } from '$lib/api/client';
import { contentWriteFor, isContentPendingFlush } from './contentWrite';

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
