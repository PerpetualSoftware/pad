import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import ListView from './ListView.svelte';

// TASK-2222: a list's group header carries the board lane's bulk actions
// (move, tag, untag, priority, assign) in the same ⋯ LaneActionsMenu, on the
// group's items. Archive keeps its own header button.

const FIELDS = [
	{ key: 'score', label: 'Score', type: 'number' },
	{ key: 'shipped', label: 'Shipped', type: 'checkbox' },
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
];

function collection(): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields: FIELDS }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, fields: Record<string, unknown>): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify({ status: 'open', ...fields }),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

function renderList(props: Record<string, unknown> = {}) {
	return render(ListView, {
		props: {
			items: [item('a1', { status: 'open' }), item('a2', { status: 'open' }), item('b1', { status: 'done' })],
			collection: collection(),
			wsSlug: 'ws',
			groupField: 'status',
			statusOptions: ['open', 'done'],
			canEdit: true,
			...props,
		} as never,
	});
}

const menuButtons = (c: HTMLElement) => [...c.querySelectorAll<HTMLButtonElement>('.group-menu-btn')];
const menuItem = (c: HTMLElement, text: string) =>
	[...c.querySelectorAll<HTMLButtonElement>('.lane-menu-item')].find((b) => b.textContent?.includes(text));

afterEach(() => cleanup());

describe('list group bulk actions (TASK-2222)', () => {
	it('each non-empty group gets a ⋯ menu when the caller passes bulk verbs', () => {
		const { container } = renderList({ onMoveGroup: vi.fn() });
		expect(menuButtons(container).map((b) => b.getAttribute('aria-label'))).toEqual([
			'Open group actions',
			'Done group actions',
		]);
	});

	it('no verbs (a viewer), no menu; archive alone keeps its button', () => {
		const { container } = renderList({ onArchiveGroup: vi.fn() });
		expect(menuButtons(container)).toHaveLength(0);
		expect(container.querySelectorAll('.archive-group-btn').length).toBeGreaterThan(0);
	});

	it('"Move all to" moves exactly the group\'s items, and opening the menu does not collapse the group', async () => {
		const onMoveGroup = vi.fn();
		const { container } = renderList({ onMoveGroup });
		const [openBtn] = menuButtons(container);
		await fireEvent.click(openBtn);
		await tick();
		const header = openBtn.closest('.group-header')!.querySelector('.group-toggle')!;
		expect(header.getAttribute('aria-expanded'), 'the ⋯ click toggled the group').toBe('true');
		// The ⋯ is a sibling of the toggle, not inside it (codex round 1).
		expect(header.contains(openBtn)).toBe(false);
		await fireEvent.click(menuItem(container, 'Move all to')!);
		await tick();
		expect(header.getAttribute('aria-expanded'), 'a click inside the menu toggled the group').toBe('true');
		await fireEvent.click(menuItem(container, 'Done')!);
		await tick();
		expect(onMoveGroup).toHaveBeenCalledTimes(1);
		const [movedItems, target] = onMoveGroup.mock.calls[0];
		expect(movedItems.map((i: Item) => i.id)).toEqual(['a1', 'a2']);
		expect(target).toBe('done');
	});

	it('a click on the menu panel itself (padding, a separator) does not toggle the group', async () => {
		// The menu's buttons and input stop propagation themselves; the panel
		// around them does not, which is what the header's guard is for.
		const { container } = renderList({ onTagGroup: vi.fn(), onMoveGroup: vi.fn() });
		const [openBtn] = menuButtons(container);
		await fireEvent.click(openBtn);
		await tick();
		const header = openBtn.closest('.group-header')!.querySelector('.group-toggle')!;
		await fireEvent.click(container.querySelector('.lane-menu')!);
		await tick();
		expect(header.getAttribute('aria-expanded')).toBe('true');
	});

	it('a key pressed inside the menu does not toggle the group', async () => {
		const { container } = renderList({ onTagGroup: vi.fn() });
		const [openBtn] = menuButtons(container);
		await fireEvent.click(openBtn);
		await tick();
		const header = openBtn.closest('.group-header')!.querySelector('.group-toggle')!;
		await fireEvent.keyDown(menuItem(container, 'Tag all')!, { key: 'Enter' });
		await tick();
		expect(header.getAttribute('aria-expanded')).toBe('true');
	});
});

describe('the group header is one toggle button, with the actions beside it (TASK-2222)', () => {
	it('no role=button wrapper; the toggle button carries aria-expanded and toggles', async () => {
		const { container } = renderList({ onMoveGroup: vi.fn(), onArchiveGroup: vi.fn() });
		const header = container.querySelector('.group-header')!;
		expect(header.getAttribute('role')).toBeNull();
		const toggle = header.querySelector<HTMLButtonElement>('button.group-toggle')!;
		expect(toggle.querySelector('button')).toBeNull();
		expect(toggle.getAttribute('aria-expanded')).toBe('true');
		await fireEvent.click(toggle);
		await tick();
		expect(toggle.getAttribute('aria-expanded')).toBe('false');
		await fireEvent.click(header);
		await tick();
		expect(toggle.getAttribute('aria-expanded'), 'a click on the header space still toggles').toBe('true');
	});
});
