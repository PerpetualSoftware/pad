// TASK-3597 — the copy and push calls carry the server's idempotency key as
// `op_key` in the body (TASK-3585), and only when the caller passed one: a
// keyless call keeps the exact body it always sent.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from './client';

function okFetch(body: unknown) {
	return vi.fn(
		async (_url: string, _init?: RequestInit) =>
			new Response(JSON.stringify(body), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
	);
}

function sentBody(fn: ReturnType<typeof okFetch>): unknown {
	const init = fn.mock.calls[0]?.[1];
	return JSON.parse(String(init?.body));
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => vi.unstubAllGlobals());

describe('api.items.push body', () => {
	const ok = { ref: 'TASK-5', workspace: 'ws', pushed: true, message: 'ok' };

	it('without a target or key: exactly { message }, the pre-S5 broadcast body', async () => {
		const fn = okFetch(ok);
		vi.stubGlobal('fetch', fn);
		await api.items.push('ws', 'task-5', 'hi');
		expect(sentBody(fn)).toStrictEqual({ message: 'hi' });
	});

	it('an undefined target sends no target_session_id key at all', async () => {
		const fn = okFetch(ok);
		vi.stubGlobal('fetch', fn);
		await api.items.push('ws', 'task-5', 'hi', undefined, 'k-1');
		expect(sentBody(fn)).toStrictEqual({ message: 'hi', op_key: 'k-1' });
	});

	it('target and key both ride in the body', async () => {
		const fn = okFetch(ok);
		vi.stubGlobal('fetch', fn);
		await api.items.push('ws', 'task-5', 'hi', 'sess-1', 'k-1');
		expect(sentBody(fn)).toStrictEqual({
			message: 'hi',
			target_session_id: 'sess-1',
			op_key: 'k-1'
		});
	});
});

describe('api.items.copy body', () => {
	const req = { target_workspace: 'dst', target_collection: 'tasks' };

	it('without a key: the request exactly as given', async () => {
		const fn = okFetch({});
		vi.stubGlobal('fetch', fn);
		await api.items.copy('ws', 'task-5', req as never);
		expect(sentBody(fn)).toStrictEqual(req);
	});

	it('with a key: the request plus op_key', async () => {
		const fn = okFetch({});
		vi.stubGlobal('fetch', fn);
		await api.items.copy('ws', 'task-5', req as never, 'k-2');
		expect(sentBody(fn)).toStrictEqual({ ...req, op_key: 'k-2' });
	});
});
