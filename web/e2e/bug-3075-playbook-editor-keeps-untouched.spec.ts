import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson, createWorkspace, deleteWorkspace } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3075: saving the playbook editor writes only what the user changed.
 *
 * The editor sent status, trigger, scope, arguments and invocation_slug on
 * every save, and its load turns a stored value the controls cannot hold into
 * a default. A playbook whose `status` is stored as the number 5 (the field was
 * a number when it was written; a retype rewrites nothing) loaded as `draft`,
 * and a save that only edited the TITLE wrote `draft` over it.
 *
 * Its own workspace, because the leg retypes the playbooks schema, and the
 * shared suite workspace's playbooks are used by other specs running in
 * parallel.
 */

type Schema = { fields: Array<Record<string, unknown>> };

async function playbooksSchema(fixture: SuiteFixture, request: APIRequestContext, ws: string): Promise<Schema> {
	const resp = await request.get(`/api/v1/workspaces/${ws}/collections`, { headers: authJson(fixture) });
	const list = (await resp.json()) as Array<{ slug: string; schema: string | Schema }>;
	const pb = list.find((c) => c.slug === 'playbooks');
	if (!pb) throw new Error('no playbooks collection');
	return typeof pb.schema === 'string' ? (JSON.parse(pb.schema) as Schema) : pb.schema;
}

async function setSchema(fixture: SuiteFixture, request: APIRequestContext, ws: string, schema: Schema) {
	const resp = await request.patch(`/api/v1/workspaces/${ws}/collections/playbooks`, {
		headers: authJson(fixture),
		data: { schema: JSON.stringify(schema) },
	});
	if (!resp.ok()) throw new Error(`schema patch failed (${resp.status()}): ${await resp.text()}`);
}

async function stored(fixture: SuiteFixture, request: APIRequestContext, ws: string, idOrSlug: string) {
	const resp = await request.get(`/api/v1/workspaces/${ws}/items/${idOrSlug}`, { headers: authJson(fixture) });
	if (!resp.ok()) throw new Error(`item read failed (${resp.status()}): ${await resp.text()}`);
	const body = (await resp.json()) as { title: string; fields: string | Record<string, unknown> };
	const fields = typeof body.fields === 'string' ? JSON.parse(body.fields) : body.fields;
	return { title: body.title, status: (fields as Record<string, unknown>).status };
}

test.describe('the playbook editor keeps a field the user did not touch (BUG-3075)', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a write-shape concern');
	});

	test('a title-only save leaves a stored status the form cannot show as it was', async ({ page, fixture, request }) => {
		const { slug: ws } = await createWorkspace(fixture, request, 'bug3075');
		try {
			const original = await playbooksSchema(fixture, request, ws);
			const asNumber: Schema = {
				...original,
				fields: original.fields.map((f) =>
					f.key === 'status' ? { key: 'status', label: 'Status', type: 'number' } : f,
				),
			};
			await setSchema(fixture, request, ws, asNumber);
			const create = await request.post(`/api/v1/workspaces/${ws}/collections/playbooks/items`, {
				headers: authJson(fixture),
				data: { title: 'Numbered', fields: JSON.stringify({ status: 5 }), content: '' },
			});
			if (!create.ok()) throw new Error(`playbook create failed (${create.status()}): ${await create.text()}`);
			// Read back by ID: a title edit changes the slug.
			const { slug, id } = (await create.json()) as { slug: string; id: string };
			// Back to the select: the stored 5 stays a number.
			await setSchema(fixture, request, ws, original);
			expect((await stored(fixture, request, ws, id)).status).toBe(5);

			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}/playbooks/${slug}`);
			await expect(page.locator('.stored-mismatch')).toContainText('Status is stored as 5');

			await page.locator('.title-input').fill('Numbered, renamed');
			await page.getByRole('button', { name: /^Save/ }).click();
			await expect.poll(async () => (await stored(fixture, request, ws, id)).title).toBe('Numbered, renamed');
			// The untouched status is still the stored number, not the form's default.
			expect((await stored(fixture, request, ws, id)).status).toBe(5);
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});
});
