// A tab's OLDER content write must not overwrite its NEWER one when it reaches
// the server last (BUG-3080).
//
// The teardown flushes are the case the bug was filed for: two of them can be
// in flight at once, each carrying the whole body, and whichever lands last
// wins. Every pane content PATCH now carries `client_write: {tab, n}`, and the
// server refuses one whose n is below a write it already applied for the same
// (item, tab). That rule is not teardown-specific — it orders every stamped
// write, deliberately — so this drives it with two ordinary collab flushes,
// which is the same writer (`flushCollabContent`) the teardown uses, without
// the native unload dialog in the loop.
//
// The FIRST flush is held BEFORE it reaches the server (route not continued),
// so the server really does receive the older body second. Nothing is
// synthesised: both are the page's own requests.
import { test, expect } from './fixtures';
import type { Route } from '@playwright/test';
import { browserLogin, seedDoc, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';


test('an older flush that reaches the server last is refused, and the newer content stays', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(90_000);
	const first = `first-${Date.now()}`;
	const second = `second-${Date.now()}`;
	const { slug } = await seedDoc(fixture, request, `Order ${first}`);

	let held: Route | null = null;
	const stamps: Array<{ tab: string; n: number }> = [];
	let firstHeld!: () => void;
	const firstIsHeld = new Promise<void>((r) => (firstHeld = r));
	await page.route('**/api/v1/workspaces/*/items/**', async (route) => {
		const req = route.request();
		if (req.method() !== 'PATCH' || !req.url().includes('source=collab-snapshot')) return route.fallback();
		const body = JSON.parse(req.postData() ?? '{}');
		if (body.client_write) stamps.push(body.client_write);
		if (!held) {
			held = route;
			firstHeld();
			return;
		}
		return route.fallback();
	});

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	await editor.click();
	await page.keyboard.type(first);
	await firstIsHeld; // the idle flush for `first` is held before the server

	// A NEWER flush, carrying both markers, goes through and lands first.
	const newerLanded = page.waitForResponse(
		(r) => r.request().method() === 'PATCH' && r.url().includes('source=collab-snapshot') && r.ok(),
		{ timeout: SYNC_TIMEOUT },
	);
	await page.keyboard.type(` ${second}`);
	await newerLanded;
	const read = async () =>
		(await (await page.request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`)).json()) as {
			content: string;
		};
	expect((await read()).content, 'PREMISE: the newer flush landed').toContain(second);

	// Now the OLDER one reaches the server. Its answer is awaited whatever it
	// is, so the OUTCOME is asserted first and the mechanism after — a
	// regression then reports the consequence, not a premise.
	const olderAnswered = page.waitForResponse(
		(r) =>
			r.request().method() === 'PATCH' &&
			r.url().includes('source=collab-snapshot') &&
			r.request() === (held as Route | null)?.request(),
		{ timeout: SYNC_TIMEOUT },
	);
	const route = held as Route | null;
	await route!.continue();
	const older = await olderAnswered;

	expect((await read()).content, 'BUG-3080: the older flush overwrote the newer content').toContain(second);

	// The mechanism: one tab, rising counters, and the older refused as such.
	expect(stamps.length, 'both flushes must be stamped').toBeGreaterThanOrEqual(2);
	expect(stamps[1].tab).toBe(stamps[0].tab);
	expect(stamps[1].n).toBeGreaterThan(stamps[0].n);
	expect(older.status(), 'the older flush must be refused').toBe(409);
	expect((await older.json()).error?.code).toBe('superseded_write');
	// Superseded is success to the pane: no error toast.
	await expect(page.getByText('Failed to save content')).toHaveCount(0);
});
