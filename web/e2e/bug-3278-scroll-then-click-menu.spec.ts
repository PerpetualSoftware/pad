import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * BUG-3278: a card's ⋮ menu below a board lane's fold never opened. Clicking
 * it makes Playwright scroll the lane first, the same scroll a tap landing as
 * a momentum scroll ends leaves behind. That scroll's event is dispatched at
 * the next rendering step, after the click has opened the portal Menu, and
 * Menu's scroll-dismiss closed it in the same frame although the trigger had
 * not moved since it opened.
 *
 * Each leg scrolls the lane back to the top and clicks the trigger of a card
 * below the fold, so every click is preceded by a lane scroll. The collection
 * is the spec's own, so no other spec's cards change the lane.
 */

const CARDS = 14;

test('BUG-3278: a card menu below the lane fold opens and stays open', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'a lane-scroll race; one desktop browser is enough');
	await page.setViewportSize({ width: 1280, height: 800 });
	await browserLogin(page);

	const stamp = Date.now();
	const ws = `/api/v1/workspaces/${fixture.workspaceSlug}`;
	const coll = await request.post(`${ws}/collections`, {
		headers: authJson(fixture),
		data: {
			name: `Fold ${stamp}`,
			schema: JSON.stringify({
				fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'], default: 'open' }],
			}),
		},
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = (await coll.json()) as { slug: string };
	for (let i = 1; i <= CARDS; i++) {
		const r = await request.post(`${ws}/collections/${collSlug}/items`, {
			headers: authJson(fixture),
			data: { title: `Fold card ${String(i).padStart(2, '0')}`, fields: JSON.stringify({ status: 'open' }) },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
	}

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}?view=board`);
	const lane = page.locator('.column-cards').first();
	await expect(lane.locator('.item-card')).toHaveCount(CARDS, { timeout: 15_000 });

	// Precondition: the lane overflows, or no click below needs a scroll.
	const overflow = await lane.evaluate((el) => el.scrollHeight - el.clientHeight);
	expect(overflow, 'precondition: the lane scrolls').toBeGreaterThan(200);

	const cards = lane.locator('.item-card');
	let checked = 0;
	for (let i = CARDS - 1; i >= CARDS - 6; i--) {
		await lane.evaluate((el) => (el.scrollTop = 0));
		const trigger = cards.nth(i).locator('.iam-trigger');
		// Precondition per leg: the trigger starts below the lane's visible
		// area, so the click scrolls it into view.
		const below = await trigger.evaluate((t) => {
			const lane = t.closest('.column-cards')!;
			return t.getBoundingClientRect().top > lane.getBoundingClientRect().bottom;
		});
		if (!below) continue;
		checked++;
		await trigger.click();
		await expect(trigger, `card ${i + 1}: the menu opened`).toHaveAttribute('aria-expanded', 'true', { timeout: 2_000 });
		await expect(page.getByRole('menu')).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(trigger).toHaveAttribute('aria-expanded', 'false');
	}
	expect(checked, 'precondition: at least three legs clicked below the fold').toBeGreaterThanOrEqual(3);
});
