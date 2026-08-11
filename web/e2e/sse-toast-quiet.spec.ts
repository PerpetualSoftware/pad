import { test, expect, suiteFixture } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';

/**
 * BUG-2334 — the cross-actor SSE toast kill switch, pinned from BOTH sides.
 *
 * The shared e2e fixture sets `pad:e2e-quiet-external-toasts` on every
 * context, silencing the "X created: …" info toasts that OTHER specs'
 * seeding used to stack over bottom-right UI (the graph drawer's detail
 * card), racing unrelated clicks suite-wide.
 *
 * Leg 1 proves the flag works where the suite relies on it. Leg 2 is the
 * CONTROL: a context WITHOUT the flag still shows the toast — the real
 * product behavior cannot silently regress behind the suite-wide silence.
 *
 * The quiet leg's anchor is the layout handler's OWN observable: its
 * `item_created` branch fetches the item BY EVENT UUID (`api.items.get`,
 * +layout.svelte) before the toast line runs, and on a bare list URL
 * nothing else issues that exact by-uuid GET (deltaSync uses /changes and
 * slug routes). A pre-attached response LOG (no arm-order race) is polled
 * for that pathname, the SSE stream's own response gates the create (the
 * heading doesn't prove connectedness), and a bounded settle covers the
 * fetch→toast continuation — so a missing toast is real suppression, never
 * a not-yet-arrived event or a row painted by the page's own sync.
 */

const DESKTOP = { width: 1280, height: 900 };

function authHeaders(token: string) {
	return { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
}

async function createTask(
	request: APIRequestContext,
	token: string,
	ws: string,
	title: string,
): Promise<{ id: string }> {
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
		headers: authHeaders(token),
		data: { title, fields: { status: 'open' } },
	});
	if (!resp.ok()) throw new Error(`task create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string };
}

/** The list row for an item title, in the tasks list view. */
function rowFor(page: Page, title: string) {
	return page.getByText(title, { exact: true }).first();
}

/** Any live cross-actor creation toast mentioning the title. */
function creationToast(page: Page, title: string) {
	return page.locator('.toast-container .toast-message', { hasText: `created: ${title}` });
}

test.beforeEach(async ({}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough — DOM-level behavior');
});

test('the shared-fixture flag silences another actor\'s creation toast (row arrives, toast does not)', async ({
	page,
	request,
	fixture,
}) => {
	await page.setViewportSize(DESKTOP);
	// A LISTENER LOG, attached before navigation, is the anchor mechanism:
	// unlike an armed waitForResponse it has no arm-order race — every
	// response is recorded whenever it lands, and we poll the log afterwards.
	const responseLog: string[] = [];
	page.on('response', (r) => {
		if (r.request().method() === 'GET') responseLog.push(new URL(r.url()).pathname);
	});
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`, {
		waitUntil: 'domcontentloaded',
	});
	await expect(page.getByRole('heading', { name: /tasks/i }).first()).toBeVisible();
	// SSE must actually be CONNECTED before the external create — the heading
	// alone doesn't prove it. The stream's response headers land in the log
	// the moment the EventSource opens.
	await expect
		.poll(() => responseLog.some((u) => u.startsWith('/api/v1/events')), { timeout: 15_000 })
		.toBe(true);

	const title = `Quiet toast probe ${Date.now()}`;
	const { id } = await createTask(request, fixture.apiToken, fixture.workspaceSlug, title);
	// The layout's item_created branch fetches the item by EVENT UUID before
	// its toast line; on a bare /tasks list URL nothing else issues that GET
	// (exact-pathname match — /children, /progress etc. don't count).
	await expect
		.poll(() => responseLog.some((u) => u.endsWith(`/items/${id}`)), { timeout: 15_000 })
		.toBe(true);
	// The response landing is not the toast line running — give the branch's
	// json+microtask continuation a bounded, generous settle, then assert the
	// suppression for real.
	await expect(rowFor(page, title)).toBeVisible({ timeout: 15_000 });
	await page.waitForTimeout(1000);
	await expect(creationToast(page, title)).toHaveCount(0);
});

test('CONTROL: an unflagged context still shows the creation toast — the product surface stays pinned', async ({
	browser,
	request,
}) => {
	const fixture = suiteFixture();
	// A context built OUTSIDE the shared fixture: token auth applied the same
	// way, but NO init script — localStorage stays empty, prod behavior.
	const context = await browser.newContext({ viewport: DESKTOP });
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${fixture.apiToken}` });
	const page = await context.newPage();
	try {
		const responseLog: string[] = [];
		page.on('response', (r) => {
			if (r.request().method() === 'GET') responseLog.push(new URL(r.url()).pathname);
		});
		await page.goto(
			`${fixture.baseURL}/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`,
			{ waitUntil: 'domcontentloaded' },
		);
		await expect(page.getByRole('heading', { name: /tasks/i }).first()).toBeVisible();
		await expect
			.poll(() => responseLog.some((u) => u.startsWith('/api/v1/events')), { timeout: 15_000 })
			.toBe(true);

		const title = `Loud toast control ${Date.now()}`;
		await createTask(request, fixture.apiToken, fixture.workspaceSlug, title);

		await expect(rowFor(page, title)).toBeVisible({ timeout: 15_000 });
		await expect(creationToast(page, title)).toBeVisible({ timeout: 5_000 });
	} finally {
		await context.close();
	}
});
