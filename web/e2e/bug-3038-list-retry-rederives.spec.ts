import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3038 — a conflict retry of a whole-list write must re-apply the user's
 * GESTURE to the fresh row, not re-send the list it computed before.
 *
 * The field holds [A,B,C]. The user removes A, so [B,C] is sent. Another writer
 * adds D first, so our PATCH conflicts (409) and the pane refetches and retries.
 * The retry used to re-send [B,C], erasing D although the gesture only removed
 * A. It now re-derives: fresh [A,B,C,D] minus A is [B,C,D].
 *
 * The conflict is REAL: the other writer's PATCH goes to the server through the
 * API, inside our request's route handler, before our request is let through.
 * So the server itself refuses our first attempt, and everything after that is
 * the product's own retry.
 */

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function post(request: APIRequestContext, fixture: SuiteFixture, path: string, data: unknown) {
	const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}${path}`, {
		headers: authHeaders(fixture),
		data,
	});
	expect(r.ok(), await r.text()).toBeTruthy();
	return r.json();
}

function ownersRow(page: Page) {
	return page.locator('.item-page-host > .item-page').locator('.field-row:has(.field-label:text-is("Owners"))');
}

test('BUG-3038: a conflict retry keeps a list element another writer added', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a retry body');

	await page.setViewportSize(DESKTOP);
	await browserLogin(page);
	// No SSE: its re-read would refresh the pane's copy before our click and
	// there would be no conflict to retry. The conflict is the premise.
	await page.route('**/api/v1/events*', (route) => route.abort());

	const stamp = Date.now();
	const people = await post(request, fixture, '/collections', {
		name: `B3038 people ${stamp}`,
		prefix: 'BPP',
		schema: JSON.stringify({ fields: [] }),
	});
	const person = async (name: string) =>
		(await post(request, fixture, `/collections/${people.slug}/items`, {
			title: `${name} ${stamp}`,
			fields: JSON.stringify({}),
			content: '',
		})) as { id: string };
	const [A, B, C, D] = [await person('P-A'), await person('P-B'), await person('P-C'), await person('P-D')];

	const coll = await post(request, fixture, '/collections', {
		name: `B3038 owned ${stamp}`,
		prefix: 'BOW',
		schema: JSON.stringify({
			fields: [{ key: 'owners', label: 'Owners', type: 'multi_relation', collection: people.slug }],
		}),
	});
	const item = (await post(request, fixture, `/collections/${coll.slug}/items`, {
		title: `B3038 item ${stamp}`,
		fields: JSON.stringify({ owners: [A.id, B.id, C.id] }),
		content: '',
	})) as { id: string; slug: string };

	// Our first PATCH: before it reaches the server, the other writer adds D.
	const sent: string[][] = [];
	let otherWriterRan = false;
	await page.route(`**/api/v1/workspaces/*/items/${item.id}`, async (route) => {
		if (route.request().method() !== 'PATCH') return route.fallback();
		const owners = JSON.parse(route.request().postData() ?? '{}').fields_patch?.owners as string[] | undefined;
		if (owners) sent.push(owners);
		if (owners && !otherWriterRan) {
			otherWriterRan = true;
			const other = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
				headers: authHeaders(fixture),
				data: { fields_patch: { owners: [A.id, B.id, C.id, D.id] } },
			});
			expect(other.ok(), await other.text()).toBeTruthy();
		}
		return route.fallback();
	});

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}/${item.slug}`);
	// One row per ELEMENT: the list's Add control shares the row class, so an
	// element row is the one carrying a Remove button.
	const rows = ownersRow(page).locator('.relation-row').filter({ has: page.getByRole('button', { name: 'Remove' }) });
	await expect(rows).toHaveCount(3);

	const settled = page.waitForResponse(
		(r) => r.request().method() === 'PATCH' && r.url().includes(`/items/${item.id}`) && r.ok(),
	);
	await rows.filter({ hasText: `P-A ${stamp}` }).getByRole('button', { name: 'Remove' }).click();
	await settled;

	// PREMISES: the other writer ran, our first attempt was refused and retried,
	// and the first attempt was the gesture's result against the old list.
	expect(otherWriterRan, 'PREMISE: the concurrent add ran').toBe(true);
	expect(sent.length, 'PREMISE: the first attempt conflicted and was retried').toBeGreaterThanOrEqual(2);
	expect(sent[0], 'PREMISE: the first attempt was [B,C]').toEqual([B.id, C.id]);

	const row = await (
		await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.id}`, {
			headers: authHeaders(fixture),
		})
	).json();
	expect(JSON.parse(row.fields).owners, 'BUG-3038: the retry erased the element another writer added').toEqual([
		B.id,
		C.id,
		D.id,
	]);
	// And the screen shows what the server holds.
	await expect(rows).toHaveCount(3);
	await expect(ownersRow(page)).toContainText(`P-D ${stamp}`);
	await expect(ownersRow(page)).not.toContainText(`P-A ${stamp}`);
});
