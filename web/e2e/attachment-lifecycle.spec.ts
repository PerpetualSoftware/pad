import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	DESKTOP,
	REAL_PNG,
	TILE,
	STRIP_DELETE,
	VIEWER,
	VIEWER_IMAGE,
	VIEWER_COUNTER,
	VIEWER_MISSING,
	archiveItem,
	bulkItems,
	createDoc,
	createWorkspace,
	deleteWorkspace,
	itemUrl,
	postComment,
	uploadAttachment
} from './lib/attachment-viewer';

/**
 * 3c-iii ATTACHMENT LIFECYCLE COMPLETENESS — the browser proof (PLAN-2392 U5 /
 * TASK-2514, the final task in the serial spine).
 *
 * The falsifiable subset jsdom cannot see, one leg per lifecycle mechanism the
 * chain built:
 *
 *  - U3 (navigation-step revalidation): arrowing to a sibling deleted from
 *    ANOTHER context force-probes it no-store and tombstone-advances — a real
 *    browser HTTP cache, a real cross-context delete, a real arrow. jsdom has no
 *    HTTP cache to bypass and no network to 404.
 *  - U2 (strip restore revalidation): the archive-no-op / restore-refetch
 *    list-request choreography, driven through the REAL SSE bulk path
 *    (`items_bulk_updated`, no `item_id`) that only a browser + server exercise.
 *  - U1 (timeline lifecycle reconciliation): a comment thumbnail flipping to the
 *    missing presentation off the process-local deletion bus, live, no reload.
 *
 * Desktop-chromium only: these drive keyboard navigation, the strip's delete
 * control and multi-tab layout — the mobile sheet's own lifecycle is 3d.
 */
test.describe('attachment lifecycle completeness — 3c-iii browser proof (TASK-2514)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop legs only');
		await page.setViewportSize(DESKTOP);
	});

	test('a navigation step force-probes the arrival no-store — a sibling deleted from another context tombstone-advances despite a primed 200 (PLAN-2392 3c-iii U5, U3 seam)', async ({
		page,
		fixture,
		request
	}) => {
		// THE U3 BROWSER PROOF. T6 forced a no-store metadata probe of the OPENED
		// entry only; U3 forces one per (open, entry) pair, so ARROWING to a
		// not-yet-probed sibling revalidates it too — catching a delete the
		// process-local bus never announced (another tab, a job, a direct API call).
		// Observable here as: arrow onto a server-deleted sibling → its forced no-store
		// HEAD 404s → the surface tombstones it and ADVANCES to a survivor, rather than
		// presenting a live-looking stale row from its complete strip seed.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'U5 nav-step delete');
		// Three attachments. The surface pages them in the strip's list order
		// (`created_at DESC`), which is NOT derivable here — three uploads inside one
		// second tie on `created_at`, so their order is the DB's tie-break, not the
		// upload order. So DERIVE the viewer order at runtime rather than assume it (a
		// hard-coded position would be a same-second coincidence that flips the moment
		// the uploads straddle a second — Codex round 1).
		const ids = [
			await uploadAttachment(fixture, request, doc.id, 'nav-1.png'),
			await uploadAttachment(fixture, request, doc.id, 'nav-2.png'),
			await uploadAttachment(fixture, request, doc.id, 'nav-3.png')
		];

		const attPath = (id: string) =>
			`/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${id}`;
		const shownId = async (): Promise<string> =>
			(await page.locator(VIEWER_IMAGE).getAttribute('src'))!.match(/attachments\/([0-9a-f-]+)/)![1];

		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(3);

		// DISCOVERY PASS — learn the actual viewer order without assuming it. Open ANY
		// of the three (identify it by its own shown id), then ArrowRight twice to read
		// the two neighbours in `←/→` order. `arrival` is the entry a single ArrowRight
		// from `opened` lands on; `advanceTarget` is the entry that FOLLOWS `arrival`,
		// where a tombstone-advance off `arrival` lands. All three are distinct (a
		// 3-set), so `advanceTarget !== opened` — the advance is observable. No
		// deletions here, so the set order is stable across the pass.
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		const opened = await shownId();
		await page.keyboard.press('ArrowRight');
		await expect
			.poll(shownId, 'arrow lands on the right neighbour')
			.not.toBe(opened);
		const arrival = await shownId();
		await page.keyboard.press('ArrowRight');
		await expect.poll(shownId, 'second arrow lands on the third entry').not.toBe(arrival);
		const advanceTarget = await shownId();
		expect(new Set([opened, arrival, advanceTarget]).size, 'three distinct entries').toBe(3);
		expect(ids).toContain(arrival);
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// Count 404 HEAD RESPONSES of `arrival`'s EXACT variant-less canonical URL. The
		// metadata existence probe is a HEAD of the variant-less URL (no `?variant`), so
		// anchor on an empty search AND the exact pathname — a thumb GET or a `?variant=`
		// HEAD is never miscounted. Keyed on the RESPONSE, not `requestfinished`: a
		// bodyless HEAD is reported as `net::ERR_ABORTED` after its headers arrive, so it
		// fires `requestfailed`, never `requestfinished`.
		//
		// ONLY 404s are counted, deliberately: the discovery pass above already
		// force-probed `arrival` once (a 200, while it was alive), and one such probe
		// may still be in flight as we register this listener. Counting 200s would let
		// that straggler pollute the count (Codex round 2); a 404 can ONLY come from a
		// probe issued AFTER the API delete below — i.e. the post-reopen navigation
		// probe this leg is proving. The priming's cacheability is asserted directly off
		// the `page.evaluate` return value, not off a 200 counter.
		let headArrival404 = 0;
		page.on('response', (r) => {
			const rq = r.request();
			if (rq.method() !== 'HEAD') return;
			const u = new URL(rq.url());
			if (u.search !== '' || u.pathname !== attPath(arrival)) return;
			if (r.status() === 404) headArrival404 += 1;
		});

		// PRIME `arrival`'s metadata in the PAGE context with a SUCCESSFUL, CACHEABLE
		// response — while it is still alive. A page-side `fetch` (NOT the Playwright
		// `request` fixture, whose separate API context Chromium's page cache never
		// consults) warms the browser HTTP cache with the GET/HEAD handler's
		// `Cache-Control: private, max-age=3600` 200. That warm 200 is exactly what a
		// cache-RESPECTING probe would replay for the LATER 404 — so proving the
		// navigation probe still sees the 404 is the cache-bypass proof. Assert the
		// priming actually got a cacheable 200, or the leg's premise is silently vacuous.
		const primed = await page.evaluate(async (url) => {
			const r = await fetch(url, { method: 'HEAD', credentials: 'same-origin' });
			return { status: r.status, cacheControl: r.headers.get('cache-control') };
		}, attPath(arrival));
		expect(primed.status, 'priming HEAD is a live 200').toBe(200);
		expect(primed.cacheControl ?? '', 'the primed 200 is browser-cacheable').toContain('max-age');
		// No 404 of `arrival` has been seen yet — it is still alive and, crucially,
		// unprobed-since-reopen; the only 404 will come from the post-delete navigation.
		expect(headArrival404, 'no 404 before the delete + reopen').toBe(0);

		// Delete `arrival` via a SEPARATE request context (the API), NOT the same-page
		// strip UI: a same-page delete fires `announceAttachmentDeleted` on the
		// process-local bus and would tombstone it BEFORE the arrow — the leg would then
		// pass WITHOUT U3's per-navigation probe existing. Deleted this way, the ONLY
		// way the surface learns it is gone is the navigation-step probe. The strip is
		// never re-listed (no bus, no SSE to it), so its tiles — and the surface set —
		// still carry the row.
		const del = await request.delete(attPath(arrival), {
			headers: { Authorization: `Bearer ${fixture.apiToken}` }
		});
		expect(del.ok(), await del.text()).toBe(true);

		// RE-OPEN `opened`. A fresh open mints a fresh nonce, so its own entry is
		// force-probed once (200, alive); `arrival` stays UNPROBED until the arrow.
		// (Re-open is REQUIRED: within the discovery open the arrival was already
		// force-probed once for that nonce, so arrowing back would take the fast path —
		// only a fresh nonce re-probes it, now against the 404.)
		const openedFilename = `nav-${ids.indexOf(opened) + 1}.png`;
		await page.locator(`${TILE}[aria-label*="${openedFilename}"]`).click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect.poll(shownId, 'reopened on the opened entry').toBe(opened);
		expect(headArrival404, 'reopening the opened entry does not probe the arrival').toBe(0);

		// ARROW to `arrival`. U3 forces one no-store HEAD of the ARRIVAL; against the
		// server-deleted row it 404s DESPITE the primed cacheable 200 (the cache-bypass
		// proof). The 404 is a deletion by another door: the row tombstones and the
		// surface ADVANCES to the entry that followed it — two survivors remain, so it
		// does not close, and it is never a live-looking stale row.
		//
		// ARM the 404 response wait BEFORE the arrow so the observed 404 is causally the
		// ARROW's probe, not a straggler (Codex round 3): armed after the delete + reopen
		// and immediately before the keypress, it can only catch a HEAD 404 issued by the
		// navigation that follows. The `headArrival404 === 0` guard above confirms none
		// preceded it.
		const arrowProbe404 = page.waitForResponse(
			(r) => {
				const rq = r.request();
				if (rq.method() !== 'HEAD' || r.status() !== 404) return false;
				const u = new URL(rq.url());
				return u.search === '' && u.pathname === attPath(arrival);
			},
			{ timeout: 10_000 }
		);
		await page.keyboard.press('ArrowRight');
		await arrowProbe404;
		// The advance. Shown is `advanceTarget` (the arrival's follower), the viewer
		// stays open, the counter recounts the two survivors, and no missing overlay
		// shows (that is the single-item path, not a multi-set advance). `advanceTarget`
		// is DISTINCT from `opened` and from the gone `arrival`, so BOTH failure modes
		// fall here: a lost per-navigation probe (the arrival trusted from its complete
		// seed, shown live) OR a cache-respecting probe (the primed 200 replayed, shown
		// live).
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect.poll(shownId, 'tombstone-advances to the arrival’s follower').toBe(advanceTarget);
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('2 / 2');
		await expect(page.locator(VIEWER_MISSING)).toHaveCount(0);
	});

	test('restore revalidates the strip through the bulk SSE path: no list on archive, a fresh list after restore, tiles never blank (PLAN-2392 3c-iii U5, U2 seam)', async ({
		page,
		fixture,
		request
	}) => {
		// THE U2 BROWSER PROOF. The strip has NO SSE subscription of its own — its
		// only live inputs are the in-process delete/upload buses — so a restore that
		// happened while this browser was elsewhere never reaches it without the
		// `parentArchived` prop edge ItemDetail threads down. The RESTORE (the direction
		// that actually fires `revalidateAfterRestore`) is driven through the BULK
		// endpoint: `items_bulk_updated` carries NO `item_id` and routes only to
		// `sync_required`, so it proves the PROP-driven mechanism covers exactly what a
		// per-item SSE subscription would miss. The archive uses the per-item event —
		// its only role here is to establish the archived level, and a reliable banner
		// keeps the no-op assertion honest.
		//
		// The list-request choreography is three-step (round-9): ARCHIVE fires no
		// `attachments.list` (the content no-op), then baseline the count immediately
		// BEFORE the restore, then a NEW list must fire AFTER it. Baselining before the
		// archive would let an incorrect archive-triggered reload false-satisfy the
		// after-restore assertion.
		//
		// ISOLATED WORKSPACE: the `sync_required` → `/changes` delta path this leg
		// depends on is starved on the shared suite workspace — every other parallel
		// spec's mutation rides the same SSE stream, and a competing sync landing in the
		// same second advances the RFC3339-second `/changes` cursor past this restore,
		// dropping it (existing archived tests avoid the sync path for exactly this
		// reason, using per-item events). A private workspace gives this leg an SSE
		// stream nothing else touches.
		await browserLogin(page);
		// `ws` is captured OUTSIDE the try so the finally can always reclaim it; the
		// try opens immediately after so a failure during doc/upload setup still cleans
		// up (Codex round 2).
		const ws = (await createWorkspace(fixture, request, 'U5 restore revalidate')).slug;
		try {
			const doc = await createDoc(fixture, request, ws, `U5 restore ${Date.now()}`);
			await uploadAttachment(fixture, request, doc.id, 'restore-a.png', 'image/png', REAL_PNG, ws);
			await uploadAttachment(fixture, request, doc.id, 'restore-b.png', 'image/png', REAL_PNG, ws);

			// Count the STRIP's own list GETs — the item-scoped `attachments.list`
			// (`?...item_id=<doc.id>`), never an unscoped page-1 listing (BUG-2504
			// discipline). A GET of this workspace's attachments carrying THIS item's id.
			// Used only for the ARCHIVE no-op (a dispatch count is right for "prove
			// nothing fired"); the restore is proven by a settled RESPONSE below.
			const listPath = `/api/v1/workspaces/${ws}/attachments`;
			const isItemListReq = (u: URL) =>
				u.pathname === listPath && u.searchParams.get('item_id') === doc.id;
			const isItemListResp = (r: import('@playwright/test').Response) =>
				r.request().method() === 'GET' && isItemListReq(new URL(r.url())) && r.status() === 200;
			let listCalls = 0;
			page.on('request', (r) => {
				if (r.method() === 'GET' && isItemListReq(new URL(r.url()))) listCalls += 1;
			});

			// Arm the mount-load response BEFORE navigating so the settle is deterministic
			// (no fixed-time sleep): the strip's first item-scoped list must land 200.
			const mountList = page.waitForResponse(isItemListResp);
			await page.goto(`/${fixture.adminUsername}/${ws}/docs/${doc.slug}`);
			await expect(page.locator(TILE)).toHaveCount(2);
			await mountList;
			const afterMount = listCalls;

			// ARCHIVE (per-item — a reliable banner; the archive is a strip content
			// no-op either way). ItemDetail flips `parentArchived=true`; the strip's
			// archive edge keeps the painted tiles and fires NO list.
			await archiveItem(fixture, request, doc.slug, ws);
			await expect(page.locator('.archived-banner')).toBeVisible({ timeout: 15_000 });
			// Tiles are NOT blanked by the archive.
			await expect(page.locator(TILE)).toHaveCount(2);
			// Give any erroneous archive-triggered reload a window to fire, then assert
			// it did not: the archive is a strip content no-op. A bounded wait is the
			// right shape for proving ABSENCE — there is nothing to poll toward.
			await page.waitForTimeout(500);
			expect(listCalls, 'archiving fires no attachments.list — the content no-op').toBe(afterMount);

			// Baseline immediately BEFORE the restore (round-9): the after-restore delta
			// is measured from here, so an archive-time reload (were there one) could not
			// pre-satisfy it.
			const beforeRestore = listCalls;

			// DELAY the restore's revalidation list so its IN-FLIGHT window is observable
			// (Codex round 3): "never blank" is a claim about the interval WHILE the list
			// is fetching, so hold that response open and assert the tiles stay painted
			// during it. A blank-then-refill implementation would blank at fetch start and
			// be caught here; a post-settle count alone could not tell the two apart. The
			// route is registered only now — AFTER the mount load — and one-shot
			// (`times: 1`), so it delays ONLY the restore's revalidation list.
			let listHeld = false;
			await page.route(
				(u) => u.pathname === listPath && u.searchParams.get('item_id') === doc.id,
				async (route) => {
					listHeld = true; // the revalidation list reached the wire (held in flight)
					await new Promise((r) => setTimeout(r, 1200));
					await route.continue();
				},
				{ times: 1 }
			);
			// Arm the settled-200 wait BEFORE firing so what we assert is causally the
			// restore's own list (Codex round 1), not a bare dispatch a failed/unrelated
			// GET could satisfy.
			const restoreList = page.waitForResponse(isItemListResp, { timeout: 15_000 });
			// RESTORE through the BULK lane — `items_bulk_updated`, no `item_id`.
			// `parentArchived` flips true→false and the strip's restore edge REVALIDATES.
			await bulkItems(fixture, request, 'restore', [doc.slug], ws);
			// The restore reaches the page through the bulk SSE → deltaSync round-trip,
			// which is slower than a per-item event — give it room past the 5s default.
			await expect(page.locator('.archived-banner')).toHaveCount(0, { timeout: 15_000 });
			// The revalidation list has reached the wire and is HELD in flight by the
			// route. Prove the strip did NOT blank while it fetches — the U2 contract.
			await expect.poll(() => listHeld, 'the restore dispatched the revalidation list').toBe(true);
			await expect(page.locator(TILE)).toHaveCount(2);
			// Release + settle: the held response lands 200 and the merge keeps the tiles.
			await restoreList;
			await expect(page.locator(TILE)).toHaveCount(2);
			// Corroborate against the dispatch counter: exactly the restore moved it off
			// the pre-restore baseline (the archive did not).
			expect(listCalls, 'the restore fired a fresh list; the archive did not').toBeGreaterThan(
				beforeRestore
			);
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});

	test('deleting a strip attachment reconciles its comment thumbnail to the missing presentation, live, no reload (PLAN-2392 3c-iii U5, U1 seam)', async ({
		page,
		fixture,
		request
	}) => {
		// THE U1 BROWSER PROOF. The deletion bus is process-local (`events.ts`), so the
		// delete MUST go through the same-page strip UI for `announceAttachmentDeleted`
		// to run — a direct API delete exercises nothing U1 built. The timeline's
		// deletion listener then drops the cached metadata + probe mark and tombstones
		// the id, so the comment's HTML re-renders the reference through the missing
		// path — a live `<img>` becoming the `.attachment-missing` placeholder with no
		// reload.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'U5 timeline reconcile');
		// One attachment, bound to the item (so it lands in the strip — the delete
		// surface) AND referenced by a comment (so the timeline renders its thumbnail).
		const att = await uploadAttachment(fixture, request, doc.id, 'tl-shot.png');
		await postComment(fixture, request, doc.slug, `shot\n\n![shot](pad-attachment:${att})\n`);

		await page.goto(itemUrl(fixture, doc.slug));

		// Comments live under the item CONTENT on the Details tab (IDEA-2843);
		// they used to be behind the Activity tab and this opened it. Confirm the
		// comment thumbnail resolved to a LIVE image first — the baseline the
		// reconciliation flips away from. (The tab-panels are CSS-hidden, not
		// `{#if}`-gated, so the strip and the timeline stay MOUNTED across a
		// switch — the timeline instance never remounts, which is what makes the
		// reconciliation "live". That property is exercised at the end of the
		// test, which still round-trips through another tab.)
		const tlImg = `#item-comments img[data-attachment-id="${att}"]`;
		const tlMissing = `#item-comments .attachment-missing[data-attachment-id="${att}"]`;
		await expect(page.locator('#item-comments .timeline')).toBeVisible();
		await expect(page.locator(tlImg)).toBeVisible();
		await expect(page.locator(tlMissing)).toHaveCount(0);

		// Stamp the document AND the timeline element: a full reload clears the window
		// stamp, and a subtree remount replaces the stamped `.timeline` node. Both
		// surviving proves the flip is the live bus reconciliation of the SAME mounted
		// instance, not a reload or a remount.
		await page.evaluate(() => {
			(window as unknown as { __spa?: number }).__spa = 1;
			const el = document.querySelector('.timeline') as (HTMLElement & { __tl?: number }) | null;
			if (el) el.__tl = 2;
		});

		// Delete the attachment through the STRIP UI. The strip and the comments
		// now share the Details tab (IDEA-2843), so this click is a no-op where it
		// used to be a switch away from Activity; kept because the test must be
		// ON Details for the strip's `.att-delete` to be clickable, whatever tab
		// a future edit leaves us on.
		await page.getByRole('tab', { name: 'Details' }).click();
		// The delete control is CSS-revealed on hover / focus-within and sits over the
		// tile, so focus it first (keyboard-reachable by contract) before clicking —
		// the same handoff `item-attachment-strip.spec.ts` uses.
		const del = page.locator(STRIP_DELETE).first();
		await del.focus();
		await expect(del).toBeFocused();
		await del.click();
		const confirmMenu = page.locator('[role="menu"]');
		await expect(confirmMenu).toBeVisible();
		await confirmMenu.getByRole('menuitem', { name: 'Delete file' }).click();
		// The strip tile goes (the delete committed and reconciled its own surface).
		await expect(page.locator(TILE)).toHaveCount(0);

		// The mounted-but-hidden timeline already reconciled off the bus — its comment
		// thumbnail is now the missing placeholder and the live `<img>` is gone, with
		// NO tab switch and NO reload in between. Query the hidden panel directly (count
		// assertions do not require visibility), then reveal it.
		await expect(page.locator(tlMissing)).toHaveCount(1);
		await expect(page.locator(tlImg)).toHaveCount(0);
		expect(
			await page.evaluate(() => {
				const el = document.querySelector('.timeline') as (HTMLElement & { __tl?: number }) | null;
				return (window as unknown as { __spa?: number }).__spa === 1 && el?.__tl === 2;
			}),
			'the reconciliation is the live bus reaching the mounted timeline — not a reload or a remount'
		).toBe(true);

		// And it renders correctly after a tab round-trip. Comments are on
		// Details now, so the switch that proves the panel is CSS-hidden rather
		// than unmounted has to go OUT and BACK — leaving on Activity would
		// assert against a hidden panel and pass for the wrong reason.
		await page.getByRole('tab', { name: 'Activity' }).click();
		await page.getByRole('tab', { name: 'Details' }).click();
		await expect(page.locator(tlMissing)).toBeVisible();
	});
});
