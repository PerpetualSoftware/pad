import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineCommentCard from './TimelineCommentCard.svelte';
import TimelineEntryList from './TimelineEntryList.svelte';
import type { Comment, TimelineEntry } from '$lib/types';

/**
 * BUG-3437. "commented on update" used to show for any comment with an
 * activity_id, and every comment has one: a standalone comment links to its
 * own "commented" activity. The label now follows the timeline entry's
 * comment_on_update, which the server sets only for a comment an item update
 * carried.
 */
function comment(overrides: Partial<Comment> = {}): Comment {
	return {
		id: 'c-1',
		item_id: 'item-1',
		workspace_id: 'ws-1',
		author: 'Dave',
		body: 'hello',
		created_by: 'user',
		source: 'cli',
		activity_id: 'a-1', // every comment has one
		created_at: new Date('2026-10-06T12:00:00Z').toISOString(),
		updated_at: new Date('2026-10-06T12:00:00Z').toISOString(),
		...overrides
	};
}

const noop = () => {};
const handlers = { onDelete: noop, onReply: noop, onEdit: noop, onReaction: noop, onRemoveReaction: noop };

function entry(onUpdate: boolean | undefined): TimelineEntry {
	return {
		id: 'c-1',
		kind: 'comment',
		created_at: '2026-10-06T12:00:00Z',
		actor: 'user',
		source: 'cli',
		comment: comment(),
		...(onUpdate === undefined ? {} : { comment_on_update: onUpdate })
	} as TimelineEntry;
}

describe('"commented on update" label (BUG-3437)', () => {
	it('a standalone comment with an activity_id shows no label', () => {
		const { queryByText } = render(TimelineCommentCard, { comment: comment(), wsSlug: 'ws', items: [], ...handlers });
		expect(queryByText('commented on update')).toBeNull();
	});

	it('the card shows it when told the comment came with an update', () => {
		const { getByText } = render(TimelineCommentCard, { comment: comment(), onUpdate: true, wsSlug: 'ws', items: [], ...handlers });
		expect(getByText('commented on update')).toBeTruthy();
	});

	it('the entry list passes comment_on_update through, and only true counts', () => {
		for (const [flag, shown] of [
			[true, true],
			[false, false],
			[undefined, false]
		] as const) {
			const { queryByText, unmount } = render(TimelineEntryList, { entries: [entry(flag)], wsSlug: 'ws', items: [], ...handlers });
			expect(queryByText('commented on update') !== null, `comment_on_update=${flag}`).toBe(shown);
			unmount();
		}
	});
});
