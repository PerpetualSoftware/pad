import { describe, it, expect } from 'vitest';
import { copyResultToast } from './copyResultToast';
import type { ItemCopyResult } from '$lib/types';

function result(over: { archived?: boolean; stale?: boolean; notUnique?: string[] } = {}): ItemCopyResult {
	return {
		source: { archived: !!over.archived },
		destination: { workspace_name: 'Other', ref: 'TASK-9', slug: 'x' },
		archive_source: !!over.archived,
		warnings: {
			dropped_fields: [],
			...(over.notUnique ? { not_unique: over.notUnique.map((m) => ({ key: 'k', value: 'v', message: m })) } : {}),
			dropped_assignee: false,
			dropped_agent_role: false,
			attachment_count: 0,
			attachment_bytes: 0,
			unresolvable_ref_count: 0,
			...(over.stale ? { source_content_state: 'applied_pending_flush' } : {}),
		},
	} as unknown as ItemCopyResult;
}

// BUG-3230 U1: a copy whose source body was behind its live document says so.
describe('copyResultToast', () => {
	it('a clean copy and a clean move are green', () => {
		expect(copyResultToast(result())).toEqual({ message: 'Copied to Other as TASK-9', type: 'success' });
		expect(copyResultToast(result({ archived: true }))).toEqual({ message: 'Moved to Other as TASK-9', type: 'success' });
	});

	it('a stale source on a copy is not green, and says to copy again once saved', () => {
		const t = copyResultToast(result({ stale: true }));
		expect(t.type).toBe('info');
		expect(t.message).toMatch(/^Copied to Other as TASK-9\. /);
		expect(t.message).toMatch(/may be missing them/);
		expect(t.message).toMatch(/Copy it again once they are saved/);
	});

	it('a stale source on a move names the archived original as where the edits are', () => {
		const t = copyResultToast(result({ stale: true, archived: true }));
		expect(t.type).toBe('info');
		expect(t.message).toMatch(/^Moved to Other as TASK-9\. /);
		expect(t.message).toMatch(/archived original still has them/);
	});

	it('not-unique drops keep their wording (BUG-2367), and combine with a stale source', () => {
		expect(copyResultToast(result({ notUnique: ['slug "a" is taken'] }))).toEqual({
			message: 'Copied to Other as TASK-9, without: slug "a" is taken',
			type: 'info',
			duration: 10000,
		});
		const both = copyResultToast(result({ notUnique: ['slug "a" is taken'], stale: true }));
		expect(both.message).toMatch(/without: slug "a" is taken\. An open tab/);
		expect(both.duration).toBe(15000);
	});
});
