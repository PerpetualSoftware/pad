import { test, expect } from './fixtures';
import {
	browserLogin,
	seedDoc,
	EDITOR_SELECTOR,
	SYNCED_BADGE_SELECTOR,
} from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Pane mini-browser — async-race hardening suite (PLAN-2154 R14 / TASK-2167).
 *
 * R14 is the pane controller's late-async-continuation invariant: the
 * persistent no-`{#key}` `<ItemDetail>` plus the controller's debounce timers
 * (`schedulePaneFollow`), `history.go` + `afterNavigate` latches, the popstate
 * mint-settle (`paneMintSettle`), and `loadData` awaits all resume AFTER the
 * user may have re-targeted, drilled, or closed. The invariant every one of
 * those sites must honor: an async continuation that writes pane / URL /
 * history / editor state must (a) re-check the CURRENT `{paneDepth,
 * openItemRef, loadGeneration}` immediately before it writes, and (b) be
 * cancelled on re-target / drill / close — a fence-on-continuation, gen+id
 * checked. The audit (TASK-2155/2156/2157/2161/2162/2166) fenced every site;
 * this suite is the capstone that exercises the fences END-TO-END under
 * adversarial timing so a regression at any one of them fails loudly.
 *
 * Each test asserts the SAME acceptance criterion from a different angle: NO
 * late continuation writes stale state over a newer item — no `?item=`
 * clobber, no depth desync, no crossed content (asserted at the EDITOR / Y.Doc
 * level, not just the title), no stale field write-back. Where the pure
 * decision logic is unit-tested (paneController.test.ts, paneMintSettle.test.ts),
 * this verifies the runtime wiring in a real browser, which jsdom's absent
 * history/navigation model can't reach.
 *
 * Deliberate design against VACUOUS passes (Codex review): the races are made
 * DETERMINISTIC — the stale continuation is proven to have actually run (its
 * gated request is observed entering the gate and draining after release; the
 * fenced-out action is proven to have issued no fetch) rather than merely
 * assumed to have raced. Scenario 1 isolates the continuation-time re-check
 * (b) from the cancellation belt (a) by driving the pane deeper via a POPSTATE
 * that never calls `cancelPaneFollow`, so only the fired callback's own depth
 * re-check can save it. Where a fence is proven by an ASYNCHRONOUS action's
 * absence (a follow reset, a duplicate close traversal), the discriminator
 * counts the underlying `history.go` CALL — which a regressed continuation
 * makes synchronously the instant it fires — rather than observing a settled
 * URL, so the check is immune to traversal-commit timing.
 *
 * Documented residuals (narrowest mitigation applied; not chased further —
 * they are timing/completeness tails, not false-passes in what a scenario
 * claims):
 *   • The post-release loadData drains (scenarios 4/7) order via the browser's
 *     single-threaded event loop — `Response.finished()` (full body delivered)
 *     + a `setTimeout(0)` browser task turn that runs after the continuation's
 *     microtasks. A perfectly deterministic happens-before would need a
 *     renderer-side "loadData resolved" acknowledgement (production
 *     instrumentation), out of scope for a test-only change.
 *   • The suite gates the FIRST loadData await (the item GET). The later
 *     progress/links/members/roles awaits and the SSE/sync refetch handlers
 *     reuse the byte-identical `gen !== loadGeneration` + `item.id` idiom at
 *     every resume; exercising the pattern once end-to-end stands for the
 *     family rather than gating each endpoint separately.
 *
 * Viewport is driven explicitly (desktop split), so the desktop project alone
 * covers the controller — it is viewport-agnostic.
 */

const DESKTOP = { width: 1200, height: 900 };
const FOLLOW_DEBOUNCE_MS = 140; // == PANE_FOLLOW_DEBOUNCE_MS / PANE_MINT_SETTLE_MS
const SYNC_TIMEOUT = 20_000; // slow collab handshake + reconnect backoff

function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

function collUrl(fixture: SuiteFixture, collSlug: string, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}${query}`;
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

/** Create a fresh, test-scoped collection with a plain `note` text field so a
 *  per-item value can prove which item a stale continuation wrote to (mirrors
 *  pane-controller.spec.ts / pane-collection-migration-race.spec.ts). */
async function seedNoteCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	namePrefix: string,
	itemPrefix: string,
): Promise<{ id: string; slug: string }> {
	const name = `${namePrefix} ${Date.now()}`;
	const schema = JSON.stringify({ fields: [{ key: 'note', label: 'Note', type: 'text' }] });
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: itemPrefix, schema },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

async function seedNoteItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
	note: string,
	body = '',
): Promise<{ id: string; slug: string }> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authHeaders(fixture), data: { title, fields: JSON.stringify({ note }), content: body } },
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

/** Seed a fresh, test-scoped collection prefilled with items (optionally with
 *  distinct bodies). Isolating each timing test in its OWN collection keeps its
 *  list deterministic and — critically — keeps its ~4 items OUT of the shared
 *  `docs` collection, whose DOM bloat under parallel load flakes the other pane
 *  specs' row-clicks (Codex/stability review). Returns the collection + seeded
 *  items in insertion order. */
async function seedFreshCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	namePrefix: string,
	itemPrefix: string,
	items: { title: string; body?: string }[],
): Promise<{ collSlug: string; seeded: { id: string; slug: string }[] }> {
	const coll = await seedNoteCollection(fixture, request, namePrefix, itemPrefix);
	const seeded: { id: string; slug: string }[] = [];
	for (const it of items) {
		seeded.push(await seedNoteItem(fixture, request, coll.slug, it.title, '', it.body ?? ''));
	}
	return { collSlug: coll.slug, seeded };
}

/** Seed an item in an arbitrary existing collection (e.g. the startup
 *  template's `tasks`) — for the cross-collection route-reuse scenario. */
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

/** The live "Note" field input inside the pane (asserts a stale continuation
 *  never wrote the wrong item's field). */
function noteInput(page: Page) {
	return page
		.locator('.item-pane .field-row:has(.field-label:text-is("Note"))')
		.locator('input.field-input');
}

/** The pane's live ProseMirror editor surface. */
function paneEditor(page: Page) {
	return page.locator(`.item-pane ${EDITOR_SELECTOR}`);
}

/**
 * Record every bare item-GET the browser issues (path ending in `/items/{seg}`
 * — the plain item fetch `loadData` fires; `/items/{seg}/links` etc. carry an
 * extra segment and are excluded). Returns a live array of the trailing path
 * segments, so a test can prove a fenced-out continuation issued NO fetch, or
 * that a re-target fetched the RIGHT item and not the stale one.
 */
function trackItemGets(page: Page): string[] {
	const segs: string[] = [];
	page.on('request', (req) => {
		if (req.method() !== 'GET') return;
		const m = /\/api\/v1\/workspaces\/[^/]+\/items\/([^/?]+)(?:\?|$)/.exec(req.url());
		if (m) segs.push(decodeURIComponent(m[1]));
	});
	return segs;
}

test.describe('pane async-race hardening (PLAN-2154 R14 / TASK-2167)', () => {
	// The controller is viewport-agnostic; the desktop split project is enough.
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'the pane is a desktop-split concern; one project is enough',
		);
	});

	// ── Scenario 1 — a late j/k pane-follow timer firing AFTER a drill ──────
	// The KEY case: isolate the continuation-time re-check (R14 clause b) from
	// the cancellation belt (clause a). A drill via `navigatePaneTo` cancels
	// the pending follow, so the existing R3 test (pane-controller.spec.ts) can
	// pass on cancellation alone. Here the pane is driven deeper by a browser
	// FORWARD — a popstate that NEVER calls `cancelPaneFollow` — so the armed
	// follow timer survives into depth>0, and ONLY its fired callback's own
	// `currentPaneState().paneDepth > 0` re-check can stop it re-targeting the
	// pane over the forwarded-to item. Remove that re-check and this fails.
	test('a late j/k follow surviving a popstate-forward drill is stopped by its own depth re-check (not cancellation)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		// A FRESH collection with exactly four items — so the list order this test
		// reads is deterministic and can't be contaminated by other tests'/repeats'
		// look-alike docs in the shared `docs` collection (which broke a global-list
		// approach: `titles[0/1]` resolved to a different same-named item than the
		// one drilled to). Four distinct items so the follow target and the forward
		// target can be held DIFFERENT — the crux of isolating the re-check. Keep a
		// title→slug map so we drill by slug.
		const coll = await seedNoteCollection(fixture, request, 'Race recheck', 'RRCK');
		const slugByName = new Map<string, string>();
		for (const name of ['Race recheck alpha', 'Race recheck bravo', 'Race recheck charlie', 'Race recheck delta']) {
			const item = await seedNoteItem(fixture, request, coll.slug, name, '');
			slugByName.set(name, item.slug);
		}
		// Pin list view: this test dispatches `j` and asserts the `.focused`
		// marker steps the flat LIST deterministically. The default is now Board
		// (IDEA-2274), so request list explicitly.
		await page.goto(collUrl(fixture, coll.slug, '?view=list'));

		const pane = page.locator('.item-pane');
		const cardTitles = page.locator('.list-column .item-card .card-title');
		await expect(cardTitles.first()).toBeVisible();
		// Read the list order so we KNOW which row `j` will focus. Opening the
		// first card snaps focus to it; `j` then moves to the second card. The
		// FORWARD target must be a THIRD, distinct item so the follow's callback
		// same-item guard (openItemRef === focused row) can't fire — leaving the
		// depth re-check as the ONLY thing that can stop the follow (Codex round 2).
		const titles = (await cardTitles.allInnerTexts()).map((t) => t.trim());
		const openTitle = titles[0];
		const followTitle = titles[1]; // the row `j` will focus
		const forwardTitle = titles.find((t) => t !== openTitle && t !== followTitle)!;
		expect(forwardTitle, 'need a forward target distinct from the follow target').toBeTruthy();
		expect(forwardTitle).not.toBe(followTitle);
		// Map the rendered titles back to seeded slugs (titles carry the Date.now()
		// suffix; the seeded name is the stable prefix).
		const slugForTitle = (t: string): string => {
			for (const [name, slug] of slugByName) {
				if (t.startsWith(name)) return slug;
			}
			throw new Error(`no seeded slug for rendered title "${t}"`);
		};
		const forwardSlug = slugForTitle(forwardTitle);

		// Open the first card (depth 0). Its `?item=` is the follow's future base.
		await page.locator('.item-card').filter({ has: page.locator('.card-title', { hasText: openTitle }) }).first().click();
		await expect(pane).toBeVisible();
		const refOpen = openItemParam(page);
		// Set up a FORWARD entry to the forward target by drilling to it (its slug)
		// then Back — reachable by a popstate that does NOT touch the controller
		// (so `cancelPaneFollow` never runs).
		await drillTo(page, forwardSlug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		const refForward = openItemParam(page);
		expect(refForward).not.toBe(refOpen);
		await page.goBack();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(refOpen);
		// Wait for the pane-snap to settle the list cursor onto the OPENED row
		// (its `.focused` marker) before pressing `j`, so `j` deterministically
		// steps to the SECOND row (followTitle) and not to row 0 from a still
		// -reset (-1) cursor.
		await expect(page.locator('.list-column .item-card.focused .card-title')).toHaveText(openTitle);

		// Arm the follow AND fire the popstate-forward in ONE synchronous evaluate
		// so the 140ms debounce timer PHYSICALLY cannot elapse between them — the
		// follow is PROVABLY still pending when the forward supersedes it, not just
		// probably (Codex round 3). The synthetic keydown reaches the window-level
		// `handlePageKeydown` (`<svelte:window onkeydown>`); with the cursor on
		// row 0 it steps to row 1 (followTitle) and schedules the follow, then
		// `history.forward()` drills to depth 1 via a popstate that never calls
		// `cancelPaneFollow`. So ONLY the fired callback's own depth re-check can
		// stop the (distinct-target) follow.
		// Also instrument `history.go` so a regressed follow is caught even if its
		// RESET traversal hasn't committed yet by assertion time (Codex round 6): a
		// depth>0 follow re-target is an `openItemPane` RESET → `paneHistoryGo` →
		// `history.go(-depth)`, and that CALL happens synchronously the instant the
		// follow callback fires — long before its ~500ms settle commits. Counting
		// the call (not observing the settled URL) makes the fence check immune to
		// traversal-commit timing. `history.forward()` above is a separate API, not
		// counted.
		await page.evaluate(() => {
			(window as unknown as { __go1Count: number }).__go1Count = 0;
			const orig = history.go.bind(history);
			history.go = ((delta?: number) => {
				(window as unknown as { __go1Count: number }).__go1Count += 1;
				return orig(delta);
			}) as typeof history.go;
			document.querySelector<HTMLElement>('.list-column')?.focus();
			window.dispatchEvent(new KeyboardEvent('keydown', { key: 'j', bubbles: true, cancelable: true }));
			history.forward();
		});
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(refForward);

		// Past the debounce: the surviving follow fired, re-checked depth>0, and
		// bailed. Had ONLY the same-item guard existed it could not have helped
		// (follow target followTitle ≠ forward target), so a passing assertion
		// here means the depth re-check is doing the work — the pane stays on the
		// forward target at depth 1, never reset to depth 0 on the followTitle row.
		const forwardRe = new RegExp(forwardTitle.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
		const followRe = new RegExp(followTitle.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
		await page.waitForTimeout(FOLLOW_DEBOUNCE_MS + 300);
		// Timing-immune discriminator: the follow issued NO reset traversal. A
		// regressed follow's `history.go(-depth)` call would have incremented this
		// the moment the callback fired, whether or not the traversal has settled.
		expect(await page.evaluate(() => (window as unknown as { __go1Count: number }).__go1Count)).toBe(0);
		// The follow also left depth AND `?item=` unmoved (a reset would flip both).
		expect(openItemParam(page)).toBe(refForward);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		// Content confirmation (collab-load tolerant): forward target shown, the
		// follow's target never rendered.
		await expect(pane.locator('.title', { hasText: forwardRe })).toBeVisible({ timeout: SYNC_TIMEOUT });
		await expect(pane.locator('.title', { hasText: followRe })).toHaveCount(0);
	});

	// ── Scenario 2 — a drill fired immediately after a browser Back ─────────
	// The Back is a popstate → it arms a ~140ms mint-settle for the popped-to
	// ref. A drill fired inside that window is a deliberate `goto` that MUST
	// cancel the pending settle and apply immediately, so the stale settle can
	// never revert the pane back to the popped-to item after the drill landed.
	test('a drill fired right after a browser Back lands on the new target; the stale back-settle never reverts it', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const { collSlug, seeded } = await seedFreshCollection(fixture, request, 'Race drillback', 'RDBK', [
			{ title: 'Race drillback alpha' },
			{ title: 'Race drillback bravo' },
			{ title: 'Race drillback charlie' },
		]);
		const [, b, c] = seeded;
		await page.goto(collUrl(fixture, collSlug));

		const itemGets = trackItemGets(page);
		await page.locator('.item-card', { hasText: 'Race drillback alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const refA = openItemParam(page);
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });

		// Back to A (depth 0) — a popstate that arms the mint-settle for A — then
		// IMMEDIATELY drill to C, inside the settle window.
		const getsBeforeBack = itemGets.length;
		await page.goBack();
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await drillTo(page, c.slug);

		await expect.poll(() => openItemParam(page)).toBe(c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await expect(pane.locator('.title', { hasText: /Race drillback charlie/ })).toBeVisible();

		// The cancelled A-settle must NOT fire late and drag the pane back to A.
		await page.waitForTimeout(FOLLOW_DEBOUNCE_MS + 120);
		expect(openItemParam(page)).toBe(c.slug);
		await expect(pane.locator('.title', { hasText: /Race drillback charlie/ })).toBeVisible();
		// Non-vacuous proof the settle was pending-then-CANCELLED, not merely
		// overwritten late: A's ref was never minted (fetched) during the window —
		// the drill's `goto` cancelled the settle before it could re-fetch A. Had
		// the settle fired, A would appear in the post-Back fetch sequence (Codex
		// round 3).
		const postBack = itemGets.slice(getsBeforeBack);
		expect(postBack).toContain(c.slug);
		expect(postBack).not.toContain(refA);
	});

	// ── Scenario 3 — a close fired during an in-flight history.go + latch ────
	// A cold-loaded, then drilled pane closes via a TWO-PHASE cold-base
	// `history.go(-depth)` + a latched `replaceState`-delete fired on the
	// settling popstate. While that traversal is in flight (`paneNavInFlight()`
	// true), a second close AND a drill must both be no-ops — otherwise a
	// stacked second traversal overshoots the cold base, or the drill re-opens
	// a superseded item. Proven non-vacuously: the fenced-out drill's target C
	// is never fetched.
	test('a close + drill fired during a cold-base close traversal are no-ops; C is never fetched and the pane closes on the cold base', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const { collSlug, seeded } = await seedFreshCollection(fixture, request, 'Race closerace', 'RCLR', [
			{ title: 'Race closerace alpha' },
			{ title: 'Race closerace bravo' },
			{ title: 'Race closerace charlie' },
		]);
		const [a, b, c] = seeded;
		const itemGets = trackItemGets(page);

		// COLD LOAD straight into an open pane → UNOWNED base (no history stamp).
		await page.goto(collUrl(fixture, collSlug, `?item=${a.slug}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: false });
		const coldBasePath = pathname(page);

		// Drill A→B: inherits the cold base's UNOWNED stamp (depth 1, unowned) —
		// the branch that closes via cold-base-go + latch.
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: false });
		const getsBeforeClose = itemGets.length;

		// Fire close, then a drill, then close AGAIN — all in one tick. Only the
		// FIRST close acts (issues the go + arms the latch); the drill and the
		// second close hit the in-flight fence and no-op. We COUNT `history.go`
		// calls across the whole synchronous burst: the fence must let EXACTLY ONE
		// through (the first close's cold-base go(-1)). Counting is the only way to
		// distinguish "second close fenced" from "second close's go(-1) was
		// browser-coalesced into the first" — both leave the same end URL, so the
		// end-state assertions alone are vacuous for the second-close fence (Codex
		// round 4). navigatePaneTo/closeItemPane issue NO `history.go` when fenced
		// (the drill uses `goto`; the latch's delete uses `goto`), so a fenced burst
		// yields exactly 1.
		const goCount = await page.evaluate((cRef) => {
			let count = 0;
			const orig = history.go.bind(history);
			history.go = ((delta?: number) => {
				count += 1;
				return orig(delta);
			}) as typeof history.go;
			const ctrl = (
				window as unknown as {
					__padPaneController?: {
						closeItemPane(): void;
						navigatePaneTo(ref: string): void;
					};
				}
			).__padPaneController;
			ctrl?.closeItemPane();
			ctrl?.navigatePaneTo(cRef);
			ctrl?.closeItemPane();
			history.go = orig; // restore before the async traversal/latch runs
			return count;
		}, c.slug);
		// The second close AND the drill were fenced: only the first close's single
		// cold-base traversal was issued.
		expect(goCount).toBe(1);

		// Lands closed on the SAME cold base (depth 0, `?item=` deleted by the
		// latch) — never go(-2) off the base into /login, never re-opened onto C.
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		expect(pathname(page)).toBe(coldBasePath);
		expect(page.url()).not.toContain('/login');
		// Settle any in-flight fetches, then prove the fenced-out drill issued no
		// load: C's item GET never fired (C was seeded but never opened).
		await page.waitForTimeout(200);
		expect(itemGets.slice(getsBeforeClose)).not.toContain(c.slug);
		await expect(page.locator('.item-pane')).toBeHidden();
	});

	// ── Scenario 4 — rapid `?item=` re-target during an in-flight loadData ───
	// The ItemDetail loadGeneration fence: a stalled item GET for A must not,
	// on resolving, write A's title/fields/body over B after `?item=` moved to
	// B. The route gate holds A's GET open until AFTER the re-target so the race
	// is DETERMINISTIC — and the test proves A's request actually entered the
	// gate and drained after release, so the fence is genuinely exercised (not
	// a vacuous pass where A never raced). This gates the FIRST loadData await
	// (the item GET); the later progress/links/members/roles continuations — and
	// the SEPARATE SSE `item_updated` / tab-resume sync handlers — all guard on
	// the byte-identical `{sseGen,syncGen,myGen} !== loadGeneration` + `item.id`
	// idiom at every await-resume (ItemDetail.svelte). This item-GET race stands
	// for that family rather than duplicating a gated-endpoint test per handler;
	// the SSE/sync path is additionally covered by pane-collection-migration-race
	// (a post-await refetch superseded by a switch). (Codex rounds 2-3 residual.)
	test('a stale loadData GET resolving after a rapid re-target does not clobber the newer item’s title, field, or editor body', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const coll = await seedNoteCollection(fixture, request, 'Race loaddata', 'RLDA');
		const a = await seedNoteItem(
			fixture,
			request,
			coll.slug,
			'Race loaddata alpha',
			'note-ALPHA',
			'body marker ALPHA-11',
		);
		const b = await seedNoteItem(
			fixture,
			request,
			coll.slug,
			'Race loaddata bravo',
			'note-BRAVO',
			'body marker BRAVO-22',
		);

		// Gate A's item GET (only the bare item fetch — `/items/{slug}/links`
		// etc. carry an extra segment and pass straight through). Count hits so
		// the test can PROVE A's load actually reached the gate.
		let aHits = 0;
		let releaseA: () => void = () => {};
		const aGate = new Promise<void>((resolve) => {
			releaseA = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${a.slug}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			aHits += 1;
			await aGate;
			await route.continue();
		});

		// COLD LOAD `?item=A`: the pane mounts and fires loadData for A, which
		// stalls on the gate — the pane sits on the minimal (loading) header.
		await page.goto(collUrl(fixture, coll.slug, `?item=${a.slug}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();
		// A's load genuinely reached (and is stuck in) the gate — the race is real.
		await expect.poll(() => aHits).toBeGreaterThanOrEqual(1);

		// Re-target to B (a real list-row click) WHILE A's GET is still
		// outstanding — this bumps loadGeneration; B's GET is not gated.
		await page.locator('.item-card', { hasText: 'Race loaddata bravo' }).first().click();
		await expect(pane.locator('.title', { hasText: /Race loaddata bravo/ })).toBeVisible();
		const refB = openItemParam(page);
		expect(refB).not.toBe(a.slug);
		await expect(noteInput(page)).toHaveValue('note-BRAVO');
		await expect(paneEditor(page)).toContainText('BRAVO-22', { timeout: SYNC_TIMEOUT });

		// Release A's now-superseded GET and deterministically drain its stale
		// continuation before asserting (Codex round 5): `finished()` waits for the
		// full response BODY (so the app's `await response.json()` can resolve), and
		// a single-threaded browser task turn (`setTimeout(0)` inside the page)
		// runs strictly AFTER all the microtasks that continuation is made of — so
		// by the assertions below, loadData's `myGen !== loadGeneration` guard has
		// already run and dropped (or, if regressed, applied) every write.
		const aResponse = page.waitForResponse(
			(r) => r.request().method() === 'GET' && new URL(r.url()).pathname.endsWith(`/items/${a.slug}`),
		);
		releaseA();
		await (await aResponse).finished();
		await page.evaluate(() => new Promise<void>((r) => setTimeout(() => r(), 0)));

		// No flip back to A's title, note, or body after the stale load drained.
		await expect(pane.locator('.title', { hasText: /Race loaddata bravo/ })).toBeVisible();
		expect(openItemParam(page)).toBe(refB);
		await expect(noteInput(page)).toHaveValue('note-BRAVO');
		await expect(paneEditor(page)).toContainText('BRAVO-22');
		await expect(paneEditor(page)).not.toContainText('ALPHA-11');
		await expect(pane.locator('.title', { hasText: /Race loaddata alpha/ })).toHaveCount(0);
	});

	// ── Scenario 5 — a deep browser Back/Forward through a drilled stack ─────
	// A deep owned drill stack, then a deep `history.go(-3)` unwind and a
	// `history.go(3)` re-wind. Each is a popstate that flows through the
	// mint-settle before the `<ItemDetail>` ref prop mints, so the pane must
	// land on exactly the traversed-to item at the correct depth — asserted at
	// the EDITOR BODY level (each item seeded with a distinct body) so a crossed
	// Y.Doc, not just a crossed header, is caught.
	test('a deep Back/Forward traversal through a drilled stack lands the exact item at the right depth with the right editor body', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const { collSlug, seeded } = await seedFreshCollection(fixture, request, 'Race deep', 'RDEP', [
			{ title: 'Race deep alpha', body: 'deep body ALPHA-11' },
			{ title: 'Race deep bravo', body: 'deep body BRAVO-22' },
			{ title: 'Race deep charlie', body: 'deep body CHARLIE-33' },
			{ title: 'Race deep delta', body: 'deep body DELTA-44' },
		]);
		const [, b, c, d] = seeded;
		await page.goto(collUrl(fixture, collSlug));

		await page.locator('.item-card', { hasText: 'Race deep alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const refA = openItemParam(page);
		await expect(paneEditor(page)).toContainText('ALPHA-11', { timeout: SYNC_TIMEOUT });
		await drillTo(page, b.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 1, paneOwned: true });
		await drillTo(page, c.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 2, paneOwned: true });
		await drillTo(page, d.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });
		await expect(paneEditor(page)).toContainText('DELTA-44', { timeout: SYNC_TIMEOUT });

		// Deep Back: unwind all three drills in one traversal → the base A at
		// depth 0. The editor must render A's body (not a stale C/D body), with
		// exactly ONE pane instance.
		await page.evaluate(() => history.go(-3));
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await expect(paneEditor(page)).toContainText('ALPHA-11', { timeout: SYNC_TIMEOUT });
		await expect(paneEditor(page)).not.toContainText('DELTA-44');
		await expect(page.locator('.item-pane')).toHaveCount(1);

		// Deep Forward: re-wind straight back to the deepest drill D at depth 3.
		await page.evaluate(() => history.go(3));
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(d.slug);
		await expect(paneEditor(page)).toContainText('DELTA-44', { timeout: SYNC_TIMEOUT });
		await expect(paneEditor(page)).not.toContainText('ALPHA-11');
	});

	// ── Scenario 5b — a genuine multi-popstate Back BURST coalesces ──────────
	// `history.go(-3)` is a single popstate; the mint-settle's raison d'être is
	// coalescing MULTIPLE popstates (a HELD Back firing one popstate per entry)
	// into a single teardown/mint. Fire three separate `history.back()` calls in
	// one tick — a real burst — and assert the pane coalesces to the correct
	// final item at depth 0 with no crossed content and a single pane instance.
	test('a rapid multi-popstate Back burst through the drilled stack coalesces to the correct final item', async ({
		page,
		fixture,
		request,
	}) => {
		test.setTimeout(60_000);
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const { collSlug, seeded } = await seedFreshCollection(fixture, request, 'Race burst', 'RBST', [
			{ title: 'Race burst alpha', body: 'burst body ALPHA-11' },
			{ title: 'Race burst bravo' },
			{ title: 'Race burst charlie' },
			{ title: 'Race burst delta' },
		]);
		const [, b, c, d] = seeded;
		await page.goto(collUrl(fixture, collSlug));

		const itemGets = trackItemGets(page);
		await page.locator('.item-card', { hasText: 'Race burst alpha' }).first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const refA = openItemParam(page);
		await drillTo(page, b.slug);
		await drillTo(page, c.slug);
		await drillTo(page, d.slug);
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 3, paneOwned: true });
		const getsBeforeBurst = itemGets.length;

		// The burst: THREE genuine popstates. Each `history.back()` is fired from
		// the PRECEDING popstate handler so the traversals can't coalesce at the
		// browser level into a single event — a real held-Back that the pane's
		// mint-settle (not the browser) must coalesce. `popstateCount` proves all
		// three fired (Codex review round 2: a bare 3× synchronous `back()` can be
		// collapsed by pending-traversal coalescing, making the burst untested).
		const popstateCount = await page.evaluate(
			() =>
				new Promise<number>((resolve) => {
					let count = 0;
					const onPop = () => {
						count += 1;
						if (count < 3) {
							history.back();
						} else {
							window.removeEventListener('popstate', onPop);
							resolve(count);
						}
					};
					window.addEventListener('popstate', onPop);
					history.back();
				}),
		);
		expect(popstateCount).toBe(3);

		// Coalesced to the base A at depth 0 — exactly one pane, A's body, no
		// crossed intermediate content stuck from the burst.
		await expect.poll(() => paneState(page)).toEqual({ paneDepth: 0, paneOwned: true });
		await expect.poll(() => openItemParam(page)).toBe(refA);
		await expect(page.locator('.item-pane')).toHaveCount(1);
		await expect(paneEditor(page)).toContainText('ALPHA-11', { timeout: SYNC_TIMEOUT });
		// Non-vacuous proof of COALESCING (Codex round 3), not just the right
		// destination: the mint-settle minted ONLY the final ref. The intermediate
		// stops (C, B) traversed mid-burst were NEVER minted — their item GETs are
		// absent from the burst window. Had each popstate minted eagerly, C and B
		// would appear here.
		await page.waitForTimeout(200);
		const burstGets = itemGets.slice(getsBeforeBurst);
		expect(burstGets).not.toContain(c.slug);
		expect(burstGets).not.toContain(b.slug);
	});

	// ── Scenario 6 — a collection switch that reuses the route component ─────
	// SvelteKit REUSES `[collection]/+page.svelte` across a collection switch,
	// so `collSlug` (and the `<ItemDetail>` props built from it) flip the
	// instant a cross-collection popstate commits — BEFORE `afterNavigate`. The
	// `paneMintForRoute` clamp (falls back to the always-route-consistent
	// `openItemRef` when the pathname diverges from the settling `paneMintRef`)
	// must prevent the OLD collection's item from ever pairing with the NEW
	// collection's route. Proven non-vacuously by the FETCH SEQUENCE: after the
	// cross-collection Back onto docs?item=A, the pane must fetch A — never the
	// stale tasks item X — under the docs route. Had the clamp regressed,
	// `paneMintRef` would still be X, so ItemDetail would mint X under docs and
	// issue a GET for X.
	test('a cross-collection Back onto an open pane fetches the destination item, never the stale source item (paneMintForRoute clamp)', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Race switch docs-A');
		const x = await seedItemIn(fixture, request, 'tasks', 'Race switch tasks-X');
		const itemGets = trackItemGets(page);

		// E1: cold-load docs?item=A (pane open on A under docs).
		await page.goto(docsUrl(fixture, `?item=${a.slug}`));
		const pane = page.locator('.item-pane');
		await expect(pane.locator('.title', { hasText: /Race switch docs-A/ })).toBeVisible();

		// E2: client-side nav to the tasks collection (route REUSE — the same
		// [collection] page, so `collSlug` flips without a remount). The pane
		// closes as `?item=` drops.
		const tasksLink = page.locator(`a.nav-item[href$="/${fixture.workspaceSlug}/tasks"]`).first();
		await expect(tasksLink).toBeVisible();
		await tasksLink.click();
		await expect(page).toHaveURL(new RegExp(`/${fixture.workspaceSlug}/tasks$`));
		await expect(pane).toBeHidden();

		// E3: open X in tasks (owned depth 0) — history is now
		// [docs?item=A, tasks, tasks?item=X].
		await page.locator('.item-card', { hasText: 'Race switch tasks-X' }).first().click();
		await expect(pane.locator('.title', { hasText: /Race switch tasks-X/ })).toBeVisible();
		const refX = openItemParam(page);

		// Deep Back across the collection boundary: tasks?item=X → tasks → back
		// onto docs?item=A. The destination pane must show A under docs.
		const getsBeforeBack = itemGets.length;
		await page.evaluate(() => history.go(-2));
		await expect(page).toHaveURL(new RegExp(`/${fixture.workspaceSlug}/docs`));
		await expect.poll(() => openItemParam(page)).toBe(a.slug);
		await expect(pane.locator('.title', { hasText: /Race switch docs-A/ })).toBeVisible();
		expect(pathname(page)).toContain('/docs');
		await expect(pane.locator('.title', { hasText: /Race switch tasks-X/ })).toHaveCount(0);
		// The clamp discriminator: the reused route fetched A, and NEVER re-fetched
		// the stale tasks item X while landing on docs.
		await page.waitForTimeout(200);
		const getsAfterBack = itemGets.slice(getsBeforeBack);
		expect(getsAfterBack).toContain(a.slug);
		expect(getsAfterBack).not.toContain(refX);
		expect(getsAfterBack).not.toContain(x.slug);
	});

	// ── Scenario 7 — a pending loadData resolving after the pane CLOSES ──────
	// The other end of the loadData fence: `onDestroy` bumps `loadGeneration`,
	// so a GET still outstanding when the pane unmounts must write nothing and
	// PROCEED no further. The non-vacuous discriminator (Codex round 3): the
	// item GET is the FIRST await in loadData, and the very next line after it
	// re-checks `myGen !== loadGeneration` before fetching the item's collection
	// / progress / links. A fenced continuation bails there — so A's downstream
	// sub-resource GETs (`/items/A/progress`, `/items/A/links`) NEVER fire. Were
	// the `onDestroy` gen-bump removed, the resumed continuation would sail past
	// the check and fetch them. We ALSO open B before releasing A so any global
	// write (collectionStore/titleStore/editorStore) would corrupt the visible B,
	// and assert no pageerror + B intact.
	test('a loadData GET resolving after the pane closed and a new item opened writes nothing over the new item', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await enableHook(page);
		await browserLogin(page);
		const { collSlug, seeded } = await seedFreshCollection(fixture, request, 'Race closeload', 'RCLD', [
			{ title: 'Race closeload alpha', body: 'closeload body ALPHA-11' },
			{ title: 'Race closeload bravo', body: 'closeload body BRAVO-22' },
		]);
		const [a] = seeded;

		const pageErrors: string[] = [];
		page.on('pageerror', (err) => pageErrors.push(String(err)));
		// Track A's DOWNSTREAM sub-resource GETs (progress/links) — these fire
		// only if loadData proceeds PAST the post-item-GET gen-check, i.e. only
		// if the onDestroy fence failed.
		const aSubResourceGets: string[] = [];
		page.on('request', (req) => {
			if (req.method() !== 'GET') return;
			const p = new URL(req.url()).pathname;
			if (p.includes(`/items/${a.slug}/`)) aSubResourceGets.push(p);
		});

		let releaseA: () => void = () => {};
		const aGate = new Promise<void>((resolve) => {
			releaseA = resolve;
		});
		await page.route(`**/api/v1/workspaces/*/items/${a.slug}`, async (route) => {
			if (route.request().method() !== 'GET') {
				await route.continue();
				return;
			}
			await aGate;
			await route.continue();
		});

		// COLD LOAD `?item=A`: loadData for A stalls on the gate (minimal header).
		await page.goto(collUrl(fixture, collSlug, `?item=${a.slug}`));
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(pane.locator('.pane-header--minimal')).toBeVisible();

		// Close the cold-loaded pane (UNOWNED depth 0 → replace-delete of `?item=`)
		// while A's GET is still outstanding — the ItemDetail unmounts mid-load.
		await page.evaluate(() => {
			(
				window as unknown as { __padPaneController?: { closeItemPane(): void } }
			).__padPaneController?.closeItemPane();
		});
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();

		// Open B and let it fully load, all while A's GET is STILL gated — B is
		// now the live item and the evidence surface for any stale A write.
		await page.locator('.item-card', { hasText: 'Race closeload bravo' }).first().click();
		await expect(pane).toBeVisible();
		await expect(pane.locator('.title', { hasText: /Race closeload bravo/ })).toBeVisible();
		const refB = openItemParam(page);
		await expect(paneEditor(page)).toContainText('BRAVO-22', { timeout: SYNC_TIMEOUT });

		// Release A's now-orphaned GET and deterministically drain its post-unmount
		// continuation before asserting (Codex round 5): `finished()` waits for the
		// full response body, then a browser task turn runs after all the
		// continuation's microtasks. Its continuation must be a total no-op
		// (generation-fenced) — B's title, body, and `?item=` are unmoved, and
		// nothing throws.
		const aResponse = page.waitForResponse(
			(r) => r.request().method() === 'GET' && new URL(r.url()).pathname.endsWith(`/items/${a.slug}`),
		);
		releaseA();
		await (await aResponse).finished();
		await page.evaluate(() => new Promise<void>((r) => setTimeout(() => r(), 0)));
		await expect(pane.locator('.title', { hasText: /Race closeload bravo/ })).toBeVisible();
		expect(openItemParam(page)).toBe(refB);
		await expect(paneEditor(page)).toContainText('BRAVO-22');
		await expect(paneEditor(page)).not.toContainText('ALPHA-11');
		await expect(pane.locator('.title', { hasText: /Race closeload alpha/ })).toHaveCount(0);
		expect(pageErrors, `no page errors from the post-close load: ${pageErrors.join('; ')}`).toEqual([]);
		// The load-bearing fence proof: A's continuation bailed at the gen-check
		// immediately after its item GET resolved — it never fetched A's collection
		// /progress/links, so no sub-resource GET for A was ever issued.
		expect(
			aSubResourceGets,
			`A's post-unmount continuation must not proceed to sub-resource fetches: ${aSubResourceGets.join(', ')}`,
		).toEqual([]);
	});
});
