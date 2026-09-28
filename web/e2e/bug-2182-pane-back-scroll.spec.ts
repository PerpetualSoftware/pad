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
	const bodyHeight = await pane.locator('.ProseMirror').first().evaluate((el) => (el as HTMLElement).offsetHeight);
	expect(before, 'precondition: the pane scrolled').toBeGreaterThan(300);

	// The popover drills the pane only for a link whose prefix is in the page's
	// FRESH collection list (collectionPrefixMap); before that list lands it
	// navigates instead, and no Back button appears (BUG-3241). This collection
	// was created a moment ago, so wait for it: its sidebar row is rendered from
	// the same store write that marks the list fresh.
	await expect(page.locator(`.nav-section a[href$="/${coll.slug}"]`)).toBeVisible({ timeout: 15_000 });
	// A click on an editor link opens its popover; the popover's href drills.
	await link.click();
	const hrefs = await page.locator('.link-href').evaluateAll((els) => els.map((e) => e.getAttribute('href') ?? e.textContent ?? ''));
	const urlBefore = page.url();
	await page.locator('.link-href').first().click();
	// On failure, say whether the click drilled the pane, navigated the page,
	// or did nothing (BUG-3241): the bare "not visible" could not tell them apart.
	try {
		await expect(page.locator('.pane-back-btn')).toBeVisible({ timeout: 10_000 });
	} catch (e) {
		throw new Error(
			[
				'BUG-3241: the pane Back button never appeared after the popover click.',
				`popover .link-href entries: ${JSON.stringify(hrefs)}`,
				`url before click: ${urlBefore}`,
				`url now: ${page.url()}`,
				`item pane present: ${await pane.count()}`,
				`sidebar row for ${coll.slug}: ${await page.locator(`.nav-section a[href$="/${coll.slug}"]`).count()}`,
				`popover still open: ${await page.locator('.link-href').count()}`,
				`cause: ${(e as Error).message.split('\n')[0]}`,
			].join('\n'),
		);
	}
	await expect(pane.getByText('Target paragraph 0.')).toBeVisible();
	await page.waitForTimeout(300);
	return { coll, itemA, titleB, pane, before, scrollTop, bodyHeight };
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
		// Past the restore cap (RESTORE_CAP_MS, 2500 since BUG-3228), so a restore
		// that ignored the gesture would have landed by now.
		await page.waitForTimeout(3000);
		expect(await ctx.scrollTop(), 'the restore overrode the reader').toBeLessThan(ctx.before / 2);
	} finally {
		await page.unrouteAll({ behavior: 'ignoreErrors' });
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});

// BUG-3250: once applied, the restore HOLDS its target until the cap, because
// parts of the pane settle after the body and move scrollTop (measured: the
// attachment strip's placeholder and the timeline's loader, 70-90px). A 200px
// spacer prepended inside the pane's content stands in for that late shift,
// deterministically.
async function backAndRestore(page: import('@playwright/test').Page, ctx: Awaited<ReturnType<typeof setup>>) {
	await page.locator('.pane-back-btn').click();
	await expect(ctx.pane.locator('a', { hasText: ctx.titleB }).last()).toBeAttached({ timeout: 10_000 });
	await expect.poll(ctx.scrollTop, { timeout: 3000, message: 'premise: the restore landed' }).toBeGreaterThan(ctx.before - 40);
	return ctx.scrollTop();
}
const shiftContent = (pane: import('@playwright/test').Locator) =>
	pane.evaluate((el) => {
		const s = document.createElement('div');
		s.style.height = '200px';
		s.dataset.bug3250 = 'spacer';
		el.querySelector('.item-page')!.prepend(s);
	});

for (const leg of [
	{ name: 'a layout shift during the hold is re-applied', before: async () => {}, reapplied: true },
	{ name: 'a scrollbar-style drag during the hold is honoured', before: async (ctx: any) => { await ctx.pane.evaluate((el: HTMLElement) => { el.scrollTop -= 600; }); }, reapplied: false },
	// A gesture that scrolls nothing: only the gesture listener can end the hold
	// here, since a scrolling wheel is also caught by the drag rule above.
	{ name: 'a wheel gesture during the hold ends it, even one that scrolls nothing', before: async (ctx: any, page: any) => { const b = (await ctx.pane.boundingBox())!; await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2); await page.mouse.wheel(0, 0); }, reapplied: false },
	{ name: 'the hold ends at the cap, leaving no listener behind', before: async (_ctx: any, page: any) => { await page.waitForTimeout(3000); }, reapplied: false },
]) {
	test(`BUG-3250: ${leg.name}`, async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
		test.setTimeout(60_000);
		const ctx = await setup(page, fixture, request);
		try {
			const restored = await backAndRestore(page, ctx);
			await leg.before(ctx, page);
			await page.waitForTimeout(150);
			const beforeShift = await ctx.scrollTop();
			await shiftContent(ctx.pane);
			await page.waitForTimeout(400);
			const after = await ctx.scrollTop();
			if (leg.reapplied) {
				expect(Math.abs(after - restored), `the shift was not re-applied (restored ${restored}, now ${after})`).toBeLessThanOrEqual(3);
			} else {
				expect(Math.abs(after - restored), `the restore overrode the reader or outlived its window (restored ${restored}, before the shift ${beforeShift}, now ${after})`).toBeGreaterThan(50);
			}
		} finally {
			await deleteCollection(fixture, request, ctx.coll.slug);
		}
	});
}

// BUG-3250 codex round 1 (P1): the pane element is reused across items, and a
// link drill is a pushState, which never reaches the popstate cancel. A hold
// still live when the reader drills on must not write A's position into B.
// A GUARD, not a red control: today the drill replaces every child of the pane,
// so the hold's ResizeObserver is left on detached nodes and B was never
// written even before the explicit cancel (measured 3/3, B rendering 419-504ms
// into the hold). This pins the outcome if that DOM identity ever changes.
test('BUG-3250: drilling to another item during the hold does not carry the restore into it', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ctx = await setup(page, fixture, request);
	try {
		await backAndRestore(page, ctx);
		// Still inside the hold: drill to B again from the restored position.
		await ctx.pane.locator('a', { hasText: ctx.titleB }).last().click();
		await page.locator('.link-href').first().click();
		await expect(ctx.pane.getByText('Target paragraph 0.')).toBeVisible({ timeout: 10_000 });
		await page.waitForTimeout(1000);
		expect(await ctx.scrollTop(), "A's restore was carried into B").toBeLessThan(40);
	} finally {
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});

// BUG-3251: the body can render SHORT and grow later. Text typed just before
// leaving the item reaches the reopened connection after its replay, seconds
// later under write load (BUG-3253). A max-height on the pane's editor stands
// in for those missing lines, deterministically, and lifting it is the late
// text landing. The restore clamps at once and holds, so the reader is never
// left at the top while the body is short.
// Injected while B is on screen, so A's body is already short when the restore
// starts; B's body is shorter than the cap, so it is unaffected.
const shortenBody = (page: import('@playwright/test').Page, maxHeight: number) =>
	page.evaluate((maxHeight) => {
		const s = document.createElement('style');
		s.dataset.bug3251 = 'short';
		s.textContent = `.item-pane .ProseMirror { max-height: ${maxHeight}px; overflow: hidden; }`;
		document.head.append(s);
	}, maxHeight);
const growBody = (page: import('@playwright/test').Page) =>
	page.evaluate(() => document.querySelector('style[data-bug3251]')?.remove());

test('BUG-3251: a body that is still short when the restore ends leaves the reader as near as it reaches, not at the top', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ctx = await setup(page, fixture, request);
	try {
		// A's body renders 300px short and stays short past the cap
		// (RESTORE_CAP_MS, 2500).
		await shortenBody(page, ctx.bodyHeight - 300);
		await page.locator('.pane-back-btn').click();
		await expect(ctx.pane.locator('a', { hasText: ctx.titleB }).last()).toBeAttached({ timeout: 10_000 });
		await page.waitForTimeout(3000);
		const max = await ctx.pane.evaluate((el) => el.scrollHeight - el.clientHeight);
		expect(max, 'premise: the body was too short to reach the saved position').toBeLessThan(ctx.before - 100);
		const clamped = await ctx.scrollTop();
		expect(clamped, 'the reader was left at the top').toBeGreaterThan(max - 3);
		// Being at the bottom is also what a stick-to-bottom mechanism would
		// give. This one was a restore: it ended at the cap, so growth after it
		// leaves the reader where it put them (BUG-3251 codex round 1).
		await growBody(page);
		await page.waitForTimeout(400);
		expect(await ctx.pane.evaluate((el) => el.scrollHeight - el.clientHeight), 'premise: the body grew').toBeGreaterThan(max + 200);
		expect(Math.abs((await ctx.scrollTop()) - clamped), 'the position moved after the restore ended').toBeLessThanOrEqual(3);
	} finally {
		await growBody(page);
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});

test('BUG-3251: late growth inside the hold carries the reader to the saved position', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ctx = await setup(page, fixture, request);
	try {
		await shortenBody(page, ctx.bodyHeight - 300);
		await page.locator('.pane-back-btn').click();
		await expect(ctx.pane.locator('a', { hasText: ctx.titleB }).last()).toBeAttached({ timeout: 10_000 });
		await page.waitForTimeout(600);
		await growBody(page);
		await expect.poll(ctx.scrollTop, { timeout: 3000, message: 'the late growth was not walked to the saved position' })
			.toBeGreaterThan(ctx.before - 40);
		expect(await ctx.scrollTop()).toBeLessThan(ctx.before + 40);
	} finally {
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});

// BUG-3251 codex round 1: a scrollbar drag raises no wheel/touch/key event, and
// the height rule cannot see it when it shares a frame with late growth: the
// scroll arrives with the height already changed, so it reads as a clamp and
// the next resize re-applies the target over the reader. The press on the
// scrollbar is a pointerdown on the pane, and that ends the restore.
test('BUG-3251: a scrollbar drag in the same frame as late growth is honoured', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ctx = await setup(page, fixture, request);
	try {
		await shortenBody(page, ctx.bodyHeight - 300);
		await page.locator('.pane-back-btn').click();
		await expect(ctx.pane.locator('a', { hasText: ctx.titleB }).last()).toBeAttached({ timeout: 10_000 });
		const max = () => ctx.pane.evaluate((el) => el.scrollHeight - el.clientHeight);
		await expect.poll(async () => (await ctx.scrollTop()) - (await max()), { timeout: 3000, message: 'premise: the clamped restore landed' })
			.toBeGreaterThan(-3);
		const box = (await ctx.pane.boundingBox())!;
		await page.mouse.move(box.x + box.width - 4, box.y + box.height / 2);
		await page.mouse.down();
		const dragged = await ctx.pane.evaluate((el) => {
			document.querySelector('style[data-bug3251]')?.remove();
			el.scrollTop -= 600;
			return el.scrollTop;
		});
		await page.waitForTimeout(400);
		await page.mouse.up();
		expect(Math.abs((await ctx.scrollTop()) - dragged), `the restore overrode the drag (dragged to ${dragged})`).toBeLessThanOrEqual(3);
	} finally {
		await growBody(page);
		await deleteCollection(fixture, request, ctx.coll.slug);
	}
});
