import { test, expect } from './fixtures';
import type { Page, Request } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	BIG_PNG,
	DESKTOP,
	REAL_PDF,
	REAL_PNG,
	REAL_ZIP,
	TILE,
	VIEWER,
	VIEWER_IMAGE,
	VIEWER_COUNTER,
	VIEWER_FALLBACK,
	VIEWER_FALLBACK_NAME,
	VIEWER_FALLBACK_NOTE,
	VIEWER_MISSING,
	VIEWER_TOOLBAR,
	VIEWER_META,
	VIEWER_META_NAME,
	VIEWER_META_DETAIL,
	VIEWER_STAGE,
	dropFileIntoEditor,
	imageRect,
	itemUrl,
	postComment,
	uploadAttachment,
	viewerDialog,
	viewerOpenAnchor,
	viewerDownloadAnchor,
	viewerCopyLink,
	viewerDelete,
	viewerConfirmDeleteRow,
	viewerConfirmCancelRow,
	renderedScale
} from './lib/attachment-viewer';

/**
 * 3c-i SURFACE CHROME — the browser proof (TASK-2484, DR-9).
 *
 * The A-E chain (2473-2477) built the viewer's toolbar, metadata header, icon
 * fallback and deletion reconciliation. DR-9's rule: the a11y and interaction
 * work is verified in a browser or it is not verified. jsdom cannot show the
 * anchor semantics a real `<a download>` carries, the inert-label pointer
 * behaviour, the no-bytes network invariant, or the peek permission threading
 * through the real ItemDetail — so those legs live here. Desktop-chromium only:
 * the sheet layout has no mobile e2e until 3c-ii.
 */

const INLINE_IMG = '.editor-content .ProseMirror img[data-attachment-id]';

/**
 * Comments (and their inline images) live under the item CONTENT on the
 * Details tab — IDEA-2843 moved them out of the Activity tab, which now
 * carries changes and versions only. Details is the tab a pane opens on, so
 * there is nothing to click; the wait is what the tab click used to provide.
 */
async function openCommentsSurface(page: Page): Promise<void> {
	await expect(page.locator('#item-comments .timeline')).toBeVisible();
}

test.describe('attachment viewer — 3c-i surface chrome (TASK-2484)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop legs only');
		await page.setViewportSize(DESKTOP);
	});

	test('toolbar renders on the STRIP origin, with real Open/Download anchors', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Chrome strip toolbar');
		const id = await uploadAttachment(fixture, request, doc.id, 'strip-tool.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		// The toolbar is present, drawn over the stage.
		await expect(page.locator(VIEWER_TOOLBAR)).toBeVisible();

		// Open and Download are REAL anchors (role "link"), carrying the attachment
		// href — the DR-16 semantics jsdom cannot render (an <a download> saves; a
		// button would not).
		// The EXACT canonical, variant-less URL — not merely "contains the id", so a
		// wrong variant / query or a truncated path is caught.
		const canonical = new RegExp(
			`^/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${id}$`
		);
		const open = viewerOpenAnchor(page);
		await expect(open).toHaveAttribute('href', canonical);
		await expect(open).toHaveAttribute('target', '_blank');
		const download = viewerDownloadAnchor(page);
		await expect(download).toHaveAttribute('href', canonical);
		// The EXACT filename, so a `download=""` regression (which reopens the
		// navigate-instead-of-save hole) is caught.
		await expect(download).toHaveAttribute('download', 'strip-tool.png');

		// Copy-link is a button (JS action), not an anchor.
		await expect(viewerCopyLink(page)).toBeVisible();
		// The admin owns the item and is not peeking → Delete is offered.
		await expect(viewerDelete(page)).toBeVisible();
	});

	test('toolbar renders on the TIMELINE origin', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Chrome timeline toolbar');
		const one = await uploadAttachment(fixture, request, doc.id, 'tl-tool.png');
		await postComment(fixture, request, doc.slug, `shot\n\n![first](pad-attachment:${one})\n`);
		await page.goto(itemUrl(fixture, doc.slug));
		await openCommentsSurface(page);
		await page.locator('.timeline img[data-attachment-id]').first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect(page.locator(VIEWER_TOOLBAR)).toBeVisible();
		const tlCanonical = new RegExp(
			`^/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${one}$`
		);
		await expect(viewerOpenAnchor(page)).toHaveAttribute('href', tlCanonical);
		await expect(viewerDownloadAnchor(page)).toHaveAttribute('href', tlCanonical);
	});

	test('toolbar renders on the BODY NodeView origin', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Chrome body toolbar');
		await page.goto(itemUrl(fixture, doc.slug));
		await dropFileIntoEditor(page, 'body-tool.png', REAL_PNG.toString('base64'));
		await expect(page.locator(INLINE_IMG)).toHaveCount(1);
		await page.locator(INLINE_IMG).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect(page.locator(VIEWER_TOOLBAR)).toBeVisible();
		// The drop's attachment id isn't captured here, so pin the Download anchor to
		// the SHOWN image: both resolve the same attachment, so its download href must
		// name the same id as the <img> src.
		const shownId = (await page.locator(VIEWER_IMAGE).getAttribute('src'))!.match(
			/attachments\/([0-9a-f-]+)/
		)![1];
		const bodyCanonical = new RegExp(
			`^/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${shownId}$`
		);
		await expect(viewerOpenAnchor(page)).toHaveAttribute('href', bodyCanonical);
		await expect(viewerDownloadAnchor(page)).toHaveAttribute('href', bodyCanonical);
	});

	test('the peeked side withholds the delete affordance; the active side viewer offers Delete', async ({
		page,
		fixture,
		request
	}) => {
		// The browser proof of C1's permission threading (TASK-2474). SCOPE, stated
		// honestly: a viewer OPENED from a peeked side cannot be observed here — the
		// only way to open one is a content click on that side, and the
		// invisible-freeze model (BUG-2263) RE-ACTIVATES a side on any such click
		// (deterministically un-peeking it before the viewer captures
		// mutationsEnabled). So the peeked-side no-Delete VIEWER is jsdom-proven
		// (TASK-2474's timeline/viewer-host peek legs); what a browser proves is the
		// two ends this rests on: (a) mutationsEnabled=false really reaches a peeked
		// side (its delete affordance is gone), and (b) an ACTIVE side's viewer
		// toolbar really offers Delete. Same fixture, opposite permission.
		await browserLogin(page);
		const master = await seedDoc(fixture, request, 'Chrome peek master');
		const related = await seedDoc(fixture, request, 'Chrome peek related');
		await uploadAttachment(fixture, request, master.id, 'master-img.png');
		await uploadAttachment(fixture, request, related.id, 'pane-img.png');

		// Clicking INTO the pane freezes the MASTER into the peeking state — the
		// reliable direction the strip spec drives.
		await page.goto(`${itemUrl(fixture, master.slug)}?item=${encodeURIComponent(related.slug)}`);
		const pane = page.locator('.item-pane');
		const masterHost = page.locator('.item-page-host > .item-page');
		await expect(pane).toBeVisible();
		await pane.locator('.editor-wrapper .ProseMirror').first().click();
		await expect(masterHost.locator('.editor-wrapper .ProseMirror').first()).toHaveAttribute(
			'contenteditable',
			'false'
		);
		// (a) The peeked master's strip still SHOWS its tiles (a read affordance) but
		// its delete control is GONE — mutationsEnabled=false reached the side. This
		// is the wiring that regresses silently if ItemDetail passes raw canEdit.
		await expect(masterHost.locator(TILE)).toHaveCount(1);
		await expect(masterHost.locator('.att-delete')).toHaveCount(0);

		// (b) The ACTIVE pane's viewer toolbar OFFERS Delete.
		await pane.locator(TILE).first().click();
		await expect(page.locator(VIEWER_TOOLBAR)).toBeVisible();
		await expect(viewerDelete(page)).toBeVisible();
	});

	test('toolbar Delete → drill-down (keyboard) → confirm advances, and the strip tile goes', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Chrome viewer delete');
		const a = await uploadAttachment(fixture, request, doc.id, 'del-a.png');
		await uploadAttachment(fixture, request, doc.id, 'del-b.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);
		// Open del-a specifically (by name — the list order is created_at DESC, so its
		// POSITION isn't fixed). The counter shows "· / 2" (two in the set); which
		// index del-a lands on is incidental.
		await page.locator(`${TILE}[aria-label*="del-a.png"]`).click();
		await expect(page.locator(VIEWER_COUNTER)).toHaveText(/^[12] \/ 2$/);
		await expect(page.locator(VIEWER_IMAGE)).toHaveAttribute('src', new RegExp(`/attachments/${a}`));

		// Open the drill-down and reach the confirm rows BY KEYBOARD. Focus Delete,
		// Enter opens it, focus enters the menu on the first row (Cancel).
		await viewerDelete(page).focus();
		await page.keyboard.press('Enter');
		const cancel = viewerConfirmCancelRow(page);
		const del = viewerConfirmDeleteRow(page);
		await expect(cancel).toBeVisible();
		await expect(cancel).toBeFocused();
		// ROVING TABINDEX: exactly the active row is the tab stop (0), the rest -1 —
		// so Tab exits the menu and Up/Down move between the rows (TASK-2477).
		await expect(cancel).toHaveAttribute('tabindex', '0');
		await expect(del).toHaveAttribute('tabindex', '-1');
		await page.keyboard.press('ArrowDown');
		await expect(del).toBeFocused();
		await expect(del).toHaveAttribute('tabindex', '0');
		await expect(cancel).toHaveAttribute('tabindex', '-1');
		// Confirm by KEYBOARD (Enter), not a click — the drill-down is keyboard-usable
		// end to end.
		await page.keyboard.press('Enter');

		// The viewer ADVANCES to the survivor (del-b), not closes — the retired C1
		// latch — and the deleted tile disappears from the strip (bus reconciliation).
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(page.locator(VIEWER_IMAGE)).toHaveAttribute('src', new RegExp('/attachments/'));
		await expect(page.locator(VIEWER_IMAGE)).not.toHaveAttribute('src', new RegExp(`/attachments/${a}`));
		await expect(page.locator(`${TILE}[aria-label*="del-a.png"]`)).toHaveCount(0);
		await expect(page.locator(TILE)).toHaveCount(1);
	});

	test('the metadata header shows name/type/size, ellipsizes a long name, and is inert to a press', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Chrome header');
		const long = `${'x'.repeat(180)}.png`;
		// BIG_PNG (1600×1200) OVERFLOWS the stage, so zooming makes a real pan
		// possible — a smaller image stays inside the stage and clampPan pins the pan
		// to 0, which would hide a missing header exclusion (Codex round 2).
		await uploadAttachment(fixture, request, doc.id, long, 'image/png', BIG_PNG);
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		// Filename + a type · size detail line.
		const name = page.locator(VIEWER_META_NAME);
		await expect(name).toHaveText(long);
		await expect(page.locator(VIEWER_META_DETAIL)).toContainText('·'); // type · size
		// DR-13: the FULL value is in title, and the box CLIPS WITH AN ELLIPSIS — the
		// resolved style, not just overflow (a clip without `text-overflow: ellipsis`
		// would still overflow but drop the "…", which jsdom cannot show).
		await expect(name).toHaveAttribute('title', long);
		const clip = await name.evaluate((el) => {
			const cs = getComputedStyle(el);
			return {
				clipped: el.scrollWidth > el.clientWidth + 1,
				textOverflow: cs.textOverflow,
				whiteSpace: cs.whiteSpace,
				overflow: cs.overflow
			};
		});
		expect(clip.clipped, 'the long filename must overflow its clipped box').toBe(true);
		expect(clip.textOverflow).toBe('ellipsis');
		expect(clip.whiteSpace).toBe('nowrap');
		expect(clip.overflow).toBe('hidden');

		// The inert-label contract. ZOOM IN so a pan is actually possible, and wait for
		// the 150ms transform TRANSITION to SETTLE (two equal scale reads) before
		// sampling — otherwise a mid-transition rect makes the comparison flaky.
		await page.keyboard.press('+');
		await page.keyboard.press('+');
		let lastScale = NaN;
		await expect
			.poll(async () => {
				const s = await renderedScale(page);
				const stable = s > 1 && s === lastScale;
				lastScale = s;
				return stable;
			})
			.toBe(true);
		const rectBefore = await imageRect(page);
		// A DRAG starting ON the header must not pan the image; the on-screen image
		// rect stays put (a missing exclusion would move it).
		const metaBox = (await page.locator(VIEWER_META).boundingBox())!;
		await page.mouse.move(metaBox.x + 8, metaBox.y + metaBox.height / 2);
		await page.mouse.down();
		await page.mouse.move(metaBox.x + 220, metaBox.y + metaBox.height / 2, { steps: 6 });
		await page.mouse.up();
		const rectAfter = await imageRect(page);
		expect(Math.abs(rectAfter.x - rectBefore.x), 'a drag on the header did not pan (x)').toBeLessThan(2);
		expect(Math.abs(rectAfter.y - rectBefore.y), 'a drag on the header did not pan (y)').toBeLessThan(2);
		await expect(page.locator(VIEWER)).toHaveCount(1); // and it did not dismiss
	});

	test('a PDF and a ZIP open the fallback arm through a REAL producer — Open for the PDF, none for the ZIP (PLAN-2392 3c-ii)', async ({
		page,
		fixture,
		request
	}) => {
		// CLOSES THE 3c-i SCOPING NOTE. Under 3c-i the fallback arm was unreachable
		// through a real producer — the producers filtered `canOpenInViewer` before
		// emitting, so only images ever reached the viewer, and the arm was
		// jsdom-only (a mid-view MIME flip driven by a prop write). 3c-ii converges
		// the panel and the viewer: a file tile now EMITS on the surface channel, so
		// a real PDF and a real ZIP open the grown `Lightbox` and draw the no-bytes
		// fallback arm. This is the browser proof of ADMISSION (a non-raster row is
		// navigable, not dropped) and the ARM (no <img>, no bytes) together, plus the
		// per-type toolbar: Open for a browser-previewable PDF, NO Open for a ZIP.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Fallback integration');
		// Real magic bytes so the server's sniff-against-allowlist accepts them and
		// stores the canonical MIME (the multipart Content-Type is not trusted).
		await uploadAttachment(fixture, request, doc.id, 'report.pdf', 'application/pdf', REAL_PDF);
		await uploadAttachment(fixture, request, doc.id, 'logs.zip', 'application/zip', REAL_ZIP);
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);

		// ── The PDF: fallback arm, honest note, and Open IS offered ──
		await page.locator(`${TILE}[aria-label*="report.pdf"]`).click();
		await expect(viewerDialog(page, 'report.pdf')).toHaveCount(1);
		await expect(page.locator(VIEWER_FALLBACK)).toBeVisible();
		await expect(page.locator(VIEWER_FALLBACK_NAME)).toHaveText('report.pdf');
		await expect(page.locator(VIEWER_FALLBACK_NOTE)).toHaveText('No preview available');
		// NO BYTES: the raster arm never mounted, so there is no <img> on the stage.
		await expect(page.locator(VIEWER_IMAGE)).toHaveCount(0);
		// A PDF is browser-previewable, so Open in new tab is offered — and Download
		// always is, carrying the filename.
		await expect(viewerOpenAnchor(page)).toBeVisible();
		await expect(viewerDownloadAnchor(page)).toHaveAttribute('download', 'report.pdf');
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// ── The ZIP: same fallback arm, but Open is ABSENT (not previewable) ──
		await page.locator(`${TILE}[aria-label*="logs.zip"]`).click();
		await expect(viewerDialog(page, 'logs.zip')).toHaveCount(1);
		await expect(page.locator(VIEWER_FALLBACK)).toBeVisible();
		await expect(page.locator(VIEWER_FALLBACK_NAME)).toHaveText('logs.zip');
		await expect(page.locator(VIEWER_FALLBACK_NOTE)).toHaveText('No preview available');
		await expect(page.locator(VIEWER_IMAGE)).toHaveCount(0);
		// A ZIP is NOT browser-previewable — the Open action does not apply, so the
		// anchor is absent entirely (not merely disabled). Download still applies.
		await expect(viewerOpenAnchor(page)).toHaveCount(0);
		await expect(viewerDownloadAnchor(page)).toHaveAttribute('download', 'logs.zip');
	});

	test('every open AND every navigation step forces one no-store HEAD per (open, entry); a reopen forces another (PLAN-2392 3c-iii U3)', async ({
		page,
		fixture,
		request
	}) => {
		// 3c-iii U3 generalizes T6's always-revalidate-on-open to per navigation step,
		// in a browser. The strip's list rows carry a COMPLETE seed (filename + MIME +
		// size), so before T6 a strip open issued ZERO metadata HEADs. T6 forced one
		// `no-store` HEAD of the OPENED entry; U3 forces one per (open, attachment)
		// pair, so ARROWING to a not-yet-probed sibling revalidates it too — catching a
		// cross-tab / background delete the browser's `max-age` HEAD cache would hide.
		// Observable as the HEAD count: one on the open, one MORE when arrowing to a
		// fresh entry, and a fresh probe on a reopen (a fresh nonce). (The complementary
		// "arrowing BACK to an already-probed pair does NOT re-probe" is proven
		// deterministically in surfaceMetadata.svelte.test.ts — it turns on a JS-internal
		// mark with no race-free browser observable, so it is not asserted here.)
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'No-store reopen');
		const a = await uploadAttachment(fixture, request, doc.id, 'ns-a.png');
		const b = await uploadAttachment(fixture, request, doc.id, 'ns-b.png');

		// Count HEADs per attachment id. The image bytes are GETs (thumb-sm for the
		// tiles, thumb-md/original for the viewer); the metadata existence probe is a
		// HEAD of the EXACT variant-less URL, so match the pathname with a `$` anchor
		// (no `?variant`) — never a query-bearing URL.
		// The EXACT canonical API path per id — a full-path compare, not a suffix, so a
		// different workspace or a prefixed route can never be miscounted.
		const attPath = (id: string) => `/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${id}`;
		const isCanonicalHead = (r: Request) => {
			if (r.method() !== 'HEAD') return null;
			const u = new URL(r.url());
			// EXACT variant-less canonical URL: no query at all (a `?variant=` HEAD, were
			// one ever issued, must not be miscounted as the canonical existence probe).
			if (u.search !== '') return null;
			for (const id of [a, b]) if (u.pathname === attPath(id)) return id;
			return null;
		};
		// `heads` counts DISPATCHED probes; `headsDone` counts COMPLETED ones. The
		// count barriers below poll on COMPLETION rather than dispatch so every earlier
		// probe has fully landed before the next exact count is read — a settled-state
		// sync point, not a timing crutch.
		//
		// COMPLETION is keyed on the RESPONSE, not `requestfinished` (PLAN-2392 3c-iii
		// U5): a bodyless HEAD gets its 200 headers and is then reported by Chromium as
		// `net::ERR_ABORTED` — it fires `requestfailed`, NOT `requestfinished`, so a
		// `requestfinished` barrier for a HEAD never completes. The `response` event
		// fires with the arrived status, which is the true settle point for a HEAD.
		const heads: Record<string, number> = { [a]: 0, [b]: 0 };
		const headsDone: Record<string, number> = { [a]: 0, [b]: 0 };
		page.on('request', (r) => {
			const id = isCanonicalHead(r);
			if (id) heads[id] += 1;
		});
		page.on('response', (r) => {
			const id = isCanonicalHead(r.request());
			if (id) headsDone[id] += 1;
		});

		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);
		// BASELINE, before any open: the strip lists via GET, never a metadata HEAD,
		// so no forced probe has fired yet. This makes the post-open count a proven
		// DELTA of the open, not a pre-existing HEAD the open happens to inherit.
		expect(heads[a], 'no HEAD before the first open').toBe(0);
		expect(heads[b]).toBe(0);

		// OPEN ns-a: exactly one forced HEAD of ns-a, and none of ns-b (only the
		// opened entry is probed at open, not the whole set). Barrier on COMPLETION so
		// every earlier probe has landed before the next count is read.
		await page.locator(`${TILE}[aria-label*="ns-a.png"]`).click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect.poll(() => headsDone[a], 'ns-a open probe completes').toBe(1);
		expect(heads[a], 'the opened entry gets one no-store HEAD').toBe(1);
		expect(heads[b], 'a non-opened sibling is NOT probed at open').toBe(0);

		// ARROW to ns-b within the SAME open: THE U3 INVERSION. T6 forced only the
		// opened entry, so arrowing here forced NOTHING and ns-b stayed at 0; U3 forces
		// one no-store HEAD of the ARRIVAL (a fresh (nonce, entry) pair), so ns-b is now
		// probed exactly once by the navigation step. ns-a is untouched by the arrow —
		// it stays at 1, quiescent (no arrow-back is issued, so no count can transiently
		// inflate; the reopen below is the only thing that moves ns-a again).
		await page.keyboard.press('ArrowRight');
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('2 / 2');
		await expect.poll(() => headsDone[b], 'arrowing to a fresh entry forces one no-store HEAD (U3)').toBe(1);
		expect(heads[b]).toBe(1);
		expect(heads[a], 'the arrow does not re-probe the departed entry').toBe(1);
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// REOPEN ns-a: a fresh open mints a fresh nonce → every entry is unseen again,
		// so ns-a is force-probed once more, reaching exactly 2 (its two opens). ns-a
		// climbs 1→2 with no intervening probe, so this exact poll cannot false-green on
		// a transient. ns-b remains at its single arrow-forced probe.
		await page.locator(`${TILE}[aria-label*="ns-a.png"]`).click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect
			.poll(() => `${heads[a]},${heads[b]}`, 'exactly {a:2, b:1} — reopen re-forces ns-a; the arrow forced ns-b once')
			.toBe('2,1');
	});

	test('an archived-parent open is PROBE-GATED — the missing overlay, not a live surface (PLAN-2392 3c-ii DR-14)', async ({
		page,
		fixture,
		request
	}) => {
		// THE ARCHIVED-AT-OPEN GATE. An archived parent makes attachment reads 404,
		// so a complete strip seed (mime + size) is NOT evidence the bytes are
		// REACHABLE (DR-14). The host threads `parentArchived` to the surface, which
		// forces the metadata machine to PROBE rather than trust the seed; the probe
		// 404s and the single-item surface settles on the inert "no longer available"
		// missing overlay instead of a live image + working toolbar. Remove the gate
		// (the host stops threading `parentArchived`) and the seed is trusted: no
		// probe, so the raster arm loads bytes that 404 into the generic image-error
		// state — never this overlay. That is the mutation this leg catches.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Archived at open');
		const attId = await uploadAttachment(fixture, request, doc.id, 'archived-shot.png');
		// Record whether a HEAD probe of THIS attachment came back 404 — the
		// reachability probe the gate forces. This is the request-level proof that
		// the overlay is reached THROUGH the 404 probe, not incidentally.
		const attPath = `/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${attId}`;
		let head404 = 0;
		page.on('response', (r) => {
			const rq = r.request();
			if (rq.method() !== 'HEAD' || r.status() !== 404) return;
			const u = new URL(rq.url());
			// EXACT variant-less canonical URL of THIS attachment — the existence probe.
			if (u.search === '' && u.pathname === attPath) head404 += 1;
		});
		await page.goto(itemUrl(fixture, doc.slug));
		const tile = page.locator(TILE).first();
		await expect(tile).toBeVisible();

		// Archive (soft-delete) the parent from under the loaded page. The item GET
		// still returns it (deleted_at set), so the page stays up with its banner and
		// its already-loaded strip tile — this is the archived-AT-open path: the next
		// open mounts the surface with `parentArchived` already true.
		const del = await request.delete(
			`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.slug}`,
			{ headers: { Authorization: `Bearer ${fixture.apiToken}` } }
		);
		expect(del.ok(), await del.text()).toBe(true);
		// Wait for the archive to REACH the page (the SSE `item_archived` flip), so
		// `parentArchived` is true before the open — otherwise the assertion is about
		// a live open that raced the archive.
		await expect(page.locator('.archived-banner')).toBeVisible();

		// Open the surface. `parentArchived` is true, so the probe fires and 404s.
		// Baseline the cumulative 404 counter so the assertion below is a proven
		// DELTA of THIS open, not a probe that happened earlier in the test.
		const before404 = head404;
		await tile.click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		// BARRIER FIRST — the request-level proof: a 404 HEAD of this exact attachment
		// (the archived-parent reachability probe) fired on THIS open. Asserting it
		// BEFORE the overlay makes the overlay a consequence of the observed 404.
		await expect.poll(() => head404, 'a 404 HEAD probe fired on this open').toBeGreaterThan(before404);
		// The gate's tell: the single-item MISSING overlay, announced, with its
		// "no longer available" copy — reached ONLY because the forced probe 404'd.
		await expect(page.locator(VIEWER_MISSING)).toBeVisible();
		await expect(page.locator(VIEWER_MISSING)).toContainText('no longer available');
		// No bytes, and no generic image-error arm — the probe pre-empted the load.
		await expect(page.locator(VIEWER_IMAGE)).toHaveCount(0);
		await expect(page.locator(`${VIEWER} .lightbox-error`)).toHaveCount(0);
		// EVERY toolbar action is inert (not just Open): the missing state drops both
		// anchors' hrefs (so neither is a link), disables Copy-link, and Delete is
		// absent (an archived parent withholds mutations).
		await expect(viewerOpenAnchor(page), 'Open href dropped').toHaveCount(0);
		await expect(viewerDownloadAnchor(page), 'Download href dropped').toHaveCount(0);
		await expect(viewerCopyLink(page), 'Copy-link disabled').toBeDisabled();
		await expect(viewerDelete(page), 'Delete withheld on an archived parent').toHaveCount(0);
	});

	test('archiving the parent while the surface is OPEN closes it (PLAN-2392 3c-ii DR-14, host lifecycle gate)', async ({
		page,
		fixture,
		request
	}) => {
		// The archive-WHILE-OPEN half of the DR-14 parent lifecycle gate, which is
		// the host's own — distinct from the metadata `missing` path (T6's forced
		// probe reaches that too). An archived parent makes every open toolbar action
		// 404, so the host CLOSES an open surface on the `parentArchived` false→true
		// transition rather than leaving a live-looking toolbar over dead bytes.
		// Break the host's transition-close and the surface lingers — the mutation
		// this leg catches (the missing-overlay legs above cannot: they are satisfied
		// by the metadata probe regardless of the host gate).
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Archive while open');
		await uploadAttachment(fixture, request, doc.id, 'live-then-archived.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		// Stamp BOTH the document AND the item-page element: a full reload clears the
		// window stamp, and a subtree remount (ItemDetail re-created) replaces the
		// stamped `.item-page` node. Both surviving proves the close is the host's
		// client-side transition, not a reload OR a remount that would drop the viewer
		// for a reason unrelated to the gate.
		await page.evaluate(() => {
			(window as unknown as { __spa?: number }).__spa = 1;
			const el = document.querySelector('.item-page') as (HTMLElement & { __page?: number }) | null;
			if (el) el.__page = 2;
		});

		// Archive the parent out from under the OPEN surface.
		const del = await request.delete(
			`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.slug}`,
			{ headers: { Authorization: `Bearer ${fixture.apiToken}` } }
		);
		expect(del.ok(), await del.text()).toBe(true);

		// The `item_archived` SSE flips `parentArchived` false→true, and the host
		// closes the open surface on that transition.
		await expect(page.locator('.archived-banner')).toBeVisible();
		await expect(page.locator(VIEWER), 'archiving the parent closes the open surface').toHaveCount(0);
		expect(
			await page.evaluate(() => {
				const el = document.querySelector('.item-page') as (HTMLElement & { __page?: number }) | null;
				return (window as unknown as { __spa?: number }).__spa === 1 && el?.__page === 2;
			}),
			'the close must be the client-side host transition — not a reload, and not a subtree remount'
		).toBe(true);
	});

	test('a wheel over the toolbar neither zooms the image nor scrolls the page', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Chrome wheel seam');
		await uploadAttachment(fixture, request, doc.id, 'wheel.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		// WAIT FOR DECODE: zoom is disabled until a bitmap exists (bitmapPresent), so
		// a wheel before the image decodes is inert regardless of the exclusion — the
		// exclusion is only observable once zoom is live (a false-pass without this).
		await expect
			.poll(() =>
				page.locator(VIEWER_IMAGE).evaluate((img) => (img as HTMLImageElement).naturalWidth)
			)
			.toBeGreaterThan(0);

		const scaleBefore = await renderedScale(page);
		const scrollBefore = await page.evaluate(() => window.scrollY);
		// Wheel directly over a toolbar control. deltaY NEGATIVE (zoom IN): a
		// positive delta at fit is clamped to fit and would be a no-op regardless of
		// the exclusion — so an inbound zoom is what makes the exclusion observable.
		const box = (await page.locator(VIEWER_TOOLBAR).boundingBox())!;
		await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
		await page.mouse.wheel(0, -300);
		await page.mouse.wheel(0, -300);
		// Settle past the 150ms zoom transition before sampling: a mutation that let
		// the toolbar wheel through would animate the scale up, and reading too soon
		// could catch a still-near-baseline value (Codex round 3). Asserting the
		// ABSENCE of a zoom has nothing to poll for, so a bounded wait is right here.
		await page.waitForTimeout(250);

		// The image did not zoom, and the inert page behind did not scroll.
		expect(await renderedScale(page)).toBeCloseTo(scaleBefore);
		expect(await page.evaluate(() => window.scrollY)).toBe(scrollBefore);
		// A control wheel over the STAGE image DOES zoom — proving the exclusion is
		// what stopped the toolbar wheel, not a dead wheel path.
		const stage = (await page.locator(VIEWER_STAGE).boundingBox())!;
		await page.mouse.move(stage.x + stage.width / 2, stage.y + stage.height / 2);
		await page.mouse.wheel(0, -300);
		await expect
			.poll(() => renderedScale(page))
			.toBeGreaterThan(scaleBefore);
	});
});
