import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
	api,
	parseRetryAfterMs,
	isRateLimitError,
	PadApiError,
	setRateLimitHandler,
	isNotFoundError,
	isUpdateConflictError,
	isConflictOrNotFound
} from './client';

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

// BUG-2265 Pattern C: retry paths must recover from BOTH a 409 update_conflict
// (settings/schema changed) AND a 404 not_found (a competing RENAME killed the
// slug). These verify the classification helpers those retries branch on, and
// that a real 404/409 response surfaces as the right PadApiError code.
describe('conflict/not-found classification (BUG-2265 Pattern C)', () => {
	beforeEach(() => vi.unstubAllGlobals());
	afterEach(() => vi.unstubAllGlobals());

	it('a 404 surfaces as not_found and is classified for retry', async () => {
		mockFetchOnce(404, { error: { code: 'not_found', message: 'gone' } });
		const err = await api.collections
			.update('ws', 'renamed-away', { settings: '{}' })
			.then(() => null)
			.catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect((err as PadApiError).code).toBe('not_found');
		expect(isNotFoundError(err)).toBe(true);
		expect(isConflictOrNotFound(err)).toBe(true);
		expect(isUpdateConflictError(err)).toBe(false);
	});

	it('a 409 surfaces as update_conflict and is classified for retry', async () => {
		mockFetchOnce(409, {
			error: { code: 'update_conflict', message: 'stale', details: { ref: 'tasks' } },
		});
		const err = await api.collections
			.update('ws', 'tasks', { settings: '{}', expected_updated_at: 'x' })
			.then(() => null)
			.catch((e) => e);
		expect(err).toBeInstanceOf(PadApiError);
		expect((err as PadApiError).code).toBe('update_conflict');
		expect(isUpdateConflictError(err)).toBe(true);
		expect(isConflictOrNotFound(err)).toBe(true);
		expect(isNotFoundError(err)).toBe(false);
	});

	it('an unrelated error is NOT classified for conflict/not-found retry', async () => {
		mockFetchOnce(500, { error: { code: 'internal_error', message: 'boom' } });
		const err = await api.collections
			.update('ws', 'tasks', { settings: '{}' })
			.then(() => null)
			.catch((e) => e);
		expect(isConflictOrNotFound(err)).toBe(false);
	});
});

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

describe('rate-limit UI seam / setRateLimitHandler (TASK-2080)', () => {
	beforeEach(() => {
		vi.unstubAllGlobals();
	});
	afterEach(() => {
		vi.unstubAllGlobals();
		setRateLimitHandler(null);
	});

	it('fires the registered handler once (with the Retry-After delay) when a 429 surfaces', async () => {
		const handler = vi.fn();
		setRateLimitHandler(handler);
		mockFetchSequence([
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '0' },
			{ status: 429, body: { error: { code: 'rate_limited' } }, retryAfter: '2' },
		]);

		const err = await api.workspaces.list().catch((e) => e);
		expect(isRateLimitError(err)).toBe(true);
		// The surfaced 429 is the single chokepoint — handler fires exactly once,
		// receiving the parsed Retry-After so the toast can name the wait.
		expect(handler).toHaveBeenCalledTimes(1);
		expect(handler).toHaveBeenCalledWith(2000);
	});

	it('does NOT fire the handler for a non-429 error', async () => {
		const handler = vi.fn();
		setRateLimitHandler(handler);
		mockFetchOnce(500, { error: { code: 'internal', message: 'boom' } });

		await expect(api.workspaces.list()).rejects.toBeTruthy();
		expect(handler).not.toHaveBeenCalled();
	});
});

// TASK-2120 — /server/capabilities is static for the binary's lifetime, so the
// client memoizes it at module scope. This locks in the two properties the
// Editor-remount hot path relies on: repeat callers share one fetch, and a
// failed fetch is NOT cached (a later call retries). NOTE: the cache is a
// module singleton, so these tests must be the file's only capabilities()
// callers and must run in order — the failure test runs first (cold cache),
// leaving it cold for the dedupe test that follows.
describe('server.capabilities() caching (TASK-2120)', () => {
	beforeEach(() => {
		vi.unstubAllGlobals();
	});
	afterEach(() => {
		vi.unstubAllGlobals();
	});

	it('does NOT cache a failed capabilities fetch — a later call retries', async () => {
		const failing = vi.fn(async () => ({
			status: 500,
			ok: false,
			json: async () => ({ error: { code: 'internal', message: 'boom' } }),
		}));
		vi.stubGlobal('fetch', failing);
		await expect(api.server.capabilities()).rejects.toBeTruthy();
		expect(failing).toHaveBeenCalledTimes(1);

		// The rejected promise was dropped from the cache, so a retry re-fetches.
		const ok = vi.fn(async () => ({
			status: 200,
			ok: true,
			json: async () => ({ image: { image_formats: ['png'], transcode: false, max_pixels: 1 } }),
		}));
		vi.stubGlobal('fetch', ok);
		await expect(api.server.capabilities()).resolves.toMatchObject({
			image: { image_formats: ['png'] },
		});
		expect(ok).toHaveBeenCalledTimes(1);
	});

	it('fetches /server/capabilities at most once across repeat callers', async () => {
		// The success above warmed the cache, so no further fetch should fire —
		// both callers resolve from the shared cached promise (same reference).
		const fetchMock = vi.fn(async () => ({
			status: 200,
			ok: true,
			json: async () => ({ image: { image_formats: ['jpeg'], transcode: true, max_pixels: 2 } }),
		}));
		vi.stubGlobal('fetch', fetchMock);
		const a = await api.server.capabilities();
		const b = await api.server.capabilities();
		expect(a).toBe(b);
		expect(fetchMock).not.toHaveBeenCalled();
		// Still the warmed 'png' value, proving the cache served it (not 'jpeg').
		expect(a.image.image_formats).toEqual(['png']);
	});
});
