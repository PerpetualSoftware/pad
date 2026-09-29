import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';

/**
 * PLAN-3291 DR-2 / DR-3 (TASK-3293): authStore.commerceAllowed is cloud mode
 * AND not a mobile shell, the shell being named by the `PadShell/` marker the
 * apps append to the user agent.
 */

const session = vi.hoisted(() => ({ value: null as unknown }));
vi.mock('$lib/api/client', () => ({
	api: { auth: { session: vi.fn(async () => session.value) } }
}));

const BROWSER = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1';
const IOS_SHELL = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 PadShell/1';

function withUA(ua: string) {
	vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ua);
}

async function store(cloud: boolean) {
	session.value = { authenticated: true, cloud_mode: cloud, user: { id: 'u1', email: 'u1@example.com' } };
	const { authStore } = await import('./auth.svelte');
	await authStore.load();
	return authStore;
}

describe('authStore.commerceAllowed', () => {
	beforeEach(() => vi.resetModules());
	afterEach(() => vi.restoreAllMocks());

	it('is true for a browser on Pad Cloud (control)', async () => {
		withUA(BROWSER);
		const s = await store(true);
		expect(s.nativeShell).toBe(false);
		expect(s.commerceAllowed).toBe(true);
	});

	it('is false in the app on Pad Cloud', async () => {
		withUA(IOS_SHELL);
		const s = await store(true);
		expect(s.nativeShell).toBe(true);
		expect(s.commerceAllowed).toBe(false);
	});

	it('is false off Pad Cloud, browser or not', async () => {
		withUA(BROWSER);
		expect((await store(false)).commerceAllowed).toBe(false);
	});

	it('matches a later shell version by prefix', async () => {
		withUA(BROWSER + ' PadShell/2');
		expect((await store(true)).commerceAllowed).toBe(false);
	});
});
