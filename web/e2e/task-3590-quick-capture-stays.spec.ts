import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-3590: quick capture keeps your place. Adding an item used to open it,
 * so jotting down an interruption was a context switch. "Add" now saves,
 * stays on the current screen ready for the next capture, and confirms with a
 * toast whose Open action goes to the item. "Add and open" (Ctrl/Cmd+Enter,
 * or the mobile sheet's secondary button) keeps the old behaviour.
 */
test.describe('TASK-3590: quick capture keeps your place', () => {
	test('desktop sidebar: Enter adds twice and stays; the toast opens the item; Ctrl/Cmd+Enter opens', async ({ page, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the sidebar quick-add');
		const stamp = `qc${Date.now().toString(36)}`;
		await browserLogin(page);
		const listUrl = `/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`;
		await page.goto(listUrl);

		const add = page.locator('button.nav-quick-add[title="New Task"]');
		await add.hover({ force: true });
		await add.click({ force: true });
		const dialog = page.locator('.quick-add-modal');
		const input = dialog.locator('textarea.quick-add-input');
		await expect(dialog).toBeVisible();

		for (const n of [1, 2]) {
			await input.fill(`${stamp} stay ${n}`);
			const created = page.waitForResponse(
				(r) => r.request().method() === 'POST' && /\/collections\/tasks\/items$/.test(new URL(r.url()).pathname) && r.ok(),
			);
			await input.press('Enter');
			await created;
			await expect(input).toHaveValue('');
			await expect(input).toBeFocused();
		}
		await expect(dialog).toBeVisible();
		await expect(page).toHaveURL(new RegExp(`${listUrl}(\\?.*)?$`));

		const toast = page.locator('.toast', { hasText: /Added TASK-\d+/ }).last();
		await expect(toast).toBeVisible();
		await input.press('Escape');
		await expect(dialog).toBeHidden();
		await toast.getByRole('button', { name: 'Open' }).click();
		await expect(page).toHaveURL(/\/tasks\/TASK-\d+/);
		// The URL carries ?new=1, so the item opens with its title in the editor.
		// Under kit 2 the follow-up goto's focus reset closed the editor at once;
		// kit 3's reset:false keeps it open (TASK-3423, day-87 ruling, site 3).
		await expect(page.locator('.title-input')).toHaveValue(`${stamp} stay 2`);

		// Add and open.
		await page.goto(listUrl);
		await add.hover({ force: true });
		await add.click({ force: true });
		await input.fill(`${stamp} open`);
		await input.press('ControlOrMeta+Enter');
		await expect(dialog).toBeHidden();
		await expect(page).toHaveURL(/\/tasks\/TASK-\d+/);
		await expect(page.locator('.title-input')).toHaveValue(`${stamp} open`);
	});

	test('mobile sheet: Add stays open for the next capture; Add and open navigates', async ({ page, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the mobile capture sheet');
		const stamp = `qm${Date.now().toString(36)}`;
		await browserLogin(page);
		const listUrl = `/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`;
		await page.goto(listUrl);

		await page.getByRole('button', { name: 'Quick capture' }).click();
		const sheet = page.getByRole('dialog', { name: 'Quick capture' });
		const title = sheet.getByRole('textbox', { name: 'New item title' });
		await expect(title).toBeVisible();
		await title.fill(`${stamp} stay`);
		const created = page.waitForResponse(
			(r) => r.request().method() === 'POST' && /\/collections\/tasks\/items$/.test(new URL(r.url()).pathname) && r.ok(),
		);
		await sheet.getByRole('button', { name: 'Add', exact: true }).click();
		await created;
		await expect(title).toHaveValue('');
		await expect(sheet).toBeVisible();
		await expect(page).toHaveURL(new RegExp(`${listUrl}(\\?.*)?$`));
		await expect(page.locator('.toast', { hasText: /Added TASK-\d+/ }).last()).toBeVisible();

		await title.fill(`${stamp} open`);
		await sheet.getByRole('button', { name: 'Add and open' }).click();
		await expect(page).toHaveURL(/\/tasks\/TASK-\d+/);
		await expect(page.locator('.title-input')).toHaveValue(`${stamp} open`);
	});
});
