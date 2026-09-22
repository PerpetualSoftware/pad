import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, APIResponse, Page, Route } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3143 — the item pane's tag saver must not overwrite a concurrent tag
 * change.
 *
 * It used to PATCH the whole tag set with no version token, so another writer's
 * tag disappeared with no conflict anywhere. It now sends the row's token (so a
 * write that landed since is refused, 409) and sends the burst's GESTURE applied
 * to the row that token names, on every batch — not only after a conflict, since
 * a batch after a 409 goes out with a current token and would otherwise replay
 * a display that never showed the other writer's tag.
 *
 * Every conflict here is REAL: the other writer's PATCH goes to the server from
 * inside our request's route handler, before our request is let through.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext, tag: string) {
	const ws = `/api/v1/workspaces/${fixture.workspaceSlug}`;
	const coll = await request.post(`${ws}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `B3143 ${tag} ${Date.now()}`, prefix: 'BTG', schema: JSON.stringify({ fields: [] }) },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = await coll.json();
	const created = await request.post(`${ws}/collections/${collSlug}/items`, {
		headers: authHeaders(fixture),
		data: { title: `B3143 ${tag} ${Date.now()}`, fields: JSON.stringify({}), content: '' },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = (await created.json()) as { id: string; slug: string };
	const tagged = await request.patch(`${ws}/items/${item.id}`, {
		headers: authHeaders(fixture),
		data: { tags: JSON.stringify(['ta', 'tb']) },
	});
	expect(tagged.ok(), await tagged.text()).toBeTruthy();
	return { collSlug, item };
}

async function serverTags(fixture: SuiteFixture, request: APIRequestContext, id: string): Promise<string[]> {
	const row = await (
		await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, { headers: authHeaders(fixture) })
	).json();
	return JSON.parse(row.tags || '[]');
}

function tagChips(page: Page) {
	return page.locator('.item-page-host > .item-page').locator('.tag-input .tag-chip');
}

function isTagPatch(route: Route, itemId: string) {
	return (
		route.request().method() === 'PATCH' &&
		new URL(route.request().url()).pathname.endsWith(`/items/${itemId}`) &&
		'tags' in JSON.parse(route.request().postData() ?? '{}')
	);
}

test.describe('BUG-3143: the tag saver does not overwrite a concurrent tag change', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a write body');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		// No SSE: its re-read would refresh the pane before our edit and there
		// would be no conflict. The conflict is the premise.
		await page.route('**/api/v1/events*', (route) => route.abort());
	});

	test('(a) removing a tag keeps a tag another writer added', async ({ page, fixture, request }) => {
		const { collSlug, item } = await seed(fixture, request, 'a');
		const bodies: string[][] = [];
		let otherWriterRan = false;
		await page.route(`**/api/v1/workspaces/*/items/${item.id}`, async (route) => {
			if (!isTagPatch(route, item.id)) return route.fallback();
			bodies.push(JSON.parse(JSON.parse(route.request().postData()!).tags));
			if (!otherWriterRan) {
				otherWriterRan = true;
				const other = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
					headers: authHeaders(fixture),
					data: { tags: JSON.stringify(['ta', 'tb', 'tc']) },
				});
				expect(other.ok(), await other.text()).toBeTruthy();
			}
			return route.fallback();
		});

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		await expect(tagChips(page)).toHaveCount(2);
		const settled = page.waitForResponse(
			(r) => r.request().method() === 'PATCH' && r.url().includes(`/items/${item.id}`) && r.ok(),
		);
		await page.getByRole('button', { name: 'Remove tb' }).click();
		await settled;

		expect(otherWriterRan, 'PREMISE: the concurrent add ran').toBe(true);
		expect(bodies[0], 'PREMISE: the first attempt was the gesture against the old set').toEqual(['ta']);
		expect(await serverTags(fixture, request, item.id), 'BUG-3143: the tag save erased a concurrent tag').toEqual([
			'ta',
			'tc',
		]);
		// A chip's text carries its remove glyph, so match the tag at its start.
		await expect(tagChips(page)).toHaveText([/^\s*ta\b/, /^\s*tc\b/]);
	});

	test('(b) a later batch of the same burst keeps it too', async ({ page, fixture, request }) => {
		const { collSlug, item } = await seed(fixture, request, 'b');
		const bodies: string[][] = [];
		let otherWriterRan = false;
		let heldRetry: { route: Route; response: APIResponse } | null = null;
		let retryHeld!: () => void;
		const retryIsHeld = new Promise<void>((r) => (retryHeld = r));
		await page.route(`**/api/v1/workspaces/*/items/${item.id}`, async (route) => {
			if (!isTagPatch(route, item.id)) return route.fallback();
			bodies.push(JSON.parse(JSON.parse(route.request().postData()!).tags));
			if (!otherWriterRan) {
				otherWriterRan = true;
				const other = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
					headers: authHeaders(fixture),
					data: { tags: JSON.stringify(['ta', 'tb', 'tc']) },
				});
				expect(other.ok(), await other.text()).toBeTruthy();
				return route.fallback();
			}
			if (!heldRetry) {
				// Batch 1's retry: it commits now, and its answer is held so the
				// user's next edit coalesces into a SECOND batch of this burst.
				heldRetry = { route, response: await route.fetch() };
				retryHeld();
				return;
			}
			return route.fallback();
		});

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		await expect(tagChips(page)).toHaveCount(2);
		await page.getByRole('button', { name: 'Remove tb' }).click();
		await retryIsHeld;
		expect(await serverTags(fixture, request, item.id), 'PREMISE: batch 1 re-derived and kept tc').toEqual([
			'ta',
			'tc',
		]);

		// The display still shows the burst's own set, which never had tc. The
		// user removes ta from it: the next batch's absolute set is [].
		const batch2 = page.waitForResponse(
			(r) =>
				r.request().method() === 'PATCH' &&
				r.url().includes(`/items/${item.id}`) &&
				r.ok() &&
				bodies.length >= 3,
		);
		await page.getByRole('button', { name: 'Remove ta' }).click();
		const held = heldRetry as { route: Route; response: APIResponse } | null;
		await held!.route.fulfill({ response: held!.response });
		await batch2;

		expect(bodies.length, 'PREMISE: the second edit went out as a second batch').toBeGreaterThanOrEqual(3);
		expect(await serverTags(fixture, request, item.id), 'BUG-3143: a later batch erased a concurrent tag').toEqual([
			'tc',
		]);
		await expect(tagChips(page)).toHaveText([/^\s*tc\b/]);
	});
});
