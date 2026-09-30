import { test, expect, type Page } from '@playwright/test';
import { asNewUser, openTabs, show, tabBoxes } from './lib/workspace-tabs';

/**
 * TASK-3312: the workspace tabs FEEL like Chrome's (Dave approved all six of the
 * lead's proposals, day 83). This file pins the behaviours:
 *   - the tab uses the arrow cursor, not grab;
 *   - a middle-click closes a tab, and does not open the link;
 *   - the × shows on every tab while tabs are wide, and on hover below that;
 *   - Chrome's close-freeze: after a close the remaining tabs keep their widths
 *     while the pointer stays in the strip, so the next × lands under it. The
 *     freeze is released when the pointer leaves, on a window resize, and on a
 *     tab change that did not come from this strip's own close;
 *   - with reduced motion a close has no animated phase.
 * The visual half (flare, dividers, icon, height) is Dave's screenshot gate.
 */

const W = 1280;
/**
 * Seven tabs at 1280px are 151px each on base a90f75fd, and six are 177px
 * (measured). A close therefore GROWS the rest unless something holds them,
 * which is what gives the freeze legs a precondition that can fail. At ten
 * tabs they sit on the 120px floor and scroll, so a close changes no width
 * with or without a freeze, and those legs passed on base (measured).
 */
const FREEZE_TABS = 7;

async function widths(page: Page) {
	return (await tabBoxes(page)).map((b) => Math.round(b.width * 10) / 10);
}

async function csrfFetch(page: Page, url: string, method: string) {
	return page.evaluate(
		async ([u, m]) => {
			const csrf = document.cookie.match(/pad_csrf=([^;]+)/)?.[1] ?? '';
			const r = await fetch(u, { method: m, headers: { 'X-CSRF-Token': csrf } });
			return r.status;
		},
		[url, method] as const,
	);
}

/** Close the tab at `index` with its ×, leaving the pointer where the × was. */
async function closeAt(page: Page, index: number) {
	const x = page.locator('.workspace-tab').nth(index).locator('.workspace-tab-close').last();
	await x.hover();
	await expect(x).toHaveCSS('opacity', '1');
	await x.click();
}

test.describe('TASK-3312 workspace tabs feel', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop only');
		await page.setViewportSize({ width: W, height: 800 });
	});

	test('a tab uses the arrow cursor, not grab', async ({ page }) => {
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 3);
		await show(page, username, slugs[0], 3);
		for (const i of [0, 1]) {
			await expect(page.locator('.workspace-tab .workspace-item').nth(i)).toHaveCSS('cursor', 'default');
		}
	});

	test('a middle-click closes the tab (persisted), and opens nothing', async ({ page, context }) => {
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 3);
		await show(page, username, slugs[0], 3);
		const pagesBefore = context.pages().length;

		const target = page.locator(`.workspace-tab[data-ws-slug="${slugs[2]}"]`);
		await target.click({ button: 'middle' });
		await expect(target).toHaveCount(0);
		await expect(page, 'the active workspace did not change').toHaveURL(new RegExp(`/${slugs[0]}`));
		await page.waitForTimeout(300);
		expect(context.pages().length, 'no browser tab was opened').toBe(pagesBefore);

		await page.reload();
		await expect(page.locator('.workspace-tab')).toHaveCount(2, { timeout: 15_000 });
		await expect(page.locator(`.workspace-tab[data-ws-slug="${slugs[2]}"]`)).toHaveCount(0);
	});

	test('the × shows on every tab while tabs are wide, and on hover once they are narrow', async ({ page }) => {
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 3);
		await show(page, username, slugs[0], 3);
		await page.mouse.move(W / 2, 700);
		const inactiveX = page.locator(`.workspace-tab[data-ws-slug="${slugs[1]}"] .workspace-tab-close`).last();
		await expect(inactiveX, 'wide inactive tab: × visible without hover').toHaveCSS('opacity', '1');

		await openTabs(page, slugs, 12);
		await show(page, username, slugs[0], 12);
		await page.mouse.move(W / 2, 700);
		const w = await widths(page);
		expect(Math.max(...w), `precondition: the tabs are narrow (${w})`).toBeLessThan(150);
		const narrowX = page.locator(`.workspace-tab[data-ws-slug="${slugs[1]}"] .workspace-tab-close`).last();
		await expect(narrowX, 'narrow inactive tab: × hidden until hover').toHaveCSS('opacity', '0');
		await page.locator(`.workspace-tab[data-ws-slug="${slugs[1]}"]`).hover();
		await expect(narrowX).toHaveCSS('opacity', '1');
	});

	test('close-freeze: widths hold while the pointer stays, and are released when it leaves', async ({ page }) => {
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, FREEZE_TABS);
		await show(page, username, slugs[0], FREEZE_TABS);
		const before = await widths(page);
		expect(before[1], `precondition: the tabs are shrunk below 200 (${before})`).toBeLessThan(199);

		await closeAt(page, 3);
		await expect(page.locator('.workspace-tab')).toHaveCount(FREEZE_TABS - 1);
		await page.waitForTimeout(400);
		const frozen = await widths(page);
		for (const [i, w] of frozen.entries()) {
			expect(Math.abs(w - before[i < 3 ? i : i + 1]), `tab ${i} kept its width (${w} vs ${before})`).toBeLessThan(1);
		}

		await page.mouse.move(W / 2, 700);
		await expect
			.poll(async () => Math.min(...(await widths(page))), { message: 'released: the tabs grow into the freed room' })
			.toBeGreaterThan(before[1] + 2);
	});

	test('close-freeze is released by a window resize', async ({ page }) => {
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, FREEZE_TABS);
		await show(page, username, slugs[0], FREEZE_TABS);
		const before = await widths(page);
		await closeAt(page, 3);
		await expect(page.locator('.workspace-tab')).toHaveCount(FREEZE_TABS - 1);
		await page.waitForTimeout(400);
		expect(Math.abs((await widths(page))[1] - before[1]), 'precondition: frozen').toBeLessThan(1);

		await page.setViewportSize({ width: W + 200, height: 800 });
		await expect
			.poll(async () => Math.min(...(await widths(page))), { message: 'released by the resize' })
			.toBeGreaterThan(before[1] + 2);
	});

	test('close-freeze is released by a tab change that came from elsewhere', async ({ page }) => {
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, FREEZE_TABS);
		await show(page, username, slugs[0], FREEZE_TABS);
		const before = await widths(page);
		await closeAt(page, 3);
		await expect(page.locator('.workspace-tab')).toHaveCount(FREEZE_TABS - 1);
		await page.waitForTimeout(400);
		expect(Math.abs((await widths(page))[1] - before[1]), 'precondition: frozen').toBeLessThan(1);

		// Deleting an open workspace reaches the tab store through the access
		// stream, not through this strip. The pointer never moves.
		expect(await csrfFetch(page, `/api/v1/workspaces/${slugs[5]}`, 'DELETE')).toBeLessThan(300);
		await expect(page.locator('.workspace-tab')).toHaveCount(FREEZE_TABS - 2, { timeout: 15_000 });
		await expect
			.poll(async () => Math.min(...(await widths(page))), { message: 'released by the outside change' })
			.toBeGreaterThan(before[1] + 2);
	});

	// The pair below: without the first, "no .closing phase under reduced
	// motion" passes on a build that never animates at all.
	for (const motion of ['no-preference', 'reduce'] as const) {
	test(`a close ${motion === 'reduce' ? 'has NO' : 'HAS an'} animated phase (prefers-reduced-motion: ${motion})`, async ({ page }) => {
		await page.emulateMedia({ reducedMotion: motion });
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 3);
		await show(page, username, slugs[0], 3);
		await page.evaluate(() => {
			(window as unknown as { __closing: number }).__closing = 0;
			new MutationObserver((ms) => {
				for (const m of ms) {
					const t = m.target as Element;
					if (t.classList?.contains('closing')) (window as unknown as { __closing: number }).__closing++;
				}
			}).observe(document.querySelector('.workspace-list')!, { attributes: true, subtree: true, attributeFilter: ['class'] });
		});
		await closeAt(page, 2);
		await expect(page.locator('.workspace-tab')).toHaveCount(2);
		const phases = await page.evaluate(() => (window as unknown as { __closing: number }).__closing);
		if (motion === 'reduce') expect(phases, 'no .closing phase under reduced motion').toBe(0);
		else expect(phases, 'the close animates through .closing').toBeGreaterThan(0);
	});
	}
});
