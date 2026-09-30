import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * PLAN-2348 U3: the History tab fetches only history kinds. It used to share
 * one feed with the comments, so on an item whose newest 50 entries were
 * comments it opened on a page it rendered nothing from — a bare "Load more"
 * (found on TASK-2198 on the live instance: 47 comments + 3 activity rows).
 */
function authJson(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

test.describe('PLAN-2348 U3: History is not paged through comments', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'a paging check; one project is enough');
		test.setTimeout(90_000);
	});

	test('an item with 60 newer comments opens History on its events', async ({ page, fixture, request }) => {
		const h = authJson(fixture);
		const ws = fixture.workspaceSlug;
		const stamp = Date.now();
		const created = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers: h,
			data: { title: `History first page ${stamp}`, fields: JSON.stringify({ status: 'open', priority: 'low' }) }
		});
		expect(created.ok(), await created.text()).toBeTruthy();
		const item = (await created.json()) as { slug: string };
		const upd = await request.patch(`/api/v1/workspaces/${ws}/items/${item.slug}`, {
			headers: h,
			data: { fields_patch: { status: 'in-progress' } }
		});
		expect(upd.ok(), await upd.text()).toBeTruthy();

		// Every comment strictly newer than the events: created_at is whole-second.
		await new Promise((r) => setTimeout(r, 1100));
		for (let i = 0; i < 60; i += 10) {
			await Promise.all(
				Array.from({ length: 10 }, (_, j) =>
					request
						.post(`/api/v1/workspaces/${ws}/items/${item.slug}/comments`, { headers: h, data: { body: `c${i + j}` } })
						.then(async (r) => expect(r.ok(), await r.text()).toBeTruthy())
				)
			);
		}

		// PREMISE: unfiltered, the first page is all comments.
		const first = await request.get(`/api/v1/workspaces/${ws}/items/${item.slug}/timeline`, { headers: h });
		const kinds = ((await first.json()) as { entries: { kind: string }[] }).entries.map((e) => e.kind);
		expect(new Set(kinds), 'the unfiltered first page is not all comments').toEqual(new Set(['comment']));

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${ws}/tasks/${item.slug}`);
		await page.getByRole('tab', { name: /History/ }).click();
		const panel = page.locator('[role="tabpanel"][aria-label="History"]');
		await expect(panel.locator('[data-testid="history-row"]').filter({ hasText: 'created this task' })).toBeVisible({
			timeout: 15_000
		});
		// The update folds into the create's event (one writer, one door, one burst).
		await expect(panel.locator('.field-row[data-field="status"]').first()).toBeVisible();
		await expect(panel.getByRole('button', { name: 'Load more' })).toHaveCount(0);
	});
});
