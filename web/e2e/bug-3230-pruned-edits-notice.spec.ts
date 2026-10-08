import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Browser, APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3230 U2: a web write the user sent with "Overwrite them" can DELETE another
 * tab's unsaved edits from the op-log (the direct path, when no tab is open).
 * The server counts them in `warnings.pruned_pending_edits`; the web discarded
 * that count, so the save looked no different from one that deleted nothing.
 *
 * The state is built for real: tab A types with its collab-snapshot flush
 * blocked, the edit is confirmed pending, and A is closed. So there is no
 * writer, and the next overwrite takes the direct path and prunes.
 */

async function createItem(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string, fields: Record<string, unknown>) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data: { title, content: 'Original body.', fields: JSON.stringify(fields) },
	});
	if (!resp.ok()) throw new Error(`create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string; id: string };
}

/** Leave an unsaved edit on the item from a tab that is then gone. */
async function strandEdit(browser: Browser, fixture: SuiteFixture, request: APIRequestContext, ref: string, id: string) {
	const a = await browser.newPage();
	await browserLogin(a);
	await a.route(/source=collab-snapshot/, (r) => r.abort());
	// The docs route opens any collection's item in the pane (as BUG-3050's spec does).
	await a.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${ref}`);
	const editor = a.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(a.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await editor.click();
	await a.keyboard.press('End');
	await a.keyboard.type(' stranded');
	await expect
		.poll(async () => {
			const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers: authJson(fixture) });
			return ((await r.json()) as { content_state?: string }).content_state ?? '';
		}, { timeout: 10_000, message: 'PRECONDITION: the stranded edit must be pending' })
		.toBe('applied_pending_flush');
	await a.close({ runBeforeUnload: false });
}

test.describe('an overwrite that deletes another tab\'s edits says so (BUG-3230 U2)', () => {
	test.setTimeout(120_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a write-path concern');
	});

	test('conventions inline edit', async ({ page, browser, fixture, request }) => {
		const title = `Pruned CONV ${Date.now()}`;
		const conv = await createItem(fixture, request, 'conventions', title, { status: 'active', trigger: 'on-implement' });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		const row = page.locator('.convention-row', { has: page.locator('.row-title', { hasText: title }) });
		await row.locator('.row-main').click();
		await row.getByRole('button', { name: 'Edit' }).click();

		await strandEdit(browser, fixture, request, conv.ref, conv.id);

		await row.locator('.edit-textarea').fill('Replacement convention body.');
		await row.getByRole('button', { name: 'Save' }).click();
		const dialog = page.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		await dialog.getByRole('button', { name: 'Overwrite them' }).click();
		await expect(page.getByText(/Convention updated\. \d+ unsaved changes? from another tab (was|were) discarded\./)).toBeVisible({ timeout: 10_000 });
	});

	test('playbook editor', async ({ page, browser, fixture, request }) => {
		const title = `Pruned PB ${Date.now()}`;
		const pb = await createItem(fixture, request, 'playbooks', title, { status: 'draft' });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${pb.slug}`);
		await expect(page.locator('.title-input')).toHaveValue(title);

		await strandEdit(browser, fixture, request, pb.ref, pb.id);

		await page.locator('textarea').first().fill('Replacement playbook body.');
		await page.getByRole('button', { name: /^Save$/ }).click();
		const dialog = page.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		await dialog.getByRole('button', { name: 'Overwrite them' }).click();
		await expect(page.getByText(/Playbook saved\. \d+ unsaved changes? from another tab (was|were) discarded\./)).toBeVisible({ timeout: 10_000 });
	});

	test('item pane raw-markdown save', async ({ page, browser, fixture, request }) => {
		const doc = await createItem(fixture, request, 'docs', `Pruned RAW ${Date.now()}`, {});
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
		await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await page.getByTitle('Raw markdown editor').click();
		await expect(page.locator('.raw-textarea')).toBeVisible();

		await strandEdit(browser, fixture, request, doc.ref, doc.id);

		await page.locator('.raw-textarea').click();
		await page.keyboard.press('End');
		await page.keyboard.type(' raw-typed');
		const dialog = page.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
		await expect(dialog).toBeVisible({ timeout: 10_000 });
		await dialog.getByRole('button', { name: 'Overwrite them' }).click();
		await expect(page.getByText(/^\d+ unsaved changes? from another tab (was|were) discarded\.$/)).toBeVisible({ timeout: 10_000 });
	});

	test('version restore confirmed over the pending edits', async ({ page, browser, fixture, request }) => {
		const doc = await createItem(fixture, request, 'docs', `Pruned RESTORE ${Date.now()}`, {});
		// A second body mints a version to restore to.
		const v = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.slug}`, {
			headers: authJson(fixture),
			data: { content: 'Second body.' },
		});
		expect(v.ok(), await v.text()).toBeTruthy();

		await strandEdit(browser, fixture, request, doc.ref, doc.id);

		// This page's own pre-restore drain (BUG-2271) would store the stranded
		// edit, since its editor replays it, and leave nothing to discard. Block it,
		// so the restore meets the pending edits and asks.
		await page.route(/source=collab-snapshot/, (r) => r.abort());
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${doc.slug}`);
		await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await page.getByRole('tab', { name: /History/ }).click();
		const card = page.locator('#item-timeline .version-card').first();
		await card.locator('.show-changes').click();
		await card.getByRole('button', { name: /^Restore to / }).click();
		await card.getByRole('button', { name: 'Confirm Restore' }).click();
		await card.getByRole('button', { name: 'Discard edits and restore' }).click();
		await expect(page.getByText(/^\d+ unsaved changes? from another tab (was|were) discarded\.$/)).toBeVisible({ timeout: 10_000 });
	});
});
