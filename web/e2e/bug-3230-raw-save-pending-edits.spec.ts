import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Browser, Page, APIRequestContext, Locator } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3230 U0: the item pane's raw-markdown saves carried no version token and
 * no refusal, so a raw save REPLACED another tab's edits that were not stored
 * yet (BUG-3133): through the live document while that tab is open, which is
 * what these legs watch, since the tab's editor loses the text.
 *
 * Page B switches to the raw editor BEFORE tab A types, so B's text is behind
 * A's by construction, and B's save follows A's typing by about the raw
 * debounce (1.2s), well inside A's 5s idle flush. The precondition poll proves
 * A's edit is still unstored when B is about to save; a leg whose precondition
 * fails measures nothing and says so.
 */

async function createDoc(fixture: SuiteFixture, request: APIRequestContext, title: string, body: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture),
		data: { title, content: body, fields: JSON.stringify({}) },
	});
	if (!resp.ok()) throw new Error(`create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string; id: string };
}

async function itemUrl(fixture: SuiteFixture, ref: string) {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${ref}`;
}

/** Tab A: the item open in a synced collab pane. */
async function tabOpen(page: Page, fixture: SuiteFixture, ref: string): Promise<Locator> {
	await browserLogin(page);
	await page.goto(await itemUrl(fixture, ref));
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	return editor;
}

async function typeInTab(page: Page, editor: Locator, marker: string) {
	await editor.click();
	await page.keyboard.press('End');
	await page.keyboard.type(` ${marker}`);
	await expect(editor).toContainText(marker);
}

/** Page B: the same item, switched to the raw markdown editor. */
async function rawPage(browser: Browser, fixture: SuiteFixture, ref: string): Promise<Page> {
	const b = await browser.newPage();
	await browserLogin(b);
	await b.goto(await itemUrl(fixture, ref));
	await expect(b.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await b.getByTitle('Raw markdown editor').click();
	await expect(b.locator('.raw-textarea')).toBeVisible();
	return b;
}

async function expectPending(request: APIRequestContext, fixture: SuiteFixture, id: string) {
	await expect
		.poll(async () => {
			const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers: authJson(fixture) });
			return ((await r.json()) as { content_state?: string }).content_state ?? '';
		}, { timeout: 4_000, message: "PRECONDITION: tab A's edit must still be unstored when B saves" })
		.toBe('applied_pending_flush');
}

test.describe('raw-markdown saves meet edits another tab has not stored (BUG-3230 U0)', () => {
	test.setTimeout(120_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a write-path concern');
	});

	test('a raw save asks, and Keep leaves the tab\'s typed text in place', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const doc = await createDoc(fixture, request, `Raw keep ${stamp}`, 'Original body.');
		const editor = await tabOpen(page, fixture, doc.ref);
		const b = await rawPage(browser, fixture, doc.ref);

		const marker = `tab-${stamp}`;
		await typeInTab(page, editor, marker);
		await expectPending(request, fixture, doc.id);
		await b.locator('.raw-textarea').click();
		await b.keyboard.press('End');
		await b.keyboard.type(' raw-typed');

		const dialog = b.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		await dialog.getByRole('button', { name: "Keep the tab's edits" }).click();
		await expect(dialog).toBeHidden();
		// Nothing replaced A's document, and B's text is still B's, unsaved.
		await expect(editor).toContainText(marker);
		await expect(editor).not.toContainText('raw-typed');
		await expect(b.locator('.raw-textarea')).toHaveValue(/raw-typed/);
		await b.close();
	});

	test('a raw save asks, and Overwrite replaces the tab\'s text with the raw body', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const doc = await createDoc(fixture, request, `Raw overwrite ${stamp}`, 'Original body.');
		const editor = await tabOpen(page, fixture, doc.ref);
		const b = await rawPage(browser, fixture, doc.ref);

		const marker = `tab-${stamp}`;
		await typeInTab(page, editor, marker);
		await expectPending(request, fixture, doc.id);
		await b.locator('.raw-textarea').click();
		await b.keyboard.press('End');
		await b.keyboard.type(' raw-typed');

		const dialog = b.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		await dialog.getByRole('button', { name: 'Overwrite them' }).click();
		await expect(editor).toContainText('raw-typed', { timeout: 10_000 });
		await expect(editor).not.toContainText(marker);
		await b.close();
	});

	test('a raw save sent on unload is refused, kept in the browser, and offered back on the next open', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const doc = await createDoc(fixture, request, `Raw unload ${stamp}`, 'Original body.');
		const editor = await tabOpen(page, fixture, doc.ref);
		const b = await rawPage(browser, fixture, doc.ref);

		const marker = `tab-${stamp}`;
		await typeInTab(page, editor, marker);
		await expectPending(request, fixture, doc.id);
		await b.locator('.raw-textarea').click();
		await b.keyboard.press('End');
		await b.keyboard.type(' unload-typed');
		// Leave before the debounce fires: the unload flush sends it.
		await b.goto('about:blank');

		await expect(editor).toContainText(marker);
		await expect(editor).not.toContainText('unload-typed');

		await b.goto(await itemUrl(fixture, doc.ref));
		const notice = b.getByTestId('refused-raw-draft');
		await expect(notice).toBeVisible({ timeout: SYNC_TIMEOUT });
		await notice.getByRole('button', { name: 'Discard' }).click();
		await expect(notice).toBeHidden();
		await b.reload();
		await expect(b.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await expect(b.getByTestId('refused-raw-draft')).toHaveCount(0);
		await b.close();
	});
});
