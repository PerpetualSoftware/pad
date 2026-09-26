import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Browser, Page, APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3050 U1: a browser editor must not send a stale body back over edits an
 * open tab has not stored yet.
 *
 * The playbook editor and the conventions inline edit loaded `item.content` and
 * sent it back on EVERY save with no version token. A tab typing into the same
 * item holds content that is not in `item.content` yet, and a tokenless content
 * write REPLACES it (BUG-3133): through the live document while the tab is
 * open, which is what these legs watch, since the tab's editor loses the text.
 *
 * Tab A types first, so the body page B loaded is stale by construction. No
 * assumption is made about when A's edits flush: B's save is correct only if A's
 * text survives it either way.
 */

async function create(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string, body: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data: { title, content: body, fields: JSON.stringify(coll === 'playbooks' ? { status: 'draft' } : {}) },
	});
	if (!resp.ok()) throw new Error(`create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string; id: string };
}

/** Tab A: the item open in a collab pane, with `marker` typed and shown. */
async function tabTyping(page: Page, fixture: SuiteFixture, ref: string, marker: string) {
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${ref}`);
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await editor.click();
	await page.keyboard.press('End');
	await page.keyboard.type(` ${marker}`);
	await expect(editor).toContainText(marker);
	return editor;
}

async function secondPage(browser: Browser): Promise<Page> {
	const p = await browser.newPage();
	await browserLogin(p);
	return p;
}

test.describe('stale body writes (BUG-3050 U1)', () => {
	test.setTimeout(120_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a write-path concern');
	});

	test('playbook editor: a title-only save leaves a tab\'s typed text in place', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const pb = await create(fixture, request, 'playbooks', `Stale PB ${stamp}`, 'Original playbook body.');
		const b = await secondPage(browser);
		await b.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${pb.slug}`);
		await expect(b.locator('.title-input')).toHaveValue(`Stale PB ${stamp}`);

		const marker = `tab-${stamp}`;
		const editor = await tabTyping(page, fixture, pb.ref, marker);

		await b.locator('.title-input').fill(`Stale PB ${stamp} renamed`);
		await b.getByRole('button', { name: /^Save/ }).click();
		await expect(b).toHaveURL(/\/playbooks$/);
		// The save landed (the title), and the tab's text is still there.
		await expect(page.locator('body')).toContainText(`Stale PB ${stamp} renamed`, { timeout: 10_000 });
		await expect(editor).toContainText(marker);
		await b.close();
	});

	test('playbook editor: a BODY save meeting the tab\'s edits asks, and Keep leaves them in place', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const pb = await create(fixture, request, 'playbooks', `Stale PB2 ${stamp}`, 'Original playbook body.');
		const b = await secondPage(browser);
		await b.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${pb.slug}`);
		await expect(b.locator('.title-input')).toHaveValue(`Stale PB2 ${stamp}`);

		const marker = `tab-${stamp}`;
		const editor = await tabTyping(page, fixture, pb.ref, marker);

		await b.locator('textarea').first().fill('A body typed in the playbook editor.');
		await b.getByRole('button', { name: /^Save/ }).click();
		// Refused one way or another: the pending-edits choice (tab edits not
		// stored yet), or a plain conflict (they flushed first, so the token no
		// longer matches). Never a silent replace.
		const dialog = b.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		const conflict = b.getByText(/conflict|changed since/i).first();
		await expect(dialog.or(conflict)).toBeVisible({ timeout: 10_000 });
		if (await dialog.isVisible()) await dialog.getByRole('button', { name: "Keep the tab's edits" }).click();
		await expect(editor).toContainText(marker);
		await expect(editor).not.toContainText('A body typed in the playbook editor.');
		await b.close();
	});

	test('conventions: an inline edit meeting the tab\'s edits asks, and Keep leaves them in place', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const title = `Stale CONV ${stamp}`;
		const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/conventions/items`, {
			headers: authJson(fixture),
			data: { title, content: 'Original convention body.', fields: JSON.stringify({ status: 'active', trigger: 'on-implement' }) },
		});
		if (!resp.ok()) throw new Error(`convention create failed (${resp.status()}): ${await resp.text()}`);
		const conv = (await resp.json()) as { ref: string };
		const b = await secondPage(browser);
		await b.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		const row = b.locator('.convention-row', { has: b.locator('.row-title', { hasText: title }) });
		await row.locator('.row-main').click();
		await row.getByRole('button', { name: 'Edit' }).click();

		const marker = `tab-${stamp}`;
		const editor = await tabTyping(page, fixture, conv.ref, marker);

		await row.locator('.edit-textarea').fill('A body typed on the conventions page.');
		await row.getByRole('button', { name: 'Save' }).click();
		const dialog = b.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		const conflict = b.getByText(/conflict|changed since/i).first();
		await expect(dialog.or(conflict)).toBeVisible({ timeout: 10_000 });
		if (await dialog.isVisible()) await dialog.getByRole('button', { name: "Keep the tab's edits" }).click();
		await expect(editor).toContainText(marker);
		await expect(editor).not.toContainText('A body typed on the conventions page.');
		await b.close();
	});

	test('playbook duplicate: a source with a tab\'s unstored edits asks first, and Cancel creates nothing', async ({ page, browser, fixture, request }) => {
		const stamp = Date.now();
		const title = `Stale DUP ${stamp}`;
		const pb = await create(fixture, request, 'playbooks', title, 'Original playbook body.');
		const marker = `tab-${stamp}`;
		await tabTyping(page, fixture, pb.ref, marker);
		// PRECONDITION: the source reads as pending when the list loads.
		await expect
			.poll(async () => {
				const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${pb.id}`, { headers: authJson(fixture) });
				return ((await r.json()) as { content_state?: string }).content_state ?? '';
			}, { timeout: 10_000 })
			.toBe('applied_pending_flush');

		const b = await secondPage(browser);
		await b.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks`);
		const card = b.locator('.card', { has: b.locator('.card-title', { hasText: title }) });
		await card.locator('.card-header').click();
		await card.getByRole('button', { name: 'Duplicate' }).click();
		const dialog = b.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		await expect(dialog).toBeVisible();
		await dialog.getByRole('button', { name: 'Cancel' }).click();
		await expect(dialog).toBeHidden();
		const list = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/playbooks/items`, { headers: authJson(fixture) });
		const titles = ((await list.json()) as { title: string }[]).map((i) => i.title);
		expect(titles.filter((t) => t.startsWith(title))).toEqual([title]);
		await b.close();
	});
});
