import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { Page } from '@playwright/test';

/**
 * Mobile split-pane full-screen overlay e2e (PLAN-2105 Phase 4 / TASK-2121).
 *
 * On the collection page the Asana-style detail pane is URL-derived: `?item=`
 * is the single source of truth for which item is open, and the pane restyles
 * itself by viewport. At ≤768px (the app-wide mobile breakpoint) there's no
 * room to split, so the pane becomes a `position: fixed` full-screen overlay
 * with the list column left mounted BEHIND it (no remount — the PLAN-2105
 * invariant). Above the breakpoint it's a side-by-side split with a draggable
 * divider.
 *
 * The load-bearing correctness property is that crossing the breakpoint is
 * PURE PRESENTATION: it must NOT drop `?item=`. `ui.svelte.ts` has a matchMedia
 * handler that force-closes the legacy `detailPanelOpen` boolean on mobile
 * entry; the URL-derived pane is deliberately NOT wired to it. If a regression
 * ever routes pane visibility through that boolean, entering mobile would
 * silently clear the open item — this spec drives a real breakpoint crossing
 * and asserts `?item=` survives it in both directions.
 *
 * The URL assertions are placed AFTER `settleBreakpoint()`, which blocks until
 * the app's matchMedia `change` listeners AND the Svelte effect flush have run
 * (media query reflects the new width + two animation frames). Asserting before
 * that would false-pass: CSS `position` flips synchronously on resize, so a
 * regression that drops `?item=` from the *async* handler wouldn't have fired
 * yet (Codex review, TASK-2121).
 *
 * Viewport is driven explicitly with `setViewportSize`, so the behaviour is
 * project-independent; we run on one project to avoid doubling the work. The
 * widths straddle the exact inclusive boundary — 768 (overlay) / 769 (split).
 */

const OVERLAY = { width: 768, height: 1024 }; // == breakpoint (max-width: 768 is inclusive) → overlay
const SPLIT = { width: 769, height: 1024 }; // one px over → split

function collectionUrl(fixture: { adminUsername: string; workspaceSlug: string }, slug: string): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`;
}

/** Computed `position` of the detail pane — 'fixed' in the overlay, 'static' in the split. */
function panePosition(page: Page): Promise<string> {
	return page.locator('.item-pane').evaluate((el) => getComputedStyle(el).position);
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

/**
 * Resize and block until the app has fully reacted: the media query reflects
 * the new width (so the app's `matchMedia` change listeners have been able to
 * fire) and two animation frames have elapsed (strictly after the resize task,
 * so any change-listener side effect + Svelte flush has settled). Deterministic
 * — no fixed timeouts.
 */
async function settleBreakpoint(page: Page, size: { width: number; height: number }): Promise<void> {
	await page.setViewportSize(size);
	const wantMobile = size.width <= 768;
	await page.waitForFunction(
		(mobile) => window.matchMedia('(max-width: 768px)').matches === mobile,
		wantMobile,
	);
	await page.evaluate(
		() => new Promise<void>((r) => requestAnimationFrame(() => requestAnimationFrame(() => r()))),
	);
}

test.describe('mobile split-pane full-screen overlay (PLAN-2105 Phase 4 / TASK-2121)', () => {
	test('at the ≤768px boundary the pane is a fixed full-screen overlay covering the viewport', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(OVERLAY);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'Mobile overlay');
		await page.goto(collectionUrl(fixture, slug));

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => panePosition(page)).toBe('fixed');

		// Overlay covers the FULL viewport EXACTLY: each edge pinned within a
		// ±1px tolerance band (not one-sided bounds — a shifted `x=-100,
		// width=868` or an oversized pane must fail, Codex review pass 2).
		const box = await pane.boundingBox();
		expect(box).not.toBeNull();
		expect(Math.abs(box!.x - 0)).toBeLessThanOrEqual(1); // left edge at 0
		expect(Math.abs(box!.y - 0)).toBeLessThanOrEqual(1); // top edge at 0
		expect(Math.abs(box!.width - OVERLAY.width)).toBeLessThanOrEqual(1); // exactly viewport-wide
		expect(Math.abs(box!.height - OVERLAY.height)).toBeLessThanOrEqual(1); // exactly viewport-tall

		// Stacks above the mobile chrome (BottomNav / MobileContextBar at 40).
		expect(await pane.evaluate((el) => getComputedStyle(el).zIndex)).toBe('60');

		// The list column is still mounted behind the overlay, and the resize
		// divider is meaningless (hidden) in a full-screen overlay.
		await expect(page.locator('.list-column')).toBeAttached();
		await expect(page.locator('.pane-divider')).toBeHidden();

		// URL still carries the open item.
		expect(openItemParam(page)).toBe(slug);
	});

	test('crossing the 768px boundary swaps split ⇄ overlay without dropping ?item= or remounting the pane', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(SPLIT);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'Overlay resize');
		await page.goto(collectionUrl(fixture, slug));

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		// Desktop (769px): side-by-side split — pane is an in-flow flex child
		// (not fixed) and the draggable divider is visible.
		await expect.poll(() => panePosition(page)).not.toBe('fixed');
		await expect(page.locator('.pane-divider')).toBeVisible();
		expect(openItemParam(page)).toBe(slug);

		// Tag the LIVE pane element. If a breakpoint crossing remounts it, the
		// tag is lost — proving the switch is a pure restyle, not a remount
		// (the PLAN-2105 no-remount invariant), stronger than toBeAttached().
		await pane.evaluate((el) => ((el as HTMLElement).dataset.t2121 = 'persist'));

		// Enter mobile (768px): the ui.svelte.ts matchMedia handler fires here
		// (detailPanelOpen = false). Assert AFTER settleBreakpoint so the async
		// handler has definitely run — the URL-derived pane must NOT be dropped.
		await settleBreakpoint(page, OVERLAY);
		await expect.poll(() => panePosition(page)).toBe('fixed');
		await expect(pane).toBeVisible();
		expect(openItemParam(page)).toBe(slug); // ?item= survived entering mobile
		expect(await pane.evaluate((el) => (el as HTMLElement).dataset.t2121)).toBe('persist');

		// Leave mobile (769px): split restored, same item, same element.
		await settleBreakpoint(page, SPLIT);
		await expect.poll(() => panePosition(page)).not.toBe('fixed');
		await expect(page.locator('.pane-divider')).toBeVisible();
		expect(openItemParam(page)).toBe(slug);
		expect(await pane.evaluate((el) => (el as HTMLElement).dataset.t2121)).toBe('persist');
	});
});
