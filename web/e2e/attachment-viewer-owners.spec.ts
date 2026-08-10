import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	DESKTOP,
	MOBILE,
	TILE,
	VIEWER,
	activeInViewer,
	collectionUrl,
	deleteWorkspace,
	focusAttemptTakes,
	itemUrl,
	uploadAttachment
} from './lib/attachment-viewer';

/**
 * THE SEVEN GLOBAL KEY / GESTURE OWNERS, DEFERRING TO A FRONTMOST VIEWER
 * (TASK-2430, proven in a browser by TASK-2436).
 *
 * TASK-2430 routed seven global owners through `isBlockedByModal()`. Its own
 * tests mount each component in jsdom and drive its handler directly, which
 * proves the GUARD is called — but not that the owner is actually the thing
 * that would have acted on a real page, nor that the app is genuinely inert
 * around it, nor that a captured touch gesture behaves. Its commit message
 * says so: the native top-layer leg "is not asserted against a real engine …
 * end-to-end proof belongs to TASK-2436's Playwright suite".
 *
 * EVERY owner here is asserted twice: once with a viewer frontmost (must stand
 * down) and once with an EMPTY stack (must behave exactly as it always did).
 * The empty-stack half is the contract TASK-2430 promises and the half a
 * blanket "always decline" mutation would break.
 *
 * A viewer is frequently opened with a programmatic `click()` on a tile: once
 * ANY overlay is up the tile may be covered, and once the viewer is up the
 * page is inert by design. The state under test is "a viewer is frontmost",
 * which is what the guards key on — not the path the user took to get there.
 */

/**
 * Long enough for the collection route's j/k pane-follow debounce to land, so
 * consecutive presses in a test are consecutive presses to the app too.
 */
const PANE_FOLLOW_SETTLE_MS = 400;

/**
 * Assert the collection list's cursor MOVES, without depending on which
 * direction is available: the focused row can be the last of its group, where
 * `j` legitimately does nothing. Used for the before and after legs of the
 * arbitration test, where the claim is "navigation is live", not "j moves down".
 */
async function expectCursorMoves(
	page: Page,
	focusedCard: () => Promise<string | null>,
	message: string
): Promise<void> {
	const before = await focusedCard();
	await page.keyboard.press('j');
	if ((await focusedCard()) === before) {
		await page.waitForTimeout(PANE_FOLLOW_SETTLE_MS);
		await page.keyboard.press('k');
	}
	await expect.poll(focusedCard, { message }).not.toBe(before);
}

/** Open a viewer from the first strip tile, past whatever is covering it. */
async function openViewer(page: Page): Promise<void> {
	await page
		.locator(TILE)
		.first()
		.evaluate((el) => (el as HTMLElement).click());
	await expect(page.locator(VIEWER)).toHaveCount(1);
}

/**
 * Open the strip's DELETE-CONFIRM BottomSheet for `filename` (PLAN-2392 3c-ii).
 *
 * Owner 4's co-mounted `BottomSheet` used to be the file-tile OPTIONS panel, but
 * the converged surface retired that channel (a file tile now opens the viewer,
 * not a menu). The strip's delete confirmation is still a `Menu` with
 * `sheetOnMobile`, i.e. a real `.bs-sheet` at the mobile breakpoint — the
 * surviving BottomSheet a user actually meets on this page. The `.att-delete`
 * control is hover/focus-revealed, so a programmatic `click()` opens it without
 * depending on a pointer hover the mobile viewport does not have. Firing only a
 * `click` (no `pointerdown`) also means the sheet's outside-close does not later
 * fire when a viewer is opened over it (the same trick the tile opens use).
 */
async function openStripDeleteSheet(page: Page, filename: string): Promise<void> {
	await page
		.locator('.attachment-strip .att-delete')
		.and(page.getByRole('button', { name: `Delete ${filename}` }))
		.evaluate((el) => (el as HTMLElement).click());
	await expect(page.locator('.bs-sheet')).toBeVisible();
}

test.describe('attachment viewer — global key & gesture owners (TASK-2436)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough'
		);
		await page.setViewportSize(DESKTOP);
	});

	test('owner 1 — the root app-shell shortcuts all stand down, and all still fire with no viewer', async ({
		page,
		fixture,
		request
	}) => {
		// The root shortcuts were entirely unguarded before TASK-2430, and
		// `isInputFocused()` cannot help: a focused viewer BUTTON is not a text
		// control, so every one of them used to fire straight through the viewer.
		// All six are checked, because the guard is one `return` covering all of
		// them — and a test for only one of them would leave five that a
		// re-ordering could resurrect.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner shortcuts');
		await uploadAttachment(fixture, request, doc.id, 'shortcuts.png');
		await page.goto(collectionUrl(fixture, `?item=${encodeURIComponent(doc.slug)}`));
		await expect(page.locator(`.item-pane ${TILE}`).first()).toBeVisible();

		const state = () =>
			page.evaluate(() => ({
				palette: !!document.querySelector('.palette'),
				shortcutsModal: !!document.querySelector('#keyboard-shortcuts-title'),
				quickAdd: !!document.querySelector('.quick-add-modal'),
				detailPanel: localStorage.getItem('pad-detail-panel'),
				sidebarCollapsed: !!document.querySelector('aside.sidebar.collapsed'),
				filterBar: !!document.querySelector('.filter-bar'),
				// Cmd+F's effect once the bar is already open is to FOCUS the
				// search input. Without this the blocked pass would be asserting
				// that an already-true boolean stayed true — i.e. nothing.
				searchFocused: !!document.activeElement?.classList.contains('search-input')
			}));

		// ── BASELINE: with no viewer, each shortcut does what it always did ──
		await page.keyboard.press('Control+k');
		await expect.poll(async () => (await state()).palette).toBe(true);
		await page.keyboard.press('Escape');
		await expect.poll(async () => (await state()).palette).toBe(false);

		const detailBefore = (await state()).detailPanel;
		await page.keyboard.press('Control+]');
		await expect.poll(async () => (await state()).detailPanel).not.toBe(detailBefore);
		const detailAfterToggle = (await state()).detailPanel;

		const sidebarBefore = (await state()).sidebarCollapsed;
		await page.keyboard.press('Control+\\');
		await expect.poll(async () => (await state()).sidebarCollapsed).toBe(!sidebarBefore);
		const sidebarAfterToggle = !sidebarBefore;

		await page.keyboard.press('Control+n');
		await expect.poll(async () => (await state()).quickAdd).toBe(true);
		await page.keyboard.press('Escape');
		await expect.poll(async () => (await state()).quickAdd).toBe(false);

		// `?` BEFORE Cmd+F, deliberately: Cmd+F focuses the filter bar's search
		// INPUT, and `?` is (correctly) ignored while a text control has focus.
		await page.keyboard.press('?');
		await expect.poll(async () => (await state()).shortcutsModal).toBe(true);
		await page.keyboard.press('Escape');
		await expect.poll(async () => (await state()).shortcutsModal).toBe(false);

		await page.keyboard.press('Control+f');
		await expect.poll(async () => (await state()).filterBar).toBe(true);
		await expect.poll(async () => (await state()).searchFocused).toBe(true);
		// Leave the search input, so the `?` in the blocked pass below is a real
		// test of the viewer guard rather than of `isInputFocused()`, and so the
		// Cmd+F assertion below has something to move.
		await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
		await expect.poll(async () => (await state()).searchFocused).toBe(false);

		// ── WITH A VIEWER FRONTMOST: nothing moves ──
		await openViewer(page);
		const before = await state();
		for (const key of ['Control+k', 'Control+]', 'Control+\\', 'Control+n', 'Control+f', '?']) {
			await page.keyboard.press(key);
		}
		// Give any of them a chance to land before asserting nothing did.
		await page.waitForTimeout(300);
		expect(await state(), 'no root shortcut may act while a viewer is frontmost').toEqual(before);
		// Sanity: the values asserted-unchanged are the ones the baseline moved,
		// so "unchanged" is a real claim about six live shortcuts.
		expect(before.detailPanel).toBe(detailAfterToggle);
		expect(before.sidebarCollapsed).toBe(sidebarAfterToggle);
		expect(before.filterBar).toBe(true);
		expect(
			before.searchFocused,
			'Cmd+F re-focuses the search input when the bar is already open — the blocked pass ' +
				'asserts that it did NOT, which needs it unfocused first'
		).toBe(false);
		await expect(page.locator(VIEWER)).toHaveCount(1);
	});

	test('owner 2 — the collection route stops navigating the list under the viewer, and resumes after', async ({
		page,
		fixture,
		request
	}) => {
		// The collection route's keydown handler is the one with TWO halves: the
		// Escape dispatch (which must NOT be arbitrated, or the viewer becomes
		// undismissable) and the navigation switch below it (which must be).
		// This asserts the second half; the first is asserted in
		// `attachment-viewer-modal.spec.ts` — together they pin the guard between
		// its two bounds.
		//
		// Every reading is taken RELATIVE to the one immediately before it, never
		// against a value captured earlier in the test. The suite runs fully
		// parallel against one server, so rows from other specs land in the
		// shared list and re-sort it — the interference the pre-existing
		// `pane-a11y-focus` j/k test suffers from. What this test claims is only
		// "these keys moved the cursor" / "these keys did not", which survives it.
		await browserLogin(page);
		const alpha = await seedDoc(fixture, request, 'Owner nav alpha');
		await seedDoc(fixture, request, 'Owner nav bravo');
		await uploadAttachment(fixture, request, alpha.id, 'nav.png');
		await page.goto(collectionUrl(fixture, `?item=${encodeURIComponent(alpha.slug)}`));
		await expect(page.locator('.item-pane')).toBeVisible();
		await expect(page.locator(`.item-pane ${TILE}`).first()).toBeVisible();

		const focusedCard = () =>
			page.evaluate(() => document.querySelector('.item-card.focused')?.textContent?.trim() ?? null);

		await page.locator('.item-card').first().click();
		await expect.poll(focusedCard).not.toBe(null);

		// BASELINE: the list cursor moves. DIRECTION-AGNOSTIC and one press per
		// direction — the row the cursor lands on can be the last of its group,
		// where `j` legitimately does nothing, and the shared list gains rows
		// from other specs mid-run. The claim is "navigation is live", and the
		// anti-cancellation property belongs to the BLOCKED loop below, which is
		// what this test actually turns on.
		await expectCursorMoves(page, focusedCard, 'baseline: list navigation is live');
		// The cursor re-targets the pane on a debounce; settle before doing
		// anything else so this test measures the arbitration guard rather than
		// the follow debounce.
		await page.waitForTimeout(PANE_FOLLOW_SETTLE_MS);

		// Back to the row that HAS the attachment: j/k re-target the pane as they
		// move (that is the behaviour under test), so the cursor has walked the
		// pane onto a doc with no strip.
		await page.locator('.item-card', { hasText: 'Owner nav alpha' }).first().click();
		await expect(page.locator(`.item-pane ${TILE}`).first()).toBeVisible();
		await page.waitForTimeout(PANE_FOLLOW_SETTLE_MS);

		// WITH A VIEWER: nothing moves, asserted after every individual press.
		// The reference reading is re-taken each time, so a row arriving from
		// another spec (the suite is fully parallel and this is the shared list)
		// cannot be mistaken for a keystroke landing.
		await openViewer(page);
		for (const key of ['j', 'j', 'ArrowDown', 'ArrowDown', 'k', 'ArrowUp']) {
			const before = await focusedCard();
			await page.keyboard.press(key);
			expect(await focusedCard(), `${key} must not move the list under the viewer`).toBe(before);
		}

		// AND IT RESUMES — the guard is a lease, not a latch.
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		// Focus went back to the invoking tile INSIDE the pane; list navigation
		// belongs to the list, so click back onto a row first, as a user would —
		// a row OTHER than the one the pane already shows, since re-selecting the
		// open row re-targets the pane through a null item instead of just moving
		// the cursor.
		await page.locator('.item-card', { hasText: 'Owner nav bravo' }).first().click();
		// Same settle as the baseline: the click re-targets the pane, and a press
		// arriving mid-traversal is deliberately dropped by the route.
		await page.waitForTimeout(PANE_FOLLOW_SETTLE_MS);

		await expectCursorMoves(
			page,
			focusedCard,
			'list navigation must be live again once the viewer closes'
		);
	});

	test('owner 3 — a DockedSheet keeps its Escape while no viewer is open', async ({
		page,
		fixture,
		request
	}) => {
		// EMPTY-STACK REGRESSION for `DockedSheet`. Paired with the
		// viewer-frontmost case in the next test.
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner docked baseline');
		await uploadAttachment(fixture, request, doc.id, 'docked.png');
		await page.goto(itemUrl(fixture, doc.slug));

		await page.getByRole('button', { name: /You/ }).first().click();
		await expect(page.locator('.ds-panel')).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(page.locator('.ds-panel')).toHaveCount(0);
	});

	test('owner 3 — one Escape over a DockedSheet closes the viewer ONLY', async ({
		page,
		fixture,
		request
	}) => {
		// ─────────────────────────────────────────────────────────────────────
		// BUG-2441, FIXED BY TASK-2448 — this asserted the contract and FAILED
		// until the fix landed; the `test.fail()` annotation is retired with it.
		//
		// WHAT USED TO HAPPEN: one Escape closed BOTH layers. Both the route's
		// escape driver and `DockedSheet.onKeydown` are `window` keydown
		// listeners, and Svelte flushes the viewer's teardown SYNCHRONOUSLY inside
		// the driver's handler — so by the time the sheet's listener ran, in the
		// SAME event, the lease was already released and `isBlockedByModal(panelEl)`
		// answered `false`. The guard was correct; the moment it was read was not.
		//
		// WHAT FIXES IT: the viewer marks the EVENT it consumed
		// (`noteEscapeConsumedByViewer`), and the sheet passes that event to
		// `isBlockedByModal` — so the mark outlives the lease and this listener
		// stands down. Remove either half and this test fails again.
		//
		// It is invisible to the unit suite because that suite drives one
		// component's handler in isolation, so nothing releases a lease
		// mid-dispatch. It is invisible to a CLICK-driven test too: closing the
		// same viewer with its Close button leaves the sheet open, which is how
		// this was isolated. `BottomSheet` has the identical shape (next test).
		// ─────────────────────────────────────────────────────────────────────
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner docked viewer');
		await uploadAttachment(fixture, request, doc.id, 'docked.png');
		await page.goto(itemUrl(fixture, doc.slug));

		await page.getByRole('button', { name: /You/ }).first().click();
		const sheet = page.locator('.ds-panel');
		await expect(sheet).toBeVisible();

		await openViewer(page);
		// The sheet is BEHIND the viewer — inert through its ancestor, since
		// unlike the viewer it is not body-portaled.
		expect(
			await page.evaluate(() => !!document.querySelector('.ds-panel')?.closest('[inert]'))
		).toBe(true);

		// Setup is complete and verified; from here the assertion is the one
		// BUG-2441 used to break.
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		// SETTLE FIRST. `DockedSheet` leaves on a fly/fade transition, so it is
		// still in the DOM and still visible for ~200ms after `onclose()` — an
		// assertion made immediately after the press passes even when the sheet
		// HAS been closed. This wait is what makes the test able to fail.
		await page.waitForTimeout(600);
		await expect(sheet, 'the sheet is a LOWER layer and must survive the press').toBeVisible();

		// ...and the SECOND press closes it: "one Escape closes one layer" is a
		// claim about ordering, not about the sheet having gone deaf. Without this
		// the fix could pass by permanently disabling the sheet's Escape.
		await page.keyboard.press('Escape');
		await expect(sheet).toHaveCount(0);
	});

	test('owner 4 — a BottomSheet keeps Escape and its Tab trap with no viewer', async ({
		page,
		fixture,
		request
	}) => {
		// EMPTY-STACK REGRESSION for `BottomSheet`. The strip's DELETE-CONFIRM is a
		// `Menu` with `sheetOnMobile`, i.e. a real `BottomSheet` — the surviving
		// BottomSheet a user meets on this page now the file-tile options panel is
		// retired (PLAN-2392 3c-ii).
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner sheet baseline');
		await uploadAttachment(fixture, request, doc.id, 'pic.png');
		await page.goto(itemUrl(fixture, doc.slug));

		await openStripDeleteSheet(page, 'pic.png');
		const sheet = page.locator('.bs-sheet');
		// The trap holds: Tab keeps focus inside the sheet.
		for (let i = 0; i < 4; i++) {
			await page.keyboard.press('Tab');
			expect(
				await page.evaluate(
					() => !!document.activeElement?.closest('.bs-sheet')
				),
				'with no viewer the sheet still traps Tab'
			).toBe(true);
		}
		await page.keyboard.press('Escape');
		await expect(sheet).toHaveCount(0);
	});

	test("owner 4 — a BottomSheet's Tab trap stands down while the viewer is frontmost", async ({
		page,
		fixture,
		request
	}) => {
		// The half of `BottomSheet`'s guard that is NOT affected by the release
		// ordering: `Tab` is not consumed by the escape driver, so the sheet's
		// handler is the only one that could act — and it must not, or it would
		// drag focus out of the viewer and into inert chrome.
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner sheet tab');
		await uploadAttachment(fixture, request, doc.id, 'pic.png');
		await uploadAttachment(fixture, request, doc.id, 'shot.png');
		await page.goto(itemUrl(fixture, doc.slug));

		// Co-mount a BottomSheet (shot.png's delete confirm) and then a viewer over
		// it (pic.png's tile). The tile open is a programmatic `click()` — only a
		// `click`, no `pointerdown` — so the sheet's outside-close does not fire and
		// it stays mounted BEHIND the frontmost viewer.
		await openStripDeleteSheet(page, 'shot.png');
		await page
			.locator(`${TILE}[aria-label*="pic.png"]`)
			.evaluate((el) => (el as HTMLElement).click());
		await expect(page.locator(VIEWER)).toHaveCount(1);

		for (let i = 0; i < 5; i++) {
			await page.keyboard.press('Tab');
			expect(await activeInViewer(page), `Tab ${i + 1} took focus out of the viewer`).toBe(true);
		}
		// Tab staying put would ALSO be explained by the viewer's own trap over a
		// sheet that is still fully interactive, so aim focus straight at the
		// sheet and confirm the browser refuses it.
		expect(
			await focusAttemptTakes(page, '.bs-sheet button'),
			'the sheet behind the viewer must be inert, not merely skipped by the trap'
		).toBe(false);
	});

	test('owner 4 — one Escape over a BottomSheet closes the viewer ONLY', async ({
		page,
		fixture,
		request
	}) => {
		// ─────────────────────────────────────────────────────────────────────
		// BUG-2441, FIXED BY TASK-2448 — same shape as owner 3's, kept separate
		// because `BottomSheet` and `DockedSheet` are different components with
		// different guards, and the bug specifies this one by name.
		//
		// One Escape used to close both the viewer and the sheet beneath it. The
		// sheet's `isBlockedByModal(sheetEl)` guard was correct; it was READ too
		// late, after the escape driver's handler had already closed the viewer
		// and Svelte had synchronously released the backdrop lease inside the
		// SAME event dispatch. The guard now takes the EVENT as well, and the
		// viewer marks the event it consumed before closing.
		// ─────────────────────────────────────────────────────────────────────
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner sheet escape');
		await uploadAttachment(fixture, request, doc.id, 'pic.png');
		await uploadAttachment(fixture, request, doc.id, 'shot.png');
		await page.goto(itemUrl(fixture, doc.slug));

		await openStripDeleteSheet(page, 'shot.png');
		const sheet = page.locator('.bs-sheet');
		await page
			.locator(`${TILE}[aria-label*="pic.png"]`)
			.evaluate((el) => (el as HTMLElement).click());
		await expect(page.locator(VIEWER)).toHaveCount(1);

		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		// Settle past the sheet's leave transition — see owner 3.
		await page.waitForTimeout(600);
		await expect(sheet, 'the sheet is a LOWER layer and must survive the press').toBeVisible();

		// A SECOND press closes it — the sheet keeps its Escape, it just doesn't
		// get to spend the viewer's one (see owner 3).
		await page.keyboard.press('Escape');
		await expect(sheet).toHaveCount(0);
	});

	test('owner 5 — the TopBar overflow menu keeps Escape with no viewer, and stands down under one', async ({
		page,
		fixture,
		request
	}) => {
		// The overflow menu only exists when workspaces do not fit the bar, so
		// the test manufactures that condition and cleans up after itself.
		// (Its Up/Down branch is knowingly dead in a browser —
		// `svelte-dnd-action` rewrites the roles it queries — and TASK-2430
		// deliberately left it that way, so only Escape is asserted.)
		// Creation happens INSIDE the try, appending as it goes, so a failure
		// half-way through still cleans up what was already made.
		const created: string[] = [];
		try {
			for (let i = 0; i < 6; i++) {
				const resp = await request.post('/api/v1/workspaces', {
					headers: {
						Authorization: `Bearer ${fixture.apiToken}`,
						'Content-Type': 'application/json'
					},
					data: { name: `Viewer overflow ${Date.now()}-${i}` }
				});
				expect(resp.ok(), await resp.text()).toBe(true);
				created.push(((await resp.json()) as { slug: string }).slug);
			}
			await page.setViewportSize({ width: 900, height: 900 });
			await browserLogin(page);
			const doc = await seedDoc(fixture, request, 'Owner overflow');
			await uploadAttachment(fixture, request, doc.id, 'overflow.png');
			await page.goto(itemUrl(fixture, doc.slug));

			const trigger = page.locator('[aria-controls="workspace-overflow-menu"]');
			await expect(trigger).toBeVisible();
			const menu = page.locator('#workspace-overflow-menu.open');

			// BASELINE: Escape closes the menu.
			await trigger.click();
			await expect(menu).toHaveCount(1);
			await page.keyboard.press('Escape');
			await expect(menu).toHaveCount(0);

			// WITH A VIEWER: the menu's window handler must not consume the key.
			//
			// ORDER MATTERS, and not for a test-convenience reason: TopBar's
			// outside-CLICK dismisser is deliberately left unguarded (a pointer
			// dismisser only tears down lower UI), so opening the viewer by
			// clicking a tile legitimately closes an already-open overflow menu.
			// The state under test — menu open BEHIND a frontmost viewer — is
			// therefore reached by opening the menu second.
			await openViewer(page);
			await trigger.evaluate((el) => (el as HTMLElement).click());
			await expect(menu).toHaveCount(1);
			await page.keyboard.press('Escape');
			await expect(page.locator(VIEWER)).toHaveCount(0);
			await page.waitForTimeout(400);
			await expect(menu, 'the overflow menu is a LOWER layer and must survive').toHaveCount(1);
		} finally {
			for (const slug of created) await deleteWorkspace(fixture, request, slug);
		}
	});

	test('owner 6 — the sidebar edge swipe: opens with no viewer, declines under one, and a STRADDLING gesture is abandoned', async ({
		page,
		fixture,
		request
	}) => {
		// The gesture owners are the ones a keyboard-only suite cannot reach at
		// all, and the STRADDLE — a swipe that begins before the viewer opens and
		// continues after — is the case the start gate alone does not cover:
		// touch events keep going to their original target.
		//
		// Touch events are synthesised rather than driven through
		// `page.touchscreen` because the straddle needs the viewer to open
		// BETWEEN two moves of one gesture, which the high-level API cannot
		// express.
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner swipe');
		await uploadAttachment(fixture, request, doc.id, 'swipe.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE).first()).toBeVisible();

		const installTouch = () =>
			page.evaluate(() => {
				(window as any).__touch = (x: number, type: string) => {
					const t = new Touch({ identifier: 1, target: document.body, clientX: x, clientY: 400 });
					window.dispatchEvent(
						new TouchEvent(type, {
							touches: type === 'touchend' ? [] : [t],
							targetTouches: [t],
							changedTouches: [t],
							bubbles: true,
							cancelable: true
						})
					);
				};
			});
		await installTouch();
		const swipeOpen = async () => {
			await page.evaluate(() => {
				const t = (window as any).__touch;
				t(5, 'touchstart');
				t(90, 'touchmove');
				t(150, 'touchmove');
				t(150, 'touchend');
			});
			await page.waitForTimeout(250);
		};
		const sidebarOpen = () =>
			page.evaluate(() => !document.querySelector('aside.sidebar')?.classList.contains('collapsed'));

		// BASELINE: the edge swipe opens the drawer.
		expect(await sidebarOpen()).toBe(false);
		await swipeOpen();
		expect(await sidebarOpen(), 'baseline: an edge swipe opens the sidebar').toBe(true);

		// Close it again so the next two cases start from the same place.
		await page.evaluate(() => {
			document.querySelector<HTMLElement>('.sidebar-backdrop, .mobile-backdrop')?.click();
		});
		await page.reload();
		await expect(page.locator(TILE).first()).toBeVisible();
		await installTouch();
		expect(await sidebarOpen()).toBe(false);

		// WITH A VIEWER: the start gate declines.
		await openViewer(page);
		await swipeOpen();
		expect(await sidebarOpen(), 'the edge swipe must not open the sidebar under a viewer').toBe(
			false
		);
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// STRADDLE: the gesture BEGINS with no viewer and ends with one.
		await page.evaluate(() => {
			const t = (window as any).__touch;
			t(5, 'touchstart');
			// SHORT of the open threshold, so the gesture is genuinely still in
			// flight when the viewer appears — a longer first move would have
			// opened the sidebar before there was anything to straddle.
			t(20, 'touchmove');
		});
		await openViewer(page);
		await page.evaluate(() => {
			const t = (window as any).__touch;
			t(150, 'touchmove');
			t(150, 'touchend');
		});
		await page.waitForTimeout(250);
		expect(
			await sidebarOpen(),
			'a swipe that straddles the viewer opening must be abandoned, not completed'
		).toBe(false);
	});

	test('owner 7 — the co-mounted item graph stops zooming under the viewer, and zooms again after', async ({
		page,
		fixture,
		request
	}) => {
		// The graph is the owner that CO-MOUNTS with the viewer: its drawer stays
		// in the DOM while a viewer opens over it. A user's wheel cannot reach it
		// then (the viewer covers the screen), so the event is dispatched at the
		// graph viewport directly — which is exactly the reachability the guard
		// exists for, and the only way to distinguish "the guard declined" from
		// "the pointer never got there".
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Owner graph');
		await uploadAttachment(fixture, request, doc.id, 'graph.png');
		await page.goto(itemUrl(fixture, doc.slug));

		await page.getByRole('button', { name: 'More item actions' }).first().click();
		await page.getByRole('menuitem', { name: /Dependency graph/ }).click();
		await expect(page.locator('.graph-drawer .item-graph')).toBeVisible();
		const scale = () =>
			page.evaluate(() => {
				const g = document.querySelector('.graph-drawer svg g');
				const m = /scale\(([-\d.]+)\)/.exec(g?.getAttribute('transform') ?? '');
				return m ? Number(m[1]) : null;
			});
		// The graph fits itself to the viewport after its data lands, so the
		// scale moves on its own for a moment. Wait for it to STOP moving —
		// otherwise the baseline below is a reading of the fit, not of the wheel.
		await expect
			.poll(
				async () => {
					const a = await scale();
					await page.waitForTimeout(250);
					return a !== null && a === (await scale());
				},
				{ timeout: 15_000 }
			)
			.toBe(true);

		// Dispatched at the graph viewport rather than delivered by the browser:
		// with the viewer up, a real wheel CANNOT reach the graph (the viewer
		// covers the screen), so a real-input version of the blocked leg would
		// pass whether or not the guard exists. Dispatching directly is the only
		// way to distinguish "the guard declined" from "the pointer never got
		// there". The baseline below uses real input as well, so the handler is
		// known to be wired to what a user actually does.
		const wheel = () =>
			page.evaluate(() => {
				const target = document.querySelector<HTMLElement>('.graph-drawer .viewport')!;
				const rect = target.getBoundingClientRect();
				target.dispatchEvent(
					new WheelEvent('wheel', {
						// ZOOM OUT. The graph fits itself on load and can land on
						// MAX_SCALE, where a zoom-IN wheel is clamped to a no-op —
						// which would make the baseline leg unfalsifiable.
						deltaY: 240,
						clientX: rect.left + rect.width / 2,
						clientY: rect.top + rect.height / 2,
						bubbles: true,
						cancelable: true
					})
				);
			});

		// BASELINE: a REAL wheel, delivered by the browser to whatever is under
		// the pointer, zooms. (Only the baseline can use real input — once the
		// viewer covers the screen no real wheel can reach the graph, which is
		// the whole point of the guard.)
		const baseline = await scale();
		const box = await page.locator('.graph-drawer .viewport').boundingBox();
		await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
		await page.mouse.wheel(0, 240);
		await expect.poll(scale).not.toBe(baseline);
		const zoomed = await scale();

		// WITH A VIEWER: it does not.
		await openViewer(page);
		await wheel();
		await page.waitForTimeout(250);
		expect(await scale(), 'the graph must not zoom under the viewer').toBe(zoomed);

		// AND IT RESUMES — through the same dispatched event, so this leg and the
		// blocked leg above differ ONLY in whether a viewer is up.
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		await wheel();
		await expect.poll(scale).not.toBe(zoomed);
	});
});
