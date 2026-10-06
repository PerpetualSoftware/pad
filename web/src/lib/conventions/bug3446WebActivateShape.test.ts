import { describe, it, expect, vi, afterEach } from 'vitest';
import { api } from '$lib/api/client';
import { conventionCreatePayload } from './createPayload';

// BUG-3446: a blank workspace's Conventions collection seeds trigger [always]
// and scope [all], and the server now adds a library word a write needs. The
// Go test that proves the web Activate path
// (internal/server/bug3446_blank_library_options_test.go, webConventionBody)
// replays the body these two web writers send. This pins that body, so the
// Go mirror cannot drift from what the browser actually posts without one of
// the two going red. The entry is the library's one multi-surface convention:
// its scope is surfaces[0], a word blank does not list either.

const entry = {
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

	it('Library page Activate posts trigger and scope = surfaces[0] to conventions', async () => {
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

		expect(url).toContain('/workspaces/ws/collections/conventions/items');
		expect(JSON.parse(String(sent?.fields))).toEqual(expectedFields);
	});

	it('Conventions page create sends the same fields', () => {
		const { title, content, ...meta } = entry;
		const payload = conventionCreatePayload(title, content, meta);
		expect(JSON.parse(String(payload.fields))).toEqual(expectedFields);
	});
});
