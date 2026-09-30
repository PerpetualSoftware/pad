import { test, expect } from '@playwright/test';
import { asNewUser, openTabs, show, tabBoxes } from './lib/workspace-tabs';

/**
 * TASK-3306: the workspace tab bar as real tabs. Left-aligned, 200px each
 * while there is room, shrinking equally to a 120px minimum, then scrolling
 * with an edge fade and the active tab kept in view; the close X inside its
 * tab's box; "+" right after the last tab; nothing under the collapse button;
 * arrow keys move focus between tabs (roving tabindex).
 *
 * Tabs are per user, and other specs move the admin's, so this spec works as
 * its own registered user with its own workspaces.
 */

test.describe('TASK-3306 workspace tabs', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop only; mobile has the workspace switcher');
	});

	test('left-aligned, 200px each with room, X inside, "+" after the last tab, clear of the right-hand controls', async ({ page }) => {
		test.setTimeout(90_000);
		await page.setViewportSize({ width: 1440, height: 900 });
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 2);
		await show(page, username, slugs[1], 2);

		const tabs = await tabBoxes(page);
		const logo = (await page.locator('.topbar-left').boundingBox())!;
		// Left-aligned: the first tab starts just after the logo, not centered.
		expect(tabs[0].x - (logo.x + logo.width), 'first tab starts right after the logo').toBeLessThan(40);
		expect(tabs[1].x - (tabs[0].x + tabs[0].width)).toBeLessThanOrEqual(4);
		for (const t of tabs) expect(Math.round(t.width)).toBe(200);

		// The close X of the active tab is visible and inside its tab's box.
		for (const slug of slugs.slice(0, 2)) {
			const tab = page.locator(`.workspace-tab[data-ws-slug="${slug}"]`);
			await tab.hover();
			const t = (await tab.boundingBox())!;
			const x = (await tab.locator('.workspace-tab-close').last().boundingBox())!;
			expect(x.x, `${slug}: X left edge inside the tab`).toBeGreaterThanOrEqual(t.x);
			expect(x.x + x.width, `${slug}: X right edge inside the tab`).toBeLessThanOrEqual(t.x + t.width);
			expect(x.y).toBeGreaterThanOrEqual(t.y);
			expect(x.y + x.height).toBeLessThanOrEqual(t.y + t.height);
		}

		// The shape is the TAB's: the active tab paints the background and the
		// link does not, so the X sits inside what the reader sees as the tab.
		// (Before TASK-3306 the link painted it and the X hung outside, while
		// the X was still inside the wrapper div's box, which is why box
		// containment alone is not the claim.)
		const paint = await page.locator('.workspace-tab.active').evaluate((t) => ({
			tab: getComputedStyle(t).backgroundColor,
			link: getComputedStyle(t.querySelector('.workspace-item')!).backgroundColor
		}));
		expect(paint.tab, 'the active tab paints its own background').not.toBe('rgba(0, 0, 0, 0)');
		expect(paint.link, 'the link paints none of its own').toBe('rgba(0, 0, 0, 0)');

		const add = (await page.locator('.workspace-add').boundingBox())!;
		const last = tabs[tabs.length - 1];
		expect(add.x - (last.x + last.width), '"+" sits right after the last tab').toBeLessThan(16);
		expect(add.x).toBeGreaterThan(last.x + last.width - 1);
	});

	test('shrink equally to 120px, then scroll with fades and keep the active tab in view', async ({ page }) => {
		test.setTimeout(120_000);
		await page.setViewportSize({ width: 1440, height: 900 });
		const { username, slugs } = await asNewUser(page);

		// Six fit at 200px each.
		await openTabs(page, slugs, 6);
		await show(page, username, slugs[0], 6);
		for (const t of await tabBoxes(page)) expect(Math.round(t.width)).toBe(200);

		// Fifteen do not: every tab at the 120px minimum, and the list scrolls.
		await openTabs(page, slugs, 15);
		await page.setViewportSize({ width: 900, height: 800 });
		await show(page, username, slugs[0], 15);
		const widths = (await tabBoxes(page)).map((t) => Math.round(t.width));
		expect(new Set(widths), `tab widths ${widths}`).toEqual(new Set([120]));
		const list = page.locator('.workspace-list');
		const m = await list.evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth, sl: el.scrollLeft }));
		expect(m.sw).toBeGreaterThan(m.cw);
		await expect(list).toHaveClass(/fade-right/);
		await expect(list).not.toHaveClass(/fade-left/);

		// Nothing slides under the collapse button or the avatar.
		const row = (await page.locator('.workspace-row').boundingBox())!;
		const right = (await page.locator('.topbar-right').boundingBox())!;
		expect(row.x + row.width).toBeLessThanOrEqual(right.x);

		// The last workspace, when active, is scrolled into view, and the
		// left side now has tabs out of view.
		const lastSlug = slugs[14];
		await show(page, username, lastSlug, 15);
		const lb = (await list.boundingBox())!;
		const ab = (await page.locator(`.workspace-tab[data-ws-slug="${lastSlug}"]`).boundingBox())!;
		expect(ab.x, 'active tab starts inside the list').toBeGreaterThanOrEqual(lb.x - 1);
		expect(ab.x + ab.width, 'active tab ends inside the list').toBeLessThanOrEqual(lb.x + lb.width + 1);
		await expect(list).toHaveClass(/fade-left/);
		// The page itself did not scroll to bring it into view.
		expect(await page.evaluate(() => window.scrollY)).toBe(0);
	});

	test('arrow keys move focus between tabs; one tab link is in the Tab order', async ({ page }) => {
		test.setTimeout(90_000);
		await page.setViewportSize({ width: 1440, height: 900 });
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 4);
		await show(page, username, slugs[1], 4);

		const links = page.locator('.workspace-tab .workspace-item');
		const tabbable = async () => links.evaluateAll((els) => els.filter((e) => e.getAttribute('tabindex') === '0').length);
		expect(await tabbable()).toBe(1);
		await expect(links.nth(1)).toHaveAttribute('tabindex', '0'); // the active tab

		await links.nth(1).focus();
		await page.keyboard.press('ArrowRight');
		await expect(links.nth(2)).toBeFocused();
		await page.keyboard.press('End');
		await expect(links.nth(3)).toBeFocused();
		await page.keyboard.press('Home');
		await expect(links.nth(0)).toBeFocused();
		await page.keyboard.press('ArrowLeft');
		await expect(links.nth(0)).toBeFocused();
		expect(await tabbable()).toBe(1);
		await expect(links.nth(0)).toHaveAttribute('tabindex', '0');
		// Moving focus did not navigate.
		expect(new URL(page.url()).pathname).toBe(`/${username}/${slugs[1]}`);

		// Tab from the focused tab reaches its own close button next.
		await page.keyboard.press('Tab');
		await expect(page.locator(`.workspace-tab[data-ws-slug="${slugs[0]}"] .workspace-tab-close`).last()).toBeFocused();
	});

	test('the tab bar costs one Tab stop per tab set, and Ctrl+Shift+Arrow moves a tab (persisted)', async ({ page }) => {
		test.setTimeout(90_000);
		await page.setViewportSize({ width: 1440, height: 900 });
		const { username, slugs } = await asNewUser(page);
		await openTabs(page, slugs, 3);
		await show(page, username, slugs[0], 3);

		// Walk Tab from the logo: the zone and the tab wrappers are never
		// stops (the dndzone defaults put both in the order; codex r1).
		await page.locator('.pad-logo').focus();
		const stops: string[] = [];
		for (let i = 0; i < 5; i++) {
			await page.keyboard.press('Tab');
			stops.push(await page.evaluate(() => (document.activeElement as HTMLElement).className.split(' ')[0]));
		}
		expect(stops, `Tab stops ${stops}`).not.toContain('workspace-list');
		expect(stops, `Tab stops ${stops}`).not.toContain('workspace-tab');
		expect(stops[0]).toBe('workspace-item');

		let putsSent = 0;
		let putsDone = 0;
		const isPut = (u: string, m: string) => u.endsWith('/api/v1/me/workspace-tabs') && m === 'PUT';
		page.on('request', (r) => { if (isPut(r.url(), r.method())) putsSent++; });
		page.on('requestfinished', (r) => { if (isPut(r.url(), r.method())) putsDone++; });
		page.on('requestfailed', (r) => { if (isPut(r.url(), r.method())) putsDone++; });
		const order = () => page.locator('.workspace-tab').evaluateAll((els) => els.map((e) => e.getAttribute('data-ws-slug')));
		const links = page.locator('.workspace-tab .workspace-item');
		await links.nth(0).focus();
		const put = page.waitForResponse((r) => r.url().endsWith('/api/v1/me/workspace-tabs') && r.request().method() === 'PUT');
		await page.keyboard.press('Control+Shift+ArrowRight');
		await put;
		expect(await order()).toEqual([slugs[1], slugs[0], slugs[2]]);
		// Focus followed the moved tab.
		await expect(page.locator(`.workspace-tab[data-ws-slug="${slugs[0]}"] .workspace-item`)).toBeFocused();
		// A move past either end does nothing.
		await page.keyboard.press('Control+Shift+ArrowLeft');
		await page.keyboard.press('Control+Shift+ArrowLeft');
		await page.waitForTimeout(500);
		expect(await order()).toEqual([slugs[0], slugs[1], slugs[2]]);
		// No navigation: moving is not opening.
		expect(new URL(page.url()).pathname).toBe(`/${username}/${slugs[0]}`);

		// Persisted: two quick moves, then a reload shows the stored order.
		// Counted from the test's start, so an earlier write's late answer is
		// not taken for one of these.
		const sentBefore = putsSent;
		await page.keyboard.press('Control+Shift+ArrowRight');
		await page.keyboard.press('Control+Shift+ArrowRight');
		await expect.poll(order).toEqual([slugs[1], slugs[2], slugs[0]]);
		// Both of these moves' writes SENT (the second goes out only after the
		// first settles, so "all sent have answered" alone passes in the gap
		// between them; codex r2), and every write sent has answered.
		await expect
			.poll(() => putsSent >= sentBefore + 2 && putsSent === putsDone, { timeout: 10_000 })
			.toBe(true);
		await page.reload();
		await expect(page.locator('.workspace-tab')).toHaveCount(3, { timeout: 15_000 });
		expect(await order()).toEqual([slugs[1], slugs[2], slugs[0]]);
	});
});
