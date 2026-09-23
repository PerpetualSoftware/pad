import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * BUG-2182 — the split pane's Back button (shown after following a link from
 * item A to item B inside the pane) returned to A at the TOP, not at the scroll
 * position A was read at.
 */
test('BUG-2182: pane Back returns to the previous item at its scroll position', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
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
	try {
		const titleB = `B2182 target ${stamp}`;
		const b = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
			headers, data: { title: titleB, content: 'The linked item.' },
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

		// The element that scrolls the pane: the nearest scrollable ancestor of A's link.
		const scroller = await link.evaluateHandle((el) => {
			let n: HTMLElement | null = el.parentElement;
			while (n) {
				const cs = getComputedStyle(n);
				if (/(auto|scroll)/.test(cs.overflowY) && n.scrollHeight > n.clientHeight) return n;
				n = n.parentElement;
			}
			return document.scrollingElement;
		});
		const scrollerDesc = await scroller.evaluate((n) => (n as HTMLElement).className || (n as HTMLElement).tagName);
		await link.scrollIntoViewIfNeeded();
		await page.waitForTimeout(300);
		const before = await scroller.evaluate((n) => (n as HTMLElement).scrollTop);
		expect(before, `precondition: the pane scrolled (${scrollerDesc})`).toBeGreaterThan(300);

		// A click on a link in the editor opens its popover; the popover's href is
		// what drills the pane to the linked item.
		await link.click();
		await page.locator('.link-href').first().click();
		await expect(page.locator('.pane-back-btn')).toBeVisible({ timeout: 10_000 });
		await expect(page.locator('.pane-header-ref-text')).toBeVisible();
		await page.locator('.pane-back-btn').click();
		await expect(pane.locator('a', { hasText: titleB }).last()).toBeAttached({ timeout: 10_000 });
		await page.waitForTimeout(800);

		const after = await page.evaluate((cls) => {
			const n = [...document.querySelectorAll<HTMLElement>('*')].find((e) => (e.className || e.tagName) === cls);
			return n ? n.scrollTop : -1;
		}, scrollerDesc);
		console.log('DIAG scroller', scrollerDesc, 'before', before, 'after', after);
		expect(Math.abs(after - before), `pane Back returned to scrollTop ${after}, not ${before}`).toBeLessThan(40);
	} finally {
		await deleteCollection(fixture, request, coll.slug);
	}
});
