import { expect, type Page } from '@playwright/test';
import { test, type SuiteFixture } from './fixtures';

/**
 * BUG-2610 — in split view the pane's quick-actions (⚡) and ⋯ menus were
 * ANCHORED panels inside `.item-pane`, an `overflow-y:auto` scroll
 * container (which computes `overflow-x:auto` too). A right-aligned panel
 * opening from the pane's action bar extends LEFT past the pane's left
 * edge, so the container clipped it mid-text (Dave's screenshots on the
 * item: "age actions" for "Manage actions"). Both menus now use the Menu
 * component's portal mode, which exists precisely to escape overflow
 * containment.
 *
 * The oracle is a PAINT-level probe, because clipping doesn't shrink
 * getBoundingClientRect: `document.elementFromPoint` just inside the
 * panel's LEFT edge must resolve to the panel itself. On the anchored
 * control build the panel's left region lies outside the pane's clip, so
 * the probe hits whatever is underneath (the board) — which is exactly
 * what a user's click there did.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}` };
}

/** Probe: does the point just inside the panel's left edge actually hit
 *  the panel (i.e. is it painted / clickable there)? */
async function leftEdgeHitsMenu(page: Page): Promise<boolean> {
	const panel = page.getByRole('menu');
	await expect(panel).toBeVisible();
	const box = await panel.boundingBox();
	if (!box) return false;
	return page.evaluate(
		([x, y]) => {
			const el = document.elementFromPoint(x, y);
			return !!el && !!el.closest('[role="menu"]');
		},
		[box.x + 6, box.y + box.height / 2] as [number, number],
	);
}

test('BUG-2610: pane quick-actions and ⋯ menus are not clipped by the pane container in split view', async ({
	page,
	request,
	fixture,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'split view is a desktop layout; the mobile path renders a BottomSheet',
	);

	const uniq = `${test.info().workerIndex}-${Date.now().toString(36)}`;
	const itemResp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{ headers: authHeaders(fixture), data: { title: `b2610 probe ${uniq}` } },
	);
	expect(itemResp.ok(), await itemResp.text()).toBeTruthy();
	const item = (await itemResp.json()) as { slug: string };

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?item=${item.slug}`);
	const pane = page.locator('.item-pane');
	await expect(pane.locator('.title', { hasText: /b2610 probe/ })).toBeVisible();

	// GEOMETRY PRECONDITION (keeps the probe non-vacuous): the trigger
	// sits close enough to the pane's left edge that a right-aligned
	// panel MUST extend past it — otherwise both builds pass trivially.
	const paneBox = await pane.boundingBox();
	const qaTrigger = pane.locator('button.trigger-btn[title="Quick actions"]');
	const qaBox = await qaTrigger.boundingBox();
	expect(paneBox && qaBox && qaBox.x + qaBox.width - 260 < paneBox.x).toBeTruthy();

	// ⚡ quick-actions menu.
	await qaTrigger.click();
	expect(await leftEdgeHitsMenu(page), 'quick-actions panel clipped at its left edge').toBe(true);
	await page.keyboard.press('Escape');
	await expect(page.getByRole('menu')).not.toBeVisible();

	// ⋯ overflow menu.
	await pane.locator('button.pane-more-btn').click();
	expect(await leftEdgeHitsMenu(page), '⋯ panel clipped at its left edge').toBe(true);
	await page.keyboard.press('Escape');
});
