import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

// PLAN-3535: a child of a REFERENCE collection (tracks_work: false) is listed
// in its own References group, outside the status groups, with no done
// styling. The same child in a collection that tracks work is an ordinary row.

const childrenMock = vi.fn(async () => [] as unknown[]);
vi.mock('$lib/api/client', () => ({ api: { items: { children: (...a: unknown[]) => childrenMock(...a) } } }));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/services/sync.svelte', () => ({ syncService: { onSync: () => () => {} } }));

const store = vi.hoisted(() => ({ collections: [] as unknown[], collectionsWorkspace: 'ws-1', activeItem: null }));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: store }));

const { default: ChildItems } = await import('./ChildItems.svelte');

const schema = JSON.stringify({ fields: [{ key: 'status', type: 'select', options: ['open', 'done'], terminal_options: ['done'] }] });
const coll = (slug: string, tracks_work: boolean) => ({ id: `c-${slug}`, slug, name: slug, schema, settings: JSON.stringify({ tracks_work }) });
const kid = (id: string, collection_slug: string, status: string) => ({
	id, slug: id, title: `Child ${id}`, collection_slug, fields: JSON.stringify({ status }),
	created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
});

describe('ChildItems References group (PLAN-3535)', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;

	beforeEach(() => {
		childrenMock.mockImplementation(async () => [kid('t', 'tasks', 'done'), kid('o', 'tasks', 'open'), kid('d', 'docs', 'done')]);
		target = document.body.appendChild(document.createElement('div'));
	});
	afterEach(() => {
		if (instance) unmount(instance);
		instance = undefined;
		target.remove();
	});

	async function render() {
		instance = mount(ChildItems, { target, props: { wsSlug: 'ws-1', itemSlug: 'p', itemId: 'p' } });
		flushSync();
		await vi.waitFor(() => expect(target.querySelector('.child-count')).not.toBeNull());
		flushSync();
	}
	const statusRows = () =>
		[...target.querySelectorAll('.child-group:not(.reference-group) .child-title')].map((e) => e.textContent?.trim());

	it('lists a reference child apart, unstyled, and counts the work children only', async () => {
		store.collections = [coll('tasks', true), coll('docs', false)];
		await render();
		const refs = target.querySelector('[data-testid="child-references"]');
		expect(refs?.textContent).toContain('References (1)');
		expect(refs?.textContent).toContain('Child d');
		expect(refs?.querySelectorAll('.child-title.done').length).toBe(0);
		expect(statusRows()).not.toContain('Child d');
		expect(target.querySelector('.child-count')?.textContent?.trim()).toBe('2');
	});

	it('treats the same child as work when its collection tracks work', async () => {
		store.collections = [coll('tasks', true), coll('docs', true)];
		await render();
		expect(target.querySelector('[data-testid="child-references"]')).toBeNull();
		expect(statusRows()).toContain('Child d');
		expect(target.querySelector('.child-count')?.textContent?.trim()).toBe('3');
	});
});
