import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Browser, Page, APIRequestContext, Locator } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3540: a raw-markdown save was a blind replace. Tab B's raw editor holds
 * text seeded before tab A typed; once A's edit is STORED (its idle flush
 * landed), nothing is pending, so `refuse_pending_edits` let B's save replace
 * it. Reproduced under load in 3 of 40 runs; made deterministic here by waiting
 * until A's text is in the stored body before B types.
 *
 * The raw saves now carry `expected_seq` from the text's seed: a save over a
 * body someone else changed is refused and the user chooses, with no default.
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

async function expectStored(request: APIRequestContext, fixture: SuiteFixture, id: string, marker: string) {
	await expect
		.poll(
			async () => {
				const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers: authJson(fixture) });
				return ((await r.json()) as { content?: string }).content ?? '';
			},
			{ timeout: 20_000, message: "PRECONDITION: tab A's edit must be STORED before B saves" },
		)
		.toContain(marker);
}

async function storedBody(request: APIRequestContext, fixture: SuiteFixture, id: string): Promise<string> {
	const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers: authJson(fixture) });
	return ((await r.json()) as { content?: string }).content ?? '';
}

test.describe('a stale raw save meets a stored edit (BUG-3540)', () => {
	test.setTimeout(120_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a write-path concern');
	});

	test('B is asked; Reload shows the stored text and sends nothing', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const doc = await createDoc(fixture, request, `Stale raw reload ${stamp}`, 'Original body.');
		const editor = await tabOpen(page, fixture, doc.ref);
		const b = await rawPage(browser, fixture, doc.ref);

		const marker = `tab-${stamp}`;
		await typeInTab(page, editor, marker);
		await expectStored(request, fixture, doc.id, marker);

		await b.locator('.raw-textarea').click();
		await b.keyboard.press('End');
		await b.keyboard.type(' raw-typed');

		const dialog = b.getByRole('dialog', { name: 'This item changed since you opened the raw editor' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		// No default: neither choice holds focus when the dialog opens.
		await expect(dialog.getByRole('button', { name: 'Overwrite with my text' })).not.toBeFocused();
		await expect(dialog.getByRole('button', { name: 'Reload the stored text' })).not.toBeFocused();
		// Nothing was replaced while the question is open.
		expect(await storedBody(request, fixture, doc.id)).toContain(marker);

		await dialog.getByRole('button', { name: 'Reload the stored text' }).click();
		await expect(dialog).toBeHidden();
		await expect(b.locator('.raw-textarea')).toHaveValue(new RegExp(marker));
		await expect(b.locator('.raw-textarea')).not.toHaveValue(/raw-typed/);
		await expect(editor).toContainText(marker);
		expect(await storedBody(request, fixture, doc.id)).toContain(marker);
		await b.close();
	});

	test('Overwrite is an explicit choice, and then it replaces', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const doc = await createDoc(fixture, request, `Stale raw overwrite ${stamp}`, 'Original body.');
		const editor = await tabOpen(page, fixture, doc.ref);
		const b = await rawPage(browser, fixture, doc.ref);

		const marker = `tab-${stamp}`;
		await typeInTab(page, editor, marker);
		await expectStored(request, fixture, doc.id, marker);

		await b.locator('.raw-textarea').click();
		await b.keyboard.press('End');
		await b.keyboard.type(' raw-typed');

		const dialog = b.getByRole('dialog', { name: 'This item changed since you opened the raw editor' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		await dialog.getByRole('button', { name: 'Overwrite with my text' }).click();
		await expect(dialog).toBeHidden();
		await expect(editor).toContainText('raw-typed', { timeout: 10_000 });
		await b.close();
	});

	test('a raw save with no change underneath it saves without a question', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const doc = await createDoc(fixture, request, `Raw clean ${stamp}`, 'Original body.');
		await tabOpen(page, fixture, doc.ref);
		const b = await rawPage(browser, fixture, doc.ref);
		await b.locator('.raw-textarea').click();
		await b.keyboard.press('End');
		await b.keyboard.type(' first');
		await expect.poll(() => storedBody(request, fixture, doc.id), { timeout: 20_000 }).toContain('first');
		// A second save in the same raw session: the base moved with the first.
		await b.keyboard.type(' second');
		await expect.poll(() => storedBody(request, fixture, doc.id), { timeout: 20_000 }).toContain('second');
		await expect(b.getByRole('dialog', { name: 'This item changed since you opened the raw editor' })).toHaveCount(0);
		await b.close();
	});
});
