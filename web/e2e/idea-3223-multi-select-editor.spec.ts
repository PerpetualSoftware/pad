import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson, createCollection, deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * IDEA-3223: the item page edits a multi_select value.
 *
 * It used to fall to a text input whose write was a STRING, which the server
 * refuses for a multi_select, so the field looked editable and every edit
 * failed. The component legs pin the editor; this leg is the page: that
 * ItemDetail renders this editor for the field, and that what it sends is
 * stored as an array.
 */

async function storedLabels(fixture: SuiteFixture, request: APIRequestContext, slug: string): Promise<unknown> {
	const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authJson(fixture),
	});
	const body = (await resp.json()) as { fields: string | Record<string, unknown> };
	const fields = typeof body.fields === 'string' ? JSON.parse(body.fields) : body.fields;
	return (fields as Record<string, unknown>).labels;
}

test.describe('multi_select editor on the item page (IDEA-3223)', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a field-editor concern');
	});

	test('toggling options stores the array', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const coll = await createCollection(fixture, request, `Labelled ${stamp}`);
		try {
			const patch = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}`, {
				headers: authJson(fixture),
				data: {
					schema: JSON.stringify({
						fields: [{ key: 'labels', label: 'Labels', type: 'multi_select', options: ['alpha', 'beta', 'gamma'] }],
					}),
				},
			});
			if (!patch.ok()) throw new Error(`collection patch failed (${patch.status()})`);
			const create = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
				headers: authJson(fixture),
				data: { title: `Tagged ${stamp}`, fields: JSON.stringify({ labels: ['alpha'] }), content: '' },
			});
			if (!create.ok()) throw new Error(`item create failed (${create.status()})`);
			const { slug } = (await create.json()) as { slug: string };

			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}/${slug}`);
			const trigger = page.locator('.select-trigger', { hasText: 'Alpha' });
			await expect(trigger).toBeVisible();
			await trigger.click();
			const list = page.locator('[role="listbox"][aria-multiselectable="true"]');
			await list.getByRole('option', { name: /Beta/ }).click();
			await expect.poll(() => storedLabels(fixture, request, slug)).toEqual(['alpha', 'beta']);
			await list.getByRole('option', { name: /Alpha/ }).click();
			await expect.poll(() => storedLabels(fixture, request, slug)).toEqual(['beta']);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});
});
