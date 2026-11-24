import { describe, expect, it } from 'vitest';
import type { Collection, Item, SearchResult } from '$lib/types';
import type { PaletteResult } from './groupResults';
import {
	matchMarker,
	reselectAfterAppend,
	resultAnnouncement,
	workspaceLabel,
	collectionChipLabel,
	localStatusFactor,
	sortLocalResults,
} from './groupResults';

function hit(id: string, collection_slug: string, extra?: Partial<Item>): PaletteResult {
	return {
		item: { id, collection_slug, fields: '{}', updated_at: '2026-10-01T00:00:00Z', ...extra } as SearchResult['item'],
		snippet: '',
		rank: 0,
	};
}

function makeCollection(slug: string, schema?: object, settings?: object): Collection {
	return {
		id: `col-${slug}`,
		workspace_id: 'ws-1',
		name: slug,
		slug,
		icon: '📋',
		description: '',
		schema: JSON.stringify(schema ?? { fields: [] }),
		settings: JSON.stringify(settings ?? {}),
		sort_order: 0,
		is_default: false,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'TASK',
	};
}

// TASK-2234: an append can shift rows; the selection follows the item, not the index.
describe('reselectAfterAppend (TASK-2234)', () => {
	const a1 = hit('a1', 'a');
	const b1 = hit('b1', 'b');
	const a2 = hit('a2', 'a');

	it('keeps the selected item selected when an appended row lands above it', () => {
		const before = [a1, b1];
		const after = [a1, a2, b1];
		expect(reselectAfterAppend(before, after, 1)).toBe(2);
	});

	it('leaves no selection as none, and an unmoved selection where it was', () => {
		const before = [a1, b1];
		const after = [a1, b1, a2];
		expect(reselectAfterAppend(before, after, -1)).toBe(-1);
		expect(reselectAfterAppend(before, after, 0)).toBe(0);
	});
});

// TASK-2234: a body-only term has a local total of 0 while the
// content search runs; announcing "0 results" then would be false.
describe('resultAnnouncement (TASK-2234)', () => {
	it('says nothing while either search is still running', () => {
		expect(resultAnnouncement('quokka', true, false, 0)).toBe('');
		expect(resultAnnouncement('quokka', false, true, 0)).toBe('');
	});

	it('counts once both have answered, and says nothing for an empty query', () => {
		expect(resultAnnouncement('quokka', false, false, 1)).toBe('1 result');
		expect(resultAnnouncement('quokka', false, false, 3)).toBe('3 results');
		expect(resultAnnouncement('  ', false, false, 0)).toBe('');
	});
});

describe('matchMarker', () => {
	it('labels a comment match and no other source', () => {
		expect(matchMarker({ matched_in: 'comment' })).toBe('in comment');
		const others: SearchResult['matched_in'][] = ['title', 'content', 'tags', 'ref'];
		for (const matched_in of others) expect(matchMarker({ matched_in })).toBe('');
	});

	it('labels a note and a decision match', () => {
		expect(matchMarker({ matched_in: 'note' })).toBe('in note');
		expect(matchMarker({ matched_in: 'decision' })).toBe('in decision');
	});

	it('gives no label to a locally built row, which names no source', () => {
		expect(matchMarker(hit('a', 'tasks'))).toBe('');
	});
});

describe('workspaceLabel', () => {
	it('returns null when workspace is absent', () => {
		expect(workspaceLabel({}, 'alpha', false)).toBeNull();
		expect(workspaceLabel({}, 'alpha', true)).toBeNull();
	});

	it('hides the current workspace unless allMode is true', () => {
		const r = { workspace: { slug: 'alpha', name: 'Alpha' } };
		expect(workspaceLabel(r, 'alpha', false)).toBeNull();
		expect(workspaceLabel(r, 'alpha', true)).toBe('Alpha');
	});

	it('shows another workspace in both modes, preferring name over slug', () => {
		const named = { workspace: { slug: 'beta', name: 'Beta' } };
		expect(workspaceLabel(named, 'alpha', false)).toBe('Beta');
		expect(workspaceLabel(named, 'alpha', true)).toBe('Beta');
	});

	it('falls back to slug when name is absent or empty', () => {
		expect(workspaceLabel({ workspace: { slug: 'beta' } }, 'alpha', false)).toBe('beta');
		expect(workspaceLabel({ workspace: { slug: 'alpha', name: '' } }, 'alpha', true)).toBe('alpha');
	});
});

describe('collectionChipLabel', () => {
	it('uses collection_name from the first matching result', () => {
		const results = [
			{ item: { collection_slug: 'bugs', collection_name: 'Bugs' } },
			{ item: { collection_slug: 'tasks', collection_name: 'Alpha Tasks' } },
			{ item: { collection_slug: 'tasks', collection_name: 'Beta Tasks' } },
		];
		expect(collectionChipLabel('tasks', results)).toBe('Alpha Tasks');
		expect(collectionChipLabel('bugs', results)).toBe('Bugs');
	});

	it('falls back to the slug when no matching result carries a name', () => {
		const results = [{ item: { collection_slug: 'tasks' } }];
		expect(collectionChipLabel('tasks', results)).toBe('tasks');
		expect(collectionChipLabel('docs', results)).toBe('docs');
	});
});

describe('localStatusFactor', () => {
	const defaultTasks = makeCollection('tasks', {
		fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done', 'cancelled'] }],
	});
	const customTasks = makeCollection('custom', {
		fields: [
			{
				key: 'stage',
				label: 'Stage',
				type: 'select',
				options: ['queued', 'shipped', 'scrapped'],
				terminal_options: ['shipped', 'scrapped'],
				abandoned_options: ['scrapped'],
			},
		],
	}, { board_group_by: 'stage' });
	const collections = [defaultTasks, customTasks];

	it('returns 0.35 for an abandoned status', () => {
		const item = { collection_slug: 'tasks', fields: JSON.stringify({ status: 'cancelled' }) } as Item;
		expect(localStatusFactor(item, collections)).toBe(0.35);
		const customItem = { collection_slug: 'custom', fields: JSON.stringify({ stage: 'scrapped' }) } as Item;
		expect(localStatusFactor(customItem, collections)).toBe(0.35);
	});

	it('returns 0.6 for a terminal non-abandoned status', () => {
		const item = { collection_slug: 'tasks', fields: JSON.stringify({ status: 'done' }) } as Item;
		expect(localStatusFactor(item, collections)).toBe(0.6);
		const customItem = { collection_slug: 'custom', fields: JSON.stringify({ stage: 'shipped' }) } as Item;
		expect(localStatusFactor(customItem, collections)).toBe(0.6);
	});

	it('returns 1 for an open status or unknown collection', () => {
		const openItem = { collection_slug: 'tasks', fields: JSON.stringify({ status: 'open' }) } as Item;
		expect(localStatusFactor(openItem, collections)).toBe(1);
		const unknownColl = { collection_slug: 'unknown', fields: JSON.stringify({ status: 'done' }) } as Item;
		expect(localStatusFactor(unknownColl, collections)).toBe(1);
	});
});

describe('sortLocalResults', () => {
	it('ranks higher score first in relevance mode, and newest updated_at first in updated_at mode', () => {
		const olderOpen = { ...hit('i-open', 'tasks', { updated_at: '2026-10-01T00:00:00Z' }), rank: 10 };
		const newerDone = { ...hit('i-done', 'tasks', { updated_at: '2026-10-20T00:00:00Z' }), rank: 6 };
		expect(sortLocalResults([newerDone, olderOpen], 'relevance').map((r) => r.item.id)).toEqual(['i-open', 'i-done']);
		expect(sortLocalResults([olderOpen, newerDone], 'updated_at').map((r) => r.item.id)).toEqual(['i-done', 'i-open']);
	});
});
