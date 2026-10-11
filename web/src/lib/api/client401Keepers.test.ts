import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { api } from './client';
import { registerSessionExpiryKeeper } from '$lib/stores/sessionExpiryKeep';

// BUG-3571 part 2: a session-expiry 401 runs the open items' keepers BEFORE
// it navigates, since the redirect does not move the identity epoch and the
// unload flush after it goes out on a dead session.
function stubWindow(pathname: string, search = '') {
	const win = { location: { pathname, search, href: '' } };
	vi.stubGlobal('window', win);
	return win;
}

function mockFetchOnce(status: number, body: unknown) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => ({ status, ok: status >= 200 && status < 300, json: async () => body })),
	);
}

describe('BUG-3571 part 2: the 401 redirect keeps first', () => {
	beforeEach(() => vi.unstubAllGlobals());
	afterEach(() => vi.unstubAllGlobals());

	it('runs every keeper before the page is sent to /login', async () => {
		const win = stubWindow('/ws/tasks', '?item=TASK-1');
		const hrefAtKeep: string[] = [];
		const offA = registerSessionExpiryKeeper(() => hrefAtKeep.push(win.location.href));
		const offB = registerSessionExpiryKeeper(() => hrefAtKeep.push(win.location.href));
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Not logged in' } });
		await expect(api.items.list('ws')).rejects.toMatchObject({ code: 'unauthorized' });
		expect(hrefAtKeep).toEqual(['', '']);
		expect(win.location.href).toBe(`/login?redirect=${encodeURIComponent('/ws/tasks?item=TASK-1')}`);
		offA();
		offB();
	});

	it('a keeper that throws stops neither the others nor the redirect', async () => {
		const win = stubWindow('/ws/tasks');
		const later = vi.fn();
		const offA = registerSessionExpiryKeeper(() => {
			throw new Error('boom');
		});
		const offB = registerSessionExpiryKeeper(later);
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Not logged in' } });
		await expect(api.items.list('ws')).rejects.toMatchObject({ code: 'unauthorized' });
		expect(later).toHaveBeenCalledOnce();
		expect(win.location.href).toContain('/login');
		offA();
		offB();
	});

	it('no redirect (already on /login, or an auth-form 401): no keep', async () => {
		const keeper = vi.fn();
		const off = registerSessionExpiryKeeper(keeper);
		stubWindow('/login');
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Not logged in' } });
		await expect(api.items.list('ws')).rejects.toBeTruthy();
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Invalid email or password' } });
		await expect(api.auth.login('a@b.c', 'x')).rejects.toBeTruthy();
		expect(keeper).not.toHaveBeenCalled();
		off();
	});

	it('an unregistered keeper is not run', async () => {
		stubWindow('/ws/tasks');
		const keeper = vi.fn();
		registerSessionExpiryKeeper(keeper)();
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Not logged in' } });
		await expect(api.items.list('ws')).rejects.toBeTruthy();
		expect(keeper).not.toHaveBeenCalled();
	});

	// The population is every navigation to /login in the client, found by
	// search: one written outside the helper skips the keepers.
	it('every /login navigation in the client goes through redirectToLogin', () => {
		const src = readFileSync('src/lib/api/client.ts', 'utf8');
		const navs = src.match(/location\.(href\s*=|assign\(|replace\()[^;\n]*/g) ?? [];
		const toLogin = navs.filter((n) => n.includes('login') || n.includes('href = href'));
		expect(toLogin).toEqual(['location.href = href']);
		expect((src.match(/redirectToLogin\(/g) ?? []).length).toBeGreaterThanOrEqual(8);
	});
});
