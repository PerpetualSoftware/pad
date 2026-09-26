import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { Locator, Page } from '@playwright/test';

/**
 * BUG-3229: a text selection that starts inside a modal and is released on its
 * backdrop closed the modal. A click is dispatched to the nearest common
 * ancestor of the press and the release, and every one of these surfaces WRAPS
 * its content, so that ancestor was the backdrop itself.
 *
 * Each leg drags with a real mouse from text inside the surface to a point on
 * the backdrop, and asserts the surface is still open. Its control is a plain
 * click on the same backdrop point, which must still close it, so a surface
 * that simply stopped closing would fail the control.
 */

const DESKTOP = { width: 1280, height: 800 };

async function dragOut(page: Page, from: Locator) {
	const box = await from.boundingBox();
	if (!box) throw new Error('drag source has no box');
	await page.mouse.move(box.x + 4, box.y + box.height / 2);
	await page.mouse.down();
	// Through the content and out to the backdrop's corner, in steps, as a hand does.
	await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 5 });
	await page.mouse.move(8, DESKTOP.height - 8, { steps: 10 });
	await page.mouse.up();
}

async function clickBackdrop(page: Page) {
	await page.mouse.click(8, DESKTOP.height - 8);
}

test.describe('BUG-3229: a selection dragged out of a modal does not close it', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'a mouse-drag concern; one desktop browser is enough');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('sidebar quick-add (New item): drag out of the typed title keeps it open; a backdrop click closes it', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		const add = page.locator('button.nav-quick-add[title="New Task"]');
		await add.hover({ force: true });
		await add.click({ force: true });
		const dialog = page.locator('.quick-add-modal');
		await expect(dialog).toBeVisible();
		const input = dialog.locator('textarea.quick-add-input');
		await input.fill('A title to select from the start outward');

		await dragOut(page, input);
		await expect(dialog).toBeVisible();
		await expect(input).toHaveValue('A title to select from the start outward');

		await clickBackdrop(page);
		await expect(dialog).toBeHidden();
	});

	test('command palette: drag out of the search text keeps it open; a backdrop click closes it', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		const input = page.getByPlaceholder('Search items, collections, docs...');
		await expect(async () => {
			if (!(await input.isVisible())) await page.keyboard.press('Control+k');
			await expect(input).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		await input.fill('selectable search text');

		await dragOut(page, input);
		await expect(input).toBeVisible();

		await clickBackdrop(page);
		await expect(input).toBeHidden();
	});

	test('shared Modal (keyboard shortcuts): drag out of its heading keeps it open; a backdrop click closes it', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		const dialog = page.locator('dialog.modal[open]');
		await expect(async () => {
			if (!(await dialog.isVisible())) await page.keyboard.press('?');
			await expect(dialog).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		const heading = dialog.getByRole('heading').first();

		await dragOut(page, heading);
		await expect(dialog).toBeVisible();

		await clickBackdrop(page);
		await expect(dialog).toBeHidden();
	});
});
