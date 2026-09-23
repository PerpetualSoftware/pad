import { describe, it, expect, vi, afterEach } from 'vitest';
import { api } from '$lib/api/client';
import { conventionCreatePayload } from './createPayload';

// BUG-3163: create's `fields` refuses every reserved metadata key, so both web
// convention writers must send the metadata as the typed `convention` member
// and keep it OUT of `fields`. A writer left on the old shape is refused 400
// by the server, which is the regression each test here would catch.

const metadata = {
	category: 'quality',
	trigger: 'on-commit',
	surfaces: ['all'],
	enforcement: 'must' as const,
	commands: ['make test']
};

describe('convention writers send the typed member (BUG-3163)', () => {
	afterEach(() => vi.unstubAllGlobals());

	it('api.library.activate posts `convention` beside `fields`, not inside it', async () => {
		let sent: Record<string, unknown> | undefined;
		vi.stubGlobal(
			'fetch',
			vi.fn(async (_url: string, init: RequestInit) => {
				sent = JSON.parse(String(init.body));
				return { status: 201, ok: true, json: async () => ({ id: 'i1' }) };
			})
		);
		await api.library.activate('ws', { title: 'Run tests', content: 'body', ...metadata });

		expect(sent?.convention).toEqual(metadata);
		const fields = JSON.parse(String(sent?.fields));
		expect(fields).not.toHaveProperty('convention');
		expect(fields.trigger).toBe('on-commit');
		expect(fields.priority).toBe('must');
	});

	it('the Conventions page payload carries `convention` beside `fields`, not inside it', () => {
		const payload = conventionCreatePayload('Run tests', 'body', metadata);

		expect(payload.convention).toEqual(metadata);
		const fields = JSON.parse(String(payload.fields));
		expect(fields).not.toHaveProperty('convention');
		expect(fields).toMatchObject({ status: 'active', trigger: 'on-commit', scope: 'all', priority: 'must' });
	});
});
