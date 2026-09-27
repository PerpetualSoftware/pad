import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Browser, APIRequestContext, Locator } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3230 U3: a body saved outside the item pane while a tab has the item
 * open goes to that tab's LIVE document, not to the stored row
 * (`warnings.content_outcome: applied_pending_flush`, BUG-2995). The row, and so
 * this page's next read, keeps the old body until that tab saves. The web
 * discarded the warning and reported a plain success.
 *
 * Tab A has the item open and synced, types NOTHING, and the leg waits until
 * the item reads clean (opening seeds A's document, and the seed is pending
 * until A stamps it), so the save is accepted rather than refused. It then lands in A's document,
 * which is what A's editor showing the new body proves.
 */

async function createItem(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string, fields: Record<string, unknown>) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data: { title, content: 'Original body.', fields: JSON.stringify(fields) },
	});
	if (!resp.ok()) throw new Error(`create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string; id: string };
}

/** Tab A: the item open in a synced collab pane (the docs route opens any collection's item). */
async function openTab(browser: Browser, fixture: SuiteFixture, request: APIRequestContext, ref: string, id: string): Promise<Locator> {
	const a = await browser.newPage();
	await browserLogin(a);
	await a.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${ref}`);
	const editor = a.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(a.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	// PRECONDITION: nothing pending. Opening seeds the tab's document, and that
	// seed is itself unflushed until the tab stamps or flushes it (BUG-3124);
	// saving before then is refused as pending edits, which is U0's case, not this one.
	await expect
		.poll(async () => {
			const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers: authJson(fixture) });
			return ((await r.json()) as { content_state?: string }).content_state ?? '';
		}, { timeout: 20_000, message: 'PRECONDITION: the open tab has nothing pending' })
		.toBe('');
	return editor;
}

test.describe('a body saved into an open tab\'s live document says so (BUG-3230 U3)', () => {
	test.setTimeout(120_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a write-path concern');
	});

	test('conventions inline edit', async ({ page, browser, fixture, request }) => {
		const title = `Outcome CONV ${Date.now()}`;
		const conv = await createItem(fixture, request, 'conventions', title, { status: 'active', trigger: 'on-implement' });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		const row = page.locator('.convention-row', { has: page.locator('.row-title', { hasText: title }) });
		await row.locator('.row-main').click();
		await row.getByRole('button', { name: 'Edit' }).click();

		const editor = await openTab(browser, fixture, request, conv.ref, conv.id);

		await row.locator('.edit-textarea').fill('Body typed on the conventions page.');
		await row.getByRole('button', { name: 'Save' }).click();
		await expect(page.getByText(/^Convention updated\. The item is open in another tab/)).toBeVisible({ timeout: 10_000 });
		await expect(editor).toContainText('Body typed on the conventions page.');
	});

	test('playbook editor', async ({ page, browser, fixture, request }) => {
		const title = `Outcome PB ${Date.now()}`;
		const pb = await createItem(fixture, request, 'playbooks', title, { status: 'draft' });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${pb.slug}`);
		await expect(page.locator('.title-input')).toHaveValue(title);

		const editor = await openTab(browser, fixture, request, pb.ref, pb.id);

		await page.locator('textarea').first().fill('Body typed in the playbook editor.');
		await page.getByRole('button', { name: /^Save/ }).click();
		await expect(page.getByText(/^Playbook saved\. The item is open in another tab/)).toBeVisible({ timeout: 10_000 });
		await expect(editor).toContainText('Body typed in the playbook editor.');
	});
});
