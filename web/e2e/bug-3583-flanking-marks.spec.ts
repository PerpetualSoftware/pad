import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * BUG-3583, end to end from the real editor: bold text ending in punctuation,
 * followed directly by a letter, must be STORED as markdown that reads back as
 * bold. The serializer wrote `**Hello.**World`, which CommonMark cannot close,
 * so every reader of the stored body (share pages, search, the API and agents,
 * and the editor itself once the op-log is pruned and it re-seeds from the
 * body) saw literal asterisks. Measured on main: an open tab reloading does
 * NOT show it, since it re-seeds from the op-log, so the stored body is the
 * assertion here.
 */
test('bold ending in punctuation before a letter is stored as markdown that reads back as bold', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a serializer path');
	test.setTimeout(90_000);
	const { slug } = await seedDoc(fixture, request, 'B3583 flank');
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	const editor = page.locator(EDITOR_SELECTOR);
	await editor.click();
	await page.keyboard.press('Control+b');
	await page.keyboard.type('Hello.');
	await page.keyboard.press('Control+b');
	await page.keyboard.type('World');
	await expect(editor.locator('strong', { hasText: 'Hello.' }), 'premise: typed as bold').toBeVisible();

	// The idle flush stores the body.
	const stored = async () =>
		((await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, { headers: authJson(fixture) })).json()) as { content: string }).content;
	await expect.poll(stored, { timeout: 20_000 }).toContain('World');

	expect(await stored()).toBe('<strong>Hello.</strong>World');
});
