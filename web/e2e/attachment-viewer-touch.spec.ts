import { test, expect } from './fixtures';
import type { SuiteFixture } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	BIG_PNG,
	HUGE_PNG,
	MID_PNG,
	TILE,
	VIEWER,
	VIEWER_IMAGE,
	VIEWER_STAGE,
	WIDE_PNG,
	CdpTouch,
	imageRect,
	itemUrl,
	renderedScale,
	settleScale,
	transformOf,
	uploadAttachment,
	viewerTapLoad
} from './lib/attachment-viewer';

/**
 * TOUCH GESTURES IN A REAL MOBILE BROWSER (PLAN-2392 phase 3d / TASK-2519, the
 * V3 device proof).
 *
 * The unit suite (`Lightbox.svelte.test.ts`) dispatches synthesised
 * `PointerEvent`s at the gesture state machine, and jsdom has no layout, no
 * compositor and no genuine two-pointer sequencing. So it proves the arbitration
 * LOGIC but never that REAL touch — injected through the compositor — reaches
 * that logic, that two fingers actually pinch a painted bitmap, that the anchor
 * math holds against live geometry, or that a `touchCancel` tears the gesture
 * down. Each test here drives Chrome DevTools Protocol touch (see `CdpTouch`) and
 * is written against "what mutation would this catch that the jsdom-equivalent
 * survives?" — named per test.
 *
 * The `mobile-chromium` project (Pixel 7: `isMobile`, `hasTouch`) is the one
 * place these run — the viewer only owns touch there, and CDP touch needs a
 * Chromium session. Every leg skips on desktop.
 *
 * WHAT CDP CANNOT EXPRESS (recorded honestly, proven on hardware by the
 * device-proof checklist DOC, not faked here):
 *  • The 2→1 survivor-pan CONTINUATION. `degradeToPan` calls
 *    `setPointerCapture(survivor)`, and CDP's synthetic touch releases the
 *    survivor's IMPLICIT capture on the next move (a `lostpointercapture` the
 *    handler reads as a teardown) — so the fresh single-finger pan dies before a
 *    move lands. A real digitiser keeps the survivor's implicit capture, so the
 *    pan continues. This leg therefore proves the lift is JUMP-FREE and the pan
 *    ARMS (`.panning`, scale preserved); the continuation is checklist item 3.
 *  • Per-pointer `touchCancel` (all-or-nothing in CDP — see `CdpTouch`).
 */

/** Open the viewer on `buffer` and wait until a bitmap has painted (naturalWidth
 *  settles), so gestures ARM (`paintedGen === loadToken`) and geometry is stable. */
async function openTouch(
	page: Page,
	fixture: SuiteFixture,
	request: APIRequestContext,
	title: string,
	buffer: Buffer = BIG_PNG,
	filename = 'touch.png'
): Promise<void> {
	await browserLogin(page);
	const doc = await seedDoc(fixture, request, title);
	await uploadAttachment(fixture, request, doc.id, filename, 'image/png', buffer);
	await page.goto(itemUrl(fixture, doc.slug));
	await expect(page.locator(TILE).first()).toBeVisible();
	await page.locator(TILE).first().click();
	await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
	await expect
		.poll(() => page.locator(VIEWER_IMAGE).evaluate((el) => (el as HTMLImageElement).naturalWidth))
		.toBeGreaterThan(0);
	// Settle the FULL fit geometry before any gesture measures it: not just the
	// image width but the STAGE box too — on the phone sheet the docked meta /
	// toolbar resolve async (a late metadata HEAD populates the header, changing the
	// dock height and so the shortened stage's y/height), which would leave gesture
	// coordinates and the anchor oracle keyed to a stale rect. Poll every axis of
	// both boxes to a fixpoint (two equal reads).
	let prev = { iw: -1, ih: -1, sx: -1, sy: -1, sh: -1 };
	await expect
		.poll(async () => {
			const r = await imageRect(page);
			const s = await stageBox(page);
			const cur = { iw: r.width, ih: r.height, sx: s.x, sy: s.y, sh: s.h };
			const stable =
				Math.abs(cur.iw - prev.iw) < 0.5 &&
				Math.abs(cur.ih - prev.ih) < 0.5 &&
				Math.abs(cur.sx - prev.sx) < 0.5 &&
				Math.abs(cur.sy - prev.sy) < 0.5 &&
				Math.abs(cur.sh - prev.sh) < 0.5;
			prev = cur;
			return stable;
		})
		.toBe(true);
}

/** The stage box in client coords (the shortened sheet stage on a phone). */
function stageBox(page: Page): Promise<{ x: number; y: number; w: number; h: number }> {
	return page.evaluate((sel) => {
		const s = document.querySelector(sel)!.getBoundingClientRect();
		return { x: s.x, y: s.y, w: s.width, h: s.height };
	}, VIEWER_STAGE);
}

test.describe('attachment viewer — touch gestures (PLAN-2392 3d / TASK-2519)', () => {
	test.beforeEach(async ({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'mobile-chromium',
			'touch gestures are Pixel 7 (isMobile/hasTouch) + CDP only'
		);
	});

	test('a two-finger spread zooms in and a converge zooms back out', async ({ page, fixture, request }) => {
		// The bedrock pinch leg: a genuine two-finger spread must RAISE the rendered
		// scale, and a converge must LOWER it. jsdom has no compositor, so the pinch
		// distance→scale path has never driven a real painted matrix. Disabling the
		// pinch handler leaves the scale pinned — this goes red.
		await openTouch(page, fixture, request, 'Pinch spread/converge');
		const touch = await CdpTouch.attach(page);
		const r = await imageRect(page);
		const cx = r.x + r.width / 2;
		const cy = r.y + r.height / 2;

		expect(await renderedScale(page), 'opens at fit').toBeCloseTo(1, 1);

		// SPREAD: two fingers straddling the centre move apart in one-frame steps.
		await touch.down(0, cx - 25, cy);
		await touch.down(1, cx + 25, cy);
		for (let i = 1; i <= 5; i++) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 22, y: cy },
				{ id: 1, x: cx + 25 + i * 22, y: cy }
			]);
		}
		const spread = await settleScale(page);
		expect(spread, 'a two-finger spread zoomed in').toBeGreaterThan(1.6);

		// CONVERGE: bring them back together — the scale must shrink from the peak.
		for (let i = 5; i >= 1; i--) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 22, y: cy },
				{ id: 1, x: cx + 25 + i * 22, y: cy }
			]);
		}
		const converged = await settleScale(page);
		expect(converged, 'a converge zoomed back out').toBeLessThan(spread - 0.3);
		await touch.lift(0);
		await touch.lift(1);
	});

	test('an off-centre pinch keeps the tracked image point under the moving midpoint (affine anchor oracle)', async ({
		page,
		fixture,
		request
	}) => {
		// THE DISCRIMINATING ORACLE (round-2 P2). A pinch composes anchor-zoom around
		// the previous midpoint PLUS the midpoint translation, so the IMAGE point under
		// the midpoint stays under it as the midpoint both MOVES and the image SCALES.
		// We fix a known image-local point (its fraction of the image rect) from the
		// START midpoint, then drive a simultaneous translate+spread with an OFF-CENTRE
		// midpoint and assert that same fraction, mapped through the FINAL rect, still
		// lands under the final midpoint — within a tight px tolerance.
		//
		// A zoom-around-CENTRE mutant (anchoring to the stage centre) drifts the
		// off-centre point away from the moving midpoint; a translation-ONLY mutant
		// never grows the rect so the fraction can't track a moving midpoint AND the
		// scale-climb assertion fails. jsdom computes no rect, so this has never run.
		await openTouch(page, fixture, request, 'Pinch anchor oracle');
		const touch = await CdpTouch.attach(page);
		const stage = await stageBox(page);
		const stageCx = stage.x + stage.w / 2;
		const stageCy = stage.y + stage.h / 2;

		// Pre-zoom past the vertical-room threshold. On the tall phone-sheet stage the
		// fitted image fills the WIDTH but is letterboxed in HEIGHT, so vertical pan
		// room (and thus vertical midpoint tracking) only exists once the scaled image
		// height exceeds the stage — a few discrete zoom steps clear it, giving BOTH
		// axes room so the oracle can assert X and Y.
		for (let i = 0; i < 5; i++) await page.keyboard.press('+');
		const preScale = await settleScale(page);
		expect(preScale, 'pre-zoomed so both axes have pan room').toBeGreaterThan(2.3);

		const r0 = await imageRect(page);
		// An OFF-CENTRE start midpoint, well away from the stage centre (a centre-anchor
		// mutant must visibly drift the tracked point away from it).
		const M0 = { x: stageCx - 55, y: stageCy - 50 };
		const fx = (M0.x - r0.x) / r0.width;
		const fy = (M0.y - r0.y) / r0.height;
		expect(Math.hypot(M0.x - stageCx, M0.y - stageCy), 'the midpoint is genuinely off-centre').toBeGreaterThan(60);

		// Fingers straddle M0 diagonally (separation ~72).
		const A0 = { id: 0, x: M0.x - 30, y: M0.y - 20 };
		const B0 = { id: 1, x: M0.x + 30, y: M0.y + 20 };
		await touch.down(A0.id, A0.x, A0.y);
		await touch.down(B0.id, B0.x, B0.y);

		// Target: midpoint MOVES by (+50, +45) while the separation grows ~72 → ~150
		// (both a translate AND a spread, simultaneously — the composed invariant).
		const M1 = { x: M0.x + 50, y: M0.y + 45 };
		const A1 = { x: M1.x - 60, y: M1.y - 40 };
		const B1 = { x: M1.x + 60, y: M1.y + 40 };
		const N = 8;
		for (let i = 1; i <= N; i++) {
			const t = i / N;
			await touch.moveAll([
				{ id: 0, x: A0.x + (A1.x - A0.x) * t, y: A0.y + (A1.y - A0.y) * t },
				{ id: 1, x: B0.x + (B1.x - B0.x) * t, y: B0.y + (B1.y - B0.y) * t }
			]);
		}
		const finalScale = await settleScale(page);
		expect(finalScale, 'the spread climbed the scale (rules out a translation-only mutant)').toBeGreaterThan(preScale + 0.6);

		const r1 = await imageRect(page);
		const predX = r1.x + fx * r1.width;
		const predY = r1.y + fy * r1.height;
		// TOL = 6px: the empirical residual is sub-pixel (~0.2px); 6px stays highly
		// discriminating (a centre-anchor mutant drifts this point by >100px) while
		// tolerating discrete-step + integer-touch-coord rounding.
		const TOL = 6;
		expect(Math.abs(predX - M1.x), 'the tracked point stays under the midpoint in X').toBeLessThan(TOL);
		expect(Math.abs(predY - M1.y), 'the tracked point stays under the midpoint in Y').toBeLessThan(TOL);
		await touch.lift(0);
		await touch.lift(1);
	});

	test('a double-tap toggles fit↔actual; a single image tap is inert; a backdrop tap closes', async ({
		page,
		fixture,
		request
	}) => {
		// The touch double-tap detector (pointerup-timing, DOUBLE_TAP_MS=300). Two taps
		// on the image toggle fit↔actual; a lone tap on the image has NO action (it is
		// only the pending first of a possible pair); a tap on the letterbox backdrop
		// closes. jsdom can dispatch the pointers but proves none of the hit-testing —
		// which surface a tap lands on, whether the toggle paints. The control legs
		// (single-tap inert, backdrop tap closes) are what a jsdom-equivalent survives.
		await openTouch(page, fixture, request, 'Double-tap toggle');
		const touch = await CdpTouch.attach(page);
		const r = await imageRect(page);
		const cx = r.x + r.width / 2;
		const cy = r.y + r.height / 2;

		expect(await renderedScale(page), 'opens at fit').toBeCloseTo(1, 1);

		// DOUBLE-TAP → actual (a jump well past fit).
		await touch.tap(cx, cy);
		await touch.tap(cx, cy);
		const actual = await settleScale(page);
		expect(actual, 'a double-tap jumped to actual size').toBeGreaterThan(1.3);

		// DOUBLE-TAP again → back to fit.
		await touch.tap(cx, cy);
		await touch.tap(cx, cy);
		expect(await settleScale(page), 'a second double-tap toggled back to fit').toBeCloseTo(1, 1);

		// CONTROL: a LONE tap on the image is inert — no toggle, viewer stays open.
		// Wait out the double-tap window first so it cannot pair with the toggle above.
		await page.waitForTimeout(400);
		await touch.tap(cx, cy);
		expect(await renderedScale(page), 'a single image tap does not zoom').toBeCloseTo(1, 1);
		await expect(page.locator(VIEWER), 'a single image tap does not close').toHaveCount(1);

		// CONTROL: a tap on the LETTERBOX backdrop (above the fitted image, clear of the
		// top-left counter) closes — a real touch tap, reaching the backdrop through the
		// pointer-events:none stage.
		const stage = await stageBox(page);
		const img = await imageRect(page);
		const bandY = (stage.y + img.top) / 2;
		expect(bandY, 'there is a real letterbox band above the fitted image').toBeLessThan(img.top - 5);
		await touch.tap(cx, bandY);
		await expect(page.locator(VIEWER), 'a backdrop touch tap dismisses').toHaveCount(0);
	});

	test('lifting one finger of a pinch degrades to a pan with no transform jump (2→1)', async ({
		page,
		fixture,
		request
	}) => {
		// The 2→1 degrade. Mid-pinch, lifting ONE finger must hand off to a single-
		// finger pan on the survivor WITHOUT resetting or lurching the transform: the
		// painted matrix is IDENTICAL across the lift, the scale is preserved (not
		// snapped to fit), `.pinching` clears and `.panning` engages. A broken degrade
		// (that clears the gesture, or resets the zoom) fails this.
		//
		// The survivor-pan CONTINUATION is NOT asserted here — CDP's synthetic touch
		// releases the survivor's implicit pointer-capture on the next move, which the
		// handler reads as a teardown, so the fresh pan dies before a move lands (a real
		// digitiser keeps the capture). That continuation is device-checklist item 3;
		// this leg proves everything the emulator CAN: the jump-free hand-off + arming.
		await openTouch(page, fixture, request, 'Pinch 2 to 1 degrade');
		const touch = await CdpTouch.attach(page);
		const r = await imageRect(page);
		const cx = r.x + r.width / 2;
		const cy = r.y + r.height / 2;

		await touch.down(0, cx - 25, cy);
		await touch.down(1, cx + 25, cy);
		for (let i = 1; i <= 3; i++) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 14, y: cy },
				{ id: 1, x: cx + 25 + i * 14, y: cy }
			]);
		}
		const pinchScale = await settleScale(page);
		expect(pinchScale, 'the two-finger pinch zoomed in').toBeGreaterThan(1.5);
		await expect(page.locator(VIEWER_IMAGE)).toHaveClass(/pinching/);
		const before = await transformOf(page);

		// Lift finger A. The degrade must be seamless.
		await touch.lift(0);

		const after = await transformOf(page);
		expect(after, 'the transform does not jump or reset across the lift').toBe(before);
		expect(await renderedScale(page), 'the scale is preserved (not reset to fit)').toBeCloseTo(pinchScale, 1);
		await expect(page.locator(VIEWER_IMAGE), 'the pinch class clears').not.toHaveClass(/pinching/);
		await expect(page.locator(VIEWER_IMAGE), 'a single-finger pan is armed on the survivor').toHaveClass(/panning/);
		await touch.lift(1);
	});

	test('pressing + mid-pinch rebases the baseline so continued spread does not snap back', async ({
		page,
		fixture,
		request
	}) => {
		// The pinch's absolute scale is `startScale * curDist/startDist`. An EXTERNAL
		// zoom mid-pinch (the `+` key) moves `zoom.scale` from outside the pinch, so the
		// pinch must re-seed its baseline (`rebaseDrag`) or the next spread move snaps
		// the scale back toward the pre-`+` value, ignoring the keypress. We spread a
		// little, press `+` (scale jumps), then keep spreading and assert the scale
		// climbs FURTHER from the post-`+` value — never dropping back below it.
		await openTouch(page, fixture, request, 'Pinch plus rebase');
		const touch = await CdpTouch.attach(page);
		const r = await imageRect(page);
		const cx = r.x + r.width / 2;
		const cy = r.y + r.height / 2;

		// Spread to a separation of ~114 (fingers at ±57), scale ~2.3.
		await touch.down(0, cx - 25, cy);
		await touch.down(1, cx + 25, cy);
		for (let i = 1; i <= 2; i++) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 16, y: cy },
				{ id: 1, x: cx + 25 + i * 16, y: cy }
			]);
		}
		const midPinch = await settleScale(page);

		// External zoom mid-pinch (ZOOM_STEP = 1.25) — the scale jumps ~25%.
		await page.keyboard.press('+');
		const afterPlus = await settleScale(page);
		expect(afterPlus, 'the + key raised the scale mid-pinch').toBeGreaterThan(midPinch + 0.1);

		// THE DISCRIMINATOR (round-2 P2, Codex): a TINY continued spread (±57 → ±60, a
		// separation barely wider than at the keypress). WITH the rebase, the pinch's
		// baseline was re-seeded to (afterPlus, currentDist), so this near-zero-delta
		// move holds the scale AT/ABOVE afterPlus. WITHOUT the rebase, the next move
		// recomputes `startScale·curDist/startDist` from the ORIGINAL fit baseline —
		// which SNAPS the scale back toward afterPlus/1.25 (~20% below), discarding the
		// keypress. A big continued spread would let the stale baseline climb PAST
		// afterPlus and mask the snap, so the sample must be taken on a tiny delta.
		await touch.moveAll([
			{ id: 0, x: cx - 60, y: cy },
			{ id: 1, x: cx + 60, y: cy }
		]);
		const afterTiny = await settleScale(page);
		expect(afterTiny, 'a tiny continued spread did NOT snap the scale back below the post-+ value').toBeGreaterThan(afterPlus - 0.15);

		// And a larger continued spread climbs FURTHER from the rebased baseline.
		for (let i = 4; i <= 7; i++) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 18, y: cy },
				{ id: 1, x: cx + 25 + i * 18, y: cy }
			]);
		}
		const finalScale = await settleScale(page);
		expect(finalScale, 'continued spread climbed further from the rebased baseline').toBeGreaterThan(afterPlus + 0.3);
		await touch.lift(0);
		await touch.lift(1);
	});

	test('a touchCancel tears the whole gesture down and the next gesture arms fresh (all-cancel)', async ({
		page,
		fixture,
		request
	}) => {
		// CDP's `touchCancel` is all-or-nothing — it fires `pointercancel` for every
		// active pointer at once (the per-pointer degrade stays a unit proof). A cancel
		// mid-pinch must tear the gesture down cleanly: the transform is LEFT WHERE IT
		// WAS (cancel does not undo the pan/zoom), `.pinching` clears, the viewer stays
		// open — and, critically, the NEXT gesture arms from scratch (a stale founder or
		// latched `pinching` would corrupt or block it). A leaked-state bug survives a
		// jsdom check that only inspects the cancelled gesture.
		await openTouch(page, fixture, request, 'Pinch cancel-all');
		const touch = await CdpTouch.attach(page);
		const r = await imageRect(page);
		const cx = r.x + r.width / 2;
		const cy = r.y + r.height / 2;

		await touch.down(0, cx - 25, cy);
		await touch.down(1, cx + 25, cy);
		for (let i = 1; i <= 3; i++) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 16, y: cy },
				{ id: 1, x: cx + 25 + i * 16, y: cy }
			]);
		}
		const cancelledScale = await settleScale(page);
		expect(cancelledScale, 'the pinch zoomed in before the cancel').toBeGreaterThan(1.5);
		const frozen = await transformOf(page);

		await touch.cancelAll();

		expect(await settleScale(page), 'the transform is stable across the cancel (no undo)').toBeCloseTo(cancelledScale, 1);
		expect(await transformOf(page), 'the painted matrix is left where it was').toBe(frozen);
		await expect(page.locator(VIEWER_IMAGE), 'the pinch class clears on cancel').not.toHaveClass(/pinching/);
		await expect(page.locator(VIEWER), 'the viewer stays open').toHaveCount(1);

		// ARMS FRESH: a brand-new pinch after the cancel must zoom again — proving no
		// stale founder / latched pinch survived the teardown.
		await touch.down(0, cx - 25, cy);
		await touch.down(1, cx + 25, cy);
		for (let i = 1; i <= 4; i++) {
			await touch.moveAll([
				{ id: 0, x: cx - 25 - i * 20, y: cy },
				{ id: 1, x: cx + 25 + i * 20, y: cy }
			]);
		}
		expect(await settleScale(page), 'a fresh pinch after the cancel arms and zooms again').toBeGreaterThan(cancelledScale + 0.3);
		await touch.lift(0);
		await touch.lift(1);
	});

	test('a letterbox touch never arms a pan, and a letterbox tap still closes', async ({ page, fixture, request }) => {
		// The letterbox rule (V2 round-2 P2). A touch that lands on the STAGE letterbox
		// — not the painted image — must NOT arm a pan (the stage is pointer-events:none
		// so the target is the backdrop, and `touchOnImage` gates gesture arming); it
		// stays a native tap-to-close. We zoom in (real pan room), then drag from the
		// letterbox band: `.panning` must NEVER flip and the image transform must not
		// move. Then a clean letterbox tap closes. Only a real hit-test proves this —
		// jsdom can't tell the letterbox from the image.
		await openTouch(page, fixture, request, 'Letterbox no-pan', WIDE_PNG, 'wide.png');
		const touch = await CdpTouch.attach(page);
		const stage = await stageBox(page);
		const img = await imageRect(page);
		const bandY = (stage.y + img.top) / 2; // clear letterbox band above the wide image
		const cx = stage.x + stage.w / 2;
		expect(bandY, 'there is a real letterbox band above the wide image').toBeLessThan(img.top - 5);

		// Zoom in so a pan WOULD be visible if the letterbox wrongly armed one.
		for (let i = 0; i < 3; i++) await page.keyboard.press('+');
		await settleScale(page);
		const before = await transformOf(page);

		// A drag STARTING in the letterbox band, past the drag threshold.
		let sawPanning = false;
		await touch.down(0, cx, bandY);
		for (let i = 1; i <= 6; i++) {
			await touch.move(0, cx + i * 20, bandY);
			if (await page.locator(VIEWER_IMAGE).evaluate((el) => el.classList.contains('panning'))) sawPanning = true;
		}
		await touch.lift(0);
		expect(sawPanning, 'a letterbox drag never armed a pan').toBe(false);
		expect(await transformOf(page), 'the image did not move under a letterbox drag').toBe(before);
		await expect(page.locator(VIEWER), 'the letterbox drag did not close the viewer').toHaveCount(1);

		// A clean letterbox TAP still closes.
		await touch.tap(cx, bandY);
		await expect(page.locator(VIEWER), 'a letterbox tap dismisses').toHaveCount(0);
	});

	test('the backdrop keeps touch-action:auto while the image and stage lock it to none', async ({
		page,
		fixture,
		request
	}) => {
		// The touch-ownership split, on computed style. The image and stage take
		// `touch-action: none` so the viewer's pointer handlers own pan/pinch/double-tap
		// with no native gesture competing; the BACKDROP keeps `touch-action: auto` so a
		// letterbox touch still reaches it as a native tap-to-close. The image/stage half
		// is also asserted by the sheet spec; the backdrop:auto half is this leg's own
		// (a regression locking the backdrop would silently kill the letterbox tap).
		await openTouch(page, fixture, request, 'Touch-action split');
		const facts = await page.evaluate((viewerSel) => {
			const root = document.querySelector(viewerSel) as HTMLElement | null;
			const img = document.querySelector(`${viewerSel} .lightbox-image`) as HTMLElement | null;
			const stage = document.querySelector(`${viewerSel} .lightbox-stage`) as HTMLElement | null;
			return {
				backdrop: root ? getComputedStyle(root).touchAction : null,
				img: img ? getComputedStyle(img).touchAction : null,
				stage: stage ? getComputedStyle(stage).touchAction : null
			};
		}, VIEWER);
		expect(facts.img, 'the image locks touch-action to none').toBe('none');
		expect(facts.stage, 'the stage locks touch-action to none').toBe('none');
		expect(facts.backdrop, 'the backdrop keeps touch-action auto for the native tap-to-close').toBe('auto');
	});

	test('tap-to-load takes first-tap priority over gesture arming (deferred cell)', async ({
		page,
		fixture,
		request
	}) => {
		// DR-5b mobile deferral meets touch (TASK-2460 / TASK-2518). A large image shows
		// the tap-to-load affordance and fetches NOTHING until tapped. The first touch on
		// that button must LOAD the original (chrome-excluded, so it never arms a pan /
		// double-tap) — first-tap priority. A regression that let the gesture machine
		// swallow the tap would leave the original unfetched. Real touch + a real
		// network count is the only proof.
		let originalRequests = 0;
		await page.route('**/attachments/*', async (route) => {
			const url = new URL(route.request().url());
			if (/\/attachments\/[^/?]+$/.test(url.pathname) && !url.searchParams.get('variant')) {
				if (route.request().method() !== 'GET') return route.continue();
				originalRequests++;
				return route.fulfill({ contentType: 'image/png', body: HUGE_PNG });
			}
			if (url.searchParams.get('variant') === 'thumb-md')
				return route.fulfill({ contentType: 'image/png', body: MID_PNG });
			return route.continue();
		});
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Tap-to-load first-tap');
		// >8 MP so the mobile classifier defers it (tap affordance, no auto fetch).
		await uploadAttachment(fixture, request, doc.id, 'deferred.png', 'image/png', HUGE_PNG);
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(viewerTapLoad(page)).toBeVisible();
		expect(originalRequests, 'no original fetched before the tap').toBe(0);

		// A REAL CDP touch tap on the affordance loads the original exactly once.
		const touch = await CdpTouch.attach(page);
		const b = (await viewerTapLoad(page).boundingBox())!;
		await touch.tap(b.x + b.width / 2, b.y + b.height / 2);
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();
		await expect.poll(() => page.locator(VIEWER_IMAGE).evaluate((el) => (el as HTMLImageElement).naturalWidth)).toBeGreaterThan(0);
		await expect(page.locator(`${VIEWER} .lightbox-error`)).toHaveCount(0);
		expect(originalRequests, 'the tap issued exactly one original request').toBe(1);
	});
});
