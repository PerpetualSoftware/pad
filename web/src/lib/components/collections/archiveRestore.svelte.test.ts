import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { ArchivedCollection, Collection } from '$lib/types';

/**
 * TASK-2189: archiving a collection showed no count, had a two-click guard
 * whatever its size, and could not be undone. Now the confirm names the item
 * count, a big collection needs its name typed, the success toast carries an
 * Undo that restores it, and Settings lists archived collections to restore.
 */
const api = vi.hoisted(() => ({
	del: vi.fn(),
	restore: vi.fn(),
	archived: vi.fn()
}));
const toastShow = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: vi.fn(),
			list: vi.fn().mockResolvedValue([]),
			delete: (...a: unknown[]) => api.del(...a),
			restore: (...a: unknown[]) => api.restore(...a),
			archived: (...a: unknown[]) => api.archived(...a)
		},
		items: { listByCollection: vi.fn().mockResolvedValue([]) }
	},
	isConflictOrNotFound: () => false
}));
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: toastShow, dismiss: vi.fn(), get toasts() { return []; } }
}));

const { default: EditCollectionModal } = await import('./EditCollectionModal.svelte');
const { default: ArchivedCollections } = await import('./ArchivedCollections.svelte');

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle(): Promise<void> {
	flushSync();
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

function coll(itemCount: number): Collection {
	return {
		id: 'c1',
		slug: 'field-notes',
		name: 'Field Notes',
		icon: '',
		description: '',
		prefix: 'FN',
		schema: JSON.stringify({ fields: [] }),
		settings: '{}',
		is_default: false,
		item_count: itemCount,
		updated_at: '2026-10-08T00:00:00Z'
	} as unknown as Collection;
}

function button(text: RegExp): HTMLButtonElement | undefined {
	return [...document.querySelectorAll<HTMLButtonElement>('button')].find((b) => text.test(b.textContent?.trim() ?? ''));
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	api.del.mockReset().mockResolvedValue(undefined);
	api.restore.mockReset().mockResolvedValue({ id: 'c1', slug: 'field-notes', name: 'Field Notes' });
	api.archived.mockReset();
	toastShow.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

async function openConfirm(itemCount: number) {
	app = mount(EditCollectionModal, {
		target: host,
		props: { open: true, collection: coll(itemCount), wsSlug: 'ws', onclose: () => {}, onupdated: () => {} }
	}) as Record<string, unknown>;
	await settle();
	button(/^Archive collection$/)!.click();
	await settle();
}

describe('archive confirm', () => {
	it('names the item count, and a small collection needs no typing', async () => {
		await openConfirm(3);
		expect(document.body.textContent?.replace(/\s+/g, ' ')).toContain('Archive "Field Notes" and its 3 items?');
		expect(document.body.textContent).not.toMatch(/can't be undone/);
		expect(document.body.textContent).toContain('restore it any time');
		expect(document.querySelector('.danger-zone-typed')).toBeNull();
		expect(button(/^Yes, archive$/)!.disabled).toBe(false);
	});

	it('a collection of 25 or more items needs its name typed first', async () => {
		await openConfirm(25);
		const yes = button(/^Yes, archive$/)!;
		expect(yes.disabled).toBe(true);
		const input = document.querySelector<HTMLInputElement>('.danger-zone-typed input')!;
		input.value = 'Field Note';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		expect(yes.disabled).toBe(true);
		input.value = 'Field Notes';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		expect(yes.disabled).toBe(false);
		yes.click();
		await settle();
		expect(api.del).toHaveBeenCalledTimes(1);
	});

	it('an UNKNOWN item count fails closed: the name must be typed', async () => {
		await openConfirm(undefined as unknown as number);
		expect(document.querySelector('.danger-zone-typed')).not.toBeNull();
		expect(button(/^Yes, archive$/)!.disabled).toBe(true);
	});

	it('the success toast carries an Undo that restores the collection by id', async () => {
		await openConfirm(2);
		button(/^Yes, archive$/)!.click();
		await settle();
		const call = toastShow.mock.calls.find((c) => String(c[0]).startsWith('Archived'));
		expect(call, 'an Archived toast').toBeTruthy();
		const action = call![4] as { label: string; onAction: () => void };
		expect(action.label).toBe('Undo');
		action.onAction();
		await settle();
		expect(api.restore).toHaveBeenCalledWith('ws', 'c1');
		expect(toastShow.mock.calls.some((c) => String(c[0]).startsWith('Restored'))).toBe(true);
	});
});

describe('Settings › Collections › Archived collections', () => {
	const archived: ArchivedCollection[] = [
		{ id: 'c1', name: 'Field Notes', slug: 'field-notes', prefix: 'FN', item_count: 214, archived_at: '2026-10-08T00:00:00Z' }
	];

	it('lists archived collections with their counts and restores one', async () => {
		api.archived.mockResolvedValue(archived);
		const onrestored = vi.fn();
		app = mount(ArchivedCollections, { target: host, props: { wsSlug: 'ws', refreshKey: 'a', onrestored } }) as Record<string, unknown>;
		await settle();
		expect(document.body.textContent).toContain('Field Notes');
		expect(document.body.textContent).toContain('214 items');
		button(/^Restore$/)!.click();
		await settle();
		expect(api.restore).toHaveBeenCalledWith('ws', 'c1');
		expect(onrestored).toHaveBeenCalledOnce();
		expect(document.querySelector('[data-testid="archived-collections"]')).toBeNull();
	});

	it('renders nothing when nothing is archived', async () => {
		api.archived.mockResolvedValue([]);
		app = mount(ArchivedCollections, { target: host, props: { wsSlug: 'ws', refreshKey: 'a' } }) as Record<string, unknown>;
		await settle();
		expect(document.querySelector('[data-testid="archived-collections"]')).toBeNull();
	});
});
