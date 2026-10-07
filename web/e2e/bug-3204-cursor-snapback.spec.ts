import type { Route } from '@playwright/test';
import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-3204 — with the split pane open, j moves the list cursor at once and the
 * pane follows ~140ms later (the pane-follow debounce). The route also snaps
 * the cursor to the pane's item whenever the list changes ("keep the row
 * highlight on the OPEN pane's item"). A list change inside that debounce
 * window therefore snapped the cursor BACK to the pane's old item, then the
 * pane followed and it snapped forward again: a visible flicker, and the
 * cause of attachment-viewer-owners' owner-2 flake (CI trace: alpha, alpha,
 * BRAVO, alpha within 600ms of one j).
 *
 * The list change here is a rename of the open item, held until j is pressed
 * so it lands inside the debounce. In CI it was another worker writing to the
 * shared workspace; for a user, anyone editing the list.
 * Every change of the focused row is recorded in the page, so a snap-back
 * shows even when it lasts a frame.
 */
test('BUG-3204: a list change while the pane follows j does not snap the cursor back', async ({
	page,
	fixture,
	request
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	await page.setViewportSize({ width: 1400, height: 900 });
	await browserLogin(page);
	const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const ws = fixture.workspaceSlug;
	const stamp = `${Date.now()}-${testInfo.repeatEachIndex}`;
	const res = await request.post(`/api/v1/workspaces/${ws}/collections`, {
		headers: h,
		data: { name: `B3204 ${stamp}`, prefix: `BS${String(Date.now()).slice(-5)}` }
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const coll = await res.json();
	try {
		const made: { id: string; slug: string; title: string }[] = [];
		for (const t of ['first', 'second', 'third']) {
			const r = await request.post(`/api/v1/workspaces/${ws}/collections/${coll.slug}/items`, {
				headers: h,
				data: { title: `B3204 ${t} ${stamp}` }
			});
			expect(r.ok(), await r.text()).toBeTruthy();
			made.push(await r.json());
		}
		await page.goto(`/${fixture.adminUsername}/${ws}/${coll.slug}?view=list`);
		const cards = page.locator('.item-card');
		await expect(cards).toHaveCount(3);
		// Open the pane on the top row; the snap-back puts the cursor there.
		const top = await cards.first().getAttribute('data-item-key');
		await cards.first().click();
		await expect(page.locator('.item-pane')).toBeVisible();
		await expect(page.locator('.item-card.focused')).toHaveAttribute('data-item-key', top!);
		// Hand the keyboard back to the list, as owner 2 does after a click.
		await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
		await page.waitForTimeout(400);

		// Record every focused-row change from here on.
		await page.evaluate(() => {
			const w = window as unknown as { __b3204: string[] };
			w.__b3204 = [];
			const read = () => document.querySelector('.item-card.focused')?.getAttribute('data-item-key') ?? '';
			let last = read();
			new MutationObserver(() => {
				const now = read();
				if (now !== last) {
					w.__b3204.push(now);
					last = now;
				}
			}).observe(document.body, { subtree: true, attributes: true, attributeFilter: ['class'], childList: true });
		});

		// j, and list changes landing all through the pane-follow debounce:
		// a rename every ~25ms for 250ms, so at least one arrives over SSE
		// before the pane follows whatever the round-trip time is.
		// Change the list: rename the open item over the API. It reaches the
		// list through SSE -> a coalesced reconcile -> `/items-changes`, which
		// rarely lands inside the 140ms debounce on its own (an unheld run
		// reproduced 1 in 20). So HOLD that fetch, press j, then release it:
		// the list changes inside the debounce every time. Rows are tracked by
		// ref, which a rename keeps.
		const topSlug = await cards.first().getAttribute('data-item-slug');
		const open = made.find((m) => m.slug === topSlug)!;
		const held: Route[] = [];
		let holding = true;
		await page.route('**/items-changes**', (route) => {
			if (holding) held.push(route);
			else void route.continue();
		});
		const r = await request.patch(`/api/v1/workspaces/${ws}/items/${open.id}`, {
			headers: h,
			data: { title: `${open.title} renamed` }
		});
		expect(r.ok(), await r.text()).toBeTruthy();
		await expect.poll(() => held.length, { timeout: 10_000 }).toBeGreaterThan(0);

		await page.keyboard.press('j');
		holding = false;
		for (const route of held) await route.continue();
		await expect(page.locator(`.item-card[data-item-key="${top}"]`)).toContainText('renamed');
		await page.waitForTimeout(800);

		const log = await page.evaluate(() => (window as unknown as { __b3204: string[] }).__b3204);
		const left = log.findIndex((s) => s !== top);
		expect(left, `the cursor never left the top row: ${JSON.stringify(log)}`).toBeGreaterThanOrEqual(0);
		expect(
			log.slice(left).filter((s) => s === top),
			`the cursor snapped back to the pane's old item after j: ${JSON.stringify(log)}`
		).toEqual([]);
		// And it ends where j put it, not back on the pane's old item.
		await expect(page.locator('.item-card.focused')).not.toHaveAttribute('data-item-key', top!);
	} finally {
		await request.delete(`/api/v1/workspaces/${ws}/collections/${coll.slug}`, { headers: h });
	}
});
