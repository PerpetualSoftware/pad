import { describe, it, expect, vi, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

// BUG-3195: the nested x/y count is a per-row progress count, so an abandoned
// grandchild leaves both numbers, and 0/0 shows nothing. Mounts the REAL
// component so the wiring from the abandonedStatuses prop to the rendered
// count is what is measured, not the helper alone.

const childrenMock = vi.fn(async () => [] as unknown[]);

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			children: (...args: unknown[]) => childrenMock(...args)
		}
	}
}));

const collectionsState: { collections: unknown[]; collectionsWorkspace: string | null } = {
	collections: [],
	collectionsWorkspace: 'ws'
};
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: collectionsState }));

const { default: NestedChildren } = await import('./NestedChildren.svelte');

const item = (id: string, status: unknown) => ({
	id,
	slug: id,
	title: id,
	collection_slug: 'tasks',
	fields: JSON.stringify({ status })
});

let instance: ReturnType<typeof mount> | null = null;
let target: HTMLElement | null = null;

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	target?.remove();
	target = null;
	childrenMock.mockReset();
});

async function render(statuses: unknown[], abandonedOptions: string[]) {
	collectionsState.collections = [
		{
			slug: 'tasks',
			schema: JSON.stringify({
				fields: [{ key: 'status', type: 'select', terminal_options: ['done', 'cancelled'], abandoned_options: abandonedOptions }]
			}),
			settings: '{}'
		}
	];
	childrenMock.mockResolvedValue(statuses.map((s, i) => item(`c${i}`, s)));
	target = document.createElement('div');
	document.body.appendChild(target);
	instance = mount(NestedChildren, {
		target,
		props: {
			wsSlug: 'ws',
			parentSlug: 'p',
			terminalStatuses: ['done', 'cancelled']
		}
	});
	for (let i = 0; i < 5; i++) {
		await tick();
		await Promise.resolve();
	}
	flushSync();
	return target.querySelector('.nested-count')?.textContent ?? null;
}

describe('NestedChildren progress (BUG-3195)', () => {
	it('ignores a collection store stamped for another workspace', async () => {
		collectionsState.collectionsWorkspace = 'other-ws';
		try {
			// The stale store declares `done` abandoned, which would give 1/1
			// (both done out, cancelled delivered). Ignored, the default lists
			// apply: cancelled leaves, both done count, 2/2.
			expect(await render(['done', 'done', 'cancelled'], ['done'])).toBe('2/2');
		} finally {
			collectionsState.collectionsWorkspace = 'ws';
		}
	});

	it('control for the staleness case: the fresh store gives the declared answer', async () => {
		try {
			expect(await render(['done', 'done', 'cancelled'], ['done'])).toBe('1/1');
		} finally {
			collectionsState.collectionsWorkspace = 'ws';
		}
	});

	it('leaves an abandoned child out of both numbers', async () => {
		expect(await render(['done', 'cancelled', 'open'], ['cancelled'])).toBe('1/2');
	});

	it('shows no count when every child is abandoned', async () => {
		expect(await render(['cancelled'], ['cancelled'])).toBeNull();
		// The rows themselves still render: the child exists, it just is not
		// part of the progress.
		expect(target?.querySelectorAll('.nested-children').length).toBe(1);
	});

	it('control: the declared list decides which child leaves', async () => {
		// A declared list REPLACES the NegativeTerminals fallback, so declaring
		// only `done` makes `cancelled` a delivered terminal: done leaves, and
		// cancelled now counts as done. The exclusion follows the declaration.
		expect(await render(['done', 'cancelled', 'open'], ['done'])).toBe('1/2');
	});
});

describe('NestedChildren done styling for a status that is not a string (BUG-3052 unit 3)', () => {
	it('a status stored as ["done"] sits in the done lane, so its row is styled done', async () => {
		await render([['done'], 'open', 5], []);
		const done = [...(target?.querySelectorAll('.nested-title.done') ?? [])].map((el) => el.textContent?.trim());
		expect(done).toEqual(['c0']);
	});
});
