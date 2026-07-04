import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api } from './client';

// The 401 interceptor branches on `typeof window !== 'undefined'`. The
// vitest environment here is 'node' (see vitest.config.ts), so `window` is
// absent by default — exactly like calling the client from a non-browser
// context. Tests that need to observe the redirect side effect stub a
// minimal `window` global and restore it afterward so other tests keep
// running in the "no window" (no-op redirect) branch.
function stubWindow(pathname: string, search = '') {
	const win = {
		location: {
			pathname,
			search,
			href: '',
		},
	};
	vi.stubGlobal('window', win);
	return win;
}

function mockFetchOnce(status: number, body: unknown) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => ({
			status,
			ok: status >= 200 && status < 300,
			json: async () => body,
		}))
	);
}

describe('api client 401 handling (BUG-1929)', () => {
	beforeEach(() => {
		vi.unstubAllGlobals();
	});

	afterEach(() => {
		vi.unstubAllGlobals();
	});

	it('surfaces the real server message for a bad /auth/login attempt, without redirecting', async () => {
		const win = stubWindow('/login');
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Invalid email or password' } });

		await expect(api.auth.login('a@b.com', 'wrong')).rejects.toMatchObject({
			message: 'Invalid email or password',
			code: 'unauthorized',
		});
		expect(win.location.href).toBe('');
	});

	it('surfaces the real server message for a bad /auth/2fa/login-verify attempt, without redirecting', async () => {
		const win = stubWindow('/login');
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Invalid 2FA verification' } });

		await expect(api.auth.verify2FA('challenge-token', '000000')).rejects.toMatchObject({
			message: 'Invalid 2FA verification',
			code: 'unauthorized',
		});
		expect(win.location.href).toBe('');
	});

	it('surfaces the real server message for a bad /auth/register attempt, without redirecting', async () => {
		const win = stubWindow('/register');
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Registration failed' } });

		await expect(api.auth.register('a@b.com', 'A', 'password123')).rejects.toMatchObject({
			message: 'Registration failed',
			code: 'unauthorized',
		});
		expect(win.location.href).toBe('');
	});

	it('still redirects to /login with a ?redirect= return-to path for a non-auth-form 401 (session expiry)', async () => {
		const win = stubWindow('/some/workspace/items', '?foo=bar');
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Not logged in' } });

		await expect(api.items.list('some-workspace')).rejects.toMatchObject({
			message: 'Authentication required',
			code: 'unauthorized',
		});
		expect(win.location.href).toBe(
			`/login?redirect=${encodeURIComponent('/some/workspace/items?foo=bar')}`
		);
	});

	it('does not redirect (or loop) when the 401 fires while already on /login', async () => {
		const win = stubWindow('/login', '?redirect=%2Fconsole');
		mockFetchOnce(401, { error: { code: 'unauthorized', message: 'Not logged in' } });

		await expect(api.items.list('some-workspace')).rejects.toMatchObject({
			message: 'Authentication required',
		});
		expect(win.location.href).toBe('');
	});
});
