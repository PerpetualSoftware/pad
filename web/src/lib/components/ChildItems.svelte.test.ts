import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { ItemEvent } from '$lib/services/sse.svelte';

// PLAN-2154 Phase 0 / R4 (TASK-2156): ChildItems used to read the GLOBAL
// `editorStore.dirty`/`lastSaveTime` singleton to skip reloading on a
// self-triggered content-save echo. With a second concurrently-mounted
// <ItemDetail> (the future full-page-host + docked pane), that singleton is
// shared — one instance's save could suppress a DIFFERENT instance's
// ChildItems reload. The fix reroutes ChildItems to instance-scoped
// `selfDirty`/`selfLastSaveTime` props the owning ItemDetail passes down,
// instead of reading the module singleton directly.
//
// This test mounts the REAL ChildItems.svelte (not a reimplementation) with
// mocked api/sse/sync dependencies and drives the actual SSE callback it
// registers, so it exercises the production suppression logic directly.

const childrenMock = vi.fn(async () => [] as unknown[]);

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			children: (...args: unknown[]) => childrenMock(...args),
		},
	},
}));

let sseCallback: ((event: ItemEvent) => void) | null = null;

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (cb: (event: ItemEvent) => void) => {
			sseCallback = cb;
			return () => {
				sseCallback = null;
			};
		},
	},
}));

vi.mock('$lib/services/sync.svelte', () => ({
	syncService: {
		onSync: () => () => {},
	},
}));

// Dynamic import AFTER the mocks are registered above (vi.mock is hoisted,
// but the component module itself is imported here to keep the mock wiring
// visible at the top of the file).
const { default: ChildItems } = await import('./ChildItems.svelte');

function fireItemUpdated(itemId: string) {
	sseCallback?.({
		type: 'item_updated',
		workspace_id: 'ws-1',
		item_id: itemId,
		title: 'x',
		collection: 'tasks',
		actor: 'user',
		source: 'test',
		timestamp: Date.now(),
	});
}

describe('ChildItems self-save reload suppression (PLAN-2154 Phase 0 / TASK-2156)', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;

	beforeEach(() => {
		childrenMock.mockClear();
		sseCallback = null;
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		if (instance) unmount(instance);
		target.remove();
	});

	async function mountChildItems(props: { selfDirty?: boolean; selfLastSaveTime?: number }) {
		instance = mount(ChildItems, {
			target,
			props: {
				wsSlug: 'ws-1',
				itemSlug: 'task-1',
				itemId: 'item-1',
				...props,
			},
		});
		flushSync();
		// loadChildren() is async; let its promise settle before asserting.
		await vi.waitFor(() => expect(childrenMock).toHaveBeenCalledTimes(1));
	}

	it('reloads on an item_updated event when this instance is NOT self-dirty', async () => {
		await mountChildItems({ selfDirty: false, selfLastSaveTime: 0 });

		fireItemUpdated('some-child-id');
		await vi.waitFor(() => expect(childrenMock).toHaveBeenCalledTimes(2));
	});

	it('suppresses the reload when THIS instance is self-dirty', async () => {
		await mountChildItems({ selfDirty: true, selfLastSaveTime: 0 });

		fireItemUpdated('some-child-id');
		// Give any (wrongly) scheduled reload a chance to fire before asserting
		// the count never moved.
		await new Promise((r) => setTimeout(r, 20));
		expect(childrenMock).toHaveBeenCalledTimes(1);
	});

	it('suppresses the reload within the 5s self-lastSaveTime window even when not dirty', async () => {
		await mountChildItems({ selfDirty: false, selfLastSaveTime: Date.now() });

		fireItemUpdated('some-child-id');
		await new Promise((r) => setTimeout(r, 20));
		expect(childrenMock).toHaveBeenCalledTimes(1);
	});

	it("a DIFFERENT (unrelated) instance's dirty state does not suppress THIS instance's reload", async () => {
		// This is the actual regression PLAN-2154 D2/R4 guards against: two
		// concurrently-mounted ItemDetail instances used to share ONE
		// editorStore.dirty flag. Mounting this instance with its OWN
		// selfDirty=false must reload on an external item_updated event
		// regardless of what any other (unmodeled) instance's dirty state is —
		// there's no shared global left for another instance to pollute.
		await mountChildItems({ selfDirty: false, selfLastSaveTime: 0 });

		fireItemUpdated('some-child-id');
		await vi.waitFor(() => expect(childrenMock).toHaveBeenCalledTimes(2));
	});
});

// BUG-3192 Unit B: the header's completion is the SERVER's (`progress` prop),
// or absent. There is no local fallback: counting `status` against a terminal
// list is the computation that was wrong (a collection whose done field is not
// `status` counted 0 done while the server counted 1).
describe('ChildItems header completion comes from the server or not at all (BUG-3192)', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;
	const kid = (id: string, fields: Record<string, unknown>) => ({
		id, slug: id, title: `Child ${id}`, collection_slug: 'stages', fields: JSON.stringify(fields),
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
	});

	beforeEach(() => {
		childrenMock.mockClear();
		// Two children whose done field is `stage`: one shipped, one not. A
		// `status` count says 0 done.
		childrenMock.mockImplementation(async () => [kid('a', { stage: 'shipped' }), kid('b', { stage: 'todo' })]);
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		if (instance) unmount(instance);
		instance = undefined;
		target.remove();
		childrenMock.mockImplementation(async () => []);
	});

	async function mountWith(progress?: { done: number; total: number; percentage: number }) {
		instance = mount(ChildItems, {
			target,
			props: { wsSlug: 'ws-1', itemSlug: 'p-1', itemId: 'p-1', ...(progress ? { progress } : {}) },
		});
		flushSync();
		await vi.waitFor(() => expect(target.querySelector('.child-count')).not.toBeNull());
	}

	it('with the server numbers: shows them, not a local status count', async () => {
		await mountWith({ done: 1, total: 2, percentage: 50 });
		expect(target.querySelector('.child-count')?.textContent?.trim()).toBe('1/2 done');
		expect((target.querySelector('.progress-fill') as HTMLElement | null)?.style.width).toBe('50%');
	});

	it('without them: the child count alone and no bar, never a locally computed "0/2 done"', async () => {
		await mountWith();
		expect(target.querySelector('.child-count')?.textContent?.trim()).toBe('2');
		expect(target.querySelector('.progress-bar')).toBeNull();
	});
});
