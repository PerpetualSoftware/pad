import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	DESKTOP,
	TILE,
	VIEWER,
	itemUrl,
	uploadAttachment,
	viewerDialog
} from './lib/attachment-viewer';

/**
 * THE TEXT-PREVIEW ARM, IN A REAL BROWSER (IDEA-2712 / GitHub #1169).
 *
 * DR-9's rule again: the interaction work is verified in a browser or it is not
 * verified. This file exists because three of the arm's guarantees are CSS
 * MECHANISMS, and the jsdom unit suite does not inject component styles at all —
 * `getComputedStyle` there returns the engine default for every element, so an
 * assertion like "the layer's pointer-events is none" cannot fail and is not an
 * instrument. Two such assertions were written during codex R1 and deleted for
 * exactly that reason.
 *
 * What lives here, and the escape each one closes:
 *
 *  - BACKDROP CLOSE over the arm. The first version of the arm gave its
 *    full-bleed layer `pointer-events: auto`, so a click on the empty area
 *    targeted the layer rather than the root and the viewer would not close —
 *    for this arm alone, while a comment two lines away claimed it worked. Only
 *    a real engine can hit-test that.
 *  - SCROLLING. The wheel handler consumed every wheel before its exclusions;
 *    the card could not scroll and the page behind must still not. Both halves
 *    need real scroll behaviour and `overscroll-behavior`.
 *  - SELECTION. The backdrop sets `user-select: none` for the pan gesture; the
 *    card opts back out. jsdom has no selection model.
 *
 * The MIME-gating and loader logic are unit-tested; they are not repeated here.
 */

const MD_BODY = `# Preview heading\n\n${'Filler paragraph for scrolling. '.repeat(200)}\n\n## Tail heading\n`;
const TEXT_ARM = `${VIEWER} .lightbox-text`;
const TEXT_CARD = `${VIEWER} .lightbox-text-scroll`;
const TEXT_RENDERED = `${VIEWER} .lightbox-text-body`;

test.describe('attachment viewer — text preview (IDEA-2712)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop legs only');
		await page.setViewportSize(DESKTOP);
	});

	async function openMarkdown(page, fixture, request, title: string) {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, title);
		await uploadAttachment(
			fixture,
			request,
			doc.id,
			'preview.md',
			'text/markdown',
			Buffer.from(MD_BODY, 'utf8')
		);
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(TEXT_CARD)).toBeVisible();
	}

	test('renders the markdown, through the shared pipeline', async ({ page, fixture, request }) => {
		await openMarkdown(page, fixture, request, 'Text preview render');
		// A real heading element, not a literal '#': the pipeline ran.
		await expect(page.locator(`${TEXT_RENDERED} h1`)).toHaveText('Preview heading');
		// The no-preview fallback must NOT be painted over it — the negative
		// control for the fallback arm's condition.
		await expect(page.locator(`${VIEWER} .lightbox-fallback`)).toHaveCount(0);
	});

	test('BACKDROP CLICK over the arm still closes the viewer', async ({
		page,
		fixture,
		request
	}) => {
		await openMarkdown(page, fixture, request, 'Text preview backdrop');
		const dialog = viewerDialog(page, 'preview.md');
		await expect(dialog).toBeVisible();

		// Click the far top-left of the stage — inside the arm's full-bleed layer,
		// outside the centred card. With the layer interactive this hits the layer
		// and nothing happens; with it inert the root receives it and closes.
		const box = await page.locator(TEXT_ARM).boundingBox();
		if (!box) throw new Error('text arm has no box');
		await page.mouse.click(box.x + 4, box.y + 4);

		await expect(dialog).toHaveCount(0);
	});

	test('the CARD scrolls, and the SURFACE BEHIND does NOT', async ({
		page,
		fixture,
		request
	}) => {
		await openMarkdown(page, fixture, request, 'Text preview scroll');
		const card = page.locator(TEXT_CARD);

		// THE RIGHT SCROLLER (codex R2 #5). The app does not scroll the window —
		// `.main-content` is the `overflow-y: auto` element (`+layout.svelte`), so
		// asserting `window.scrollY` would have held still whatever the wheel
		// handler did, and the leak this test exists for would have passed. Read
		// both, and fail if EITHER moves.
		const behind = () =>
			page.evaluate(() => ({
				win: window.scrollY,
				main: document.querySelector('.main-content')?.scrollTop ?? 0
			}));
		const before = await behind();
		expect(await card.evaluate((el) => el.scrollTop)).toBe(0);

		await card.hover();
		await page.mouse.wheel(0, 600);
		await expect
			.poll(async () => card.evaluate((el) => el.scrollTop), { timeout: 2000 })
			.toBeGreaterThan(0);

		// The other half of the guarantee: the wheel handler stopped consuming this
		// event, so `overscroll-behavior: contain` is what keeps the surface behind
		// still. Scroll to the very end and keep going, which is when chaining
		// would kick in.
		await card.evaluate((el) => {
			el.scrollTop = el.scrollHeight;
		});
		await page.mouse.wheel(0, 1200);
		expect(await behind()).toEqual(before);
	});

	test('the document is SELECTABLE and touch-scrollable, unlike the pan surface', async ({
		page,
		fixture,
		request
	}) => {
		await openMarkdown(page, fixture, request, 'Text preview selection');

		// A `Range` + `addRange()` selects text even under `user-select: none`
		// (codex R2 #4) — the first version of this test did exactly that and
		// could not fail. The computed style IS the mechanism, and unlike jsdom a
		// real engine actually applies it, so reading it here is a measurement
		// rather than an engine default.
		const style = await page.locator(TEXT_CARD).evaluate((el) => {
			const cs = getComputedStyle(el);
			return { userSelect: cs.userSelect || cs.webkitUserSelect, touchAction: cs.touchAction };
		});
		expect(style.userSelect).toBe('text');

		// `touch-action` INTERSECTS down the ancestor chain, so the card's `pan-y`
		// is worthless if an ancestor still says `none`. Assert the effective
		// chain, not just this element: the stage must have given up its claim.
		expect(style.touchAction).toBe('pan-y');
		const stage = await page
			.locator(`${VIEWER} .lightbox-stage`)
			.evaluate((el) => getComputedStyle(el).touchAction);
		expect(stage).not.toBe('none');

		// The control leg: the backdrop around the card still refuses selection,
		// so the card opted out for itself rather than the rule being dropped.
		const backdrop = await page
			.locator(VIEWER)
			.evaluate((el) => getComputedStyle(el).userSelect || getComputedStyle(el).webkitUserSelect);
		expect(backdrop).toBe('none');
	});
});
