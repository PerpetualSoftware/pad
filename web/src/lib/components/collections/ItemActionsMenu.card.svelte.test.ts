import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, screen, fireEvent } from '@testing-library/svelte';
import type { Item } from '$lib/types';
import ItemActionsMenu from './ItemActionsMenu.svelte';

/**
 * TASK-2244 — the card's ONE touch-sized ⋯ at phone width (Dave's ruling, day
 * 83). Star, copy-ref and the reorder ⋮ become entries of this menu.
 *
 * The entry set depends on the HOST: board and list wire reorder, but the
 * starred, tags and roles pages do not. Before this change the menu rendered
 * nothing without `onReorder`, so on those pages a mobile card would have had
 * no way to star or copy at all. The cases below cover each combination, plus
 * the desktop shape (no `card`), which must stay the reorder-only ⋮.
 */

const item = { id: 'i1', slug: 'one', title: 'One', fields: '{}', tags: '[]' } as unknown as Item;

afterEach(() => cleanup());

async function openAndList() {
	await fireEvent.click(screen.getByRole('button', { expanded: false }));
	return screen.getAllByRole('menuitem').map((el) => el.textContent?.trim() ?? '');
}

describe('ItemActionsMenu card mode (TASK-2244)', () => {
	it('without reorder (starred/tags/roles pages): only Star and Copy', async () => {
		render(ItemActionsMenu, {
			props: { item, label: 'One', card: { starred: false, onToggleStar: () => {}, onCopyRef: () => {} } },
		});
		const trigger = screen.getByRole('button', { name: 'Actions for One' });
		expect(trigger.textContent?.trim()).toBe('⋯');
		const entries = await openAndList();
		expect(entries.map((e) => e.replace(/^\S+\s*/, ''))).toEqual(['Star', 'Copy item ID']);
	});

	it('with reorder (board/list): Star, Copy, then the reorder entries', async () => {
		render(ItemActionsMenu, {
			props: {
				item,
				label: 'One',
				onReorder: () => {},
				card: { starred: true, onToggleStar: () => {}, onCopyRef: () => {} },
			},
		});
		const entries = (await openAndList()).map((e) => e.replace(/^\S+\s*/, ''));
		expect(entries).toEqual(['Unstar', 'Copy item ID', 'Move to top', 'Move up', 'Move down', 'Move to bottom']);
	});

	it('an item with no ref offers no Copy entry', async () => {
		render(ItemActionsMenu, {
			props: { item, label: 'One', card: { starred: false, onToggleStar: () => {} } },
		});
		const entries = (await openAndList()).map((e) => e.replace(/^\S+\s*/, ''));
		expect(entries).toEqual(['Star']);
	});

	it('the entries call their handlers', async () => {
		const onToggleStar = vi.fn();
		const onCopyRef = vi.fn();
		const onReorder = vi.fn();
		render(ItemActionsMenu, {
			props: { item, label: 'One', onReorder, card: { starred: false, onToggleStar, onCopyRef } },
		});
		for (const [name, spy] of [
			[/Star/, onToggleStar],
			[/Copy item ID/, onCopyRef],
			[/Move to top/, onReorder],
		] as const) {
			await fireEvent.click(screen.getByRole('button', { expanded: false }));
			await fireEvent.click(screen.getByRole('menuitem', { name }));
			expect(spy).toHaveBeenCalledTimes(1);
		}
		expect(onReorder).toHaveBeenCalledWith('top');
	});

	it('desktop shape (no card): the reorder-only ⋮, unchanged', async () => {
		render(ItemActionsMenu, { props: { item, label: 'One', onReorder: () => {} } });
		const trigger = screen.getByRole('button', { name: 'Reorder One' });
		expect(trigger.textContent?.trim()).toBe('⋮');
		const entries = (await openAndList()).map((e) => e.replace(/^\S+\s*/, ''));
		expect(entries).toEqual(['Move to top', 'Move up', 'Move down', 'Move to bottom']);
	});

	// Base rendered a ⋮ here whose entries called an absent onReorder; only
	// ItemCard's `{#if onReorderItem}` kept it off screen. Now the menu itself
	// renders nothing when it has no entry to offer.
	it('no card and no reorder renders nothing', () => {
		render(ItemActionsMenu, { props: { item, label: 'One' } });
		expect(screen.queryByRole('button')).toBeNull();
	});
});
