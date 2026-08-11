import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	DESKTOP,
	REAL_PDF,
	TILE,
	VIEWER,
	VIEWER_SHEET,
	VIEWER_FALLBACK,
	VIEWER_FALLBACK_NAME,
	VIEWER_FALLBACK_NOTE,
	VIEWER_IMAGE,
	VIEWER_COUNTER,
	VIEWER_STAGE,
	VIEWER_TOOLBAR,
	VIEWER_META,
	imageRect,
	itemUrl,
	renderedScale,
	uploadAttachment,
	viewerCopyLink,
	viewerDialog,
	viewerNext,
	viewerPrev
} from './lib/attachment-viewer';

/**
 * THE MOBILE PHONE-SHEET LAYOUT, IN A REAL PIXEL 7 (PLAN-2392 3c-ii / T5, DR-9).
 *
 * T5 re-lays-out the converged surface for a phone: the toolbar and metadata
 * leave their desktop absolute anchors and DOCK, stacked, at the bottom edge as
 * a sheet; the stage yields them the room and fills the rest; the counter moves
 * to the top-left. The switch is a `.lightbox-sheet` CLASS the root toggles off
 * `viewport.isMobile`, NOT a bare `@media` — so JS and CSS share one breakpoint
 * and the flip is a DOM fact. jsdom lays nothing out and computes no box, so the
 * geometry, the docked order, the shortened-stage zoom, and the backdrop-vs-chrome
 * dismissal can ONLY be proven in a real engine at a real phone viewport.
 *
 * The `mobile-chromium` project ships the Pixel 7 device (isMobile, ~412px), so
 * the sheet class flips naturally there — no explicit viewport is set. Every leg
 * skips on desktop, where the layout is byte-identical to the pre-T5 surface.
 */

/** Box for a viewer descendant, in viewport coordinates, or null if absent. */
async function box(page: import('@playwright/test').Page, selector: string) {
	return page.evaluate((sel) => {
		const el = document.querySelector(sel);
		if (!el) return null;
		const r = el.getBoundingClientRect();
		return { x: r.x, y: r.y, w: r.width, h: r.height, top: r.top, bottom: r.bottom, left: r.left, right: r.right };
	}, selector);
}

test.describe('attachment viewer — mobile phone-sheet layout (PLAN-2392 3c-ii / T5)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'phone-sheet layout is Pixel 7 only');
	});

	test('the sheet class flips on, and the chrome docks: stage on top, meta then toolbar at the bottom edge, counter top-left', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet layout');
		// Two images so the counter renders (it is gated on a multi-image set).
		await uploadAttachment(fixture, request, doc.id, 'sheet-a.png');
		await uploadAttachment(fixture, request, doc.id, 'sheet-b.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		// The layout FACT: the phone-sheet class is on the dialog root, at the one
		// app breakpoint `viewport.isMobile` reads.
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);

		// Every chrome element must render VISIBLY first — `box()` alone returns a
		// truthy rect for a zero-size / hidden node, so a collapsed layout would slip
		// past a mere truthiness check.
		for (const sel of [VIEWER_STAGE, VIEWER_META, VIEWER_TOOLBAR, VIEWER_COUNTER]) {
			await expect(page.locator(sel)).toBeVisible();
		}
		const stage = (await box(page, VIEWER_STAGE))!;
		const meta = (await box(page, VIEWER_META))!;
		const toolbar = (await box(page, VIEWER_TOOLBAR))!;
		const counter = (await box(page, VIEWER_COUNTER))!;
		const vp = page.viewportSize()!;
		// Non-degenerate boxes: a collapsed element has a truthy rect but no area.
		for (const [name, b] of [['stage', stage], ['meta', meta], ['toolbar', toolbar], ['counter', counter]] as const) {
			expect(b.w, `${name} has width`).toBeGreaterThan(0);
			expect(b.h, `${name} has height`).toBeGreaterThan(0);
		}

		// The STAGE fills the top: anchored at the top-left corner, full phone width,
		// and a MEANINGFUL height (well over a third of the screen) — not a collapsed
		// sliver squeezed by an overgrown dock.
		expect(Math.abs(stage.left), 'the stage is flush left').toBeLessThan(2);
		expect(Math.abs(stage.top), 'the stage starts at the top').toBeLessThan(2);
		expect(Math.abs(stage.w - vp.width), 'the stage spans the full width').toBeLessThan(2);
		expect(stage.h, 'the stage keeps a meaningful height, not a collapsed sliver').toBeGreaterThan(vp.height / 3);

		// The toolbar is the very bottom bar — its bottom edge sits ON the viewport
		// bottom (a few px of tolerance for sub-pixel rounding).
		expect(Math.abs(toolbar.bottom - vp.height), 'the toolbar docks to the bottom edge').toBeLessThan(2);
		// Docked ORDER, top to bottom: stage, then meta, then the toolbar bar.
		expect(stage.top, 'the stage is above the meta').toBeLessThan(meta.top);
		expect(meta.top, 'the meta is above the toolbar').toBeLessThan(toolbar.top);
		// CONTIGUOUS dock, no gaps: the stage ends where the meta begins, and the meta
		// ends where the toolbar begins — the chrome is one stacked sheet, not three
		// floating bars with holes between them.
		expect(Math.abs(stage.bottom - meta.top), 'the stage abuts the meta (no gap)').toBeLessThan(2);
		expect(Math.abs(meta.bottom - toolbar.top), 'the meta abuts the toolbar (no gap)').toBeLessThan(2);
		// Meta and toolbar span the full phone width (they became flow items).
		expect(Math.abs(meta.w - vp.width), 'the docked meta spans the width').toBeLessThan(2);
		expect(Math.abs(toolbar.w - vp.width), 'the docked toolbar spans the width').toBeLessThan(2);
		// The counter is docked TOP-LEFT at the CSS inset var(--space-3) — matched
		// against the RESOLVED token, so a `(0,0)` or negative-offset regression is
		// caught (merely "< quadrant" would pass those). The counter box's left/top
		// equals the inset; allow a couple px for sub-pixel rounding.
		const inset = await page.evaluate(() => {
			const v = getComputedStyle(document.documentElement).getPropertyValue('--space-3').trim();
			const probe = document.createElement('div');
			probe.style.cssText = `position:fixed;left:${v};top:${v};`;
			document.body.appendChild(probe);
			const r = probe.getBoundingClientRect();
			probe.remove();
			return { left: r.left, top: r.top };
		});
		expect(inset.left, 'the --space-3 inset resolves to a real gutter').toBeGreaterThan(0);
		expect(Math.abs(counter.left - inset.left), 'the counter sits at the left inset').toBeLessThan(3);
		expect(Math.abs(counter.top - inset.top), 'the counter sits at the top inset').toBeLessThan(3);
	});

	test('the prev/next nav anchor to the SHORTENED stage, clear of the dock, and stay hittable on a short/landscape phone', async ({
		page,
		fixture,
		request
	}) => {
		// THE 3c-ii NAV-PLACEMENT FIX. The prev/next arrows are `top: 50%` centred.
		// On desktop and in a tall portrait phone the dock is a small fraction of the
		// screen, so a viewport-centred arrow lands in the open stage. But on a SHORT
		// viewport — a landscape phone, or portrait with the keyboard up — the docked
		// meta+toolbar eat a big slice of the bottom, and an arrow centred on the FULL
		// viewport lands in (or overlapping) the dock: obscured, or stealing the dock's
		// taps. The fix moves the nav INSIDE the `position: relative` sheet stage, so
		// `top: 50%` re-anchors to the SHORTENED stage box and the arrows clear the dock
		// with no magic-number dock height. This is the browser-only geometry proof.
		//
		// A literal Pixel 7 landscape is 863px wide — past the 768px sheet breakpoint,
		// so the sheet would not engage at all. The sheet's real domain is ≤768px wide;
		// the trait that triggers the bug is the SHORT HEIGHT (the dock's fraction of the
		// screen), so we use a short, landscape-shaped, in-sheet viewport.
		await page.setViewportSize({ width: 720, height: 400 });
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet nav placement');
		// Two images so the nav (and counter) render — both are gated on a set > 1.
		await uploadAttachment(fixture, request, doc.id, 'nav-a.png');
		await uploadAttachment(fixture, request, doc.id, 'nav-b.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);

		// Both arrows are present and visible (multi-image set).
		await expect(viewerPrev(page)).toBeVisible();
		await expect(viewerNext(page)).toBeVisible();

		const stage = (await box(page, VIEWER_STAGE))!;
		const toolbar = (await box(page, VIEWER_TOOLBAR))!;
		const prev = (await viewerPrev(page).boundingBox())!;
		const next = (await viewerNext(page).boundingBox())!;
		const vp = page.viewportSize()!;

		// The dock genuinely shortens the stage on this viewport — the whole premise.
		// The stage stops at the dock, and its centre sits WELL above the viewport
		// centre (an arrow centred on the full viewport would therefore miss the stage).
		expect(stage.bottom, 'the stage yields the bottom to the dock').toBeLessThanOrEqual(toolbar.top + 2);
		const stageCenterY = stage.top + stage.h / 2;
		expect(stageCenterY, 'the shortened stage centre is well above the viewport centre').toBeLessThan(vp.height / 2 - 20);

		// THE FIX: each arrow's centre tracks the STAGE centre, not the viewport centre.
		// This is the discriminator — with the nav docked to the fixed backdrop (the
		// pre-fix DOM), both centres would sit at vp.height/2, a dock-half below here.
		for (const [name, b] of [['prev', prev], ['next', next]] as const) {
			const centerY = b.y + b.height / 2;
			expect(Math.abs(centerY - stageCenterY), `the ${name} arrow centres on the stage, not the viewport`).toBeLessThan(6);
			// Clear of the dock: the whole arrow sits above the toolbar's top edge …
			expect(b.y + b.height, `the ${name} arrow sits fully above the dock`).toBeLessThanOrEqual(toolbar.top + 1);
			// … and within the stage's vertical band (nav ∩ dock empty, nav ⊆ stage).
			expect(b.y, `the ${name} arrow starts within the stage`).toBeGreaterThanOrEqual(stage.top - 1);
			expect(b.y + b.height, `the ${name} arrow ends within the stage`).toBeLessThanOrEqual(stage.bottom + 1);
		}

		// HITTABLE, not merely positioned: a real click on each arrow navigates. The
		// counter names the shown index over the set, so it is the navigation oracle.
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('1 / 2');
		await viewerNext(page).click();
		await expect(page.locator(VIEWER_COUNTER), 'clicking Next advances the set').toHaveText('2 / 2');
		await viewerPrev(page).click();
		await expect(page.locator(VIEWER_COUNTER), 'clicking Previous steps back').toHaveText('1 / 2');
	});

	test('a click on the empty stage closes the sheet; a click on the docked chrome does not', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet dismiss');
		await uploadAttachment(fixture, request, doc.id, 'dismiss.png');
		await page.goto(itemUrl(fixture, doc.slug));

		const open = async () => {
			await page.locator(TILE).first().click();
			await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
			await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);
		};

		// A click on the DOCKED CHROME is not a dismissal (the backdrop closes only on
		// a click whose target is the root itself; the stage is pointer-events:none so
		// it passes through, but the docked meta/toolbar are real children). Both the
		// metadata plate AND a toolbar button keep the sheet open — the toolbar's
		// Copy-link runs its own action, it does not dismiss.
		await open();
		await page.locator(VIEWER_META).click({ position: { x: 4, y: 4 } });
		await expect(page.locator(VIEWER), 'a click on the docked meta must not dismiss').toHaveCount(1);
		await viewerCopyLink(page).click();
		await expect(page.locator(VIEWER), 'a click on a docked toolbar control must not dismiss').toHaveCount(1);

		// A click near the TOP of the stage (empty letterbox, clear of the top-left
		// counter) reaches the backdrop and closes — the stage overlay is
		// pointer-events:none, so the event's target resolves to the dialog root.
		const vp = page.viewportSize()!;
		await page.locator(VIEWER).click({ position: { x: Math.round(vp.width / 2), y: 8 } });
		await expect(page.locator(VIEWER)).toHaveCount(0);
	});

	test('a FILE tile opens the converged sheet with the fallback arm (the file route on a phone)', async ({
		page,
		fixture,
		request
	}) => {
		// The file route on mobile: a non-image tile emits on the surface channel and
		// opens the phone-SHEET presentation of the converged surface, drawing the
		// no-bytes fallback arm. The owner-4 rewrite moved the mobile BottomSheet
		// coverage to the delete-confirm menu, so this is the leg that proves a file
		// tile reaches the converged surface at the phone breakpoint.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet file route');
		await uploadAttachment(fixture, request, doc.id, 'phone.pdf', 'application/pdf', REAL_PDF);
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(`${TILE}[aria-label*="phone.pdf"]`).click();

		await expect(viewerDialog(page, 'phone.pdf')).toHaveCount(1);
		// The phone-sheet layout, with the fallback arm and no bytes.
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);
		await expect(page.locator(VIEWER_FALLBACK)).toBeVisible();
		await expect(page.locator(VIEWER_FALLBACK_NAME)).toHaveText('phone.pdf');
		await expect(page.locator(VIEWER_FALLBACK_NOTE)).toHaveText('No preview available');
		await expect(page.locator(VIEWER_IMAGE)).toHaveCount(0);

		// The overlay CENTRES IN THE STAGE region, not over the dock: the sheet stage
		// is `position: relative`, so the fallback's `inset: 0` resolves against the
		// SHORTENED stage box. Its centre tracks the stage centre and it sits entirely
		// ABOVE the docked toolbar (a full-viewport centring would push it over/behind
		// the dock).
		const stage = (await box(page, VIEWER_STAGE))!;
		const toolbar = (await box(page, VIEWER_TOOLBAR))!;
		const fb = (await box(page, VIEWER_FALLBACK))!;
		const fbCenterY = fb.top + fb.h / 2;
		expect(Math.abs(fbCenterY - (stage.top + stage.h / 2)), 'the fallback centres in the stage').toBeLessThan(6);
		expect(fb.bottom, 'the fallback sits above the docked toolbar, not over it').toBeLessThanOrEqual(toolbar.top + 2);
	});

	test('zoom is live over the SHORTENED stage — its box tracks the dock, not the full viewport', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet zoom');
		await uploadAttachment(fixture, request, doc.id, 'zoomable.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		// Wait for the bitmap to decode — zoom is disabled until one exists.
		await expect
			.poll(() => page.locator(VIEWER_IMAGE).evaluate((img) => (img as HTMLImageElement).naturalWidth))
			.toBeGreaterThan(0);

		const stage = (await box(page, VIEWER_STAGE))!;
		const toolbar = (await box(page, VIEWER_TOOLBAR))!;
		const vp = page.viewportSize()!;
		// The stage is SHORTENED: it stops at the docked toolbar rather than filling
		// the phone. This is the geometry the zoom module measures (`readGeometry`
		// reads `stage.clientHeight`), so a zoom anchored to the stage centre is
		// anchored to the shortened box, not the full viewport.
		expect(stage.bottom, 'the stage yields the bottom to the dock').toBeLessThanOrEqual(toolbar.top + 2);
		expect(stage.h, 'the shortened stage is well under the full viewport height').toBeLessThan(vp.height - 40);

		// The image is centred in that SHORTENED stage — so its on-screen centre sits
		// ABOVE the viewport centre (an impl that measured the full viewport would
		// centre it at vp.height/2). This is the stage-based layout proof.
		const stageCenterY = stage.top + stage.h / 2;
		const imgFit = await imageRect(page);
		const fitCenterY = imgFit.top + imgFit.height / 2;
		expect(Math.abs(fitCenterY - stageCenterY), 'the image is centred in the shortened stage').toBeLessThan(4);
		expect(fitCenterY, 'the stage centre is above the viewport centre (the dock shortened it)').toBeLessThan(vp.height / 2 - 20);

		// Zoom is live AND anchored to the stage centre: a keyboard zoom-in (about the
		// stage centre) raises the scale past fit while the image's centre stays put
		// at the stage centre — an impl anchoring to the viewport centre would drift
		// it downward. The stage box itself does not grow (zoom transforms the image).
		const before = await renderedScale(page);
		await page.keyboard.press('+');
		await page.keyboard.press('+');
		let last = NaN;
		await expect
			.poll(async () => {
				const s = await renderedScale(page);
				const stable = s > before && s === last;
				last = s;
				return stable;
			})
			.toBe(true);
		const imgZoomed = await imageRect(page);
		const zoomedCenterY = imgZoomed.top + imgZoomed.height / 2;
		expect(Math.abs(zoomedCenterY - stageCenterY), 'zoom is anchored to the STAGE centre, not the viewport').toBeLessThan(6);
		const stageAfter = (await box(page, VIEWER_STAGE))!;
		expect(Math.abs(stageAfter.h - stage.h), 'zoom transforms the image, not the stage box').toBeLessThan(2);
		expect(stageAfter.bottom, 'the stage still stops above the dock after zoom').toBeLessThanOrEqual(toolbar.top + 2);
	});

	test('DR-18 toolbar labels reveal on the phone, and native pinch is preserved (touch-action not locked)', async ({
		page,
		fixture,
		request
	}) => {
		// DR-18: the toolbar label text is in the accessible name always, but its
		// VISIBLE text shows only on the phone (≤768px) — the roomier desktop toolbar
		// stays icon-only. And the image is DELIBERATELY not `touch-action: none` (the
		// 3d pinch handler does not exist yet), so native pinch-zoom stays available to
		// phone users in the interval — an ABSENCE fact a browser can assert even
		// though genuine two-pointer pinch is 3d's device proof.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet labels');
		await uploadAttachment(fixture, request, doc.id, 'labels.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);

		// The toolbar labels are VISIBLE (revealed, not the desktop icon-only) — every
		// tool button shows its text.
		const labels = page.locator(`${VIEWER_TOOLBAR} .lightbox-tool-label`);
		await expect(labels.first()).toBeVisible();
		const displays = await labels.evaluateAll((els) => els.map((e) => getComputedStyle(e).display));
		expect(displays.length, 'the toolbar has labelled controls').toBeGreaterThan(0);
		expect(displays.every((d) => d !== 'none'), 'every toolbar label is revealed on the phone').toBe(true);

		// The viewer OWNS touch now (3d / TASK-2518): the image AND the stage carry
		// `touch-action: none`, so the browser's native pinch / scroll / double-tap-zoom
		// don't compete with the pointer-driven pan / pinch / double-tap handlers. The
		// stage stays pointer-events:none so a LETTERBOX touch still reaches the
		// backdrop (which keeps touch-action:auto) and taps to close — the letterbox rule.
		const facts = await page.evaluate((viewerSel) => {
			const img = document.querySelector(`${viewerSel} .lightbox-image`) as HTMLElement | null;
			const stage = document.querySelector(`${viewerSel} .lightbox-stage`) as HTMLElement | null;
			return {
				imgTouchAction: img ? getComputedStyle(img).touchAction : null,
				stageTouchAction: stage ? getComputedStyle(stage).touchAction : null,
				stagePointerEvents: stage ? getComputedStyle(stage).pointerEvents : null
			};
		}, '.attachment-viewer');
		expect(facts.imgTouchAction, 'the image takes touch-action none (the viewer owns touch)').toBe('none');
		expect(facts.stageTouchAction, 'the stage takes touch-action none (the viewer owns touch)').toBe('none');
		expect(facts.stagePointerEvents, 'the stage letterbox stays click-through to the backdrop').toBe('none');
	});

	test('forced-colors gives the docked sheet chrome an opaque plate and a system-colour top edge (T5 DR-4)', async ({
		page,
		fixture,
		request
	}) => {
		// T5 forced-colors: the translucent docked-plate fill and hairline border are
		// dropped by forced-colors, so the sheet would merge into the content above it.
		// The `@media (forced-colors: active)` rule gives the docked meta + toolbar an
		// OPAQUE Canvas fill and a real system-colour top border — a boundary that
		// survives. Emulating forced-colors is the only way to exercise this arm.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Sheet forced-colors');
		await uploadAttachment(fixture, request, doc.id, 'fc.png');
		await page.goto(itemUrl(fixture, doc.slug));

		// Read BOTH docked plates' resolved background + top-border colour.
		const readChrome = () =>
			page.evaluate((viewerSel) => {
				const pick = (sel: string) => {
					const el = document.querySelector(`${viewerSel} ${sel}`) as HTMLElement | null;
					if (!el) return null;
					const cs = getComputedStyle(el);
					const m = /rgba?\(([^)]+)\)/.exec(cs.backgroundColor);
					const parts = m ? m[1].split(',').map((s) => parseFloat(s)) : [];
					return {
						bg: cs.backgroundColor,
						bgAlpha: parts.length === 4 ? parts[3] : 1,
						borderTopColor: cs.borderTopColor
					};
				};
				return { meta: pick('.lightbox-meta'), toolbar: pick('.lightbox-toolbar') };
			}, '.attachment-viewer');

		// Baseline (normal colours): both docked plates are TRANSLUCENT.
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);
		const normal = await readChrome();
		expect(normal.meta!.bgAlpha, 'the normal docked meta is translucent').toBeLessThan(1);
		expect(normal.toolbar!.bgAlpha, 'the normal docked toolbar is translucent').toBeLessThan(1);

		// Under forced-colors, the AUTHOR rule repaints the SAME plates with the
		// `Canvas` fill and a `CanvasText` top edge. Resolve those two system colours
		// via a probe under the SAME emulation, then assert the plates match them —
		// this proves the author `@media (forced-colors: active)` rule fired, not
		// merely that the UA made something opaque.
		await page.emulateMedia({ forcedColors: 'active' });
		const sys = await page.evaluate(() => {
			const p = document.createElement('div');
			p.style.cssText = 'background: Canvas; border-top: 1px solid CanvasText;';
			document.body.appendChild(p);
			const cs = getComputedStyle(p);
			const v = { canvas: cs.backgroundColor, canvasText: cs.borderTopColor };
			p.remove();
			return v;
		});
		const forced = await readChrome();
		for (const [name, plate] of [['meta', forced.meta!], ['toolbar', forced.toolbar!]] as const) {
			expect(plate.bgAlpha, `forced-colors makes the docked ${name} opaque`).toBe(1);
			expect(plate.bg, `the docked ${name} takes the Canvas fill`).toBe(sys.canvas);
			expect(plate.borderTopColor, `the docked ${name} takes a CanvasText top edge`).toBe(sys.canvasText);
		}
		await page.emulateMedia({ forcedColors: null });
	});
});

test.describe('attachment viewer — the DESKTOP surface is unchanged by T5', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the desktop contrast to the phone sheet');
		await page.setViewportSize(DESKTOP);
	});

	test('no sheet class, chrome absolutely positioned (not docked), toolbar icon-only', async ({
		page,
		fixture,
		request
	}) => {
		// The other half of "the sheet is scoped to the phone": on desktop the viewer
		// is byte-identical to the pre-T5 surface. The `.lightbox-sheet` class is
		// ABSENT, the meta/toolbar keep their desktop `position: absolute` anchors
		// (not the sheet's `static` flow), and the DR-18 toolbar labels stay icon-only
		// (`display: none`). jsdom lays nothing out, so this computed-style contrast is
		// a browser-only proof.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Desktop unchanged');
		await uploadAttachment(fixture, request, doc.id, 'desktop.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		// The dialog is a plain viewer, NOT a sheet.
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(0);

		const desk = await page.evaluate((viewerSel) => {
			const meta = document.querySelector(`${viewerSel} .lightbox-meta`) as HTMLElement | null;
			const toolbar = document.querySelector(`${viewerSel} .lightbox-toolbar`) as HTMLElement | null;
			const label = document.querySelector(`${viewerSel} .lightbox-tool-label`) as HTMLElement | null;
			return {
				metaPosition: meta ? getComputedStyle(meta).position : null,
				toolbarPosition: toolbar ? getComputedStyle(toolbar).position : null,
				labelDisplay: label ? getComputedStyle(label).display : null
			};
		}, '.attachment-viewer');
		expect(desk.metaPosition, 'desktop meta keeps its absolute anchor, not the sheet flow').toBe('absolute');
		expect(desk.toolbarPosition, 'desktop toolbar keeps its absolute anchor, not the sheet flow').toBe('absolute');
		expect(desk.labelDisplay, 'desktop toolbar stays icon-only (labels hidden)').toBe('none');
	});
});
