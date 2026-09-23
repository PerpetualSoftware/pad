import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * BUG-2182 — the split pane's Back button (shown after following a link from
 * item A to item B inside the pane) returned to A at the TOP, not at the scroll
 * position A was read at.
 */
async function setup(page: import('@playwright/test').Page, fixture: import('./fixtures').SuiteFixture, request: import('@playwright/test').APIRequestContext) {
	await page.setViewportSize({ width: 1400, height: 800 });
	await browserLogin(page);
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const stamp = Date.now();
	const res = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers,
		data: { name: `B2182 ${stamp}`, prefix: `BH${String(stamp).slice(-4)}` },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	const titleB = `B2182 target ${stamp}`;
	const b = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
		headers, data: { title: titleB, content: Array.from({ length: 40 }, (_, i) => `Target paragraph ${i}.`).join('\n\n') },
	});
	expect(b.ok(), await b.text()).toBeTruthy();
	const paras = Array.from({ length: 60 }, (_, i) => `Paragraph ${i} of a long item, long enough to scroll the pane.`).join('\n\n');
	const a = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
		headers, data: { title: `B2182 source ${stamp}`, content: `${paras}\n\nSee [[${titleB}]].` },
	});
	expect(a.ok(), await a.text()).toBeTruthy();
	const itemA = await a.json();

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?item=${itemA.slug}`);
	// Scoped to the pane: an unscoped match finds B's CARD on the board behind it.
	const pane = page.locator('.item-pane');
	const link = pane.locator('a', { hasText: titleB }).last();
	await expect(link).toBeAttached({ timeout: 15_000 });
	await link.scrollIntoViewIfNeeded();
	await page.waitForTimeout(300);
	const scrollTop = () => pane.evaluate((el) => el.scrollTop);
	const before = await scrollTop();
	expect(before, 'precondition: the pane scrolled').toBeGreaterThan(300);

	// A click on an editor link opens its popover; the popover's href drills.
	await link.click();
	await page.locator('.link-href').first().click();
	await expect(page.locator('.pane-back-btn')).toBeVisible({ timeout: 10_000 });
	await expect(pane.getByText('Target paragraph 0.')).toBeVisible();
	await page.waitForTimeout(300);
	return { coll, itemA, titleB, pane, before, scrollTop };
}

test('BUG-2182: pane Back returns to the previous item at its scroll position; the new item starts at the top', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ctx = await setup(page, fixture, request);
	try {
		// A cold entry (B, just drilled to, saved nothing) opens at the top.
		expect(await ctx.scrollTop(), 'the drilled-to item did not open at the top').toBeLessThan(40);

		await page.locator('.pane-back-btn').click();
		await expect(ctx.pane.locator('a', { hasText: ctx.titleB }).last()).toBeAttached({ timeout: 10_000 });
		await expect.poll(ctx.scrollTop, { timeout: 3000, message: 'pane Back did not restore the scroll position' })
			.toBeGreaterThan(ctx.before - 40);
		expect(await ctx.scrollTop()).toBeLessThan(ctx.before + 40);
	} finally {
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});

test('BUG-2182: a reader gesture during the restore wait wins', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ctx = await setup(page, fixture, request);
	try {
		// Hold A's item read on the way back, so the restore is still WAITING
		// for content when the reader acts.
		let release!: () => void;
		const gate = new Promise<void>((r) => (release = r));
		await page.route(`**/items/${ctx.itemA.slug}**`, async (route) => {
			await gate;
			await route.continue();
		});
		await page.locator('.pane-back-btn').click();
		await page.waitForTimeout(200);
		const box = (await ctx.pane.boundingBox())!;
		await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
		await page.mouse.wheel(0, 100); // the reader's gesture, during the wait
		release();
		await expect(ctx.pane.locator('a', { hasText: ctx.titleB }).last()).toBeAttached({ timeout: 10_000 });
		await page.waitForTimeout(1500); // past the restore cap
		expect(await ctx.scrollTop(), 'the restore overrode the reader').toBeLessThan(ctx.before / 2);
	} finally {
		await page.unrouteAll({ behavior: 'ignoreErrors' });
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});
