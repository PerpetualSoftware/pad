import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson, createCollection, deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3052 unit 3: the collection page's field filter keeps exactly the items
 * the board puts in that lane.
 *
 * A declared type is not a promise about the stored value: retyping a field
 * rewrites nothing, so a `score` written as the NUMBER 5 is still a number
 * after `score` becomes a select with an option `'5'`. The board groups it
 * under `'5'`; the filter compared `fields.score === '5'` and dropped it, so
 * the card sat in lane 5 and vanished when you filtered for 5. The same for a
 * multi_select array, which a strict `===` against one value never matched.
 *
 * This is the wiring leg: the predicate has unit legs, and only a real page
 * shows the page asks it.
 */

async function createItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
	fields: Record<string, unknown>,
): Promise<void> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authJson(fixture), data: { title, fields: JSON.stringify(fields), content: '' } },
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
}

async function patchCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	slug: string,
	schema: unknown,
	settings?: unknown,
): Promise<void> {
	const data: Record<string, unknown> = { schema: JSON.stringify(schema) };
	if (settings) data.settings = JSON.stringify(settings);
	const resp = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${slug}`, {
		headers: authJson(fixture),
		data,
	});
	if (!resp.ok()) throw new Error(`collection patch failed (${resp.status()}): ${await resp.text()}`);
}

test.describe('a field filter keeps what its lane holds (BUG-3052 unit 3)', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a data-shape concern');
	});

	test('a number under a retyped select, and a multi_select array, are found by the filter', async ({
		page,
		fixture,
		request,
	}) => {
		const stamp = Date.now();
		const coll = await createCollection(fixture, request, `Scores ${stamp}`);
		try {
			const labels = { key: 'labels', label: 'Labels', type: 'multi_select', options: ['a', 'b', 'c'] };
			await patchCollection(fixture, request, coll.slug, {
				fields: [{ key: 'score', label: 'Score', type: 'number' }, labels],
			});
			await createItem(fixture, request, coll.slug, `Five ${stamp}`, { score: 5, labels: ['a', 'b'] });
			await createItem(fixture, request, coll.slug, `Three ${stamp}`, { score: 3, labels: ['b'] });
			await createItem(fixture, request, coll.slug, `Blank ${stamp}`, {});
			// The retype: stored values stay numbers.
			await patchCollection(
				fixture,
				request,
				coll.slug,
				{ fields: [{ key: 'score', label: 'Score', type: 'select', options: ['3', '5'] }, labels] },
				{ board_group_by: 'score' },
			);

			await browserLogin(page);
			const base = `/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?view=board`;
			const cards = page.locator('.kanban-column .item-card');
			const laneWith = (name: string) =>
				page.locator('.kanban-column').filter({ has: page.locator('.column-name', { hasText: new RegExp(`^\\s*${name}\\s*$`) }) });

			// Control: the board really does put the number 5 in lane 5. Without
			// this, a filter answer below could be about a card that was never
			// in that lane.
			await page.goto(base);
			await expect(cards).toHaveCount(3);
			await expect(laneWith('5').locator('.item-card')).toHaveCount(1);
			await expect(laneWith('5').locator('.item-card')).toContainText(`Five ${stamp}`);

			await page.goto(`${base}&score=5`);
			await expect(cards).toHaveCount(1);
			await expect(cards).toContainText(`Five ${stamp}`);

			// A multi_select array matches any one of its values.
			await page.goto(`${base}&labels=b`);
			await expect(cards).toHaveCount(2);
			await page.goto(`${base}&labels=a`);
			await expect(cards).toHaveCount(1);
			await expect(cards).toContainText(`Five ${stamp}`);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});
});
