import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineCommentCard from './TimelineCommentCard.svelte';
import { whoOf, sameWriter } from './historyEvents';
import { IMPORTED_TITLE } from '$lib/utils/imported';
import type { Comment, TimelineEntry, Version } from '$lib/types';

/**
 * BUG-3379. A workspace import keeps the export's author strings verbatim,
 * so the rows it wrote say so: the comment card marks an imported comment
 * (and reply), and the history never merges an imported version with a
 * native one by the same claimed writer.
 */
function comment(overrides: Partial<Comment> = {}): Comment {
	return {
		id: 'c-1',
		item_id: 'item-1',
		workspace_id: 'ws-1',
		author: 'Dave Member',
		body: 'hello',
		created_by: 'user',
		source: 'web',
		created_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		updated_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		...overrides
	};
}

const noop = () => {};

function renderCard(c: Comment) {
	return render(TimelineCommentCard, {
		comment: c,
		wsSlug: 'ws',
		items: [],
		onDelete: noop,
		onReply: noop,
		onEdit: noop,
		onReaction: noop,
		onRemoveReaction: noop
	});
}

describe('imported marker on comments', () => {
	it('marks an imported comment and its imported reply', () => {
		const reply = comment({ id: 'c-2', parent_id: 'c-1', imported: true, author: 'Someone Else' });
		const { getAllByTitle } = renderCard({ ...comment({ imported: true }), replies: [reply] } as Comment);
		expect(getAllByTitle(IMPORTED_TITLE)).toHaveLength(2);
	});

	it('does not mark a native comment, whatever its author', () => {
		const { queryByTitle } = renderCard(comment());
		expect(queryByTitle(IMPORTED_TITLE)).toBeNull();
	});
});

describe('imported marker in history', () => {
	function versionEntry(imported: boolean): TimelineEntry {
		const v: Version = {
			id: imported ? 'v-imp' : 'v-nat',
			document_id: 'item-1',
			content: '',
			change_summary: '',
			created_by: 'user',
			source: 'web',
			is_diff: false,
			created_at: '2026-08-24T12:00:00Z',
			imported: imported || undefined
		};
		return { kind: 'version', version: v } as TimelineEntry;
	}

	it('carries the flag and never merges an imported row with a native one', () => {
		const imp = whoOf(versionEntry(true));
		const nat = whoOf(versionEntry(false));
		expect(imp.imported).toBe(true);
		expect(nat.imported).toBeUndefined();
		expect(sameWriter(imp, nat)).toBe(false);
		expect(sameWriter(imp, whoOf(versionEntry(true)))).toBe(true);
	});
});
