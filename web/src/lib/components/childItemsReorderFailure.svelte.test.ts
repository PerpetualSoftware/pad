import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

// TASK-2205 (audit C42): a child reorder whose writes do not land reloads the
// real order (BUG-3259), and now says why. Before, the order snapped back with
// nothing on screen to explain it.

const kid = (id: string, n: number) => ({
	id, slug: id, title: 'Child ' + id, item_number: n, collection_slug: 'tasks', collection_id: 'c-tasks',
	fields: JSON.stringify({ status: 'open' }), sort_order: n, tags: '[]',
});
const childrenMock = vi.fn(async () => [kid('a', 1), kid('b', 2)] as unknown[]);
// The reorder is one all-or-nothing request since TASK-3517.
const reorderMock = vi.fn(async () => [] as Array<{ id: string; seq: number }>);

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			children: (...args: unknown[]) => childrenMock(...(args as [])),
			reorder: (...args: unknown[]) => reorderMock(...(args as [])),
		},
	},
}));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/services/sync.svelte', () => ({ syncService: { onSync: () => () => {} } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => true } }));
const toasts = vi.hoisted(() => [] as Array<{ message: string; kind: string }>);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (message: string, kind: string) => { toasts.push({ message, kind }); return 'id'; }, dismiss: () => {} },
}));

const { default: ChildItems } = await import('./ChildItems.svelte');

describe('ChildItems: a reorder that does not land says so (TASK-2205)', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;

	beforeEach(async () => {
		childrenMock.mockClear();
		reorderMock.mockReset();
		reorderMock.mockResolvedValue([]);
		toasts.length = 0;
		target = document.body.appendChild(document.createElement('div'));
		instance = mount(ChildItems, { target, props: { wsSlug: 'ws-1', itemSlug: 'task-1', itemId: 'item-1' } });
		flushSync();
		await vi.waitFor(() => expect(target.querySelectorAll('.child-list').length).toBeGreaterThan(0));
	});

	afterEach(() => {
		if (instance) unmount(instance);
		target.remove();
	});

	function dropReversed() {
		target.querySelector('.child-list')!.dispatchEvent(
			new CustomEvent('finalize', { detail: { items: [kid('b', 2), kid('a', 1)], info: { id: 'b', trigger: 'droppedIntoZone' } } })
		);
	}

	it('a refused reorder toasts and reloads the real order', async () => {
		reorderMock.mockRejectedValue(new Error('rate limited'));
		dropReversed();
		await vi.waitFor(() => expect(childrenMock).toHaveBeenCalledTimes(2));
		expect(toasts).toContainEqual({ message: "Couldn't save the new order, so it was put back.", kind: 'error' });
	});

	it('CONTROL: a reorder that lands toasts nothing', async () => {
		dropReversed();
		await vi.waitFor(() => expect(reorderMock).toHaveBeenCalledTimes(1));
		await new Promise((r) => setTimeout(r, 20));
		expect(toasts.filter((t) => t.kind === 'error')).toEqual([]);
	});
});
