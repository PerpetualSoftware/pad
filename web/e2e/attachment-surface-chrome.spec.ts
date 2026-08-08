import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	BIG_PNG,
	DESKTOP,
	REAL_PNG,
	TILE,
	VIEWER,
	VIEWER_IMAGE,
	VIEWER_COUNTER,
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

/** The timeline (and its comment images) live under the item's Activity tab. */
async function openActivityTab(page: Page): Promise<void> {
	await page.getByRole('tab', { name: 'Activity' }).click();
	await expect(page.locator('.timeline')).toBeVisible();
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
		await openActivityTab(page);
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

	// THE FALLBACK ARM + no-bytes invariant is NOT reachable through the real
	// producers in a browser, and is jsdom-proven instead (TASK-2476). Why: the
	// icon fallback only ever draws a NAVIGABLE unsafe entry, which exists solely
	// from a MID-VIEW safe→unsafe MIME flip — and every producer SNAPSHOTS the
	// viewer's image set at open (`ItemAttachmentStrip.openLightbox` copies the
	// derived array; `AttachmentViewerHost` spreads `request.images`), so a shown
	// image's MIME cannot change under the open viewer here. The AT-OPEN unsafe
	// case never reaches the viewer either: the producers filter `canOpenInViewer`
	// before emitting, so the last-mile gate (`unsafeAtOpenIds`) is defense in
	// depth. Route-intercepting the metadata probe flips the HEADER's fetched MIME,
	// which the seed overrides — it does not change the arm. The jsdom suite drives
	// the flip by mutating the prop directly (the one thing a component test can do
	// and a browser cannot), and asserts the no-bytes invariant (no <img>, detached
	// src cleared, loader disposed) there. Left as a visible `fixme` rather than
	// omitted so the coverage boundary is explicit.
	test.fixme(
		'the fallback arm on a mid-view MIME flip (jsdom-proven — unreachable via real producers)',
		async () => {}
	);

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
