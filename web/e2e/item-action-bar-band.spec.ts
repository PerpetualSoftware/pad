import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Item action bar (`.meta-actions`) layout invariants.
 *
 * This band was reverted out of PLAN-2326's `.tab-strip` merge and then given
 * two properties by hand: it stays ONE row at every container width, and every
 * labelled control shares one width and height (the ⋯ overflow trigger is the
 * deliberate exception — it sizes to its glyph).
 *
 * The reason this file exists rather than a unit test is the sheet assertion.
 * `.meta-actions` carries `container-type: inline-size` so the narrow-width
 * padding rule can key off the BAND's width — the band lives in a docked pane
 * whose width is user-dragged and decoupled from the viewport, so a media query
 * cannot see it. Both menus in the band render a non-portaled `position: fixed`
 * BottomSheet on mobile, INSIDE that container.
 *
 * That is safe, but only narrowly: `container-type: inline-size` applies style
 * and inline-size containment and NOT layout containment
 * (css-conditional-5 §container-type). Layout containment is what makes an
 * element a containing block for fixed-position descendants, so the sheet keeps
 * resolving against the viewport. Add `contain: layout` to this band — or a
 * transform/filter, which establish a containing block by a different route —
 * and the sheet collapses into a ~342x34 strip instead.
 *
 * Hence a test rather than a comment: the invariant depends on the band NOT
 * acquiring a property that a future styling change could plausibly add, and
 * the failure mode is an unusable mobile sheet rather than anything visible on
 * desktop. Mutation-tested by adding `contain: layout` to `.meta-actions`,
 * which fails this spec on overlay width.
 *
 * (PLAN-2326's DR-3 asserts `container-type: inline-size` implies
 * `contain: layout style inline-size`. It does not. A Codex review reached the
 * opposite conclusion from the same wrong premise. Measure, don't infer.)
 *
 * jsdom computes no layout, so none of this is unit-testable.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seedItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	title: string,
) {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`,
		{
			headers: authHeaders(fixture),
			data: { title, fields: JSON.stringify({ status: 'open' }), content: '' },
		},
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

/** Geometry of every control in the band, in DOM order. */
async function bandControls(page: import('@playwright/test').Page) {
	return page.evaluate(() => {
		const bar = document.querySelector('.meta-actions');
		if (!bar) throw new Error('.meta-actions not found');
		const kids = [...bar.children];
		const box = bar.getBoundingClientRect();
		const controls = kids.map((k) => {
			const btn = (k.matches('button') ? k : k.querySelector('button')) as HTMLElement;
			const r = btn.getBoundingClientRect();
			return {
				isMore: btn.classList.contains('pane-more-btn'),
				w: Math.round(r.width * 10) / 10,
				h: Math.round(r.height * 10) / 10,
				// scrollWidth > clientWidth means the label is being cut off.
				clipped: btn.scrollWidth - btn.clientWidth > 0,
			};
		});
		const tallest = Math.max(...kids.map((k) => k.getBoundingClientRect().height));
		return {
			// A wrap shows up as the band growing taller than its tallest item.
			wrapped: box.height > tallest + 2,
			controls,
		};
	});
}

test('action bar: labelled controls share one width and height; ⋯ is exempt', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'uniform-sizing assertion is about the wide-container case',
	);
	test.setTimeout(30_000);

	const it = await seedItem(fixture, request, 'action bar uniform sizing');
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks/${it.slug}`);
	await expect(page.locator('.title', { hasText: 'action bar uniform sizing' })).toBeVisible();
	await expect(page.locator('.meta-actions')).toBeVisible();

	const { wrapped, controls } = await bandControls(page);
	expect(controls.length, 'band should render at least the star and ⋯ controls').toBeGreaterThan(1);
	expect(wrapped, 'band must never wrap to a second row').toBe(false);

	const labelled = controls.filter((c) => !c.isMore);
	const more = controls.find((c) => c.isMore);
	expect(more, 'the ⋯ overflow trigger should be present').toBeTruthy();

	// One height for every control, ⋯ included.
	const heights = [...new Set(controls.map((c) => c.h))];
	expect(heights, `all controls share one height, got ${heights.join(',')}`).toHaveLength(1);

	// One width for the labelled controls. Guarded to the wide case: a label
	// wider than the 70px basis (e.g. a 3-digit child count) legitimately grows
	// past it rather than truncating, and this seeded item has no such badge.
	const widths = [...new Set(labelled.map((c) => c.w))];
	expect(widths, `labelled controls share one width, got ${widths.join(',')}`).toHaveLength(1);

	// The ⋯ is the deliberate exception: a control, not a labelled value.
	expect(more!.w, '⋯ sizes to its glyph rather than the uniform width').toBeLessThan(widths[0]);

	expect(controls.some((c) => c.clipped), 'no control may clip its label').toBe(false);
});

test('action bar: the mobile bottom sheet is not trapped by the band container', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'mobile-chromium',
		'the BottomSheet only replaces the anchored panel at the mobile breakpoint',
	);
	test.setTimeout(30_000);

	const it = await seedItem(fixture, request, 'action bar sheet containment');
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks/${it.slug}`);
	await expect(page.locator('.title', { hasText: 'action bar sheet containment' })).toBeVisible();

	const band = page.locator('.meta-actions');
	await expect(band).toBeVisible();
	// Establish the premise: the band really is a query container, and really is
	// much smaller than the viewport. Without this, a passing assertion below
	// could just mean the container-type was dropped.
	await expect
		.poll(() => band.evaluate((el) => getComputedStyle(el).containerType))
		.toBe('inline-size');
	const bandBox = (await band.boundingBox())!;
	const vp = page.viewportSize()!;
	expect(bandBox.height).toBeLessThan(vp.height / 4);

	// Both menus in the band get their own pass. They share the Menu +
	// BottomSheet path today, but they are separate wrappers with separate
	// styling (.menu-anchor vs .quick-actions-menu), so asserting one does not
	// establish the other.
	for (const trigger of [
		{ label: '⋯ overflow', sel: 'button.pane-more-btn' },
		{ label: '⚡ quick actions', sel: 'button.trigger-btn[title="Quick actions"]' },
	]) {
		const btn = page.locator(trigger.sel);
		if ((await btn.count()) === 0) {
			// The ⚡ trigger only renders when the collection has quick actions or
			// the viewer is an owner. Fail rather than skip silently: on this
			// fixture the admin IS the owner, so absence means something changed.
			throw new Error(`${trigger.label} trigger not present — expected it on an owner-viewed item`);
		}
		await btn.first().click();

		const overlay = page.locator('.bs-overlay');
		await expect(overlay, `${trigger.label}: sheet opens`).toBeVisible();

		// The overlay must be a DOM descendant of the container — otherwise this
		// test proves nothing about containment (e.g. if BottomSheet ever starts
		// portaling to <body>, delete this test rather than letting it pass
		// vacuously).
		expect(
			await overlay.evaluate((el) => !!el.closest('.meta-actions')),
			`${trigger.label}: sheet is rendered inside .meta-actions — if this fails, BottomSheet now portals and this test is moot`,
		).toBe(true);

		// The actual invariant: despite living inside the query container, the
		// fixed overlay resolves against the viewport.
		const box = (await overlay.boundingBox())!;
		expect(Math.round(box.width), `${trigger.label}: overlay spans the viewport width`).toBe(vp.width);
		expect(Math.round(box.height), `${trigger.label}: overlay spans the viewport height`).toBe(vp.height);
		expect(
			Math.round(box.y),
			`${trigger.label}: overlay starts at the top of the viewport, not at the band`,
		).toBe(0);

		// And the sheet itself is a usable size, not a collapsed strip.
		const sheet = page.locator('.bs-sheet');
		await expect(sheet).toBeVisible();
		const sheetBox = (await sheet.boundingBox())!;
		expect(
			sheetBox.height,
			`${trigger.label}: sheet is a real panel, not a collapsed strip`,
		).toBeGreaterThan(100);

		await page.keyboard.press('Escape');
		await expect(overlay, `${trigger.label}: sheet closes for the next pass`).toHaveCount(0);
	}
});
