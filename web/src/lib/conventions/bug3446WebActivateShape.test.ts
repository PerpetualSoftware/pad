import { describe, it, expect, vi, afterEach } from 'vitest';
import { api } from '$lib/api/client';
import { conventionCreatePayload } from './createPayload';

// BUG-3446: a blank workspace's Conventions collection seeds trigger [always]
// and scope [all], and the server now adds a library word a write needs. The
// Library page's Activate no longer builds the item: since TASK-3462 it names
// the entry to POST /library/activate, which the Go test
// (internal/server/bug3446_blank_library_options_test.go, the "activate" door)
// drives for every library convention. This pins that the browser sends the
// key and nothing else, so a body the server never sees cannot drift. The
// Conventions page's own create still builds its fields; that half is pinned
// below. The entry is the library's one multi-surface convention: its scope
// is surfaces[0], a word blank does not list either.

const entry = {
	key: 'convention/update-docs-on-api-changes',
	title: 'Update docs on API changes',
	content: 'body',
	category: 'docs',
	trigger: 'on-implement',
	surfaces: ['backend', 'docs'],
	enforcement: 'should' as const,
	commands: [] as string[]
};

const expectedFields = {
	status: 'active',
	category: 'docs',
	trigger: 'on-implement',
	scope: 'backend',
	priority: 'should',
	enforcement: 'should',
	surfaces: ['backend', 'docs'],
	commands: []
};

describe('web convention writers: the body BUG-3446 replays', () => {
	afterEach(() => vi.unstubAllGlobals());

	it('Library page Activate names the entry by key to /library/activate', async () => {
		let url = '';
		let sent: Record<string, unknown> | undefined;
		vi.stubGlobal(
			'fetch',
			vi.fn(async (u: string, init: RequestInit) => {
				url = u;
				sent = JSON.parse(String(init.body));
				return { status: 201, ok: true, json: async () => ({ id: 'i1' }) };
			})
		);
		await api.library.activate('ws', entry);

		expect(url).toContain('/workspaces/ws/library/activate');
		expect(sent).toEqual({ key: entry.key });
	});

	it('Conventions page create sends the same fields', () => {
		const { key: _key, title, content, ...meta } = entry;
		const payload = conventionCreatePayload(title, content, meta);
		expect(JSON.parse(String(payload.fields))).toEqual(expectedFields);
	});
});
