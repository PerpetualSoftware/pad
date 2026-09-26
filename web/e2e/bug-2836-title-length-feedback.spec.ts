import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';

/**
 * BUG-2836: the item title shows its length while you type, and is marked
 * invalid past the server's limit, instead of only learning on a refused save.
 * The counting rule is unit-pinned in titleLimit.test.ts; this leg is the
 * pane actually rendering it.
 */
test('the pane title shows a live count near the limit and marks it too long past it', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	const { slug } = await seedDoc(fixture, request, `Title length ${Date.now()}`);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await page.locator('button.title').first().click();
	const input = page.locator('textarea.title-input');
	await expect(input).toBeVisible();

	await input.fill('x'.repeat(210));
	const count = page.locator('.title-count');
	await expect(count).toHaveText('210 / 255');
	await expect(input).not.toHaveAttribute('aria-invalid', 'true');

	await input.fill('x'.repeat(260));
	await expect(count).toHaveText('260 / 255 (too long)');
	await expect(input).toHaveAttribute('aria-invalid', 'true');

	// Back under the threshold, it goes quiet again.
	await input.fill('short');
	await expect(count).toHaveCount(0);
	await page.keyboard.press('Escape');
});
