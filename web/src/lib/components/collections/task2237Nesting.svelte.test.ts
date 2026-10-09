// TASK-2237 (audit C29): no interactive control inside another.
//  - A card is a div whose title link stretches over it; its buttons are
//    siblings of that link, not descendants of it.
//  - The table's sorted header says so with aria-sort.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$app/state', () => ({ page: { params: { username: 'u', workspace: 'ws' }, url: new URL('http://x/') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => true } }));

import ItemCard from './ItemCard.svelte';
import TableView from './TableView.svelte';

afterEach(() => cleanup());

function collection(): Collection {
	return {
		id: 'c1', workspace_id: 'ws1', name: 'Tasks', slug: 'tasks', icon: '', description: '',
		schema: JSON.stringify({ fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }] }),
		settings: '{}', sort_order: 0, is_default: true, is_system: false,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', prefix: 'TASK',
	} as unknown as Collection;
}

function item(id: string, title: string): Item {
	return {
		id, workspace_id: 'ws1', collection_id: 'c1', slug: id, title, content: '',
		item_number: 7, collection_prefix: 'TASK',
		fields: JSON.stringify({ status: 'open' }), tags: JSON.stringify(['alpha']),
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
		code_context: { pull_request: { number: 12, url: 'https://example.test/pr/12', title: 'PR', state: 'OPEN' } },
	} as unknown as Item;
}

describe('ItemCard: a link and its controls are siblings', () => {
	function mountCard(props: Record<string, unknown> = {}) {
		return render(ItemCard, {
			props: {
				item: item('i1', 'Fix the thing'),
				collection: collection(),
				statusOptions: ['open', 'done'],
				onStatusClick: vi.fn(),
				...props,
			} as never,
		});
	}

	it('the card is not a link, and no link holds a button', () => {
		const { container } = mountCard();
		const card = container.querySelector('.item-card') as HTMLElement;
		expect(card.tagName).toBe('DIV');
		// Precondition: the card does hold buttons (PR badge, copy, star, tag, status).
		expect(card.querySelectorAll('button').length).toBeGreaterThanOrEqual(4);
		expect(container.querySelectorAll('a button, a [role="button"], a a')).toHaveLength(0);
	});

	it("the link is named by the item's title alone and points at the item", () => {
		const { container } = mountCard();
		const links = container.querySelectorAll<HTMLAnchorElement>('.item-card a[href]');
		expect(links).toHaveLength(1);
		expect(links[0].classList.contains('card-link')).toBe(true);
		expect(links[0].textContent?.trim()).toBe('Fix the thing');
		expect(links[0].getAttribute('href')).toBe('/u/ws/tasks/TASK-7');
	});

	it('a plain click on the link opens the pane; a modifier click is left to the browser', async () => {
		const onItemOpen = vi.fn();
		const { container } = mountCard({ onItemOpen });
		const link = container.querySelector('.card-link') as HTMLAnchorElement;
		const plain = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
		link.dispatchEvent(plain);
		expect(onItemOpen).toHaveBeenCalledTimes(1);
		expect(plain.defaultPrevented).toBe(true);

		const meta = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, metaKey: true });
		link.dispatchEvent(meta);
		expect(onItemOpen).toHaveBeenCalledTimes(1);
		expect(meta.defaultPrevented).toBe(false);
	});

	it('a control click does not open the item', async () => {
		const onItemOpen = vi.fn();
		const { container } = mountCard({ onItemOpen });
		await fireEvent.click(container.querySelector('.star-btn') as HTMLElement);
		await fireEvent.click(container.querySelector('.card-tag') as HTMLElement);
		expect(onItemOpen).not.toHaveBeenCalled();
	});
});

describe('TableView: the sorted header carries aria-sort', () => {
	it('none before a sort; ascending, then descending, on the clicked header only', async () => {
		const { container } = render(TableView, {
			props: { items: [item('a', 'Alpha'), item('b', 'Beta')], collection: collection() } as never,
		});
		const headers = () => [...container.querySelectorAll<HTMLElement>('[role="columnheader"]')];
		const titleHeader = () => headers().find((h) => h.textContent?.includes('Title'))!;
		expect(headers().filter((h) => h.hasAttribute('aria-sort'))).toHaveLength(0);

		await fireEvent.click(titleHeader().querySelector('button')!);
		expect(titleHeader().getAttribute('aria-sort')).toBe('ascending');
		expect(headers().filter((h) => h.hasAttribute('aria-sort'))).toHaveLength(1);

		await fireEvent.click(titleHeader().querySelector('button')!);
		expect(titleHeader().getAttribute('aria-sort')).toBe('descending');

		const status = headers().find((h) => h.textContent?.includes('Status'))!;
		await fireEvent.click(status.querySelector('button')!);
		expect(status.getAttribute('aria-sort')).toBe('ascending');
		expect(titleHeader().hasAttribute('aria-sort')).toBe(false);
	});
});
