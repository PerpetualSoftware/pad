import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';

/**
 * TASK-2201 (audit C89 + C90). When the connection comes back, the collection
 * page's "Couldn't load this collection" card (or its saved-copy banner)
 * retries by itself, as the item pane always did; it used to wait for a
 * manual Retry. And the dashboard and activity pages show the live-stream
 * status, so an outage there is no longer invisible.
 */
test.describe('TASK-2201: one recovery signal', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('a collection that failed to load heals when the connection returns, without Retry', async ({ page, context, fixture }) => {
		const url = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`;
		const fail = (route: import('@playwright/test').Route) => route.abort('internetdisconnected');
		await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs`, fail);
		await page.goto(url);
		const errorCard = page.getByRole('heading', { name: "Couldn't load this collection" });
		const savedCopy = page.getByText('Showing a saved copy');
		await expect(errorCard.or(savedCopy)).toBeVisible();

		// The network returns: the route stops failing and the browser fires
		// `online`. Nothing is clicked.
		await page.unroute(`**/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs`, fail);
		await context.setOffline(true);
		await context.setOffline(false);

		await expect(errorCard).toHaveCount(0);
		await expect(savedCopy).toHaveCount(0);
		await expect(page.locator('.page-header h1')).toContainText('Docs');
	});

	test('the dashboard and activity pages show the live-stream status', async ({ page, fixture, request }) => {
		// An item, so the dashboard renders its board rather than onboarding.
		await seedDoc(fixture, request, 'Status indicator');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
		await expect(page.locator('.dash-header [role="status"]').first()).toBeVisible();
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/activity`);
		await expect(page.locator('.header-actions [role="status"]').first()).toBeVisible();
	});
});
