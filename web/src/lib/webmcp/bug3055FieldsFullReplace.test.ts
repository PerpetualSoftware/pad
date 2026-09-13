import { describe, it, expect } from 'vitest';
import { dispatch } from './dispatch';
import type { api as ApiClient } from '$lib/api/client';

type Api = typeof ApiClient;

// BUG-3055 (and BUG-3049's class) — the browser MCP `pad_item` update action
// sent its named keys as `fields`, which is a FULL REPLACE server-side. It
// never reads the item, so every stored field the caller did not mention was
// deleted. No concurrency required: one quiet call loses data.
//
// The fake below applies the real server's two write semantics — `fields`
// replaces the stored blob, `fields_patch` merges per key with nil deleting —
// so the assertion is the OUTCOME on the row, not the shape of the payload. A
// door that goes back to `fields` fails here with the unmentioned key gone.

const WS = 'my-workspace';
const isReadOnly = (_tool: string, action: string) => action !== 'update';

function fakeApi(stored: Record<string, unknown>) {
	const state = { fields: { ...stored } };
	const api = {
		items: {
			update: async (_ws: string, _ref: string, data: Record<string, unknown>) => {
				if (typeof data.fields === 'string') {
					state.fields = JSON.parse(data.fields);
				}
				if (data.fields_patch && typeof data.fields_patch === 'object') {
					for (const [k, v] of Object.entries(data.fields_patch as Record<string, unknown>)) {
						if (v === null) delete state.fields[k];
						else state.fields[k] = v;
					}
				}
				return { ref: 'TASK-1', fields: JSON.stringify(state.fields) };
			},
		},
	};
	return { api: api as unknown as Api, state };
}

describe('webmcp pad_item update — field writes are a merge, never a replace', () => {
	it('leaves a stored field the caller did not name alone', async () => {
		const { api, state } = fakeApi({ status: 'open', priority: 'high' });

		const result = await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			status: 'done',
		});

		expect(result.isError, result.content[0]?.text).not.toBe(true);
		// Premise first: the write the caller ASKED for happened. Without this
		// leg a dispatcher that wrote nothing would pass the assertion below.
		expect(state.fields.status).toBe('done');
		// The defect: `priority` was never mentioned and must still be there.
		expect(state.fields.priority).toBe('high');
	});

	it('leaves unnamed fields alone when writing through `field` key=value entries', async () => {
		const { api, state } = fakeApi({ status: 'open', priority: 'high', category: 'billing' });

		await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			field: ['category=infra'],
		});

		expect(state.fields.category).toBe('infra');
		expect(state.fields.status).toBe('open');
		expect(state.fields.priority).toBe('high');
	});

	it('writes nothing to fields when the caller names no field param', async () => {
		const { api, state } = fakeApi({ status: 'open', priority: 'high' });

		await dispatch(api, WS, isReadOnly, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			title: 'Renamed',
		});

		expect(state.fields).toEqual({ status: 'open', priority: 'high' });
	});
});
