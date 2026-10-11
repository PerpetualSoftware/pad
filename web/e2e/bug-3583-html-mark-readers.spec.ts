import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3583 writes a run markdown delimiters cannot express as inline HTML
 * (`<strong>Hello.</strong>World`). Every reader of the stored body must show
 * it as formatting, or (a plain-text snippet) as plain text, never as literal
 * tags. The body here is the exact form the serializer stores.
 *
 * Readers with nothing to pin, checked by reading the code: no email or
 * notification template carries item content; the workspace and artifact
 * exports hand out the raw body, where inline HTML is valid CommonMark/GFM.
 */
const BODY = (w: string) => `<strong>Hello.</strong>World ${w}`;

async function create(fixture: SuiteFixture, request: APIRequestContext, title: string, body: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture),
		data: { title, content: body },
	});
	if (!resp.ok()) throw new Error(`create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string; id: string };
}

test.describe('BUG-3583: inline HTML marks render in every reader', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a render binding');
	});

	test('share page renders the tags as bold', async ({ browser, fixture, request }) => {
		const w = `zqshare${Date.now().toString(36)}`;
		const item = await create(fixture, request, `B3583 share ${w}`, BODY(w));
		const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}/share-links`, { headers: authJson(fixture), data: {} });
		expect(resp.ok(), await resp.text()).toBeTruthy();
		const { token } = (await resp.json()) as { token: string };
		const anon = await browser.newContext();
		try {
			const p = await anon.newPage();
			await p.goto(`/s/${token}`);
			const body = p.locator('.item-content');
			await expect(body.locator('strong', { hasText: 'Hello.' })).toBeVisible();
			await expect(body).not.toContainText('<strong>');
		} finally {
			await anon.close();
		}
	});

	test('the editor seeded from the body shows bold (as after an op-log prune)', async ({ page, fixture, request }) => {
		const w = `zqseed${Date.now().toString(36)}`;
		const item = await create(fixture, request, `B3583 seed ${w}`, BODY(w));
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${item.ref}`);
		const editor = page.locator(EDITOR_SELECTOR);
		await expect(editor.locator('strong', { hasText: 'Hello.' })).toBeVisible({ timeout: 20_000 });
		await expect(editor).not.toContainText('<strong>');
	});

	test('search palette snippet shows text, not tags', async ({ page, fixture, request }) => {
		const w = `zqpal${Date.now().toString(36)}`;
		const title = `B3583 pal ${Date.now()}`;
		// The word is in the body only: a title match shows the title, not a snippet.
		await create(fixture, request, title, BODY(w));
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`);
		const input = page.getByPlaceholder('Search items, collections, docs...');
		await expect(async () => {
			if (!(await input.isVisible())) await page.keyboard.press('Control+k');
			await expect(input).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		await input.fill(w);
		const row = page.locator('button.result').filter({ hasText: title });
		await expect(row).toBeVisible({ timeout: 15_000 });
		await expect(row.locator('.result-snippet')).toContainText('Hello.World');
		await expect(row).not.toContainText('<strong>');
	});

	test('backlinks snippet shows text, not tags', async ({ page, fixture, request }) => {
		const w = `zqbl${Date.now().toString(36)}`;
		const target = await create(fixture, request, `B3583 target ${w}`, 'Target body.');
		await create(fixture, request, `B3583 source ${w}`, `${BODY(w)} see [[${target.ref}]].`);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${target.ref}`);
		const pane = page.locator('.item-pane');
		await pane.getByRole('tab', { name: 'Relationships' }).click();
		const mentioned = pane.locator('.snippet').filter({ hasText: 'World' });
		await expect(mentioned).toBeVisible({ timeout: 15_000 });
		await expect(mentioned).toContainText('Hello.World');
		// No tag, whole or cut in two by the snippet's slice.
		await expect(mentioned).not.toContainText(/<\/?[a-z]|\b[a-z]*>/i);
	});
});
