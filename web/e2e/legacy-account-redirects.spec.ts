import { test, expect } from './fixtures';

test('legacy account links preserve fragments and reach the new admin instance page', async ({ page, fixture }) => {
	await page.context().setExtraHTTPHeaders({ Authorization: `Bearer ${fixture.adminSessionToken}` });
	await page.goto('/console/settings#calendar-feeds');
	await expect(page).toHaveURL((url) => url.pathname === '/settings/account' && url.hash === '#calendar-feeds');
	await expect(page.locator('#calendar-feeds')).toBeVisible();

	await page.goto('/console/admin/settings');
	await expect(page).toHaveURL((url) => url.pathname === '/settings/admin/instance');
	await expect(page.locator('.admin-segment[aria-current="page"]')).toHaveText('Instance');
	await expect(page.getByTestId('decision-provider-section')).toBeVisible();
});
