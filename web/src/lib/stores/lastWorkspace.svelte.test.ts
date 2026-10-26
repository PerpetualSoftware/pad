// TASK-4044: the in-memory "last workspace" behind /messages's Back link.
import { beforeEach, describe, expect, it, vi } from 'vitest';

const h = vi.hoisted(() => ({ listeners: [] as Array<(prev: string) => void> }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		onIdentityChange(fn: (prev: string) => void) {
			h.listeners.push(fn);
			return () => {};
		}
	}
}));

type Store = (typeof import('./lastWorkspace.svelte'))['lastWorkspace'];
let store: Store;

beforeEach(async () => {
	h.listeners = [];
	vi.resetModules();
	store = (await import('./lastWorkspace.svelte')).lastWorkspace;
});

describe('lastWorkspace (TASK-4044)', () => {
	it('starts empty', () => {
		expect(store.current).toBeNull();
	});

	it('remembers the latest workspace noted', () => {
		store.note('alice', 'one');
		store.note('alice', 'two');
		expect(store.current).toEqual({ slug: 'two', owner_username: 'alice' });
	});

	it('ignores an empty slug', () => {
		store.note('alice', 'one');
		store.note('alice', '');
		expect(store.current).toEqual({ slug: 'one', owner_username: 'alice' });
	});

	it('is cleared on an identity change', () => {
		store.note('alice', 'one');
		expect(h.listeners).toHaveLength(1);
		for (const fn of h.listeners) fn('previous-user');
		expect(store.current).toBeNull();
	});
});
