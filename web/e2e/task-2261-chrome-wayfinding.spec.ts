import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2261 (audit C99, C101, C102):
 *  - C99: seven workspace pages read "{Workspace} · Pad" in the tab, the same
 *    as the dashboard. Each now names its section.
 *  - C101: Mod+\ toggled the sidebar and the workspace bar independently, so
 *    with one hidden it showed that one and hid the other. It is one state.
 *  - C102: the shortcuts sheet opened only with `?`. The account menu opens
 *    it, and it lists Mod+F, Tab into the pane and the palette's go-to.
 */

const DESKTOP = { width: 1280, height: 900 };

test.describe('TASK-2261: chrome wayfinding', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop chrome and keyboard; one browser is enough');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('each workspace section names itself in the tab', async ({ page, fixture }) => {
		const base = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;
		const cases: Array<[string, string]> = [
			['/starred', 'Starred'],
			['/tags', 'Tags'],
			['/tags/t2261', '#t2261'],
			['/library', 'Library'],
			['/settings', 'Settings'],
			['/roles', 'Roles'],
			['/conventions', 'Conventions'],
			['/playbooks', 'Playbooks'],
		];
		for (const [path, section] of cases) {
			await page.goto(base + path);
			await expect(page, path).toHaveTitle(new RegExp(`^${section.replace(/[#]/g, '\\$&')} · .+ · Pad$`));
		}
	});

	test('Mod+\\ moves both bars together, from either one hidden', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		const sidebar = page.locator('.sidebar-expand-btn');
		const topbar = page.locator('.topbar-expand-btn');
		const mod = process.platform === 'darwin' ? 'Meta' : 'Control';
		// Both open: no expand buttons.
		await expect(sidebar).toHaveCount(0);
		await expect(topbar).toHaveCount(0);

		// Hide only the workspace bar, through its own button.
		await page.getByRole('button', { name: 'Hide workspace bar' }).click();
		await expect(topbar).toHaveCount(1);
		await expect(sidebar).toHaveCount(0);

		// Mod+\ used to close the sidebar and REOPEN the workspace bar here.
		await page.locator('body').click({ position: { x: 600, y: 600 } });
		await page.keyboard.press(`${mod}+Backslash`);
		await expect(sidebar).toHaveCount(1);
		await expect(topbar).toHaveCount(1);

		await page.keyboard.press(`${mod}+Backslash`);
		await expect(sidebar).toHaveCount(0);
		await expect(topbar).toHaveCount(0);
	});

	test('the account menu opens the shortcuts sheet, which lists every binding', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		await page.getByRole('button', { name: 'User menu' }).click();
		await page.getByRole('menuitem', { name: 'Keyboard shortcuts' }).click();
		const sheet = page.getByRole('dialog', { name: 'Keyboard Shortcuts' });
		await expect(sheet).toBeVisible();
		await expect(sheet).toContainText('Filter the list');
		await expect(sheet).toContainText('Move into the open item pane');
		await expect(sheet).toContainText('Go to that item by number or ref');
		await expect(sheet).toContainText('Show or hide the sidebar and workspace bar');
		await page.keyboard.press('Escape');
		await expect(sheet).toHaveCount(0);
		// `?` still opens it.
		await page.locator('body').click({ position: { x: 600, y: 600 } });
		await page.keyboard.press('Shift+Slash');
		await expect(sheet).toBeVisible();
	});
});
