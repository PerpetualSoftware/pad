import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';

/**
 * BUG-2844 — the sidebar footer's Settings entry must not wrap.
 *
 * WHY THIS IS AN E2E TEST AND NOT A COMPONENT TEST. The defect is a layout
 * outcome: five controls in 235px of usable row, and the Settings label
 * breaking onto a second line when it no longer fits. jsdom performs no
 * layout, so a vitest render of Sidebar.svelte reports the same geometry
 * whether the bug is present or not. Only a real engine can answer it.
 *
 * WHAT IT ASSERTS, and why not the row's height. Height would grow for other
 * reasons — a taller control, a changed line-height — so a height assertion
 * would fail for changes that are not this bug and pass for a wrap that
 * happened to keep the height. The discriminating observable is how many LINE
 * BOXES the label occupies: Range.getClientRects() returns one rect per line,
 * so the count IS the question, and a wrap is the only thing that makes it two.
 *
 * BOTH SUPPORTED WIDTHS, because the arithmetic differs and only one was ever
 * broken. Desktop is 260px of sidebar and carries five controls; below the
 * 768px breakpoint it is 280px AND drops the collapse button, so the same row
 * has four controls and 60px more for the label. Measured before the fix: two
 * line boxes on desktop, one on mobile. A desktop-only test would pass on a
 * change that broke the mobile arm, and vice versa.
 */

const DESKTOP = { width: 1280, height: 900 };
// Below the --sidebar-width breakpoint (max-width: 768px).
const MOBILE = { width: 700, height: 900 };

/**
 * Counts the line boxes the Settings label occupies.
 *
 * Distinct `top` values rather than rect count: a single line can yield
 * several rects when the content has multiple text nodes or inline children,
 * and counting those would report a wrap that is not there.
 */
async function settingsLineBoxes(page: Page): Promise<number> {
	return page.evaluate(() => {
		const settings = document.querySelector('.sidebar-footer .footer-row .settings-btn');
		if (!settings) return -1;
		const range = document.createRange();
		range.selectNodeContents(settings);
		const tops = [...range.getClientRects()]
			.filter((r) => r.width > 0 && r.height > 0)
			.map((r) => Math.round(r.top));
		return new Set(tops).size;
	});
}

async function openSidebarIfDrawer(page: Page, width: number) {
	if (width > 768) return;
	// On mobile the sidebar is a drawer and starts closed; its footer is
	// off-screen until opened, and measuring it closed reports a geometry no
	// user ever sees.
	const toggle = page.locator('button[aria-label*="idebar" i]').first();
	if (await toggle.count()) {
		await toggle.click().catch(() => {});
	}
}

for (const [name, viewport] of [
	['desktop', DESKTOP],
	['mobile', MOBILE]
] as const) {
	test(`sidebar footer: the Settings label stays on one line (${name})`, async ({
		page,
		fixture
	}) => {
		await page.setViewportSize(viewport);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
		await openSidebarIfDrawer(page, viewport.width);

		const row = page.locator('.sidebar-footer .footer-row');
		await expect(row).toBeVisible();

		// PREMISE FIRST: the row must actually hold the control this bug is
		// about. Without this the test passes trivially on a build where the
		// GitHub link — the control whose 32px pushed the label over the edge
		// (IDEA-2711) — was removed or never rendered.
		await expect(row.locator('.settings-btn')).toBeVisible();
		await expect(row.locator('.github-btn')).toBeVisible();

		expect(await settingsLineBoxes(page)).toBe(1);
	});
}

/**
 * The row's own layout, pinned because the fix depends on it.
 *
 * `.settings-btn` is the only shrinkable item, which is what makes every pixel
 * recovered from the gaps land in the label. If another control loses its
 * `flex-shrink: 0` the row starts distributing shrink elsewhere and the
 * headroom this fix bought silently drains — a change that would not fail the
 * line-box assertions until it had gone far enough to wrap again.
 */
test('sidebar footer: Settings is the only shrinkable control in the row', async ({
	page,
	fixture
}) => {
	await page.setViewportSize(DESKTOP);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	await expect(page.locator('.sidebar-footer .footer-row')).toBeVisible();

	const shrinkable = await page.evaluate(() => {
		const row = document.querySelector('.sidebar-footer .footer-row');
		if (!row) return null;
		return [...row.children]
			.filter((c) => getComputedStyle(c).flexShrink !== '0')
			.map((c) => c.className.split(' ')[0]);
	});

	expect(shrinkable).toEqual(['settings-btn']);
});
