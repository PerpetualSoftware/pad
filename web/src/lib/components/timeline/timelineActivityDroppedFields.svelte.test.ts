import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineActivityCard from './TimelineActivityCard.svelte';
import type { Activity } from '$lib/types';

// BUG-2674. A move can discard field values the destination schema has no home
// for. The server has always computed that list (items.MigrateFields' Dropped)
// and now records it in the move's audit metadata — but the report only means
// anything if the surface a user actually reads displays it. Codex round 1
// caught that the timeline renderer ignored the key, which would have left the
// loss silent in the one place someone asks "what happened to my item".
//
// The negative legs are the load-bearing ones: a row that renders
// unconditionally, or renders on every action, would pass a presence-only test.

function activity(overrides: Partial<Activity> = {}): Activity {
	return {
		id: 'act-1',
		workspace_id: 'ws-1',
		document_id: 'item-1',
		action: 'moved',
		actor: 'user',
		actor_name: 'Dave',
		source: 'web',
		metadata: JSON.stringify({
			from_collection: 'tasks',
			to_collection: 'ideas',
			dropped_fields: 'due_date, effort'
		}),
		created_at: new Date('2026-08-19T12:00:00Z').toISOString(),
		...overrides
	} as Activity;
}

describe('TimelineActivityCard dropped-fields row', () => {
	it('renders the dropped keys on a move that discarded values', () => {
		const { getByText } = render(TimelineActivityCard, { activity: activity() });

		expect(getByText('Dropped on move:')).toBeTruthy();
		// The exact keys, not merely the label — a row that rendered a fixed
		// string would satisfy a label-only assertion while telling the user
		// nothing about WHICH fields went.
		expect(getByText('due_date, effort')).toBeTruthy();
	});

	it('renders nothing when the move dropped no fields', () => {
		const { queryByText } = render(TimelineActivityCard, {
			activity: activity({
				metadata: JSON.stringify({ from_collection: 'tasks', to_collection: 'ideas' })
			})
		});

		expect(queryByText('Dropped on move:')).toBeNull();
	});

	it('renders nothing for a non-move action carrying the same key', () => {
		// Guards against keying the row on the metadata alone. `updated`
		// activities share the metadata blob shape, and a dropped_fields key
		// arriving on one is not a move report.
		const { queryByText } = render(TimelineActivityCard, {
			activity: activity({
				action: 'updated',
				metadata: JSON.stringify({ dropped_fields: 'due_date, effort' })
			})
		});

		expect(queryByText('Dropped on move:')).toBeNull();
	});

	it('survives unparseable metadata without throwing', () => {
		const { queryByText } = render(TimelineActivityCard, {
			activity: activity({ metadata: 'not json' })
		});

		expect(queryByText('Dropped on move:')).toBeNull();
	});
});
