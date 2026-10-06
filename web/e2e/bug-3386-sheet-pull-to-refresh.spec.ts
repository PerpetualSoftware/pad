import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { CdpTouch } from './lib/attachment-viewer';

/**
 * BUG-3386 (Dave's report): with the mobile Workspace sheet open, a downward
 * swipe in the collection list did not close it; at the list's top it
 * pull-to-refreshed the page instead.
 *
 * Two halves, both on the Pixel 7 project with real touch through CDP
 * (Playwright's touchscreen can only tap):
 *   - no page pull-to-refresh while a sheet is open. Headless Chromium draws no
 *     pull-to-refresh, so what this pins is the property that controls it:
 *     the ROOT's computed overscroll-behavior-y is `none` while the sheet is
 *     open (and `auto` again once it closes), and the list itself is `contain`,
 *     so a pull at its top cannot chain to the page;
 *   - a downward drag that starts in the list, with the list at its top, closes
 *     the sheet, as the grip's drag always did (the existing close gesture).
 */

async function openWorkspaceSheet(page: import('@playwright/test').Page) {
	await page.locator('.bottom-nav .bn-item', { hasText: 'Workspace' }).click();
	await expect(page.locator('.ds-panel[aria-label="Workspace"]')).toBeVisible();
}

const rootOverscroll = (page: import('@playwright/test').Page) =>
	page.evaluate(() => [getComputedStyle(document.documentElement).overscrollBehaviorY, getComputedStyle(document.body).overscrollBehaviorY]);

test('BUG-3386: no page pull-to-refresh while the Workspace sheet is open, and a pull from the list top closes it', async ({
	page,
	fixture,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'needs a touch device');
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	await expect(page.locator('.bottom-nav')).toBeVisible();

	// Closed: the page keeps its normal overscroll (pull-to-refresh allowed).
	expect(await rootOverscroll(page)).toEqual(['auto', 'auto']);

	await openWorkspaceSheet(page);
	expect(await rootOverscroll(page), 'the page can still pull-to-refresh under the open sheet').toEqual(['none', 'none']);
	const list = page.locator('.ds-panel[aria-label="Workspace"] .ds-content');
	expect(await list.evaluate((el) => getComputedStyle(el).overscrollBehaviorY)).toBe('contain');
	expect(await list.evaluate((el) => el.scrollTop)).toBe(0);

	// A real downward drag that starts inside the list, at its top.
	const box = (await list.boundingBox())!;
	const x = box.x + box.width / 2;
	const y0 = box.y + 40;
	const touch = await CdpTouch.attach(page);
	await touch.down(0, x, y0);
	for (let dy = 10; dy <= 220; dy += 15) await touch.move(0, x, y0 + dy);
	await touch.lift(0);
	await expect(page.locator('.ds-panel[aria-label="Workspace"]')).toHaveCount(0);

	// Closed again: the page's overscroll is back.
	await expect.poll(() => rootOverscroll(page)).toEqual(['auto', 'auto']);
});
