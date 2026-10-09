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
 *  - A drag started on the card's title (now the link's own text, and its
 *    overlay everywhere else) still moves the card, and no NATIVE link drag
 *    starts: svelte-dnd-action cancels dragstart on the drag item, and the
 *    link's dragstart bubbles to it (lead review, #1968).
 */

/** Count dragstart events that were NOT cancelled, i.e. a native link drag. */
async function armNativeDragProbe(page: import('@playwright/test').Page) {
	await page.evaluate(() => {
		const w = window as unknown as { __nativeDrags: number };
		w.__nativeDrags = 0;
		// CAPTURE, so a dragstart whose propagation something stops is still
		// seen; whether it was cancelled is read after dispatch has finished.
		window.addEventListener(
			'dragstart',
			(e) => {
				setTimeout(() => {
					if (!e.defaultPrevented) w.__nativeDrags++;
				});
			},
			true,
		);
	});
}
const nativeDrags = (page: import('@playwright/test').Page) =>
	page.evaluate(() => (window as unknown as { __nativeDrags: number }).__nativeDrags);

const DESKTOP = { width: 1280, height: 900 };

async function createTask(fixture: SuiteFixture, request: import('@playwright/test').APIRequestContext, title: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title, fields: JSON.stringify({ status: 'open' }) }
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return (await resp.json()) as { slug: string };
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
		// Reached by the keyboard, from its own link: the next tab stop.
		// (A programmatic focus() does not match :focus-visible in Firefox.)
		await page.locator('.nav-row a.nav-item', { has: page.locator('.nav-label', { hasText: /^Tasks$/ }) }).focus();
		await page.keyboard.press('Tab');
		await expect(add).toBeFocused();
		await expect.poll(() => add.evaluate((el) => getComputedStyle(el).opacity)).toBe('1');
		await page.keyboard.press('Enter');
		await expect(page.locator('.quick-add-modal')).toBeVisible();
	});

	test('a board drag started on the title moves the card, with no native link drag', async ({ page, fixture, request }) => {
		const title = `T2237 drag ${Date.now()}`;
		const { slug } = await createTask(fixture, request, title);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=board`);
		const card = page.locator('.item-card', { hasText: title }).first();
		await expect(card).toBeVisible();
		await armNativeDragProbe(page);
		const path = new URL(page.url()).pathname;

		const target = page.getByRole('group', { name: 'Done column', exact: true });
		const link = card.locator('.card-link');
		let entered = false;
		for (let attempt = 0; attempt < 3 && !entered; attempt++) {
			// Start ON the title text: the link itself, not just its overlay.
			// In view first: a new card joins the END of an un-dragged lane
			// (BUG-3527), and in the suite's shared workspace the Open lane
			// holds enough cards to put it below the fold, where the gesture
			// starts off-screen and no drag begins (CI, #1974's E2E run).
			await link.scrollIntoViewIfNeeded();
			const from = (await link.boundingBox())!;
			const zone = (await target.locator('.column-cards').boundingBox())!;
			await page.mouse.move(from.x + Math.min(20, from.width / 2), from.y + from.height / 2);
			await page.mouse.down();
			await page.mouse.move(from.x + 30, from.y + from.height / 2 + 10, { steps: 4 });
			await page.mouse.move(zone.x + zone.width / 2, zone.y + 40, { steps: 20 });
			entered = await expect(target.locator('.item-card', { hasText: title }))
				.not.toHaveCount(0, { timeout: 5_000 })
				.then(() => true)
				.catch(() => false);
			await page.mouse.up();
		}
		expect(entered, 'the card never entered the Done lane').toBe(true);
		expect(await nativeDrags(page), 'a native link drag started').toBe(0);
		// The gesture was a drag, not a click: nothing opened, nothing navigated.
		expect(new URL(page.url()).pathname).toBe(path);
		expect(new URL(page.url()).searchParams.get('item')).toBeNull();
		await expect
			.poll(async () => {
				const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
					headers: { Authorization: `Bearer ${fixture.apiToken}` }
				});
				return r.ok() ? (JSON.parse((await r.json()).fields) as { status?: string }).status : r.status();
			})
			.toBe('done');
	});

	test('a pointer drag on a sidebar link starts no native link drag', async ({ page, fixture }) => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
		await armNativeDragProbe(page);
		const link = page.locator('.nav-row a.nav-item', { has: page.locator('.nav-label', { hasText: /^Tasks$/ }) });
		const box = (await link.boundingBox())!;
		// A short move inside the row: enough to start any drag, too short to reorder.
		await page.mouse.move(box.x + 30, box.y + box.height / 2);
		await page.mouse.down();
		await page.mouse.move(box.x + 45, box.y + box.height / 2 + 3, { steps: 6 });
		await page.mouse.up();
		expect(await nativeDrags(page), 'a native link drag started').toBe(0);
	});
});
