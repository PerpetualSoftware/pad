import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2237 (audit C29) in a real browser, where the geometry is real: a card
 * is a div whose title link's ::after covers it, and its controls sit above
 * that overlay. jsdom has no layout, so these are the legs it cannot prove.
 *  - A click on the card's body (not its title text) still opens the item.
 *  - A click on a control (star, tag) does not.
 *  - Tab lands on the card link, and the CARD shows the focus ring.
 *  - The sidebar's quick-add is a sibling of its link, and still works.
 */

const DESKTOP = { width: 1280, height: 900 };

async function createTask(fixture: SuiteFixture, request: import('@playwright/test').APIRequestContext, title: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title, fields: JSON.stringify({ status: 'open' }) }
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
}

test.describe('TASK-2237: a card is a stretched link, not a link full of buttons', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'layout and pointer geometry; one desktop browser is enough');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('the card body opens the item; its controls do not', async ({ page, fixture, request }) => {
		const title = `T2237 card ${Date.now()}`;
		await createTask(fixture, request, title);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=list`);
		const card = page.locator('.item-card', { hasText: title }).first();
		await expect(card).toBeVisible();
		const itemParam = () => new URL(page.url()).searchParams.get('item');

		// A control first: it does not open the item.
		await card.locator('.star-btn').click();
		expect(itemParam()).toBeNull();

		// The meta row is not inside the title link; the overlay makes it a
		// target anyway. Click the age label, well away from the title text.
		const box = (await card.locator('.meta-age').boundingBox())!;
		await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
		await expect.poll(itemParam).not.toBeNull();
	});

	test('Tab reaches the card link, and the card carries the ring', async ({ page, fixture, request }) => {
		const title = `T2237 focus ${Date.now()}`;
		await createTask(fixture, request, title);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=list`);
		const card = page.locator('.item-card', { hasText: title }).first();
		const link = card.locator('.card-link');
		await expect(link).toHaveAccessibleName(title);
		await link.focus();
		await page.keyboard.press('Shift+Tab');
		await page.keyboard.press('Tab');
		await expect(link).toBeFocused();
		const outline = await card.evaluate((el) => getComputedStyle(el).outlineStyle);
		expect(outline).toBe('solid');
	});

	test('the sidebar quick-add is a sibling of its link and opens the quick-add', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		const add = page.locator('button.nav-quick-add[title="New Task"]');
		await expect(add).toHaveCount(1);
		expect(await add.evaluate((el) => !!el.closest('a'))).toBe(false);
		await add.focus();
		await expect.poll(() => add.evaluate((el) => getComputedStyle(el).opacity)).toBe('1');
		await page.keyboard.press('Enter');
		await expect(page.locator('.quick-add-modal')).toBeVisible();
	});
});
