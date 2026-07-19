import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Pane-navigation controller — depth/ownership state machine
 * (PLAN-2154 Architecture A / TASK-2157).
 *
 * The collection page's split pane (PLAN-2105) becomes a navigable
 * mini-browser: `openItemPane` handles lateral/list opens; `navigatePaneTo`
 * handles in-pane DRILLS; `closeItemPane` does a three-way, ownership-aware,
 * staged unwind. Depth + ownership are stamped in SvelteKit `page.state` so
 * they follow opaque Back/Forward, survive `history.go`, and reconstruct on
 * cold-load.
 *
 * `navigatePaneTo` has NO in-pane content-link caller yet (those land in
 * TASK-2158/2159/2160). We exercise it — and read back the depth/ownership
 * stamp — through the localStorage-gated `window.__padPaneController` test hook
 * this task ships, so the drill / reset / three-way-close arithmetic is
 * verified end-to-end in a REAL browser before the UI callers exist. jsdom has
 * no history/navigation model, so the pure arithmetic is unit-tested in
 * paneController.test.ts and the runtime wiring is verified here.
 *
 * Viewport is driven explicitly (desktop split), so one project is enough.
 */

const DESKTOP = { width: 1200, height: 900 };
const MOBILE = { width: 768, height: 1024 }; // == breakpoint (inclusive) → overlay + BottomSheet

function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

function pathname(page: Page): string {
	return new URL(page.url()).pathname;
}

interface HookState {
	paneDepth: number;
	paneOwned: boolean;
}

/** Read the controller's live {paneDepth, paneOwned} via the test hook. */
function paneState(page: Page): Promise<HookState | null> {
	return page.evaluate(() => {
		const c = (window as unknown as { __padPaneController?: { getPaneState(): HookState } })
			.__padPaneController;
		return c ? c.getPaneState() : null;
	});
}

/** Drive an in-pane DRILL (`navigatePaneTo`) via the test hook. */
async function drillTo(page: Page, ref: string): Promise<void> {
	await page.evaluate((r) => {
		(
			window as unknown as { __padPaneController?: { navigatePaneTo(ref: string): void } }
		).__padPaneController?.navigatePaneTo(r);
	}, ref);
}

/** Imperative close via the test hook (exercises the three-way close). */
async function hookClose(page: Page): Promise<void> {
	await page.evaluate(() => {
		(
			window as unknown as { __padPaneController?: { closeItemPane(): void } }
		).__padPaneController?.closeItemPane();
	});
}

function historyLength(page: Page): Promise<number> {
	return page.evaluate(() => history.length);
}

/**
 * Whether the currently-focused element is a pane-header Back button
 * (`aria-label="Back"`). A `page.evaluate` read of `document.activeElement`,
 * NOT `expect(locator).toBeFocused()` — this file's other focus checks
 * (pane-a11y-focus.spec.ts) use the same `evaluate`-based pattern, which is
 * the reliable one under heavy parallel load: a bare locator-based
 * `toBeFocused()` polls independently of the app's own microtask queue and
 * can observe a still-mid-flush DOM after a header swap, where an
 * `evaluate` round-trip naturally lets pending Svelte effects settle first.
 */
function backButtonIsFocused(page: Page): Promise<boolean> {
	return page.evaluate(() => document.activeElement?.getAttribute('aria-label') === 'Back');
}

/** Enable the controller test hook for all navigations in this context. */
async function enableHook(page: Page): Promise<void> {
	await page.addInitScript(() => {
		try {
			localStorage.setItem('pad:pane-test-hook', '1');
		} catch {
			/* private mode / disabled storage — hook simply won't install */
		}
	});
}

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/** Create a fresh, test-scoped renamable collection (letters-only prefix, name
 *  unique — see pane-collection-migration-race.spec.ts for the slug/prefix
 *  gotchas). */
async function seedCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	namePrefix: string,
	itemPrefix: string,
): Promise<{ id: string; slug: string; name: string }> {
	const name = `${namePrefix} ${Date.now()}`;
	const schema = JSON.stringify({ fields: [{ key: 'note', label: 'Note', type: 'text' }] });
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: itemPrefix, schema },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string; name: string };
}

async function seedItemIn(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
): Promise<{ id: string; slug: string }> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authHeaders(fixture), data: { title, fields: JSON.stringify({}), content: '' } },
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

test.describe('pane controller: depth/ownership state machine (PLAN-2154 / TASK-2157)', () => {
	// The controller is viewport-agnostic; the desktop split project is enough.
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'controller is viewport-agnostic; the desktop split project is enough',
		);
	});

	test('no PLAN-2105 regression: click-open (owned depth 0), j/k re-target (still depth 0), close returns to pre-pane URL', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl regress alpha');
		await seedDoc(fixture, request, 'Ctrl regress bravo');
		await page.goto(docsUrl(fixture));

		const prePaneUrl = page.url();
		const row = page.locator('.item-card', { hasText: 'Ctrl regress alpha' }).first();
		await expect(row).toBeVisible();
		await row.click();

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const refA = openItemParam(page);
		expect(refA).not.toBeNull();
		// First-open MINTS ownership at depth 0.
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// j moves the list cursor; the pane FOLLOWS (re-target replace) — still
		// depth 0, still owned, and NO new history entry (the PLAN-2105 fix).
		const lenAfterOpen = await historyLength(page);
		await page.keyboard.press('j');
		await expect.poll(() => openItemParam(page)).not.toBe(refA);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		expect(await historyLength(page)).toBe(lenAfterOpen); // replace, not push
		await expect(pane).toBeVisible();

		// Close via the pane's real ✕ (aria-label is unique across the loaded and
		// minimal headers, which are mutually exclusive) — OWNED depth 0 →
		// history.go(-1) back to the exact pre-pane URL (pane gone, `?item=` gone).
		await pane.locator('button[aria-label="Close pane"]').click();
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		await expect.poll(() => page.url()).toBe(prePaneUrl);
	});

	test('a duplicate close gesture cannot stack a second history.go and overshoot the pre-pane URL', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl dblclose alpha');
		await page.goto(docsUrl(fixture));

		const prePaneUrl = page.url();
		await page.locator('.item-card', { hasText: 'Ctrl dblclose alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// Fire closeItemPane TWICE synchronously (a double-click ✕ / ESC+click
		// race). The owned close is a one-phase history.go(-1); without the
		// in-flight fence the second call would read the still-stale state and
		// stack a SECOND go(-1), overshooting PAST the pre-pane entry (here, back
		// to /login). The fence must make the second call a no-op.
		await page.evaluate(() => {
			const c = (
				window as unknown as { __padPaneController?: { closeItemPane(): void } }
			).__padPaneController;
			c?.closeItemPane();
			c?.closeItemPane();
		});

		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		// Landed EXACTLY on the pre-pane URL — not overshot to /login.
		await expect.poll(() => page.url()).toBe(prePaneUrl);
		expect(page.url()).not.toContain('/login');
	});

	test('drill A→B pushes a depth-1 owned entry; browser Back returns to A at depth 0', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl drill alpha');
		const b = await seedDoc(fixture, request, 'Ctrl drill bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl drill alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const refA = openItemParam(page);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		const lenBeforeDrill = await historyLength(page);

		// DRILL A→B (no UI caller yet → test hook). Pushes depth 1, INHERITING
		// ownership (owned base → owned drill).
		await drillTo(page, b.slug);
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect(pane.locator('.title', { hasText: /Ctrl drill bravo/ })).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		expect(await historyLength(page)).toBe(lenBeforeDrill + 1); // push

		// Same-ref guard (D4): re-drilling to the CURRENT item is a no-op — no
		// new history entry, depth unchanged.
		const lenAtB = await historyLength(page);
		await drillTo(page, b.slug);
		await page.waitForTimeout(50);
		expect(await historyLength(page)).toBe(lenAtB);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Browser Back unwinds ONE hop → back to A at depth 0.
		await page.goBack();
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await expect(pane.locator('.title', { hasText: /Ctrl drill alpha/ })).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
	});

	test('detach: j/k is INERT at depth>0 (a list follow cannot re-target a drilled stack)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl detach alpha');
		const b = await seedDoc(fixture, request, 'Ctrl detach bravo');
		await seedDoc(fixture, request, 'Ctrl detach charlie');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl detach alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		// Drill to B → depth 1 (detached).
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		const refAtB = openItemParam(page);

		// Move focus to the list, then press j. schedulePaneFollow must BAIL at
		// depth>0 (both at schedule time and in the fired callback), so the pane
		// stays on B — j/k does NOT laterally re-target a drilled stack.
		await page.evaluate(() => document.querySelector<HTMLElement>('.list-column')?.focus());
		await page.keyboard.press('j');
		await page.waitForTimeout(250); // > PANE_FOLLOW_DEBOUNCE_MS (140ms)
		expect(openItemParam(page)).toBe(refAtB);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
	});

	test('cold-load close: a cold `?item=` then drill closes back to the cold base — never go(-2) off it', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Ctrl cold alpha');
		const b = await seedDoc(fixture, request, 'Ctrl cold bravo');

		// COLD LOAD: deep-link straight into an open pane. No history stamp →
		// UNOWNED base.
		await page.goto(docsUrl(fixture, `?item=${a.slug}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: false });
		const coldBasePath = pathname(page);

		// Drill A→B: INHERITS the cold base's unowned stamp (depth 1, UNOWNED) —
		// this is what keeps the cold-base close branch reachable.
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: false });

		// Imperative close: UNOWNED depth>0 → history.go(-1) to the cold base,
		// THEN a latched replaceState-delete of `?item=`. Must NOT go(-2) off the
		// base into whatever preceded the cold load (the /login page here).
		await hookClose(page);
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		// Landed on the SAME collection page (the cold base), not an earlier page.
		expect(pathname(page)).toBe(coldBasePath);
		expect(page.url()).not.toContain('/login');
	});

	test('detached row click RESETS the stack: a new top-level open, closing cleanly to the pre-pane URL', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl reset alpha');
		const b = await seedDoc(fixture, request, 'Ctrl reset bravo');
		await seedDoc(fixture, request, 'Ctrl reset charlie');
		await page.goto(docsUrl(fixture));

		const prePaneUrl = page.url();
		await page.locator('.item-card', { hasText: 'Ctrl reset alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		// Drill A→B (depth 1).
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Now a DIRECT list-row click on a THIRD item (charlie) while detached:
		// resets the stack (go(-depth) to base, then a latched re-target),
		// landing at depth 0 (the base ownership preserved → owned).
		await page.locator('.item-card', { hasText: 'Ctrl reset charlie' }).first().click();
		await expect(pane.locator('.title', { hasText: /Ctrl reset charlie/ })).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// The reset collapsed the stack, so closing now returns straight to the
		// pre-pane URL in a single unwind (owned go(-1)).
		await pane.locator('button[aria-label="Close pane"]').click();
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect.poll(() => page.url()).toBe(prePaneUrl);
	});

	test('collection rename keeps the pane on the NEW slug and closes cleanly (ownership rebased, not 404)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const coll = await seedCollection(fixture, request, 'Ctrl rename', 'CTRN');
		await seedItemIn(fixture, request, coll.slug, 'Rename target item');

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}`);
		await page.locator('.item-card', { hasText: 'Rename target item' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		// Owned first-open at depth 0.
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		// The `?item=` value (canonical ref or slug) that must survive the rename.
		const itemParam = openItemParam(page);
		expect(itemParam).not.toBeNull();

		// Rename the collection via the pane's Quick Actions → Manage actions →
		// Edit Collection modal (General tab). The rename changes the pathname
		// (old→new slug) while preserving `?item=` — routed through
		// onNavigateAway (must replaceState + rebase ownership).
		await pane.locator('button.trigger-btn[title="Quick actions"]').click();
		await pane.locator('button.action-item.footer-row', { hasText: 'Manage actions' }).click();
		await expect(page.locator('#edit-collection-title')).toBeVisible();
		// "Manage actions" opens the modal on the Actions tab — switch to General
		// to reach the collection-name field.
		await page.locator('button.tab', { hasText: 'General' }).click();
		const newName = `Ctrl renamed ${Date.now()}`;
		await page.locator('input.name-input').fill(newName);
		await page.locator('button.btn-save', { hasText: 'Save Changes' }).click();

		// The pane survives on the NEW slug with `?item=` intact and the pane not
		// remounted away.
		await expect.poll(() => new URL(page.url()).pathname).not.toContain(`/${coll.slug}`);
		const newPath = new URL(page.url()).pathname;
		expect(newPath).toMatch(new RegExp(`/${fixture.workspaceSlug}/[^/]+$`));
		expect(openItemParam(page)).toBe(itemParam);
		await expect(pane).toBeVisible();
		// Ownership REBASED to a fresh unowned depth-0 base on the new slug (the
		// pre-pane entry now points at the dead old slug).
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: false });

		// Closing must drop `?item=` IN PLACE (staying on the valid new slug),
		// never `history.go` back onto the now-404 old slug.
		await pane.locator('button[aria-label="Close pane"]').click();
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		expect(new URL(page.url()).pathname).toBe(newPath);
		// Still a LIVE collection page — the renamed collection renders its own
		// heading, not a "Collection not found" error. (The item list re-hydrates
		// the renamed collection asynchronously via the local index, so we assert
		// page liveness via the heading rather than a specific row.)
		await expect(page.getByRole('heading', { level: 1, name: new RegExp(newName) })).toBeVisible();
	});

	// ── Detach at depth>0 (PLAN-2154 Architecture C / D-detach / TASK-2161) ──
	// Once the pane DRILLS past its base (depth>0) it becomes an independent
	// viewer: the list row-highlight is cleared, the pane-snap effect stops
	// snapping, and j/k goes INERT (schedulePaneFollow bails at schedule time and
	// re-checks depth in its fired callback, and any pending follow is cancelled
	// on drill). A direct row-click, by contrast, RESETS the stack (covered by
	// the 'detached row click RESETS' test above). Only row-clicks reset; j/k
	// never does.

	test('detach: at depth>0 the list highlight is CLEARED, j/k cannot re-introduce it or move the pane, and unwind restores it', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl hl alpha');
		const b = await seedDoc(fixture, request, 'Ctrl hl bravo');
		await seedDoc(fixture, request, 'Ctrl hl charlie');
		await page.goto(docsUrl(fixture));

		// The list row-highlight marker (`focusedItemId === item.id`) renders as
		// `.item-card.focused` in the list column. Scoped to `.list-column` so the
		// pane's own ItemDetail can never match.
		const focusedRows = page.locator('.list-column .item-card.focused');

		// Open A at depth 0 → the pane-snap effect highlights A's row.
		await page.locator('.item-card', { hasText: 'Ctrl hl alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect(focusedRows).toHaveCount(1);
		await expect(focusedRows).toHaveText(/Ctrl hl alpha/);

		// Drill A→B → depth 1 (detached). The highlight is CLEARED (focusedItemId
		// gated to null at depth>0) and the pane-snap effect no longer snaps to B.
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		const refAtB = openItemParam(page);
		await expect(focusedRows).toHaveCount(0);

		// j/k is INERT at depth>0: pressing j moves no highlight and does NOT
		// re-target (schedulePaneFollow bails; focusedItemId stays null).
		await page.evaluate(() => document.querySelector<HTMLElement>('.list-column')?.focus());
		await page.keyboard.press('j');
		await page.waitForTimeout(250); // > PANE_FOLLOW_DEBOUNCE_MS (140ms)
		await expect(focusedRows).toHaveCount(0);
		expect(openItemParam(page)).toBe(refAtB);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Browser Back unwinds to depth 0 → the base row's highlight is restored
		// (both openItemRef and page.state change, so the gated effect re-runs).
		await page.goBack();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect(focusedRows).toHaveCount(1);
		await expect(focusedRows).toHaveText(/Ctrl hl alpha/);
	});

	test('R3 late-timer: a j/k pane-follow scheduled at depth 0 does NOT clobber a drill to depth>0', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl late alpha');
		const b = await seedDoc(fixture, request, 'Ctrl late bravo');
		await seedDoc(fixture, request, 'Ctrl late charlie');
		await page.goto(docsUrl(fixture));

		// Open A at depth 0.
		await page.locator('.item-card', { hasText: 'Ctrl late alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// SCHEDULE a pane-follow at depth 0 (press j while focused on the list) —
		// the 140ms debounce timer is now pending, armed to re-target the pane to
		// the newly focused row.
		await page.evaluate(() => document.querySelector<HTMLElement>('.list-column')?.focus());
		await page.keyboard.press('j');

		// DRILL to B BEFORE the follow fires (well within the 140ms window). The
		// drill cancels the pending follow AND pushes depth 1; even if a timer
		// somehow survived, its fired callback re-checks the CURRENT depth and
		// bails. Either way the drilled `?item=` must NOT be clobbered.
		await drillTo(page, b.slug);
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Wait PAST the debounce window: the stale follow must never fire. Had it
		// clobbered, it would have `openItemPane`-RESET the stack (depth→0) and
		// re-targeted `?item=` to the j-focused row.
		await page.waitForTimeout(250); // > PANE_FOLLOW_DEBOUNCE_MS (140ms)
		expect(openItemParam(page)).toBe(b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
	});

	// ── Depth-aware ESC (PLAN-2154 Architecture C / R2, TASK-2163) ──────────
	// Once the pane has drilled past its base (depth>0), ESC pops exactly ONE
	// level via `history.back()` and consumes the key — it must NOT route
	// through the two-level list-focus step or the list-row helpers
	// (`returnFocusToList` / `resolvePaneReturnTarget`), which are meaningless
	// once detached. Only at depth 0 does ESC fall through to the existing
	// two-level return-focus-to-list-then-close behavior (TASK-2122,
	// exhaustively covered by pane-a11y-focus.spec.ts — unaffected by this
	// change, since the new depth>0 branch returns before reaching it).

	test('depth-aware ESC: pops one level per press at depth>0 (pane stays open, ?item= stays truthy)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl esc alpha');
		const b = await seedDoc(fixture, request, 'Ctrl esc bravo');
		const c = await seedDoc(fixture, request, 'Ctrl esc charlie');
		const d = await seedDoc(fixture, request, 'Ctrl esc delta');
		await page.goto(docsUrl(fixture));

		const prePaneUrl = page.url();
		await page.locator('.item-card', { hasText: 'Ctrl esc alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// Drill to depth 3: A(0) → B(1) → C(2) → D(3).
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await drillTo(page, c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await drillTo(page, d.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });
		expect(openItemParam(page)).toBe(d.slug);

		// ESC at depth 3 pops exactly ONE level — the pane stays open, `?item=`
		// stays truthy (now C), and no list row is even focused, so the
		// two-level list-focus step never fires.
		await page.keyboard.press('Escape');
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(c.slug);
		await expect(pane).toBeVisible();

		// A second press pops again, to depth 1 (B) — one level per press, never
		// a jump straight to the base or a close.
		await page.keyboard.press('Escape');
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect(pane).toBeVisible();

		// A third press reaches the base (depth 0, back on A) — still open.
		await page.keyboard.press('Escape');
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).not.toBeNull();
		await expect(pane).toBeVisible();

		// At depth 0, ESC falls through to today's behavior: no row is focused,
		// so the two-level "return focus to list" step is skipped and the
		// escape stack's pane handler closes it directly — unchanged from
		// before TASK-2163.
		await page.keyboard.press('Escape');
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		await expect.poll(() => page.url()).toBe(prePaneUrl);
	});

	test('depth-aware ESC: at depth 0 the two-level return-focus-to-list-then-close behavior is unchanged', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl esc0 alpha');
		await seedDoc(fixture, request, 'Ctrl esc0 bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl esc0 alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// Bridge focus into the pane (as a real user would via Tab, TASK-2122).
		await page.keyboard.press('Tab');
		await expect
			.poll(() => page.evaluate(() => !!document.activeElement?.closest('.item-pane')))
			.toBe(true);

		// First ESC at depth 0, focused inside the pane: returns focus to the
		// list — the pane STAYS open, depth stays 0.
		await page.keyboard.press('Escape');
		await expect
			.poll(() => page.evaluate(() => !!document.activeElement?.closest('.item-pane')))
			.toBe(false);
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });

		// Second ESC, now from the list: closes the pane.
		await page.keyboard.press('Escape');
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
	});

	test('depth-aware ESC: a HELD key (auto-repeat keydowns) still pops only one level', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl esc-repeat alpha');
		const b = await seedDoc(fixture, request, 'Ctrl esc-repeat bravo');
		const c = await seedDoc(fixture, request, 'Ctrl esc-repeat charlie');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl esc-repeat alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await drillTo(page, c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });

		// The initial (non-repeat) physical keydown pops one level, to depth 1.
		await page.keyboard.press('Escape');
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(b.slug);

		// A held key then fires a burst of AUTO-REPEAT keydowns
		// (`KeyboardEvent.repeat === true`) — dispatched here after the first
		// pop's traversal has already settled, so `paneNavInFlight()` alone
		// would no longer block them; only the `!e.repeat` guard does. None of
		// them may pop a further level — "one level per press", not per
		// keydown.
		await page.evaluate(() => {
			for (let i = 0; i < 5; i++) {
				window.dispatchEvent(
					new KeyboardEvent('keydown', {
						key: 'Escape',
						bubbles: true,
						cancelable: true,
						repeat: true,
					}),
				);
			}
		});
		await page.waitForTimeout(100);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect(pane).toBeVisible();
	});

	test('depth-aware ESC: a HELD key that pops depth 1→0 does not also close the pane in the same press', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl esc-boundary alpha');
		const b = await seedDoc(fixture, request, 'Ctrl esc-boundary bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl esc-boundary alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// The initial (non-repeat) physical keydown pops depth 1 → 0, back at
		// the base (A). This is the exact boundary Codex round 3 flagged: a
		// naive `!e.repeat` guard scoped only to the depth>0 branch would let
		// the FOLLOWING repeat events (now depth 0, no `.item-pane` focus) fall
		// through to `runTopEscape()` and close the pane — all within the same
		// physical key hold.
		await page.keyboard.press('Escape');
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).not.toBeNull();
		await expect(pane).toBeVisible();

		// A burst of auto-repeat keydowns from the same continued hold must be
		// fully inert — no fall-through to the two-level step or a close.
		await page.evaluate(() => {
			for (let i = 0; i < 5; i++) {
				window.dispatchEvent(
					new KeyboardEvent('keydown', {
						key: 'Escape',
						bubbles: true,
						cancelable: true,
						repeat: true,
					}),
				);
			}
		});
		await page.waitForTimeout(100);
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).not.toBeNull();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
	});

	test('depth-aware ESC: an open BottomSheet owns ESC — the pane underneath must not also pop', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'mobile viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(MOBILE);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl esc-sheet alpha');
		const b = await seedDoc(fixture, request, 'Ctrl esc-sheet bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl esc-sheet alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Open the mobile Quick Actions BottomSheet from within the pane.
		// `BottomSheet.svelte` has no focus trap — it never moves focus into
		// itself on open — so `document.activeElement` stays on the trigger
		// button back inside `.item-pane` while the sheet is showing.
		await pane.locator('button.trigger-btn[title="Quick actions"]').click();
		const sheet = page.locator('[role="dialog"]', { hasText: 'Quick actions' });
		await expect(sheet).toBeVisible();

		// ESC must be owned entirely by the sheet (it has its own window
		// keydown listener, outside the shared escape stack): the sheet
		// closes, and the pane beneath it must NOT also pop a drill level —
		// a target-based dialog guard would miss this because focus never
		// moved into the sheet (Codex review, TASK-2163).
		await page.keyboard.press('Escape');
		await expect(sheet).toBeHidden();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect(pane).toBeVisible();
	});

	// ── In-pane Back chevron (PLAN-2154 Architecture C, TASK-2164) ──────────
	// The pane chrome (⤢/↗/✕ today) grows a fourth control — a Back chevron —
	// shown ONLY once the pane has drilled past its base (depth>0), wired to
	// the same fenced `paneHistoryGo(-1)`/`paneNavInFlight()` traversal the
	// depth-aware ESC handler above uses (R14). Depth is read reactively off
	// SvelteKit `page.state` inside `ItemDetail` itself (never raw
	// `history.state`), so the chevron's visibility always matches the
	// controller's own `paneState()` read back through the test hook.

	test('Back chevron: hidden at depth 0, visible once drilled, one press pops exactly one level', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl back alpha');
		const b = await seedDoc(fixture, request, 'Ctrl back bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl back alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		const refA = openItemParam(page);

		const backBtn = pane.locator('button[aria-label="Back"]');
		// Depth 0 (first-open) — the chevron is not rendered at all.
		await expect(backBtn).toHaveCount(0);

		// Drill A→B → depth 1 — the chevron appears.
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect(backBtn).toBeVisible();

		// One press pops exactly one level: back to A at depth 0, and the pane
		// stays open (this is a drill-stack pop, not a close).
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await expect(pane).toBeVisible();
		// Back at the base — the chevron is gone again.
		await expect(pane.locator('button[aria-label="Back"]')).toHaveCount(0);
	});

	test('Back chevron: pops one level per press across a multi-hop drill', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl back-multi alpha');
		const b = await seedDoc(fixture, request, 'Ctrl back-multi bravo');
		const c = await seedDoc(fixture, request, 'Ctrl back-multi charlie');
		const d = await seedDoc(fixture, request, 'Ctrl back-multi delta');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl back-multi alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		// Drill to depth 3: A(0) → B(1) → C(2) → D(3). Each `drillTo` only
		// awaits `navigatePaneTo`'s SYNCHRONOUS portion — the `goto()` it
		// fires is fire-and-forget — so poll `page.state` to settle after
		// each hop before firing the next; otherwise a later drill can read
		// a still-stale depth off an in-flight earlier one (Codex review
		// round 2, P1).
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await drillTo(page, c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await drillTo(page, d.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });
		expect(openItemParam(page)).toBe(d.slug);

		const backBtn = pane.locator('button[aria-label="Back"]');

		// Press 1: D(3) → C(2). The click triggers a reload (`loading` briefly
		// true), which swaps the loaded header for the minimal one and
		// unmounts the just-clicked, focused Back button — Codex review round
		// 2, P2. Focus must land back on the (re-mounted) Back button once the
		// reload settles, so a keyboard user can keep popping levels.
		await expect(backBtn).toBeVisible();
		await backBtn.focus();
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(c.slug);
		await expect.poll(() => backButtonIsFocused(page)).toBe(true);

		// Press 2: C(2) → B(1). Focus restored again.
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect.poll(() => backButtonIsFocused(page)).toBe(true);

		// Press 3: B(1) → A(0), the base — chevron disappears, pane stays open.
		// The terminal pop has no Back button left to land focus on, so it
		// falls back to the always-present Close button rather than stranding
		// focus on <body> (Codex review round 4).
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect(pane).toBeVisible();
		await expect(pane.locator('button[aria-label="Back"]')).toHaveCount(0);
		await expect
			.poll(() => page.evaluate(() => document.activeElement?.getAttribute('aria-label')))
			.toBe('Close pane');
	});

	test('Back chevron: a second press while the first pop is still loading still ends with focus restored (Codex review round 3)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl back-race alpha');
		const b = await seedDoc(fixture, request, 'Ctrl back-race bravo');
		const c = await seedDoc(fixture, request, 'Ctrl back-race charlie');
		const d = await seedDoc(fixture, request, 'Ctrl back-race delta');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl back-race alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await drillTo(page, c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await drillTo(page, d.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });

		// Gate C's item fetch — the FIRST Back press (D→C) lands on it and
		// stalls mid-load, so the SECOND Back press (fired while that fetch is
		// still outstanding) exercises the exact race Codex round 3 flagged:
		// a click while `loading` is already true from an EARLIER, unrelated
		// load must not let the focus-restore effect latch onto that stale
		// cycle instead of its own.
		let releaseGate: () => void = () => {};
		const gate = new Promise<void>((resolve) => {
			releaseGate = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${c.slug}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			await gate;
			await route.continue();
		});

		const backBtn = pane.locator('button[aria-label="Back"]');
		await expect(backBtn).toBeVisible();
		await backBtn.focus();

		// Press 1: D(3) → C(2). C's fetch is gated — this pop never actually
		// finishes loading until the gate is released below.
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		// Wait for the DOM to actually reach the minimal (loading) header —
		// not just for `page.state` to have updated — so press 2 below
		// deterministically lands mid-fetch regardless of system load
		// (rather than merely racing the depth-poll's own timing).
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();

		// Press 2, fired while `loading` is STILL true from the stalled C
		// fetch (the history-level `paneNavInFlight()` fence has already
		// cleared by now, since that only guards the `history.go` traversal
		// itself, not the data fetch it triggers). Targets B, whose fetch is
		// NOT gated.
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(b.slug);

		// The pane settled on B — focus must have landed on ITS Back button,
		// not been consumed early by C's stale, still-outstanding load.
		await expect.poll(() => backButtonIsFocused(page)).toBe(true);

		// Release the gate so C's now-superseded fetch can drain (generation-
		// fenced server-side by `loadData`'s own `myGen === loadGeneration`
		// checks — it must not clobber the now-current B state).
		releaseGate();
		await page.waitForTimeout(100);
		await expect.poll(() => openItemParam(page)).toBe(b.slug);
		await expect.poll(() => backButtonIsFocused(page)).toBe(true);
	});

	test('Back chevron: a superseding row-click while the pop is still loading does NOT steal focus back into the pane (Codex review round 6)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Ctrl back-supersede alpha');
		const b = await seedDoc(fixture, request, 'Ctrl back-supersede bravo');
		const c = await seedDoc(fixture, request, 'Ctrl back-supersede charlie');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl back-supersede alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		// A real row click puts the item's REF (e.g. `DOC-16`), not its slug,
		// in `?item=` (`itemUrlId` prefers `formatItemRef` — +page.svelte),
		// and that's what the fetch path segment uses too. Capture it here
		// rather than assuming `a.slug` so the route gate below actually
		// matches.
		const aRef = openItemParam(page);

		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Gate A's item fetch — the Back press (B→A) lands on it and stalls
		// mid-load, giving room to fire an UNRELATED navigation (a direct
		// row click on C) before the Back pop ever settles.
		let releaseGate: () => void = () => {};
		const gate = new Promise<void>((resolve) => {
			releaseGate = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${aRef}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			await gate;
			await route.continue();
		});

		const backBtn = pane.locator('button[aria-label="Back"]');
		await expect(backBtn).toBeVisible();
		await backBtn.focus();
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		// Confirm the pop is genuinely stalled mid-fetch before superseding it.
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();

		// SUPERSEDE: a direct row click on a THIRD item while A is still
		// loading. `paneDepth` is already 0 (the Back pop's own destination
		// settled at the history level), so this is a normal depth-0
		// re-target (`openItemPane` replace) — a completely different, later
		// navigation than the stalled Back pop, landing on C.
		await page.locator('.item-card', { hasText: 'Ctrl back-supersede charlie' }).first().click();
		await expect(pane.locator('.title', { hasText: /Ctrl back-supersede charlie/ })).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		// A row click lands the item's ref (not necessarily its slug — same
		// `itemUrlId` caveat as `aRef` above) in `?item=`; just confirm it
		// changed to something other than A's ref.
		const cRef = openItemParam(page);
		expect(cRef).not.toBe(aRef);

		// The stalled Back pop's pending focus-restore must NOT fire once A
		// eventually settles — it was superseded, so it must not steal focus
		// into C's pane chrome. `backButtonIsFocused` covers the paneDepth>0
		// case; C is at depth 0, so also check the Close button directly.
		const closeIsFocused = () =>
			page.evaluate(() => document.activeElement?.getAttribute('aria-label') === 'Close pane');
		await expect.poll(closeIsFocused).toBe(false);

		// Release the gate — A's now-superseded fetch drains (generation-
		// fenced by `loadData`'s own checks), and the abandoned restore must
		// still not fire afterward.
		releaseGate();
		await page.waitForTimeout(150);
		await expect.poll(() => openItemParam(page)).toBe(cRef);
		await expect.poll(closeIsFocused).toBe(false);
	});

	test('Back chevron: a genuinely slow (but legitimate) destination load still restores focus, past the no-op check window (Codex review round 8)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl back-slow alpha');
		const b = await seedDoc(fixture, request, 'Ctrl back-slow bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl back-slow alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		// A real row click puts the item's REF (e.g. `DOC-16`), not its slug,
		// in `?item=` (`itemUrlId` prefers `formatItemRef` — +page.svelte),
		// and that's what the fetch path segment uses too.
		const aRef = openItemParam(page);

		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Gate A's item fetch — the Back press (B→A) LANDS on it, no
		// unrelated navigation involved, so the eventual restore is fully
		// legitimate. Held for 400ms: comfortably past the 200ms no-op check
		// (which only disarms a genuine coalesced-to-nothing case — itemSlug
		// has already changed here by then, so it must NOT fire) and past
		// what the old, since-replaced flat 600ms wall-clock timeout design
		// would have tolerated on a slower run.
		let releaseGate: () => void = () => {};
		const gate = new Promise<void>((resolve) => {
			releaseGate = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${aRef}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			await gate;
			await route.continue();
		});

		const backBtn = pane.locator('button[aria-label="Back"]');
		await expect(backBtn).toBeVisible();
		await backBtn.focus();
		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();

		// Wait well past the 200ms no-op check before releasing — the pop
		// must still be pending, not silently abandoned.
		await page.waitForTimeout(400);
		releaseGate();
		await expect.poll(() => openItemParam(page)).toBe(aRef);
		await expect(pane.locator('.pane-header--minimal')).toBeHidden();

		const closeIsFocusedAfterSlowLoad = () =>
			page.evaluate(() => document.activeElement?.getAttribute('aria-label') === 'Close pane');
		await expect.poll(closeIsFocusedAfterSlowLoad).toBe(true);
	});

	test('Back chevron: a cold-loaded shared ?item= starts at depth 0 and stays hidden; browser Back still exits the pane', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Ctrl back-cold alpha');

		// COLD LOAD straight into an open pane — no history stamp, so the
		// controller normalizes to depth 0 / unowned (readPaneState's base
		// default). The chevron must NOT render here.
		await page.goto(docsUrl(fixture, `?item=${a.slug}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: false });
		await expect(pane.locator('button[aria-label="Back"]')).toHaveCount(0);

		// Browser Back at the cold base behaves exactly as before this task —
		// it leaves the page entirely (there was no pre-pane entry to unwind
		// to within this tab's history), unaffected by the chevron addition.
		await page.goBack();
		await expect.poll(() => page.url()).not.toContain(`item=${a.slug}`);
	});

	test('Back chevron: works from the mobile full-screen overlay too', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'mobile viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(MOBILE);
		await enableHook(page);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Ctrl back-mobile alpha');
		const b = await seedDoc(fixture, request, 'Ctrl back-mobile bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card', { hasText: 'Ctrl back-mobile alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const backBtn = pane.locator('button[aria-label="Back"]');
		await expect(backBtn).toHaveCount(0);

		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect(backBtn).toBeVisible();

		await backBtn.click();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect(pane).toBeVisible();
		await expect(pane.locator('button[aria-label="Back"]')).toHaveCount(0);
	});
});
