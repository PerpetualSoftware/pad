import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2998 / PLAN-2857 U7 — filtering and grouping a collection view by a
 * RELATION field, driven through a real browser against a binary built from
 * this tree (CONVE-34 / CONVE-2729).
 *
 * The unit tests cover the rules and the wiring; what only this leg can show is
 * that the whole path holds together — a schema with a relation field, rows
 * carrying target ids, the local index resolving them, the board deriving lanes
 * from them, and the filter narrowing the list to one target.
 *
 * The design table's proving row is the grouping assertion: every car lands in
 * exactly one lane, and the one pointing at nothing lands in an honest lane
 * rather than under a uuid.
 */

function authJson(fixture: SuiteFixture) {
	return {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json',
	};
}

async function createCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	name: string,
	schema?: unknown,
	settings?: unknown,
): Promise<{ slug: string; prefix: string }> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authJson(fixture),
		data: { name },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	const coll = (await resp.json()) as { slug: string; prefix: string };
	if (schema || settings) {
		const patch: Record<string, unknown> = {};
		if (schema) patch.schema = JSON.stringify(schema);
		if (settings) patch.settings = JSON.stringify(settings);
		const up = await request.patch(
			`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}`,
			{ headers: authJson(fixture), data: patch },
		);
		if (!up.ok()) throw new Error(`collection patch failed (${up.status()}): ${await up.text()}`);
	}
	return coll;
}

async function createItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
	fields: Record<string, unknown> = {},
): Promise<{ id: string; slug: string; item_number: number }> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authJson(fixture), data: { title, fields: JSON.stringify(fields), content: '' } },
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string; item_number: number };
}

/**
 * Best-effort, and it SWALLOWS everything — including the request throwing
 * because the context is already closed after a timeout. The repo's own
 * `deleteCollection` helper carries this warning and I reimplemented it without
 * the protection, so the first failing run reported
 * `apiRequestContext.delete: Target page, context or browser has been closed`
 * and hid the assertion that actually failed. A leaked scratch collection is
 * visible in the warning and harmless; a swallowed assertion failure is not.
 */
async function deleteCollection(fixture: SuiteFixture, request: APIRequestContext, slug: string) {
	try {
		const resp = await request.delete(
			`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${slug}`,
			{ headers: authJson(fixture) },
		);
		if (!resp.ok()) console.warn(`cleanup: collection ${slug} not deleted (${resp.status()})`);
	} catch (err) {
		console.warn(`cleanup: collection ${slug} not deleted: ${String(err)}`);
	}
}

/**
 * NOTE ON WHAT THIS LEG CANNOT COVER, found by trying it. A value that resolves
 * to NOTHING cannot be created through the API any more: U1's validator
 * (TASK-2878) refuses it with
 * `"…" does not name an item in collection "colors-…"`. That lane only exists
 * for LEGACY rows written before U1 landed, so it is covered by the unit tests
 * — where the fixture can hold one — and named here rather than quietly
 * dropped, because "the e2e does not test it" and "the e2e cannot test it" are
 * different facts.
 *
 * The DELETED branch IS reachable, and is the one this leg drives: soft-delete
 * a colour and the local index still holds the row, so the lane can name it
 * honestly instead of losing it.
 */

test.describe('relation filter and group (PLAN-2857 U7)', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'one project is enough for a data-shape concern',
		);
	});

	test('board lanes come from the relation targets, and the filter narrows to one', async ({
		page,
		fixture,
		request,
	}) => {
		const stamp = Date.now();
		const colors = await createCollection(fixture, request, `Colors ${stamp}`);
		const cars = await createCollection(
			fixture,
			request,
			`Cars ${stamp}`,
			{
				fields: [
					{ key: 'car_color', label: 'Colour', type: 'relation', collection: colors.slug },
				],
			},
			{ board_group_by: 'car_color' },
		);

		try {
			const red = await createItem(fixture, request, colors.slug, `Red ${stamp}`);
			const blue = await createItem(fixture, request, colors.slug, `Blue ${stamp}`);
			await createItem(fixture, request, cars.slug, `Ferrari ${stamp}`, { car_color: red.id });
			await createItem(fixture, request, cars.slug, `Bugatti ${stamp}`, { car_color: blue.id });
			await createItem(fixture, request, cars.slug, `Unpainted ${stamp}`);
			// Points at a colour that is then DELETED — the lane must keep
			// naming it rather than folding it in with "no colour".
			const doomed = await createItem(fixture, request, colors.slug, `Doomed ${stamp}`);
			await createItem(fixture, request, cars.slug, `Ghost ${stamp}`, { car_color: doomed.id });
			const del = await request.delete(
				`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doomed.slug}`,
				{ headers: authJson(fixture) },
			);
			if (!del.ok()) throw new Error(`colour delete failed (${del.status()})`);

			await browserLogin(page);
			await page.goto(
				`/${fixture.adminUsername}/${fixture.workspaceSlug}/${cars.slug}?view=board`,
			);

			const laneNames = page.locator('.kanban-column .column-name');
			await expect(laneNames.filter({ hasText: `Red ${stamp}` })).toBeVisible();
			await expect(laneNames.filter({ hasText: `Blue ${stamp}` })).toBeVisible();
			// The deleted colour keeps its own lane, and says what it is.
			const doomedLane = laneNames.filter({ hasText: `Doomed ${stamp}` });
			await expect(doomedLane).toBeVisible();
			await expect(doomedLane).toContainText('(deleted)');
			// And no lane anywhere is labelled with a raw id.
			await expect(page.locator('.board-view')).not.toContainText(red.id);

			// The ref is on the lane, so the label is the chip rather than a
			// bare title — the thing that distinguishes this from grouping by
			// any old text field.
			await expect(
				page.locator('.kanban-column .column-name .lane-ref').first(),
			).toBeVisible();

			// Every car is placed exactly once: four cars, four cards.
			await expect(page.locator('.kanban-column .item-card')).toHaveCount(4);

			// FILTER by Red: the picker is scoped to the colours collection.
			// The bar lives behind the toolbar's filter toggle — found by the
			// first run of this leg timing out on a trigger that was never
			// rendered, not by reading the page.
			await page.getByRole('button', { name: 'Toggle filters' }).click();
			await page.locator('.relation-filter-trigger').click();
			const picker = page.locator('.relation-filter-picker');
			await expect(picker).toBeVisible();
			// role=combobox, not textbox — ItemPicker's input is an ARIA
			// combobox, which is not a textbox to Playwright's role engine.
			await picker.getByRole('combobox').fill(`Red ${stamp}`);
			await picker.getByRole('option', { name: new RegExp(`Red ${stamp}`) }).first().click();

			await expect(page.locator('.relation-filter-trigger')).toContainText(`Red ${stamp}`);
			await expect(page.locator('.kanban-column .item-card')).toHaveCount(1);
			await expect(page.locator('.kanban-column .item-card')).toContainText(`Ferrari ${stamp}`);

			// And clearing restores every card — a filter that could not be
			// cleared would pass every assertion above.
			await page.locator('.relation-filter-clear').click();
			await expect(page.locator('.kanban-column .item-card')).toHaveCount(4);
		} finally {
			await deleteCollection(fixture, request, cars.slug);
			await deleteCollection(fixture, request, colors.slug);
		}
	});
});
