import { describe, it, expect } from 'vitest';
import { dispatch } from './dispatch';
import type { api as ApiClient } from '$lib/api/client';

type Api = typeof ApiClient;

// BUG-3037 (codex round 1) — the browser MCP surface must FORWARD the OCC token.
//
// The catalog advertises `expected_seq` / `expected_updated_at` on
// `pad_item.action=update`, and this dispatcher dropped both: an agent driving
// the page could not make a write conditional on the item being unchanged, and
// nothing said so. Same shape as BUG-3055 one parameter over — a declared param
// silently discarded on the way to the server.

const WS = 'my-workspace';
const isReadOnly = (_tool: string, action: string) => action !== 'update';

function capturingApi() {
	const calls: Record<string, unknown>[] = [];
	const api = {
		items: {
			update: async (_ws: string, _ref: string, data: Record<string, unknown>) => {
				calls.push(data);
				return { ref: 'TASK-1' };
			}
		}
	};
	return { api: api as unknown as Api, calls };
}

describe('webmcp pad_item update — the OCC token reaches the server', () => {
	it('forwards expected_seq', async () => {
		const { api, calls } = capturingApi();
		const result = await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			status: 'done',
			expected_seq: 42
		});
		expect(result.isError, result.content[0]?.text).not.toBe(true);
		expect(calls[0]?.expected_seq).toBe(42);
	});

	it('forwards expected_updated_at for a caller that has not moved', async () => {
		const { api, calls } = capturingApi();
		await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			status: 'done',
			expected_updated_at: '2026-09-14T01:16:11Z'
		});
		expect(calls[0]?.expected_updated_at).toBe('2026-09-14T01:16:11Z');
	});

	it('REFUSES a fractional seq rather than rounding it', async () => {
		// 42.7 truncated to 42 is a different row state, and one that may well
		// match — so rounding turns a malformed token into a silent accept.
		const { api, calls } = capturingApi();
		const result = await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			status: 'done',
			expected_seq: 42.7
		});
		expect(result.isError).toBe(true);
		expect(calls.length, 'a malformed token still reached the server').toBe(0);
	});

	it('sends no token when the caller supplies none', async () => {
		// The documented default is last-writer-wins; an absent token must not
		// become an invented one.
		const { api, calls } = capturingApi();
		await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			status: 'done'
		});
		expect(calls[0]).not.toHaveProperty('expected_seq');
		expect(calls[0]).not.toHaveProperty('expected_updated_at');
	});
});
