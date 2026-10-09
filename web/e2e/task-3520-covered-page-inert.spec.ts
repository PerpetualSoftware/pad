import { test, expect } from './fixtures';
import type { Locator, Page } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-3520: while the palette, a docked sheet or the notification panel is
 * open, the page behind it is `inert`, so a screen reader's browse cursor
 * cannot walk past the overlay into it (Tab was already trapped by TASK-2235).
 * The overlay itself stays live, the bottom nav stays live under a docked
 * overlay, and focus still returns to the trigger on close: the trigger sat
 * in an inert region a moment earlier, and focus() on an inert element does
 * nothing, which is why the overlays restore after a tick.
 */

function docsUrl(fixture: SuiteFixture): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`;
}

/** Whether the element, or any ancestor, is inert. */
async function isInert(loc: Locator): Promise<boolean> {
	return loc.first().evaluate((el) => !!el.closest('[inert]'));
}

async function pageSettled(page: Page) {
	await expect(page.locator('.collection-page').first()).toBeVisible();
}

test.describe('TASK-3520: the page behind an overlay is inert (desktop)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop overlays; one browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('the palette: page and sidebar inert, palette live, focus back on the trigger', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture));
		await pageSettled(page);
		const trigger = page.locator('.sidebar button.search-btn');
		await trigger.focus();
		await page.keyboard.press('Enter');
		const palette = page.locator('.palette');
		await expect(palette).toBeVisible();

		expect(await isInert(page.locator('main.main-content'))).toBe(true);
		expect(await isInert(page.locator('aside.sidebar'))).toBe(true);
		expect(await isInert(palette)).toBe(false);

		await page.keyboard.press('Escape');
		await expect(palette).toHaveCount(0);
		expect(await isInert(page.locator('main.main-content'))).toBe(false);
		await expect(trigger).toBeFocused();
	});

	test('the notification panel: page and sidebar inert, panel live, focus back on the bell', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture));
		await pageSettled(page);
		const bell = page.locator('.sidebar button.bell-btn');
		await bell.focus();
		await page.keyboard.press('Enter');
		const panel = page.getByRole('dialog', { name: 'Notifications' });
		await expect(panel).toBeVisible();

		expect(await isInert(page.locator('main.main-content'))).toBe(true);
		expect(await isInert(page.locator('aside.sidebar'))).toBe(true);
		expect(await isInert(panel)).toBe(false);

		await page.keyboard.press('Escape');
		await expect(panel).toHaveCount(0);
		expect(await isInert(page.locator('aside.sidebar'))).toBe(false);
		await expect(bell).toBeFocused();
	});
});

test.describe('TASK-3520: a docked sheet leaves the nav live (mobile)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the docked sheets exist only on mobile');
		await browserLogin(page);
	});

	test('the Workspace sheet: page inert, sheet and nav live, focus back on the slot', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture));
		await pageSettled(page);
		const slot = page.locator('nav.bottom-nav').getByRole('button', { name: /Workspace/ });
		await slot.focus();
		await page.keyboard.press('Enter');
		const sheet = page.locator('.ds-panel[aria-label="Workspace"]');
		await expect(sheet).toBeVisible();

		expect(await isInert(page.locator('.collection-page'))).toBe(true);
		expect(await isInert(sheet)).toBe(false);
		expect(await isInert(page.locator('nav.bottom-nav'))).toBe(false);

		await page.keyboard.press('Escape');
		await expect(sheet).toHaveCount(0);
		expect(await isInert(page.locator('.collection-page'))).toBe(false);
		await expect(slot).toBeFocused();
	});

	test('the docked palette: page inert, palette and nav live', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture));
		await pageSettled(page);
		await page.locator('nav.bottom-nav').getByRole('button', { name: 'Search' }).click();
		const palette = page.locator('.palette');
		await expect(palette).toBeVisible();

		expect(await isInert(page.locator('.collection-page'))).toBe(true);
		expect(await isInert(palette)).toBe(false);
		expect(await isInert(page.locator('nav.bottom-nav'))).toBe(false);
	});
});
