import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineActivityCard from './TimelineActivityCard.svelte';
import type { Activity } from '$lib/types';

// TASK-3525: sort_order is a gapped sort key (a card is written 1,024 from its
// neighbour, or at the midpoint between two), so "position 2048 → 1536" told a
// reader nothing. The row says which way the item moved: a lower value sorts
// earlier.

function reordered(from: string, to: string): Activity {
	return {
		id: 'act-1',
		workspace_id: 'ws-1',
		document_id: 'item-1',
		action: 'reordered',
		actor: 'user',
		actor_name: 'Dave',
		source: 'web',
		metadata: JSON.stringify({ sort_order_from: from, sort_order_to: to, reorder_batch: 'b1' }),
		created_at: new Date('2026-10-09T12:00:00Z').toISOString()
	} as Activity;
}

describe('TimelineActivityCard reordered row (TASK-3525)', () => {
	it('says "moved up" for a lower value', () => {
		const { getByText, queryByText } = render(TimelineActivityCard, { activity: reordered('2048', '1536') });
		expect(getByText('moved up')).toBeTruthy();
		expect(queryByText(/2048|1536/)).toBeNull();
	});

	it('says "moved down" for a higher value, negatives included', () => {
		const { getByText } = render(TimelineActivityCard, { activity: reordered('-1024', '512') });
		expect(getByText('moved down')).toBeTruthy();
	});
});
