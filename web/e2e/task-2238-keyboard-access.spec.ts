import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2238 (audit C61 + C108), driven by the keyboard.
 *  - C61: the admin user table's rows were mouse-only, and the row was the
 *    only way to a user's detail. The name is now a button.
 *  - C108: workspace settings declared tabs without the tabs pattern; it is
 *    complete now (panel, aria-controls, roving tabindex, arrow keys). The
 *    desktop You group uses links with aria-current.
 */
test.describe('TASK-2238: keyboard access', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'keyboard paths; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('an admin opens a user from the keyboard', async ({ page, fixture }) => {
		// The admin API wants the admin's own session, as task-2979's admin spec sends it.
		await page.context().setExtraHTTPHeaders({ Authorization: `Bearer ${fixture.adminSessionToken}` });
		await page.goto('/settings/admin');
		const name = page.locator('.user-row .user-name-btn').first();
		await expect(name).toBeVisible();
		await name.focus();
		await page.keyboard.press('Enter');
		await expect(page.getByRole('dialog')).toBeVisible();
	});

	test('workspace settings tabs follow the tabs pattern', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/settings`);
		const tablist = page.getByRole('tablist', { name: 'Workspace settings' });
		const tabs = tablist.getByRole('tab');
		await expect(tabs.first()).toHaveAttribute('aria-selected', 'true');
		await expect(tabs.first()).toHaveAttribute('aria-controls', 'settings-tabpanel');
		await expect(page.locator('#settings-tabpanel[role="tabpanel"]')).toBeVisible();
		// Roving tabindex: only the selected tab is in the tab order.
		await expect(tabs.nth(1)).toHaveAttribute('tabindex', '-1');

		await tabs.first().focus();
		await page.keyboard.press('ArrowRight');
		await expect(tabs.nth(1)).toBeFocused();
		await expect(tabs.nth(1)).toHaveAttribute('aria-selected', 'true');
		await expect(page.locator('#settings-tabpanel')).toHaveAttribute('aria-labelledby', (await tabs.nth(1).getAttribute('id'))!);
		await page.keyboard.press('End');
		await expect(tabs.last()).toBeFocused();
		await page.keyboard.press('Home');
		await expect(tabs.first()).toBeFocused();
		await page.keyboard.press('ArrowLeft');
		await expect(tabs.last()).toBeFocused();
	});

	test('the sidebar You group is links, keyboard accessible, and marks the current page', async ({ page }) => {
		await page.goto('/');
		const nav = page.getByRole('navigation', { name: 'You', exact: true });
		await expect(nav.locator('[role="menuitem"]')).toHaveCount(0);
		await expect(nav.getByRole('link')).toHaveCount(4);
		for (const [name, path] of [['Home', '/'], ['Needs you', '/needs'], ['Agenda', '/agenda']] as const) {
			const link = nav.getByRole('link', { name: new RegExp(name) });
			await link.focus();
			await page.keyboard.press('Enter');
			await expect(page).toHaveURL(url => url.pathname === path);
			await expect(nav.locator('a[aria-current="page"]')).toHaveCount(1);
			await expect(link).toHaveAttribute('aria-current', 'page');
		}
		await expect(nav.getByRole('link', { name: /^Chat/ })).toHaveAttribute('href', '/messages');
		await page.goto('/settings/deleted-workspaces');
		await expect(nav.locator('a[aria-current="page"]')).toHaveCount(0);
		const settings = page.getByRole('navigation', { name: 'Settings', exact: true });
		await expect(settings.locator('a[aria-current="page"]')).toHaveCount(1);
		await expect(settings.locator('a[aria-current="page"]')).toHaveText('Deleted workspaces');
	});
});
