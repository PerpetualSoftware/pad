import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api, parseRetryAfterMs, isRateLimitError, PadApiError } from './client';

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

// ── 429 / Retry-After handling (TASK-2026) ──────────────────────────────────

/**
 * Fetch mock that returns a scripted sequence of responses. Each entry is
 * `{ status, body, retryAfter? }`; the last entry repeats for any calls
 * beyond the script length. Responses carry a minimal `headers.get` so the
 * client's `Retry-After` lookup works.
 */
function mockFetchSequence(responses: { status: number; body: unknown; retryAfter?: string }[]) {
	let i = 0;
	const fetchMock = vi.fn(async () => {
		const r = responses[Math.min(i, responses.length - 1)];
		i += 1;
		return {
			status: r.status,
			ok: r.status >= 200 && r.status < 300,
			headers: {
				get: (name: string) =>
					name.toLowerCase() === 'retry-after' ? (r.retryAfter ?? null) : null,
			},
			json: async () => r.body,
		};
	});
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

describe('parseRetryAfterMs (TASK-2026)', () => {
	it('parses the delta-seconds form to milliseconds', () => {
		expect(parseRetryAfterMs('3')).toBe(3000);
		expect(parseRetryAfterMs('0')).toBe(0);
	});

	it('clamps a huge Retry-After to the 5s ceiling so the UI cannot hang', () => {
		expect(parseRetryAfterMs('86400')).toBe(5000);
	});

	it('parses the HTTP-date form as a clamped future delta', () => {
		const future = new Date(Date.now() + 2000).toUTCString();
		const ms = parseRetryAfterMs(future);
		expect(ms).not.toBeNull();
		// ~2s out, clamped to <= 5s; allow scheduling slack.
		expect(ms!).toBeGreaterThan(500);
		expect(ms!).toBeLessThanOrEqual(5000);
	});

	it('returns 0 for a past HTTP-date', () => {
		const past = new Date(Date.now() - 60_000).toUTCString();
		expect(parseRetryAfterMs(past)).toBe(0);
	});

	it('returns null for a missing / empty / garbage header', () => {
		expect(parseRetryAfterMs(null)).toBeNull();
		expect(parseRetryAfterMs(undefined)).toBeNull();
		expect(parseRetryAfterMs('')).toBeNull();
		expect(parseRetryAfterMs('   ')).toBeNull();
		expect(parseRetryAfterMs('soon')).toBeNull();
	});
});

describe('api client 429 handling (TASK-2026)', () => {
	beforeEach(() => {
		vi.unstubAllGlobals();
	});
	afterEach(() => {
		vi.unstubAllGlobals();
	});

	it('arms a global cooldown so a follow-up GET waits out the last Retry-After instead of bursting (Codex P1)', async () => {
		vi.useFakeTimers();
		// Anchor the clock at epoch 0 so the cooldown this test arms is a
		// small number that is safely in the past once real timers resume —
		// no module-level cooldown leaks into later tests.
		vi.setSystemTime(0);
		try {
			// First chain: a GET that 429s on both the original and its one
			// retry (Retry-After: 1s), exhausting the retry and arming the
			// cooldown ~1s out.
			mockFetchSequence([
				{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '1' },
			]);
			const p1 = api.workspaces.list().catch((e) => e);
			await vi.advanceTimersByTimeAsync(1000); // resolve the internal retry sleep
			expect(isRateLimitError(await p1)).toBe(true);

			// Second chain: an INDEPENDENT follow-up GET. It must sit in the
			// cooldown sleep — no network call — until the window elapses.
			const fetchMock2 = mockFetchSequence([{ status: 200, body: [] }]);
			const p2 = api.workspaces.list();
			expect(fetchMock2).toHaveBeenCalledTimes(0);
			await vi.advanceTimersByTimeAsync(1000);
			await p2;
			expect(fetchMock2).toHaveBeenCalledTimes(1);
		} finally {
			vi.useRealTimers();
		}
	});

	it('retries an idempotent GET exactly once after a 429, then returns the retry payload', async () => {
		const fetchMock = mockFetchSequence([
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '0' },
			{ status: 200, body: [{ id: 'w1' }] },
		]);

		const result = await api.workspaces.list();
		expect(result).toEqual([{ id: 'w1' }]);
		// One original call + one retry.
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('surfaces a distinct rate_limited error when the GET retry also 429s', async () => {
		const fetchMock = mockFetchSequence([
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '0' },
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '0' },
		]);

		const err = await api.workspaces.list().catch((e) => e);
		expect(isRateLimitError(err)).toBe(true);
		expect(err).toBeInstanceOf(PadApiError);
		expect((err as PadApiError).code).toBe('rate_limited');
		// Original + a single retry — never more.
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('does NOT retry a non-idempotent POST on 429 (avoids duplicate writes)', async () => {
		const fetchMock = mockFetchSequence([
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '0' },
			{ status: 200, body: { id: 'w2' } },
		]);

		const err = await api.workspaces
			.create({ name: 'X' } as unknown as Parameters<typeof api.workspaces.create>[0])
			.catch((e) => e);
		expect(isRateLimitError(err)).toBe(true);
		// Exactly one call: the POST was never retried.
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('carries the parsed Retry-After delay on the rate_limited error', async () => {
		mockFetchSequence([
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '0' },
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '2' },
		]);

		const err = (await api.workspaces.list().catch((e) => e)) as PadApiError;
		expect(err.code).toBe('rate_limited');
		expect(err.retryAfterMs).toBe(2000);
	});
});
