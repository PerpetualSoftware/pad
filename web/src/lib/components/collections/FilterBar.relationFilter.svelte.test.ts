import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, ItemIndexRow } from '$lib/types';

/**
 * TASK-2998 / PLAN-2857 U7 — the filter half.
 *
 * The page's `filteredItems` has always compared `fields[key] === value` for
 * any key, so the FILTERING mechanism is not new. What is new, and what is only
 * observable here, is that a relation field gets a way to SET one and a chip
 * that says what the stored id means (CONVE-19).
 */

const ROWS: Record<string, ItemIndexRow> = {
	'id-red': {
		id: 'id-red',
		title: 'Red',
		item_number: 1,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: null,
	} as unknown as ItemIndexRow,
	'id-gone': {
		id: 'id-gone',
		title: 'Gone',
		item_number: 9,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: '2026-01-01T00:00:00Z',
	} as unknown as ItemIndexRow,
};

// vi.hoisted, because vi.mock factories are hoisted above ordinary consts.
const getByCollection = vi.hoisted(() => vi.fn(() => [] as unknown[]));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		findByIdOrSlug: (_ws: string, id: string) => ROWS[id] ?? null,
		getByCollection,
		bootstrapStateFor: () => 'ready',
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'colors' }, { slug: 'cars' }] },
}));

import FilterBar from './FilterBar.svelte';

function collection(fields: unknown[]): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

const RELATION_FIELD = {
	key: 'car_color',
	label: 'Colour',
	type: 'relation',
	collection: 'colors',
};

function renderBar(overrides: Record<string, unknown> = {}) {
	return render(FilterBar, {
		props: {
			collection: collection([RELATION_FIELD]),
			activeFilters: {},
			searchQuery: '',
			onFilterChange: vi.fn(),
			onSearchChange: vi.fn(),
			wsSlug: 'ws',
			...overrides,
		} as never,
	});
}

const trigger = (screen: { container: HTMLElement }) =>
	screen.container.querySelector('.relation-filter-trigger') as HTMLButtonElement | null;

afterEach(() => {
	cleanup();
});

describe('FilterBar relation filter', () => {
	it('offers a filter for each relation field, named by its label', () => {
		const screen = renderBar();
		expect(trigger(screen)?.textContent?.trim()).toBe('All colour');
	});

	it('renders the ACTIVE value as a chip — ref and title, never the id', () => {
		const screen = renderBar({ activeFilters: { car_color: 'id-red' } });
		const el = trigger(screen)!;
		expect(el.querySelector('.relation-filter-ref')?.textContent).toBe('COLOR-1');
		expect(el.textContent).toContain('Red');
		expect(screen.container.innerHTML).not.toContain('id-red');
	});

	it('says so when the filtered target has been deleted', () => {
		// Filtering by a deleted item is legitimate — the rows still carry the
		// value — so the chip names it rather than pretending the filter is off.
		const screen = renderBar({ activeFilters: { car_color: 'id-gone' } });
		const el = trigger(screen)!;
		expect(el.querySelector('.relation-filter-note')?.textContent).toBe('(deleted)');
		expect(el.textContent).toContain('Gone');
	});

	it('never echoes an unresolvable value back at the user', () => {
		const dangling = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';
		const screen = renderBar({ activeFilters: { car_color: dangling } });
		expect(trigger(screen)?.textContent).toContain('Unresolved reference');
		expect(screen.container.innerHTML).not.toContain(dangling);
	});

	it('clears the filter through the × rather than by re-picking', () => {
		const onFilterChange = vi.fn();
		const screen = renderBar({ activeFilters: { car_color: 'id-red' }, onFilterChange });
		const clear = screen.container.querySelector('.relation-filter-clear') as HTMLButtonElement;
		// PRECONDITION: the clear only exists while a filter is set.
		expect(clear).not.toBeNull();

		clear.click();

		expect(onFilterChange).toHaveBeenCalledWith({});
	});

	it('opens a picker scoped to the field\'s DECLARED collection', async () => {
		// Scoped, not workspace-wide: a relation aimed at `colors` must not
		// offer a task.
		//
		// Asserted through what the picker ASKS THE INDEX FOR, not through the
		// placeholder text. The first version of this test checked
		// `input[placeholder="Search colors…"]` — which this file's own
		// `placeholder` prop produces — so dropping the `collection` prop
		// entirely left it green. Measured.
		getByCollection.mockClear();
		const screen = renderBar();
		await fireEvent.click(trigger(screen)!);
		await tick();
		await tick();

		const picker = screen.container.querySelector('.relation-filter-picker');
		expect(picker).not.toBeNull();
		expect(picker?.getAttribute('aria-label')).toBe('Filter by Colour');
		expect(getByCollection).toHaveBeenCalledWith('ws', 'colors');
	});

	it('offers no × until a filter is actually set', () => {
		// The counterfactual for the clear button: without it, "the × clears the
		// filter" passes against a bar that shows a clear control permanently.
		const screen = renderBar();
		expect(screen.container.querySelector('.relation-filter-clear')).toBeNull();
	});

	it('renders NO relation filter without a workspace slug', () => {
		// The public share view has no workspace context, so it cannot resolve a
		// title — and a chip showing a bare id is worse than no chip.
		const screen = renderBar({ wsSlug: '' });
		expect(trigger(screen)).toBeNull();
	});

	it('renders no relation filter for an ordinary field', () => {
		const screen = renderBar({
			collection: collection([{ key: 'status', label: 'Status', type: 'select', options: ['open'] }]),
		});
		expect(trigger(screen)).toBeNull();
	});
});
