import { test, expect } from './fixtures';
import type { SuiteFixture } from './fixtures';
import type { Page, APIRequestContext } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	BIG_PNG,
	DESKTOP,
	HUGE_PNG,
	MID_PNG,
	TILE,
	VIEWER,
	VIEWER_IMAGE,
	VIEWER_STAGE,
	WIDE_PNG,
	imageRect,
	itemUrl,
	renderedScale,
	uploadAttachment,
	viewerClose,
	viewerTapLoad
} from './lib/attachment-viewer';

/**
 * THE VIEWER'S ZOOM / PAN / LOADING BEHAVIOUR, IN A REAL BROWSER
 * (PLAN-2392 phase 3b / TASK-2461, DR-9).
 *
 * jsdom has no layout, no CSS and no gestures, so the unit suite proves the
 * zoom/pan MATH (zoom.test.ts) and the loader's request POLICY
 * (viewerImageLoader.svelte.test.ts) but never that a wheel/key/drag reaches
 * that math, that the transform actually paints, that a clamp holds against real
 * geometry, or that a control stays clickable under a scaled image. Each test
 * here is written against "what mutation would this catch that a jsdom-equivalent
 * would survive?" — named at each test.
 */

const BIG_W = 1600;

/**
 * Open the viewer on a >1024px image and wait until the ORIGINAL has painted
 * (naturalWidth === 1600), so geometry is settled past the thumb→original
 * upgrade and every zoom/pan measurement below is deterministic.
 */
async function openBig(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	title: string,
	names: string[] = ['big.png'],
	buffer: Buffer = BIG_PNG,
	naturalWidth = BIG_W
): Promise<void> {
	await browserLogin(page);
	const doc = await seedDoc(fixture, request, title);
	for (const name of names) {
		await uploadAttachment(fixture, request, doc.id, name, 'image/png', buffer);
	}
	await page.goto(itemUrl(fixture, doc.slug));
	await expect(page.locator(TILE).first()).toBeVisible();
	await page.locator(TILE).first().click();
	await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
	// Settle on the original — the final, stable geometry.
	await expect
		.poll(() => page.locator(VIEWER_IMAGE).evaluate((el) => (el as HTMLImageElement).naturalWidth))
		.toBe(naturalWidth);
}

/** Read the RENDERED scale once the CSS transition has settled (two equal reads). */
async function settledScale(page: Page): Promise<number> {
	let last = Number.NaN;
	await expect
		.poll(async () => {
			const s = await renderedScale(page);
			const stable = Math.abs(s - last) < 1e-3;
			last = s;
			return stable;
		})
		.toBe(true);
	return renderedScale(page);
}

/**
 * Zoom to maximum by pressing '+' ONE step at a time, settling each transition,
 * until the scale stops climbing (the clamp). Per-step settling avoids the
 * rapid-press race where two mid-animation reads look equal and stop early.
 */
async function zoomToMax(page: Page): Promise<number> {
	let prev = await settledScale(page);
	for (let i = 0; i < 20; i++) {
		await page.keyboard.press('+');
		// Wait for THIS press to take effect (the scale climbs past `prev`), up to a
		// deadline; if it never climbs, we are at the clamp. This avoids the race
		// where the press hasn't been processed yet and two equal reads look settled.
		const deadline = Date.now() + 1500;
		let climbed = false;
		while (Date.now() < deadline) {
			if ((await renderedScale(page)) > prev + 1e-3) {
				climbed = true;
				break;
			}
		}
		if (!climbed) return prev; // clamp reached
		prev = await settledScale(page);
	}
	return prev;
}

test.describe('attachment viewer — desktop zoom & pan (TASK-2461)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'desktop zoom/pan is viewport-driven; one project is enough'
		);
		await page.setViewportSize(DESKTOP);
	});

	test('wheel, ctrl-wheel, keyboard and double-click each transform the RENDERED image', async ({
		page,
		fixture,
		request
	}) => {
		// jsdom has no layout, so `getComputedStyle(img).transform` is never a real
		// matrix there — a handler wired to nothing (or to a no-op `zoomTo`) passes
		// every unit test. Here the proof is the painted matrix moving off identity.
		await openBig(page, fixture, request, 'Zoom transforms');
		const stage = page.locator(VIEWER_STAGE);
		const box = (await stage.boundingBox())!;
		const cx = box.x + box.width / 2;
		const cy = box.y + box.height / 2;

		expect(await renderedScale(page), 'opens at fit (scale 1)').toBeCloseTo(1, 1);

		// WHEEL (plain) in.
		await page.mouse.move(cx, cy);
		await page.mouse.wheel(0, -120);
		await expect.poll(() => renderedScale(page)).toBeGreaterThan(1.05);
		const afterWheel = await renderedScale(page);

		// KEYBOARD '0' resets to fit.
		await page.keyboard.press('0');
		await expect.poll(() => renderedScale(page)).toBeCloseTo(1, 1);

		// KEYBOARD '+' in.
		await page.keyboard.press('+');
		await expect.poll(() => renderedScale(page)).toBeGreaterThan(1.05);
		await page.keyboard.press('0');

		// CTRL+WHEEL in — a genuinely separate gesture path (Control HELD). We assert
		// it zooms the IMAGE; that the viewer's `preventDefault` also suppresses the
		// browser's own ctrl-wheel page-zoom is not reliably observable through
		// Playwright (page zoom does not move visualViewport.scale), so it is left to
		// the unit coverage of the non-passive listener.
		expect(afterWheel, 'the plain-wheel leg zoomed').toBeGreaterThan(1.05);
		await page.mouse.move(cx, cy);
		await page.keyboard.down('Control');
		await page.mouse.wheel(0, -120);
		await page.keyboard.up('Control');
		await expect.poll(() => renderedScale(page)).toBeGreaterThan(1.05);
		await page.keyboard.press('0');

		// DOUBLE-CLICK toggles fit → actual (a jump well past 1).
		await page.mouse.dblclick(cx, cy);
		await expect.poll(() => renderedScale(page)).toBeGreaterThan(1.05);
	});

	test('the close and nav controls stay clickable and focus-visible AT MAXIMUM ZOOM', async ({
		page,
		fixture,
		request
	}) => {
		// The state where a transformed image can paint over the chrome and swallow
		// its clicks. jsdom cannot see the stacking or the hit test, so only a real
		// engine proves the controls survive a full-scale image on top of the stage.
		// Two images so the nav buttons exist alongside close.
		await openBig(page, fixture, request, 'Controls at max zoom', ['ctrl-a.png', 'ctrl-b.png']);
		const scale = await zoomToMax(page);
		expect(scale, 'reached a real maximum well past fit').toBeGreaterThan(2);

		// HIT TEST — the point proof that the scaled image does not paint over the
		// controls: `elementFromPoint` at each control's centre must return that
		// control, not the IMG. jsdom cannot do this (no layout, no stacking).
		const topAt = (sel: string) =>
			page.evaluate((s) => {
				const front = [...document.querySelectorAll('.attachment-viewer')].at(-1);
				const el = front?.querySelector<HTMLElement>(s.replace('.attachment-viewer[role="dialog"] ', ''));
				if (!el) return 'missing';
				const r = el.getBoundingClientRect();
				const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
				return hit ? `${hit.tagName}.${(hit.closest('button')?.className ?? hit.className).split(' ').find((c) => c.startsWith('lightbox-')) ?? ''}` : 'none';
			}, sel);
		expect(await topAt(`${VIEWER} .lightbox-close`), 'close is the top surface at its centre').toContain('lightbox-close');
		expect(await topAt(`${VIEWER} .lightbox-nav.next`), 'next is the top surface at its centre').toContain('lightbox-nav');

		// TAB CYCLES at max zoom — focus never escapes the viewer.
		const close = viewerClose(page);
		await close.focus();
		await expect(close).toBeFocused();
		for (let i = 0; i < 4; i++) {
			await page.keyboard.press('Tab');
			expect(
				await page.evaluate(() => !!document.activeElement?.closest('.attachment-viewer')),
				`Tab ${i + 1} kept focus inside the viewer at max zoom`
			).toBe(true);
		}

		// FOCUS-VISIBLE: the focused control shows an indicator, not `none`.
		await close.focus();
		const outline = await close.evaluate((el) => {
			const cs = getComputedStyle(el);
			return { outlineWidth: cs.outlineWidth, boxShadow: cs.boxShadow };
		});
		expect(
			outline.outlineWidth !== '0px' || outline.boxShadow !== 'none',
			'the focused control must show a visible focus ring at max zoom'
		).toBe(true);

		await close.click();
		await expect(page.locator(VIEWER)).toHaveCount(0);
	});

	test('a wheel-zoom keeps the ANCHORED point under the cursor', async ({ page, fixture, request }) => {
		// The property that separates pointer-anchored zoom (TASK-2457) from
		// centre-anchored: the image content under the cursor stays under it. jsdom
		// has no cursor and no rect, so the anchor math has never been exercised
		// against real geometry. Mutating the anchor to the stage centre drifts the
		// point away and fails this.
		await openBig(page, fixture, request, 'Zoom anchor');
		const stage = (await page.locator(VIEWER_STAGE).boundingBox())!;
		// Off-centre, so a centre-anchored zoom would visibly move this point.
		const px = stage.x + stage.width * 0.35;
		const py = stage.y + stage.height * 0.4;
		const before = await imageRect(page);
		const fx = (px - before.x) / before.width;
		const fy = (py - before.y) / before.height;

		await page.mouse.move(px, py);
		await page.mouse.wheel(0, -120); // one step in
		await settledScale(page);
		const after = await imageRect(page);
		// The same image-fractional point, mapped through the NEW rect, is still
		// under the cursor (uniform scale ⇒ linear map).
		expect(Math.abs(after.x + fx * after.width - px), 'anchored X held').toBeLessThan(3);
		expect(Math.abs(after.y + fy * after.height - py), 'anchored Y held').toBeLessThan(3);
	});

	test('pan CLAMPS: an in-bounds drag moves by the delta; an over-drag stops at the edge', async ({
		page,
		fixture,
		request
	}) => {
		// TWO legs on purpose (TASK-2461): a one-legged "it panned" passes with the
		// clamp disabled, and a one-legged "it stopped" passes with pan disabled
		// entirely. jsdom clamps against all-zero geometry, so neither leg has run
		// against real bounds.
		await openBig(page, fixture, request, 'Pan clamp');
		await zoomToMax(page); // heavy overflow ⇒ real pan room
		const stage = (await page.locator(VIEWER_STAGE).boundingBox())!;
		const cx = stage.x + stage.width / 2;
		const cy = stage.y + stage.height / 2;

		// Settle at the centred max-zoom position before measuring.
		const r0 = await imageRect(page);
		// POSITIVE leg — an 80px in-bounds drag moves the image ~80px to the RIGHT
		// (the pan follows the pointer). A small hold at the start engages the drag
		// cleanly past the 4px threshold.
		await page.mouse.move(cx, cy);
		await page.mouse.down();
		await page.mouse.move(cx + 20, cy, { steps: 4 }); // engage past threshold
		await page.mouse.move(cx + 80, cy, { steps: 8 });
		await page.mouse.up();
		const r1 = await imageRect(page);
		expect(r1.x - r0.x, 'an in-bounds pan moved the image right by ~the drag delta').toBeGreaterThan(60);
		expect(r1.x - r0.x).toBeLessThan(100);

		// EDGE leg — a huge drag clamps at the stage edge, and dragging FURTHER in
		// the same direction moves it no more. (Two drags, because "it stopped at the
		// edge" is only proof of a clamp if a further push is a no-op.)
		await page.mouse.move(cx, cy);
		await page.mouse.down();
		await page.mouse.move(cx + 6000, cy, { steps: 12 });
		await page.mouse.up();
		const r2 = await imageRect(page);
		expect(Math.abs(r2.x - stage.x), 'the image left edge clamps to the stage left').toBeLessThan(3);
		await page.mouse.move(cx, cy);
		await page.mouse.down();
		await page.mouse.move(cx + 3000, cy, { steps: 8 });
		await page.mouse.up();
		const r3 = await imageRect(page);
		expect(Math.abs(r3.x - r2.x), 'at the clamp, more drag does not move the image').toBeLessThan(2);
	});

	test('dismissal: a plain backdrop click closes; a drag that releases over the backdrop does NOT', async ({
		page,
		fixture,
		request
	}) => {
		// TASK-2458's "a drag that ends where it started is a click": a press that
		// MOVES past threshold and RETURNS to its start on the backdrop synthesizes a
		// real click at that point — and must be SUPPRESSED (a drag is not a
		// dismissal). A drag that merely ended elsewhere would produce no click at
		// all, so it would prove nothing about the suppress path; returning to the
		// start is what makes leg 2 depend on the viewer's own logic. jsdom has no
		// drag. Both legs, because the no-close leg alone passes against a viewer that
		// never closes on any click.
		await openBig(page, fixture, request, 'Dismiss drag');
		const stage = (await page.locator(VIEWER_STAGE).boundingBox())!;
		// A point in the left backdrop margin, clear of the fitted image (openBig
		// uploads one image, so there are no nav buttons to hit here).
		const px = Math.max(2, Math.round(stage.x / 2));
		const py = Math.round(stage.y + stage.height / 2);

		// LEG 2 first (it must NOT close): press, drag past threshold, return to the
		// SAME backdrop point, release → a synthesized backdrop click that the drag
		// must suppress.
		await page.mouse.move(px, py);
		await page.mouse.down();
		await page.mouse.move(px + 140, py, { steps: 6 }); // past the 4px threshold
		await page.mouse.move(px, py, { steps: 6 }); // ...back to the start
		await page.mouse.up();
		await expect(
			page.locator(VIEWER),
			'a drag that returns to its start is not a dismissal'
		).toHaveCount(1);

		// LEG 1 — a PLAIN backdrop click at the same point DOES dismiss.
		await page.mouse.click(px, py);
		await expect(page.locator(VIEWER), 'a plain backdrop click dismisses').toHaveCount(0);
	});

	test('a click on BLANK STAGE SPACE inside the stage box but outside the image dismisses', async ({
		page,
		fixture,
		request
	}) => {
		// THE UNCOVERED POINT (TASK-2461). The existing suite clicks (4,4) — outside
		// the centred stage — so a stage that swallowed letterbox clicks would stay
		// green. A wide image is letterboxed top/bottom; a click in that band is
		// INSIDE the stage box, off the image, and must reach the backdrop and close
		// (the stage letterbox is `pointer-events: none`).
		await openBig(page, fixture, request, 'Dismiss letterbox', ['wide.png'], WIDE_PNG, 1600);
		const stage = (await page.locator(VIEWER_STAGE).boundingBox())!;
		const img = await imageRect(page);
		// A point inside the stage box, above the letterboxed image.
		const bandY = (stage.y + img.top) / 2;
		expect(bandY, 'there is a real letterbox band above the image').toBeLessThan(img.top - 5);
		await page.mouse.click(stage.x + stage.width / 2, bandY);
		await expect(page.locator(VIEWER)).toHaveCount(0);
	});

	test('resizing the window while zoomed to MAXIMUM clamps the scale down to the new bound', async ({
		page,
		fixture,
		request
	}) => {
		// `maxScale` is geometry-dependent: ENLARGING the window lowers `actualScale`
		// and with it the ceiling, stranding a previously-valid scale above it
		// (TASK-2455). jsdom fires no resize with real geometry, so the re-clamp
		// effect has never run against a live layout change.
		await openBig(page, fixture, request, 'Resize clamp');
		const maxBefore = await zoomToMax(page);
		await page.setViewportSize({ width: DESKTOP.width + 500, height: DESKTOP.height + 400 });
		// The ResizeObserver re-clamp fires async on layout — poll until the stranded
		// scale is pulled down to the new, lower ceiling.
		await expect.poll(() => renderedScale(page)).toBeLessThan(maxBefore - 0.2);
		const after = await settledScale(page);
		// Clamped DOWN to the new ceiling — but NOT reset to fit: a handler that
		// snapped to `resetZoom()` would also be below the old max, so the "still
		// zoomed" leg is what makes this a clamp and not a reset.
		expect(after, 'the image is still zoomed in, not reset to fit').toBeGreaterThan(1.5);
		// And it sits at the NEW maximum, not some arbitrary lower value: pressing '+'
		// again does not climb (already at the clamp).
		await page.keyboard.press('+');
		expect(await settledScale(page), 'the clamped scale IS the new maximum').toBeCloseTo(after, 1);
	});

	test('reduced-motion suppresses the zoom ANIMATION, and normal mode keeps it', async ({
		page,
		fixture,
		request
	}) => {
		// TWO legs (TASK-2461): the one-legged "reduced-motion has no transition"
		// passes against a viewer that never animates at all. `Modal.svelte:207` is
		// the precedent this follows.
		await openBig(page, fixture, request, 'Reduced motion');
		const transitionDuration = () =>
			page.locator(VIEWER_IMAGE).evaluate((el) => getComputedStyle(el).transitionDuration);

		await page.emulateMedia({ reducedMotion: 'reduce' });
		expect(await transitionDuration(), 'reduced-motion: no zoom animation').toBe('0s');

		await page.emulateMedia({ reducedMotion: 'no-preference' });
		expect(
			Number.parseFloat(await transitionDuration()),
			'normal mode: the zoom DOES animate'
		).toBeGreaterThan(0);
	});

	test('forced-colors keeps the controls and the image BOUNDARY visible (computed style)', async ({
		page,
		fixture,
		request
	}) => {
		// DR-4. Under forced-colors the custom palette is discarded and the image's
		// box-shadow BOUNDARY vanishes; the media block's explicit border is what
		// keeps it visible. Asserted on COMPUTED STYLE (a border with real width),
		// not DOM presence. jsdom has no forced-colors media and computes no border.
		//
		// The IMAGE border is the mutation-load-bearing proof: the base `.lightbox-
		// image` has NO border, so its width>0 here comes ONLY from the media block
		// (removing that rule fails this — verified). The CONTROLS already carry a
		// base 1px border that forced-colors re-colours to a system colour, so they
		// are visible regardless; the control leg is a genuine "still visible" check,
		// not a proof of the media rule (border COLOUR under forced-colors resolves
		// to opaque system values that are not worth pinning).
		await openBig(page, fixture, request, 'Forced colors', ['fc-a.png', 'fc-b.png']);
		await page.emulateMedia({ forcedColors: 'active' });
		const styles = await page.evaluate(() => {
			const front = [...document.querySelectorAll('.attachment-viewer')].at(-1);
			const get = (sel: string) => {
				const el = front?.querySelector<HTMLElement>(sel);
				if (!el) return null;
				const cs = getComputedStyle(el);
				return { w: Number.parseFloat(cs.borderTopWidth), style: cs.borderTopStyle };
			};
			return { img: get('.lightbox-image'), close: get('.lightbox-close'), next: get('.lightbox-nav.next') };
		});
		expect(styles.img, 'the image element exists').not.toBeNull();
		expect(styles.img!.w, 'the image keeps a visible boundary under forced-colors').toBeGreaterThan(0);
		expect(styles.img!.style).toBe('solid');
		expect(styles.close!.w, 'the close control stays visible (bordered)').toBeGreaterThan(0);
		expect(styles.next!.w, 'the nav control stays visible (bordered)').toBeGreaterThan(0);
	});
});

/** The viewer image's current `src` attribute (the URL the browser is loading). */
function imageSrc(page: Page): Promise<string> {
	return page.locator(VIEWER_IMAGE).evaluate((el) => el.getAttribute('src') ?? '');
}
/** The viewer image's decoded natural width — the observable of which variant painted. */
function imageNat(page: Page): Promise<number> {
	return page.locator(VIEWER_IMAGE).evaluate((el) => (el as HTMLImageElement).naturalWidth);
}

/**
 * Intercept the attachment DOWNLOAD requests so the thumb→original swap is
 * deterministic: `?variant=thumb-md` → a bounded 800px thumb, the no-variant
 * original → the 1600px BIG_PNG (optionally delayed). Other variants — the
 * strip's own tile thumbnail — fall through to the real server. The upload
 * itself goes through the Node API context, not the page, so it is never
 * intercepted and the server records the real 1600×1200 metadata.
 */
async function stubVariants(page: Page, originalDelayMs = 0): Promise<void> {
	await page.route('**/attachments/*', async (route) => {
		const url = new URL(route.request().url());
		if (!/\/attachments\/[^/?]+$/.test(url.pathname)) return route.continue();
		const variant = url.searchParams.get('variant');
		if (variant === 'thumb-md') return route.fulfill({ contentType: 'image/png', body: MID_PNG });
		if (!variant) {
			if (originalDelayMs) await new Promise((r) => setTimeout(r, originalDelayMs));
			return route.fulfill({ contentType: 'image/png', body: BIG_PNG });
		}
		return route.continue();
	});
}

test.describe('attachment viewer — desktop loading policy (TASK-2461)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop policy; one project is enough');
		await page.setViewportSize(DESKTOP);
	});

	test('the thumb→original swap is observable against a >1024px fixture', async ({ page, fixture, request }) => {
		// DR-5b: a >1024px image paints the bounded thumb first, then upgrades to the
		// original in the background. jsdom proves the request POLICY but never that
		// the browser actually swaps the painted bitmap. A small original delay makes
		// the thumb phase observable rather than a race.
		// Record the viewer's OWN download requests (the tile's thumb-sm is filtered
		// out). The swap is the request sequence thumb-md → original; the bitmap
		// naturalWidth window at 800 is too brief to sample (the upgrade swaps `src`
		// the instant the thumb decodes), so the network order is the reliable proof.
		// An ordered timeline of the viewer's own attachment events (the tile's
		// thumb-sm is filtered out). The swap PROOF is: the thumb-md response
		// FINISHED before the original request STARTED — because the loader only
		// requests the original from `decoded()`, i.e. after the thumb's bytes
		// arrived AND painted. A mere request-order check would pass an impl that
		// fired both immediately; the finish→start ordering rules that out.
		const timeline: string[] = [];
		const isViewerReq = (u: string) =>
			/\/attachments\/[^/?]+(\?variant=(thumb-md|original))?$/.test(u) && !u.includes('thumb-sm');
		page.on('request', (r) => {
			if (isViewerReq(r.url())) timeline.push((r.url().includes('variant=thumb-md') ? 'thumb-md' : 'original') + ':start');
		});
		page.on('requestfinished', (r) => {
			if (isViewerReq(r.url())) timeline.push((r.url().includes('variant=thumb-md') ? 'thumb-md' : 'original') + ':finish');
		});
		await stubVariants(page, 400);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Swap');
		await uploadAttachment(fixture, request, doc.id, 'swap.png', 'image/png', BIG_PNG);
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		// The final painted bitmap is the ORIGINAL (naturalWidth 1600, no variant).
		await expect.poll(() => imageNat(page)).toBe(1600);
		expect(await imageSrc(page)).not.toContain('variant=');
		// The thumb was requested first, its response FINISHED (bytes arrived, the
		// bitmap painted), and ONLY THEN the original was requested — the upgrade.
		const thumbStart = timeline.indexOf('thumb-md:start');
		const thumbFinish = timeline.indexOf('thumb-md:finish');
		const origStart = timeline.indexOf('original:start');
		expect(thumbStart, 'the viewer requested the bounded thumb').toBeGreaterThanOrEqual(0);
		expect(thumbFinish, 'the thumb response arrived (the bitmap could paint)').toBeGreaterThan(thumbStart);
		expect(origStart, 'the original was requested only AFTER the thumb painted').toBeGreaterThan(thumbFinish);
	});

	test('a rapid A→B→A with a slow original leaves the LIVE image correct (switch-safety / generation fence)', async ({
		page,
		fixture,
		request
	}) => {
		// TASK-2459's carried-forward concern, proven in a real engine. The originals
		// are SLOW, so a rapid A→B→A navigates away while A's first original is still
		// in flight: the first A element is torn down (the `{#key loadToken}` remount)
		// with a pending load, and a fresh element takes over for the third visit. The
		// live A must end on ITS OWN original — not B's content, not a broken image,
		// not an error — which is what the per-mount generation fence + load key
		// guarantee. jsdom has no real network timing to stage this.
		//
		// NOTE (honest scope): Chromium ABORTS a detached <img>'s in-flight request,
		// so the exact "detached element fires a late ERROR that clobbers the live
		// one" case the data-gen fence guards is not reachable in a browser — the
		// engine cancels first. That case is covered by the unit tests
		// (viewerImageLoader / Lightbox); this leg proves the rapid-nav END-STATE
		// integrity, the practical half of the concern.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Gen fence');
		const aId = await uploadAttachment(fixture, request, doc.id, 'gen-a.png', 'image/png', BIG_PNG);
		const bId = await uploadAttachment(fixture, request, doc.id, 'gen-b.png', 'image/png', BIG_PNG);

		// thumb-md → the bounded thumb (fast); the original → BIG, but SLOW, so the
		// nav races the in-flight upgrade.
		await stubVariants(page, 1500);

		// Persistent listener (set up BEFORE navigation, so it can't miss the fast
		// upgrade request the way a late `waitForRequest` would).
		let aOriginalRequested = false;
		page.on('request', (r) => {
			if (r.url().includes(aId) && !r.url().includes('variant=')) aOriginalRequested = true;
		});
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);
		await page.locator(`${TILE}[aria-label*="gen-a.png"]`).click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		// A's (slow) original request is in flight before we navigate.
		await expect.poll(() => aOriginalRequested).toBe(true);

		// A→B→A: the first A element is torn down mid-load; a fresh one takes over.
		await page.keyboard.press('ArrowRight');
		await expect.poll(() => imageSrc(page)).toContain(bId); // showing B
		await page.keyboard.press('ArrowLeft');
		await expect.poll(() => imageSrc(page)).toContain(aId); // back to A (fresh element)

		// The LIVE A resolves to ITS OWN original — 1600px, A's URL, no error, no
		// stale B content and no broken bitmap.
		await expect.poll(() => imageNat(page)).toBe(1600);
		expect(await imageSrc(page), 'the live image is A, not the B we passed through').toContain(aId);
		expect(await imageSrc(page)).not.toContain(bId);
		expect(await imageSrc(page)).not.toContain('variant=');
		await expect(page.locator(`${VIEWER} .lightbox-error`), 'no stale load clobbered the live image').toHaveCount(0);
	});
});

test.describe('attachment viewer — mobile DR-5b (TASK-2461)', () => {
	test.beforeEach(async ({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'mobile-chromium',
			'the DR-5b mobile policy needs a real mobile (Pixel 7) project'
		);
	});

	test('a large image issues NO automatic request until the tap affordance is used', async ({
		page,
		fixture,
		request
	}) => {
		// DR-5b mobile (TASK-2460): a large image auto-fetches NOTHING — a tap-to-load
		// affordance stands in, and only a real tap issues the original request. The
		// no-request half is the assertion that fails if the deferral is dropped; the
		// tap half proves the affordance is a live, hit-testable control (not pointer-
		// dead under the stage's `pointer-events: none`). Needs a real mobile project:
		// the policy keys on `viewport.isMobile`.
		let originalRequests = 0;
		await page.route('**/attachments/*', async (route) => {
			const url = new URL(route.request().url());
			if (/\/attachments\/[^/?]+$/.test(url.pathname) && !url.searchParams.get('variant')) {
				originalRequests++;
				return route.fulfill({ contentType: 'image/png', body: BIG_PNG });
			}
			if (url.searchParams.get('variant') === 'thumb-md')
				return route.fulfill({ contentType: 'image/png', body: MID_PNG });
			return route.continue();
		});
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Mobile deferred');
		// A genuinely large image (>8 MP) so mobile classifies it as the DEFERRED
		// cell — a 1.9 MP image is only the mobile thumb cell.
		await uploadAttachment(fixture, request, doc.id, 'mobile-huge.png', 'image/png', HUGE_PNG);
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);

		// The deferred cell: the tap affordance is shown, NO image, NO original fetch.
		await expect(viewerTapLoad(page)).toBeVisible();
		await expect(page.locator(VIEWER_IMAGE)).toHaveCount(0);
		expect(originalRequests, 'no original fetched before the tap').toBe(0);

		// A REAL tap loads the original — the request fires, the bitmap DECODES
		// (naturalWidth settles), and no error state appears.
		await viewerTapLoad(page).tap();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect.poll(() => imageNat(page)).toBe(1600); // the stubbed original decoded
		expect(await imageSrc(page)).not.toContain('variant=');
		await expect(page.locator(`${VIEWER} .lightbox-error`)).toHaveCount(0);
		// EXACTLY one original request — the tap loads it once (dedup), not zero and
		// not a double-fetch.
		expect(originalRequests, 'the tap issued exactly one original request').toBe(1);
	});
});
