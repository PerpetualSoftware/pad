// A card's status/priority chip asks the SCHEMA, not only the value's shape
// (BUG-3016, codex enumeration round).
//
// The card reads these two fields BY NAME and then tests "is the value a
// string" — the right question for a `multi_relation` (its value is an array,
// so it falls out) and the WRONG one for a scalar `relation`, whose value IS a
// string. A `status` retyped to `relation` therefore rendered its stored item id
// as a title-cased status pill, and that pill is CLICKABLE: cycling it writes a
// status word into a relation field, which the server refuses.
//
// Retyping is reachable — `options` survive it (BUG-3041) and nothing rewrites
// stored values — which is the same door this unit's table arm closes.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$app/state', () => ({ page: { params: { username: 'u', workspace: 'ws' }, url: new URL('http://x/') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	// BUG-3068 round 2 moved the chip's permission gate into `ItemCard`, where the
	// per-item answer lives (`canEditItem`, not the views' collection-level
	// `canEdit` prop). A suite that renders a CLICKABLE chip therefore has to say
	// who is looking; with no membership the store answers false and the chip is
	// correctly withheld. Permission-specific legs live in
	// `chipWritesStatus.svelte.test.ts`, which drives this per test.
	workspaceStore: { canEditItem: () => true },
}));

import ItemCard from './ItemCard.svelte';

afterEach(() => {
	cleanup();
});

const RELATION_ID = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

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

function item(fields: Record<string, unknown>): Item {
	return {
		id: 'i1',
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: 'car-1',
		title: 'Car One',
		content: '',
		fields: JSON.stringify(fields),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

describe('a status/priority field retyped to a relation', () => {
	it('renders no chip and never the stored id', () => {
		const retyped = collection([
			{ key: 'status', label: 'Status', type: 'relation', collection: 'colors', options: ['open', 'done'] },
		]);
		const screen = render(ItemCard, {
			props: { item: item({ status: RELATION_ID }), collection: retyped, statusOptions: ['open', 'done'] } as never,
		});
		// PRECONDITION: the card rendered. "No chip" must not be "no card".
		expect(screen.container.textContent).toContain('Car One');
		expect(screen.container.textContent).not.toContain(RELATION_ID);
		expect(screen.container.querySelectorAll('.chip')).toHaveLength(0);
	});

	it('offers no WRITE either — clicking anything on the card cycles nothing', () => {
		// The withheld chip is the point, not its styling: the chip it used to
		// draw sends a status word into a relation field.
		const retyped = collection([
			{ key: 'status', label: 'Status', type: 'relation', collection: 'colors', options: ['open', 'done'] },
		]);
		const onStatusClick = vi.fn();
		const screen = render(ItemCard, {
			props: {
				item: item({ status: RELATION_ID }),
				collection: retyped,
				statusOptions: ['open', 'done'],
				onStatusClick,
			} as never,
		});
		// PRECONDITIONS (BUG-3068 round 4). Without these the leg passes when the
		// card fails to render at all, or renders nothing clickable: an empty loop
		// followed by `not.toHaveBeenCalled()` asserts nothing about the guard.
		// The card always has other controls (star, ⋮, the ref copy), so a
		// non-empty loop is a real claim rather than a formality.
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(1);
		const clickables = [...screen.container.querySelectorAll('.chip, button, [role="button"]')];
		expect(clickables.length, 'nothing was clicked, so nothing was tested').toBeGreaterThan(0);
		for (const el of clickables) {
			(el as HTMLElement).click();
		}
		expect(onStatusClick).not.toHaveBeenCalled();
	});

	it('CONTROL: an ordinary select status still chips and still cycles', () => {
		// Without this, withholding every chip would satisfy both legs above
		// while removing a working affordance from every card in the app.
		const ordinary = collection([
			{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
		]);
		const onStatusClick = vi.fn();
		const screen = render(ItemCard, {
			props: {
				item: item({ status: 'open' }),
				collection: ordinary,
				statusOptions: ['open', 'done'],
				onStatusClick,
			} as never,
		});
		const chip = screen.container.querySelector('.chip') as HTMLElement | null;
		expect(chip, 'no chip to click').not.toBeNull();
		chip!.click();
		expect(onStatusClick).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: a retyped PRIORITY is withheld while an ordinary one still shows', () => {
		const retyped = collection([
			{ key: 'priority', label: 'Priority', type: 'relation', collection: 'colors', options: ['high'] },
		]);
		const s1 = render(ItemCard, {
			props: { item: item({ priority: RELATION_ID }), collection: retyped } as never,
		});
		expect(s1.container.textContent).not.toContain(RELATION_ID);
		cleanup();

		const ordinary = collection([{ key: 'priority', label: 'Priority', type: 'select', options: ['high'] }]);
		const s2 = render(ItemCard, {
			props: { item: item({ priority: 'high' }), collection: ordinary } as never,
		});
		expect(s2.container.textContent).toContain('High');
	});
});
