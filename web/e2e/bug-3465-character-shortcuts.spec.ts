import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-3465: quick-add moved from Mod+N, which browsers keep for a new window
 * and never deliver to a page, to `c`. WCAG 2.1.4 then requires a way to turn
 * single-character shortcuts off; the switch lives on the account settings
 * page (and in the shortcuts modal), per device.
 *
 * Playwright dispatches keys into the page past the browser's reserved-key
 * check, so it cannot show that Mod+N is swallowed; that evidence is on the
 * bug. What this proves in a real engine is the new binding, the switch, and
 * that the switch survives a reload.
 */

test.describe('BUG-3465: `c` opens quick-add; the single-key switch turns it off', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'keyboard binding; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('`c` opens quick-add; with the switch off it does not, after a reload too', async ({ page, fixture }) => {
		const tasks = `/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`;
		const dialog = page.locator('.quick-add-modal');

		await page.goto(tasks);
		// A press before the workspace's membership resolves is ignored by design
		// (BUG-3267), so retry the press rather than racing it.
		await expect(async () => {
			if (!(await dialog.isVisible())) await page.keyboard.press('c');
			await expect(dialog).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		await page.keyboard.press('Escape');
		await expect(dialog).toBeHidden();

		await page.goto('/console/settings');
		const toggle = page.getByRole('checkbox', { name: 'Single-key shortcuts' });
		await expect(toggle).toBeChecked();
		await toggle.uncheck();

		await page.goto(tasks);
		// The palette proves keys reach the shell here; then `c` must do nothing.
		const palette = page.getByPlaceholder('Search items, collections, docs...');
		await expect(async () => {
			if (!(await palette.isVisible())) await page.keyboard.press('Control+k');
			await expect(palette).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		await page.keyboard.press('Escape');
		await expect(palette).toBeHidden();
		// From the shell, not a text field, or `c` would be inert for the wrong reason.
		await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
		expect(await page.evaluate(() => document.activeElement === document.body)).toBe(true);
		await page.keyboard.press('c');
		await page.waitForTimeout(300);
		await expect(dialog).toBeHidden();

		// Back on, the same press opens it again.
		await page.goto('/console/settings');
		await page.getByRole('checkbox', { name: 'Single-key shortcuts' }).check();
		await page.goto(tasks);
		await expect(async () => {
			if (!(await dialog.isVisible())) await page.keyboard.press('c');
			await expect(dialog).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
	});
});
