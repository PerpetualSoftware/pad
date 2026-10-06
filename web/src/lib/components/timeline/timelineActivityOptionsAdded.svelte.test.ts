import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineActivityCard from './TimelineActivityCard.svelte';
import type { Activity } from '$lib/types';

// BUG-3446. A create in a blank workspace's Conventions or Playbooks
// collection can add a library trigger or scope to the collection's options.
// The created activity records which words and where; this row is how the
// owner sees the options grew and why. Negative legs: a row keyed on the
// metadata alone, or rendered on every action, would pass a presence test.

function activity(overrides: Partial<Activity> = {}): Activity {
	return {
		id: 'act-1',
		workspace_id: 'ws-1',
		document_id: 'item-1',
		action: 'created',
		actor: 'agent',
		actor_name: 'Dave',
		source: 'cli',
		metadata: JSON.stringify({
			options_added: 'scope: backend; trigger: on-implement',
			options_added_collection: 'Conventions'
		}),
		created_at: new Date('2026-10-06T12:00:00Z').toISOString(),
		...overrides
	} as Activity;
}

describe('TimelineActivityCard options-added row', () => {
	it('names the collection and the exact words a create added', () => {
		const { getByText } = render(TimelineActivityCard, { activity: activity() });
		expect(getByText('Added to Conventions options:')).toBeTruthy();
		expect(getByText('scope: backend; trigger: on-implement')).toBeTruthy();
	});

	it('renders nothing on a create that added nothing', () => {
		const { queryByText } = render(TimelineActivityCard, { activity: activity({ metadata: '' }) });
		expect(queryByText(/options:/)).toBeNull();
	});

	it('renders nothing for another action carrying the same key', () => {
		const { queryByText } = render(TimelineActivityCard, {
			activity: activity({ action: 'updated' })
		});
		expect(queryByText(/options:/)).toBeNull();
	});
});
