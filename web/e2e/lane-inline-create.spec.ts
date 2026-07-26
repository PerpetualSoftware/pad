import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Board lane inline-create destination e2e (IDEA-2298).
 *
 * The lane `+` opens a Trello-style draft card (TASK-1676); Enter creates the
 * item. TASK-1676 predates the split pane (PLAN-2105), so it hardcoded a
 * FULL-PAGE `goto(.../{item}?new=1)` — which meant the one gesture that already
 * knows the title ejected you off the board and re-opened the title editor on
 * it, while clicking any EXISTING card opened the same item in the pane. This
 * spec pins the corrected destinations:
 *
 *   - desktop → the split pane (`?item=`), same entry point a card click uses
 *   - mobile  → nothing opens; the card just appears in its lane
 *   - both    → the pathname NEVER changes, and `?new=1` is never set
 *
 * The pathname assertion is the load-bearing one on both viewports: a
 * regression back to a full-page navigate changes the pathname, and no amount
 * of pane/lane styling can hide that.
 *
 * Viewport is driven explicitly with `setViewportSize` (the widths straddle the
 * inclusive 768px breakpoint), so both cases are project-independent and we run
 * them on one project rather than doubling the work — matching
 * `pane-mobile-overlay.spec.ts`.
 */

const SPLIT = { width: 1280, height: 900 }; // over the breakpoint → split pane
const OVERLAY = { width: 768, height: 1024 }; // == breakpoint (max-width is inclusive) → mobile

/** The docs board groups by `status`; 'draft' is its first lane. */
const LANE = 'Draft';

function boardUrl(fixture: { adminUsername: string; workspaceSlug: string }): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?view=board`;
}

function collectionPathname(fixture: { adminUsername: string; workspaceSlug: string }): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`;
}

/**
 * Open the docs board with at least one item in the target lane.
 *
 * An EMPTY collection renders a "No docs yet" empty state instead of the
 * lanes, so there'd be no lane `+` to click at all — seed one `status=draft`
 * doc first so the Draft lane exists. Waits for that card so the board (fed by
 * the local index) is known-rendered before the draft flow starts.
 */
async function openBoardWithLane(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
): Promise<void> {
	await seedDoc(fixture, request, 'Lane anchor', { status: 'draft' });
	await page.goto(boardUrl(fixture));
	await expect(page.locator('.item-card', { hasText: 'Lane anchor' }).first()).toBeVisible();
}

/**
 * Drive the lane `+` → type → Enter flow, returning the title used. Waits for
 * the created card to render in the lane, which is the shared postcondition on
 * both viewports (the local-index upsert) and the sync point for the
 * destination assertions that follow.
 */
async function createViaLaneDraft(page: Page, lane: string): Promise<string> {
	const title = `Lane draft ${Date.now()}`;
	await page.getByRole('button', { name: `Add item to ${lane}` }).click();

	const input = page.locator('.lane-draft-input');
	await expect(input).toBeFocused();
	await input.fill(title);
	await input.press('Enter');

	// The item is upserted into the local index on BOTH viewports, so the card
	// appears in the lane either way.
	await expect(page.locator('.item-card', { hasText: title })).toBeVisible();
	return title;
}

test.describe('board lane inline-create destination (IDEA-2298)', () => {
	test('desktop: Enter opens the new item in the split pane without leaving the board', async ({
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
		await openBoardWithLane(page, fixture, request);

		const title = await createViaLaneDraft(page, LANE);

		// Wait for the pane open to actually land before reading the URL. The
		// created card renders off the SYNCHRONOUS local-index upsert, so it is
		// NOT a sync point for the navigation that follows it — asserting
		// straight after it would race, and a pathname assertion in particular
		// would pass vacuously against a full-page `goto` that simply hadn't
		// resolved yet (caught by mutating the fix back out).
		await page.waitForURL((u) => new URL(u).searchParams.has('item'));

		// Opened in the pane — NOT a full-page navigate. `?item=` is the pane's
		// single source of truth, and the pathname is still the collection route.
		const url = new URL(page.url());
		expect(url.pathname).toBe(collectionPathname(fixture));
		expect(url.searchParams.get('item')).toBeTruthy();
		// `?new=1` drives ItemDetail's auto-edit-title flow. The lane draft
		// already captured the title, so re-opening that editor is the exact
		// inconsistency IDEA-2298 filed — it must never be set on this path.
		expect(url.searchParams.get('new')).toBeNull();

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane).toContainText(title);

		// The board stayed mounted behind the pane (it's a split, not a
		// navigation), and the composer closed on submit — uniform on every
		// viewport, since here the pane takes focus anyway.
		await expect(page.locator('.list-column')).toBeVisible();
		await expect(page.locator('.lane-draft-input')).toHaveCount(0);
	});

	test('mobile: Enter creates the card in the lane and opens nothing', async ({
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
		await openBoardWithLane(page, fixture, request);

		await createViaLaneDraft(page, LANE);

		// Composer closed on submit here too (uniform close, IDEA-2298).
		await expect(page.locator('.lane-draft-input')).toHaveCount(0);

		// "Nothing opened" is an ABSENCE, and absences can't be polled — a bare
		// URL assertion here would pass against a full-page `goto` that just
		// hadn't resolved yet. So prove it POSITIVELY instead: re-open the lane
		// composer. That click can only succeed if the board is still mounted and
		// interactive, and awaiting it gives any would-be navigation a real chance
		// to land first. It also encodes the actual UX win — on mobile you can add
		// a second card immediately instead of navigating back.
		await page.getByRole('button', { name: `Add item to ${LANE}` }).click();
		await expect(page.locator('.lane-draft-input')).toBeVisible();
		await page.locator('.lane-draft-input').press('Escape');

		// Still on the board, with nothing opened.
		const url = new URL(page.url());
		expect(url.pathname).toBe(collectionPathname(fixture));
		expect(url.searchParams.get('item')).toBeNull();
		expect(url.searchParams.get('new')).toBeNull();
		await expect(page.locator('.item-pane')).toHaveCount(0);
	});
});
