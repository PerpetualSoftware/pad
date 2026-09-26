import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Locator, Page } from '@playwright/test';

/**
 * BUG-3231: menus that closed on a window CLICK outside them closed when a drag
 * started inside and was released outside, because that click lands on a
 * common ancestor outside the menu. They now dismiss through `clickOutside`,
 * which decides on the press.
 *
 * Each leg drags with a real mouse from inside the menu to a point outside it
 * and asserts the menu is still open, then presses that point and asserts it
 * closes, so a menu that simply stopped closing fails the second half.
 */

const DESKTOP = { width: 1280, height: 800 };
const OUTSIDE = { x: 640, y: DESKTOP.height - 20 };

async function dragOut(page: Page, from: Locator) {
	const box = await from.boundingBox();
	if (!box) throw new Error('drag source has no box');
	await page.mouse.move(box.x + 4, box.y + box.height / 2);
	await page.mouse.down();
	await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 4 });
	await page.mouse.move(OUTSIDE.x, OUTSIDE.y, { steps: 10 });
	await page.mouse.up();
}

test.describe('BUG-3231: a drag started inside a menu and released outside does not close it', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'a mouse-drag concern; one desktop browser is enough');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('field select dropdown (FieldEditor)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
			headers: authJson(fixture),
			data: { title: `Menu drag ${stamp}`, content: '', fields: JSON.stringify({ status: 'open' }) },
		});
		expect(resp.ok(), await resp.text()).toBeTruthy();
		const { slug } = (await resp.json()) as { slug: string };
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks/${slug}`);

		const trigger = page.locator('.select-trigger', { hasText: 'Open' }).first();
		await expect(trigger).toBeVisible({ timeout: 15_000 });
		await trigger.click();
		const list = page.locator('.select-dropdown[role="listbox"]');
		await expect(list).toBeVisible();

		await dragOut(page, list.getByRole('option').first());
		await expect(list).toBeVisible();

		await page.mouse.click(OUTSIDE.x, OUTSIDE.y);
		await expect(list).toBeHidden();
	});

	test('board lane actions menu, tag input (BoardView)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
			headers: authJson(fixture),
			data: { title: `Lane drag ${stamp}`, content: '', fields: JSON.stringify({ status: 'open' }) },
		});
		expect(resp.ok(), await resp.text()).toBeTruthy();
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=board`);

		const kebab = page.locator('.lane-menu-btn').first();
		await expect(kebab).toBeVisible({ timeout: 15_000 });
		await kebab.click();
		const first = page.getByRole('menuitem').first();
		await expect(first).toBeVisible();

		// Press a menu row and release outside: the click lands on a common
		// ancestor outside the menu, which the old window closer took for an
		// outside click. (A drag out of the tag INPUT does not reproduce in
		// Chromium, whose text-selection drag keeps the release on the input.)
		await dragOut(page, first);
		await expect(first).toBeVisible();

		await page.mouse.click(OUTSIDE.x, OUTSIDE.y);
		await expect(first).toBeHidden();
	});
});
