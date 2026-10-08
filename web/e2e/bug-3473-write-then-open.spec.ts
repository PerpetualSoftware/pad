import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';

/**
 * BUG-3473: an item load no longer waits for the workspace index to catch up
 * with every other writer, only for it to be populated. This pins what that
 * must not cost a person: a write this tab just made is visible in the pane
 * they open straight afterwards.
 *
 * The index-dependent case is the strong one. Y's body links [[X]], Y has
 * never been opened (so its live document is seeded from markdown on this
 * open, resolving wiki-links against the index: BUG-1461), and X is created
 * by THIS TAB through the sidebar quick-add moments before Y is opened. A
 * stale index would seed the literal `[[X]]` text instead of a link.
 */

const DESKTOP = { width: 1200, height: 900 };

test.describe('write, then open at once (BUG-3473)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one desktop browser is enough');
	});

	test('an item this tab just created resolves as a link in a pane opened straight after', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const stamp = Date.now();
		const xTitle = `Write then open target ${stamp}`;
		const yTitle = `Write then open linker ${stamp}`;

		// Y exists before X, linking a title nothing has yet.
		const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
			headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
			data: { title: yTitle, content: `See [[${xTitle}]] for details.` },
		});
		expect(resp.ok()).toBe(true);

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`);
		const yRow = page.locator('.item-card', { hasText: yTitle }).first();
		await expect(yRow).toBeVisible();

		// The write, from this tab.
		const add = page.locator('button.nav-quick-add[title="New Doc"]');
		await add.hover({ force: true });
		await add.click({ force: true });
		const dialog = page.locator('.quick-add-modal');
		await expect(dialog).toBeVisible();
		await dialog.locator('textarea.quick-add-input').fill(xTitle);
		const created = page.waitForResponse(
			(r) => r.request().method() === 'POST' && /\/collections\/docs\/items$/.test(r.url()) && r.ok(),
		);
		await dialog.locator('textarea.quick-add-input').press('Enter');
		await created;
		await expect(dialog).toBeHidden();
		// The quick-add opens the new item's page; go straight back to the list.
		await expect(page).toHaveURL(/\/docs\/[^/?]+$/);
		await page.goBack();

		// Open Y at once: no wait for the index, the SSE catch-up, or anything else.
		await yRow.click();
		const pane = page.locator('.item-pane');
		await expect(pane.locator('.title', { hasText: yTitle })).toBeVisible();
		const editor = pane.locator(EDITOR_SELECTOR);
		await expect(editor).toContainText(xTitle, { timeout: SYNC_TIMEOUT });
		await expect(editor.locator('a', { hasText: xTitle })).toBeVisible();
		await expect(editor).not.toContainText('[[');
	});
});
