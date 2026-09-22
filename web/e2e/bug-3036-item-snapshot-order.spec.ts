import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, APIResponse, Page, Route } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3036 — the item pane must not put an OLDER row on screen over a newer one.
 *
 * Every writer in the pane answers with the whole row and assigns it to `item`.
 * Per-field ordering (fieldWriteOrder.ts) cannot see across fields, so a
 * response that was merely SLOW used to revert a newer write to a different
 * field on screen, while the server kept it. The pane now refuses a snapshot of
 * the shown item whose `seq` is strictly below the one on screen.
 *
 * The second half: an SSE change that arrived while a save was in flight used
 * to be SKIPPED, so the pane stayed behind the server until something unrelated
 * refreshed it. It is now owed, and re-read once the save settles.
 *
 * Every hold below is a REAL request: it goes to the real server with
 * `route.fetch()` and commits there. Only the delivery of the response to the
 * page is delayed. So each response is a genuine older row, not one the test
 * made up.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext, tag: string) {
	const schema = JSON.stringify({
		fields: [
			{ key: 'alpha', label: 'Alpha', type: 'text' },
			{ key: 'beta', label: 'Beta', type: 'text' },
			{ key: 'gamma', label: 'Gamma', type: 'text' },
		],
	});
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `B3036 ${tag} ${Date.now()}`, prefix: 'BSO', schema },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = await coll.json();
	const created = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{
			headers: authHeaders(fixture),
			data: {
				title: `B3036 ${tag} ${Date.now()}`,
				fields: JSON.stringify({ alpha: 'alpha-0', beta: 'beta-0', gamma: 'gamma-0' }),
				content: '',
			},
		},
	);
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = (await created.json()) as { id: string; slug: string };
	return { collSlug, item };
}

async function serverFields(fixture: SuiteFixture, request: APIRequestContext, id: string) {
	const row = await (
		await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, {
			headers: authHeaders(fixture),
		})
	).json();
	return { fields: JSON.parse(row.fields) as Record<string, string>, seq: row.seq as number };
}

function fieldInput(page: Page, label: string) {
	return page
		.locator('.item-page-host > .item-page')
		.locator(`.field-row:has(.field-label:text-is("${label}"))`)
		.locator('input.field-input');
}

/** A response held after its request committed on the real server. */
type Held = { route: Route; response: APIResponse };

function isItemPatch(route: Route, itemId: string) {
	return route.request().method() === 'PATCH' && new URL(route.request().url()).pathname.endsWith(`/items/${itemId}`);
}

function patchedKeys(route: Route): string[] {
	return Object.keys(JSON.parse(route.request().postData() ?? '{}').fields_patch ?? {});
}

test.describe('BUG-3036: whole-item snapshots are ordered by the row seq', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for response ordering');
	});

	test('a slow field response does not revert a newer write to another field', async ({ page, fixture, request }) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		// No SSE: its re-read would repair the screen and hide the defect. This
		// test is about the PATCH responses alone. The SSE re-read has its own
		// test below.
		await page.route('**/api/v1/events*', (route) => route.abort());
		const { collSlug, item } = await seed(fixture, request, 'patch');

		let heldAlpha: Held | null = null;
		const alphaCommitted = new Promise<void>((resolve) => {
			void page.route(`**/api/v1/workspaces/*/items/${item.id}`, async (route) => {
				if (!isItemPatch(route, item.id) || heldAlpha || !patchedKeys(route).includes('alpha')) {
					return route.fallback();
				}
				// The write commits on the server now. Its answer is held.
				heldAlpha = { route, response: await route.fetch() };
				resolve();
			});
		});

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		const alpha = fieldInput(page, 'Alpha');
		const beta = fieldInput(page, 'Beta');
		await expect(alpha).toHaveValue('alpha-0');

		await alpha.fill('alpha-1');
		await alpha.blur();
		await alphaCommitted;
		const afterAlpha = await serverFields(fixture, request, item.id);
		expect(afterAlpha.fields.alpha, 'PREMISE: the held alpha write committed').toBe('alpha-1');

		// The newer write, to a DIFFERENT field. Its response arrives first.
		// Beta's FIRST attempt is refused 409: its token is the seq on screen,
		// which is older than alpha's held commit. The pane refetches and retries,
		// so wait for the attempt that SUCCEEDED.
		const betaResponse = page.waitForResponse(
			(r) =>
				r.request().method() === 'PATCH' &&
				r.url().includes(`/items/${item.id}`) &&
				r.ok() &&
				'beta' in (JSON.parse(r.request().postData() ?? '{}').fields_patch ?? {}),
		);
		await beta.fill('beta-1');
		await beta.blur();
		expect((await betaResponse).ok()).toBeTruthy();
		await expect(beta, 'PREMISE: the newer write rendered').toHaveValue('beta-1');
		const afterBeta = await serverFields(fixture, request, item.id);
		expect(afterBeta.seq, 'PREMISE: the held response is a genuinely older row').toBeGreaterThan(afterAlpha.seq);

		// Deliver the OLDER response now. Before the fix it assigned the whole
		// item, with beta still at beta-0.
		const held = heldAlpha as Held | null;
		expect(held, 'PREMISE: alpha response was held').not.toBeNull();
		const delivered = page.waitForResponse(
			(r) => r.request().method() === 'PATCH' && r.url().includes(`/items/${item.id}`),
		);
		await held!.route.fulfill({ response: held!.response });
		await delivered;
		// The response is delivered; give the pane's continuation a moment to run
		// before asserting the screen did NOT change. (An absence assertion, so
		// the skip mutant below is what shows this leg can go red.)
		await page.waitForTimeout(300);

		await expect(beta, 'BUG-3036: a slow response reverted a newer write on screen').toHaveValue('beta-1');
		await expect(alpha).toHaveValue('alpha-1');
		const final = await serverFields(fixture, request, item.id);
		expect(final.fields).toMatchObject({ alpha: 'alpha-1', beta: 'beta-1' });
	});

	test('a slow SSE re-read does not revert a field written while it was in flight', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		// THE EVENT STREAM IS THE TEST'S. The window this leg needs is narrow on a
		// live stream: beta's own SSE event bumps the pane's itemGen, and that
		// already drops the older re-read if it lands first (measured: with the
		// guard removed, this leg passed on a live stream). The defect is the
		// re-read resolving AFTER beta's response renders and BEFORE beta's event,
		// so the stream is held, answered once with the one event this leg is
		// about, and held again on every reconnect (`retry` keeps the browser from
		// reconnecting inside the test anyway).
		const streams: Route[] = [];
		await page.route('**/api/v1/events*', (route) => {
			streams.push(route);
		});

		const { collSlug, item } = await seed(fixture, request, 'sse');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		const beta = fieldInput(page, 'Beta');
		const gamma = fieldInput(page, 'Gamma');
		await expect(gamma).toHaveValue('gamma-0');
		await expect.poll(() => streams.length, { message: 'PREMISE: the pane opened its event stream' }).toBeGreaterThan(0);

		// Hold the pane's re-reads (it reads by SLUG), armed only now so the
		// load's own GET is not caught.
		const heldReads: Held[] = [];
		await page.route(`**/api/v1/workspaces/*/items/${item.slug}`, async (route) => {
			if (route.request().method() !== 'GET') return route.fallback();
			heldReads.push({ route, response: await route.fetch() });
		});
		// The same event also makes the sync service pull a delta (`/changes`)
		// and the local index pull its own (`/items-changes`). The sync pass bumps
		// the pane's itemGen and installs the row, which drops the re-read above by
		// the older fence before it can land (measured: bump, then install at the
		// incremental path). Hold both, so the SSE re-read is the one being tested.
		// The incremental path's own seq check is exercised when they are released
		// at the end.
		const heldDeltas: Held[] = [];
		const DELTAS = ['**/api/v1/workspaces/*/items-changes*', '**/api/v1/workspaces/*/changes*'];
		for (const pattern of DELTAS) {
			await page.context().route(pattern, async (route) => {
				heldDeltas.push({ route, response: await route.fetch() });
			});
		}

		// Someone else writes gamma for real, and the pane hears about it.
		const external = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
			headers: authHeaders(fixture),
			data: { fields_patch: { gamma: 'gamma-ext' } },
		});
		expect(external.ok(), await external.text()).toBeTruthy();
		const ext = await external.json();
		const event = { type: 'item_updated', item_id: item.id, actor: 'user', source: 'api', timestamp: Date.now(), seq: ext.seq };
		await streams[streams.length - 1].fulfill({
			status: 200,
			headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' },
			body: `retry: 600000\n\nevent: item_updated\ndata: ${JSON.stringify(event)}\n\n`,
		});
		// The re-read it causes is served from the row as it stands NOW — gamma-ext,
		// beta-0 — and held.
		await expect.poll(() => heldReads.length, { message: 'PREMISE: the event made the pane re-read' }).toBeGreaterThan(0);
		const olderCount = heldReads.length;

		// A newer write lands while that read is in flight. Beta's FIRST attempt
		// may be refused 409 (its token can predate the external write); the pane
		// refetches by id and retries, so wait for the attempt that SUCCEEDED.
		const betaResponse = page.waitForResponse(
			(r) =>
				r.request().method() === 'PATCH' &&
				r.url().includes(`/items/${item.id}`) &&
				r.ok() &&
				'beta' in (JSON.parse(r.request().postData() ?? '{}').fields_patch ?? {}),
		);
		await beta.fill('beta-1');
		await beta.blur();
		await betaResponse;
		await expect(beta, 'PREMISE: the newer write rendered').toHaveValue('beta-1');
		// beta's response carries the external write: the server merged beta onto it.
		await expect(gamma, 'PREMISE: the external write is on screen').toHaveValue('gamma-ext');

		// Deliver the older read(s) — the row from BEFORE beta.
		for (const h of heldReads.splice(0, olderCount)) await h.route.fulfill({ response: h.response });
		// An absence assertion: give the continuation a moment, then assert the
		// screen did NOT change. The mutant run is what shows it can go red.
		await page.waitForTimeout(500);

		await expect(beta, 'BUG-3036: a slow SSE re-read reverted a newer write on screen').toHaveValue('beta-1');
		await expect(gamma).toHaveValue('gamma-ext');

		// Release everything else, the held deltas included: those are older rows
		// too, and the incremental path must refuse them as well.
		for (const h of heldReads.splice(0)) await h.route.fulfill({ response: h.response });
		for (const h of heldDeltas.splice(0)) await h.route.fulfill({ response: h.response });
		await page.unroute(`**/api/v1/workspaces/*/items/${item.slug}`);
		for (const pattern of DELTAS) await page.context().unroute(pattern);
		await page.waitForTimeout(500);
		await expect(beta, 'BUG-3036: an older delta reverted a newer write on screen').toHaveValue('beta-1');
	});

	test('a change that arrives during a save is shown once the save settles', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const { collSlug, item } = await seed(fixture, request, 'owed');

		let heldAlpha: Held | null = null;
		const alphaCommitted = new Promise<void>((resolve) => {
			void page.route(`**/api/v1/workspaces/*/items/${item.id}`, async (route) => {
				if (!isItemPatch(route, item.id) || heldAlpha || !patchedKeys(route).includes('alpha')) {
					return route.fallback();
				}
				heldAlpha = { route, response: await route.fetch() };
				resolve();
			});
		});

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		const alpha = fieldInput(page, 'Alpha');
		const gamma = fieldInput(page, 'Gamma');
		await expect(gamma).toHaveValue('gamma-0');

		// A save is in flight: committed, answer held, so the pane is 'saving'.
		await alpha.fill('alpha-1');
		await alpha.blur();
		await alphaCommitted;
		await expect(page.locator('.item-page-host .save-status')).toContainText('Saving');

		// Someone else writes gamma during the save, and its SSE event reaches the
		// pane while it is still saving. That delivery cannot be observed from here
		// (a deferred event issues no request), so the wait is a plain delay. What
		// shows the event DID land mid-save is the counterfactual: had it arrived
		// after the save settled, the ordinary re-read would show gamma-ext with
		// skipping restored too, and the skip mutant would survive. It is run red
		// for exactly that reason.
		const external = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
			headers: authHeaders(fixture),
			data: { fields_patch: { gamma: 'gamma-ext' } },
		});
		expect(external.ok(), await external.text()).toBeTruthy();
		await page.waitForTimeout(1500);
		await expect(gamma, 'PREMISE: the change is not shown while the save is in flight').toHaveValue('gamma-0');

		// The save settles with its (older) row, which lacks gamma-ext.
		const held = heldAlpha as Held | null;
		await held!.route.fulfill({ response: held!.response });

		await expect(gamma, 'BUG-3036: a change skipped during a save was never shown').toHaveValue('gamma-ext', {
			timeout: 5000,
		});
		await expect(alpha).toHaveValue('alpha-1');
	});
});
