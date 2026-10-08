import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import TimelineCommentCard from './TimelineCommentCard.svelte';
import { commentChipsStore } from '$lib/decisions/commentChips.svelte';
import { commentConventionChips } from '$lib/decisions/conventionChips';
import type { Comment, ItemDecision } from '$lib/types';

/**
 * TASK-3119 U2b. A comment judged on its own to break a convention shows
 * "Possibly breaks CONVE-N" on its card (and a reply on its own), linking to
 * the convention through a full page load. No chip means nothing: no
 * "complies" state anywhere.
 */
function comment(overrides: Partial<Comment> = {}): Comment {
	return {
		id: 'c-1',
		item_id: 'item-1',
		workspace_id: 'ws-1',
		author: 'Dave',
		body: 'the password is hunter2',
		created_by: 'user',
		source: 'web',
		created_at: '2026-10-08T12:00:00Z',
		updated_at: '2026-10-08T12:00:00Z',
		...overrides
	};
}

function decision(key: string, p: number, current = true): ItemDecision {
	return {
		id: key,
		item_id: 'item-1',
		question_set: 'conventions_comments',
		question_key: key,
		kind: 'noul',
		answer: { type: 'noul', noul: p },
		confidence: null,
		provider: 'typesafe',
		model: 'm',
		evaluated_at: '2026-10-08T12:00:01Z',
		current
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

afterEach(() => {
	cleanup();
	commentChipsStore.clearFor('item-1');
});

describe('commentConventionChips', () => {
	it('keeps current conventions_comments answers at or above 0.9, per comment, sorted', () => {
		const chips = commentConventionChips(
			[
				decision('conv:CONVE-10@c-1', 0.95),
				decision('conv:CONVE-2@c-1', 0.91),
				decision('conv:CONVE-3@c-1', 0.89),
				decision('conv:CONVE-4@c-2', 0.99, false),
				{ ...decision('conv:CONVE-5@c-1', 0.99), question_set: 'conventions' },
				decision('conv:CONVE-6', 0.99)
			],
			'ws'
		);
		expect([...chips.keys()]).toEqual(['c-1']);
		expect(chips.get('c-1')?.map((c) => c.ref)).toEqual(['CONVE-2', 'CONVE-10']);
		expect(chips.get('c-1')?.[0].href).toBe('/-/r/ws/CONVE-2');
	});
});

describe('the comment card', () => {
	it('shows the chip on the comment it is about, and on a reply by itself', () => {
		commentChipsStore.setFor(
			'item-1',
			commentConventionChips([decision('conv:CONVE-2@c-1', 0.97), decision('conv:CONVE-9@c-2', 0.93)], 'ws')
		);
		const reply = comment({ id: 'c-2', parent_id: 'c-1', body: 'api key ak_live_1' });
		const { container } = renderCard({ ...comment(), replies: [reply] } as Comment);
		const chips = [...container.querySelectorAll('a.convention-chip')] as HTMLAnchorElement[];
		expect(chips.map((a) => a.textContent?.trim())).toEqual(['Possibly breaks CONVE-2', 'Possibly breaks CONVE-9']);
		expect(chips[0].getAttribute('href')).toBe('/-/r/ws/CONVE-2');
		expect(chips[0].hasAttribute('data-sveltekit-reload')).toBe(true);
		expect(container.innerHTML).not.toMatch(/complies|compliant|passes/i);
	});

	it('shows nothing for a comment with no chip, or a deleted one', () => {
		commentChipsStore.setFor('item-1', commentConventionChips([decision('conv:CONVE-2@c-1', 0.97)], 'ws'));
		const { container: clean } = renderCard(comment({ id: 'c-other' }));
		expect(clean.querySelector('a.convention-chip')).toBeNull();
		cleanup();
		const { container: gone } = renderCard(comment({ deleted: true, body: '' }));
		expect(gone.querySelector('a.convention-chip')).toBeNull();
	});
});
