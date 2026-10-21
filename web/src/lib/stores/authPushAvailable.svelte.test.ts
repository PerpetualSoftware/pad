import { describe, expect, it, vi, beforeEach } from 'vitest';

/** TASK-3940: authStore.pushAvailable follows the session's push_available; absent is off. */

const session = vi.hoisted(() => ({ value: null as unknown }));
vi.mock('$lib/api/client', () => ({
	api: { auth: { session: vi.fn(async () => session.value) } }
}));

async function store(extra: Record<string, unknown>) {
	session.value = { authenticated: true, user: { id: 'u1', email: 'u1@example.com' }, ...extra };
	const { authStore } = await import('./auth.svelte');
	await authStore.load();
	return authStore;
}

describe('authStore.pushAvailable', () => {
	beforeEach(() => vi.resetModules());

	it('is true when the session says push is available', async () => {
		expect((await store({ push_available: true })).pushAvailable).toBe(true);
	});

	it('is false when the session omits it', async () => {
		expect((await store({})).pushAvailable).toBe(false);
	});
});
