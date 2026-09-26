import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson, createCollection, deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3052 unit 2: a stored value whose shape does not match its field's
 * declared type is shown in the item page as its raw text with a note, and an
 * edit replaces it explicitly.
 *
 * The component legs mount FieldEditor with the value they choose. This leg is
 * about what the PAGE hands it, which the component legs cannot see: ItemDetail
 * passes an unset field as `''`, and the first version of the shape check read
 * that as "not a number" and marked every empty number field on every item.
 * So the proving pair is one item that must carry the marker and one that must
 * not.
 */

async function createItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
	fields: Record<string, unknown>,
): Promise<{ slug: string }> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authJson(fixture), data: { title, fields: JSON.stringify(fields), content: '' } },
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string };
}

async function setSchema(fixture: SuiteFixture, request: APIRequestContext, slug: string, schema: unknown) {
	const resp = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${slug}`, {
		headers: authJson(fixture),
		data: { schema: JSON.stringify(schema) },
	});
	if (!resp.ok()) throw new Error(`collection patch failed (${resp.status()}): ${await resp.text()}`);
}

async function storedEffort(fixture: SuiteFixture, request: APIRequestContext, slug: string): Promise<unknown> {
	const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authJson(fixture),
	});
	const body = (await resp.json()) as { fields: string | Record<string, unknown> };
	const fields = typeof body.fields === 'string' ? JSON.parse(body.fields) : body.fields;
	return (fields as Record<string, unknown>).effort;
}

test.describe('a field value whose shape does not match its type (BUG-3052 unit 2)', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a data-shape concern');
	});

	test('is marked on the item that holds it, not on an item where the field is unset, and Replace writes a number', async ({
		page,
		fixture,
		request,
	}) => {
		const stamp = Date.now();
		const coll = await createCollection(fixture, request, `Efforts ${stamp}`);
		try {
			await setSchema(fixture, request, coll.slug, { fields: [{ key: 'effort', label: 'Effort', type: 'text' }] });
			const texty = await createItem(fixture, request, coll.slug, `Texty ${stamp}`, { effort: '5' });
			const unset = await createItem(fixture, request, coll.slug, `Unset ${stamp}`, {});
			// The retype rewrites nothing: `effort` is still the STRING "5".
			await setSchema(fixture, request, coll.slug, { fields: [{ key: 'effort', label: 'Effort', type: 'number' }] });

			await browserLogin(page);
			const base = `/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}`;

			// Unset first: an empty number field is NOT a mismatch.
			await page.goto(`${base}/${unset.slug}`);
			await expect(page.locator('.number-input').first()).toBeVisible();
			await expect(page.locator('.field-mismatch')).toHaveCount(0);

			await page.goto(`${base}/${texty.slug}`);
			const marker = page.locator('.field-mismatch');
			await expect(marker).toBeVisible();
			await expect(marker).toContainText('"5"');
			await expect(marker).toContainText("Doesn't match the field type (number)");

			await marker.getByRole('button', { name: 'Replace' }).click();
			const input = page.locator('.number-input').first();
			await expect(input).toHaveValue('');
			await input.fill('3');
			await input.blur();
			await expect.poll(() => storedEffort(fixture, request, texty.slug)).toBe(3);
			await expect(page.locator('.field-mismatch')).toHaveCount(0);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});
});
