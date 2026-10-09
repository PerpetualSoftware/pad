import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-2214 (audit C32): priority is settable from a card. The chip opens the
 * same picker the status chip does (BUG-3157), and the choice is stored
 * through the page's per-key patch, so it survives a reload.
 */
test.describe('TASK-2214: set priority from a card', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'card UI; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('the priority chip opens a picker and the choice is stored', async ({ page, fixture, request }) => {
		const title = `Priority card ${Date.now()}`;
		const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
			headers: authJson(fixture),
			data: { title, fields: JSON.stringify({ status: 'open', priority: 'low' }) }
		});
		expect(created.ok(), await created.text()).toBeTruthy();
		const { slug } = (await created.json()) as { slug: string };

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=list`);
		const card = page.locator('.item-card').filter({ hasText: title }).first();
		await expect(card).toBeVisible();
		await card.locator('[title="Change priority"]').click();
		await page.getByRole('menuitemradio', { name: 'High' }).click();
		await expect(card.locator('[title="Change priority"]')).toHaveText(/High/);

		const stored = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, { headers: authJson(fixture) });
		const fields = JSON.parse(((await stored.json()) as { fields: string }).fields);
		expect(fields.priority).toBe('high');
		expect(fields.status).toBe('open');
		// The card stayed where it was: choosing a priority does not open it.
		await expect(page).toHaveURL(/\/tasks\?view=list/);
	});
});
