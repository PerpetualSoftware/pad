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
});
