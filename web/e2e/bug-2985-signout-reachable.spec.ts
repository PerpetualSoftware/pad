import { expect } from '@playwright/test';
import { test } from './fixtures';

/**
 * BUG-2985 — "Sign out" has to be INSIDE the panel, not merely in the DOM.
 *
 * The user menu is taller than the Menu primitive's 340px cap, which used to
 * be the scroller. The panel's bottom edge then looked like the end of the
 * menu: overlay scrollbars show nothing until you scroll, so there was no cue
 * that "Connect a project…" and "Sign out" existed below it. Measured before
 * the fix at 393px of content in a 338px client box, with Sign out laid out at
 * y=409 while the box ended at 384 — at every viewport height tested, 900 down
 * to 600, because the cap is fixed rather than viewport-relative.
 *
 * WHY `toBeVisible()` IS NOT THE ASSERTION, which is the whole reason this spec
 * is geometric: an element scrolled out of an `overflow: auto` container still
 * has a non-empty box and is not `display:none`, so Playwright calls it
 * visible. The pre-fix menu would have PASSED a visibility check. What was
 * wrong is where it sat relative to the panel, so that is what is asserted.
 */
test('BUG-2985: Sign out renders within the user menu panel, not below its edge', async ({
	page,
	request,
	fixture,
}) => {
	const auth = { Authorization: `Bearer ${fixture.apiToken}` };
	const slug = `b2985-${test.info().workerIndex}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
	const wsResp = await request.post('/api/v1/workspaces', {
		headers: auth,
		data: { name: 'BUG-2985 probe', slug, template: 'startup' },
	});
	expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
	const ws = (await wsResp.json()) as { slug: string };

	await page.goto(`/${fixture.adminUsername}/${ws.slug}/tasks`);

	// SKIPPED BY PROJECT NAME, not by probing the DOM. The first version asked
	// `trigger.count() === 0` immediately after `goto`, which races hydration:
	// it read 0 on a page that renders the trigger a moment later and skipped
	// BOTH projects, so the spec reported `2 skipped` with exit 0 — a green
	// gate covering nothing. A skip condition has to be something the run
	// KNOWS, not something it might be early to observe.
	test.skip(
		test.info().project.name.includes('mobile'),
		'desktop top bar only — mobile signs out through YouSheet, a different surface',
	);

	const trigger = page.locator('[aria-label="User menu"]');
	await expect(trigger).toBeVisible({ timeout: 15_000 });
	await trigger.click();

	const panel = page.locator('.menu-panel').filter({ hasText: 'Sign out' }).first();
	await expect(panel).toBeVisible();
	const signOut = page.getByRole('menuitem', { name: 'Sign out' });
	await expect(signOut).toBeVisible();

	const panelBox = await panel.boundingBox();
	const signOutBox = await signOut.boundingBox();
	expect(panelBox, 'no panel box').not.toBeNull();
	expect(signOutBox, 'no sign-out box').not.toBeNull();

	// THE LEG: the item's bottom edge is inside the panel's. A 1px slack for
	// sub-pixel layout; the pre-fix gap was 59px, so nothing about this is
	// close to the tolerance.
	expect(signOutBox!.y + signOutBox!.height).toBeLessThanOrEqual(
		panelBox!.y + panelBox!.height + 1,
	);

	// And it WORKS from there — the point is a reachable control, not a
	// well-placed one. Without this, a fix that pinned an inert row would pass.
	await signOut.click();
	await page.waitForURL(/\/login(\?|$)/, { timeout: 15_000 });
});
