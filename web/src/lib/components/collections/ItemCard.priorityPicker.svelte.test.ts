// TASK-2214: a card's priority chip is a picker when the host page provides
// the write (the collection page does), and a static chip otherwise.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';
import { CARD_PRIORITY_KEY } from '$lib/collections/cardPriority';

vi.mock('$app/state', () => ({ page: { params: { username: 'u', workspace: 'ws' }, url: new URL('http://x/') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
const perm = vi.hoisted(() => ({ canEdit: true }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => perm.canEdit } }));

import ItemCard from './ItemCard.svelte';
import { rowLabel } from './statusPickerTestKit';

const PRIORITY_CHIP = '[aria-haspopup][title="Change priority"]';

function collection(priority: unknown): Collection {
	return {
		id: 'c1', workspace_id: 'ws1', name: 'Tasks', slug: 'tasks', icon: '', description: '',
		schema: JSON.stringify({ fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }, priority] }),
		settings: '{}', sort_order: 0, is_default: true, is_system: false,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', prefix: 'TASK',
	} as unknown as Collection;
}

function item(fields: Record<string, unknown>): Item {
	return {
		id: 'i1', workspace_id: 'ws1', collection_id: 'c1', slug: 't-1', title: 'Task One', content: '',
		fields: JSON.stringify(fields), tags: '[]', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

const SELECT = { key: 'priority', label: 'Priority', type: 'select', options: ['low', 'medium', 'high'] };

afterEach(() => {
	cleanup();
	perm.canEdit = true;
});

describe('card priority picker (TASK-2214)', () => {
	it('opens a picker and writes the chosen priority through the provided writer', async () => {
		const write = vi.fn();
		const screen = render(ItemCard, {
			props: { item: item({ status: 'open', priority: 'medium' }), collection: collection(SELECT) } as never,
			context: new Map([[CARD_PRIORITY_KEY, write]])
		});
		const chip = screen.container.querySelector(PRIORITY_CHIP) as HTMLElement;
		expect(chip, 'priority chip is a picker').not.toBeNull();
		await fireEvent.click(chip);
		await tick();
		const rows = Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitemradio"]'));
		expect(rows.map(rowLabel)).toEqual(['Low', 'Medium', 'High']);
		await fireEvent.click(rows.find((r) => rowLabel(r) === 'High')!);
		expect(write).toHaveBeenCalledTimes(1);
		expect(write.mock.calls[0][1]).toBe('high');
	});

	it('choosing the current priority writes nothing', async () => {
		const write = vi.fn();
		const screen = render(ItemCard, {
			props: { item: item({ status: 'open', priority: 'medium' }), collection: collection(SELECT) } as never,
			context: new Map([[CARD_PRIORITY_KEY, write]])
		});
		await fireEvent.click(screen.container.querySelector(PRIORITY_CHIP) as HTMLElement);
		await tick();
		const rows = Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitemradio"]'));
		await fireEvent.click(rows.find((r) => rowLabel(r) === 'Medium')!);
		expect(write).not.toHaveBeenCalled();
	});

	it('is a static chip with no provider, for a reader, or for a non-select priority', () => {
		const cases: Array<{ name: string; priority: unknown; context?: Map<unknown, unknown>; canEdit?: boolean }> = [
			{ name: 'no provider', priority: SELECT },
			{ name: 'reader', priority: SELECT, context: new Map([[CARD_PRIORITY_KEY, vi.fn()]]), canEdit: false },
			{ name: 'text field', priority: { key: 'priority', label: 'Priority', type: 'text' }, context: new Map([[CARD_PRIORITY_KEY, vi.fn()]]) }
		];
		for (const c of cases) {
			perm.canEdit = c.canEdit ?? true;
			const screen = render(ItemCard, {
				props: { item: item({ status: 'open', priority: 'medium' }), collection: collection(c.priority) } as never,
				...(c.context ? { context: c.context } : {})
			});
			expect(screen.container.textContent, c.name).toContain('Medium');
			expect(screen.container.querySelector(PRIORITY_CHIP), c.name).toBeNull();
			cleanup();
		}
	});
});
