// BUG-3697: on force_refresh the provider is destroyed, the tab's text is
// captured for the recovery notice, and the editor is rebuilt only after a
// refetch of the item. The editor stayed editable through that refetch, so a
// keystroke in the window reached no Y.Doc, missed the recovery copy, and
// vanished on the rebuild. The editor is now read-only for the whole recovery.
//
// The window is held open deterministically: the refetch (a GET of this item
// by id) is parked until the test releases it. The force_refresh itself is
// BUG-3526's recipe: the tab goes offline holding a cursor, a direct write
// prunes the op-log, and the reconnect is refreshed.
import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';

test('the editor accepts no typing while a force_refresh recovery is in flight', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(120_000);
	const { id, slug } = await seedDoc(fixture, request, 'BUG-3697');
	const ctx = await browser.newContext();
	const page = await ctx.newPage();

	let hold = false;
	let held = 0;
	let release!: () => void;
	const released = new Promise<void>((r) => { release = r; });
	await page.route(new RegExp(`/api/v1/workspaces/[^/]+/items/${id}(\\?|$)`), async (route) => {
		if (!hold || route.request().method() !== 'GET') return route.continue();
		held++;
		await released;
		await route.continue();
	});

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.type('Stored first. ');
	await page.waitForTimeout(3_000);

	await ctx.setOffline(true);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeHidden({ timeout: SYNC_TIMEOUT });
	const res = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { content: 'Server body written while the tab was away.' },
	});
	expect(res.ok(), `the direct write failed: ${res.status()}`).toBeTruthy();

	hold = true;
	await ctx.setOffline(false);
	await expect.poll(() => held, { timeout: SYNC_TIMEOUT, message: 'premise: the force_refresh refetch started and is parked' }).toBeGreaterThan(0);

	// The recovery window: the editor must refuse input, and typing into it
	// must change nothing. That includes the page: a frozen editor is not
	// contenteditable, so its keys used to reach the page shortcuts, and the
	// `c` in this text opened quick-add, which took the rest (BUG-3786).
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toHaveAttribute('contenteditable', 'false');
	await editor.click({ force: true });
	await expect(editor, 'premise: the frozen editor holds focus').toBeFocused();
	await page.keyboard.type('Typed during recovery.');
	await expect(editor).not.toContainText('Typed during recovery.');
	await expect(page.locator('.quick-add-input'), 'a shortcut fired from the frozen editor').toHaveCount(0);
	await expect(editor).toBeFocused();

	hold = false;
	release();
	await expect(editor).toContainText('Server body written while the tab was away.', { timeout: SYNC_TIMEOUT });
	// Control: the rebuilt editor is editable again.
	await expect(editor).toHaveAttribute('contenteditable', 'true', { timeout: SYNC_TIMEOUT });
	await ctx.close();
});
