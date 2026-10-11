import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-3592: a filtered view says it is filtered. With the filter panel
 * closed, the only sign used to be a dot on the filter button and the heading
 * counted every item, so a filtered view looked like the whole collection.
 * Now a summary line reads "N of M · <filters>" with a Clear, the heading
 * reads "N of M", and the view trigger names the active saved view.
 */
async function seed(request: import('@playwright/test').APIRequestContext, fixture: { workspaceSlug: string }, stamp: string) {
	for (const priority of ['high', 'low']) {
		const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
			headers: authJson(fixture as never),
			data: { title: `T3592 ${priority} ${stamp}`, fields: JSON.stringify({ status: 'open', priority }) },
		});
		expect(r.ok(), await r.text()).toBeTruthy();
	}
}

test('a filtered list shows "N of M", what is filtered and a Clear; Clear restores the whole list', async ({ page, fixture, request }, testInfo) => {
	const stamp = `zfs${Date.now().toString(36)}`;
	await seed(request, fixture, stamp);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=list&priority=high&q=${stamp}`);
	await expect(page.getByText(`T3592 high ${stamp}`)).toBeVisible({ timeout: 15_000 });
	await expect(page.getByText(`T3592 low ${stamp}`)).toHaveCount(0);

	const summary = page.locator('.filter-summary');
	await expect(summary).toBeVisible();
	await expect(summary.locator('.filter-summary-count')).toHaveText(/^1 of \d+$/);
	await expect(summary).toContainText('priority: high');
	await expect(summary).toContainText(`search: “${stamp}”`);
	const isMobile = testInfo.project.name === 'mobile-chromium';
	if (!isMobile) {
		// The heading is hidden at phone width; the summary carries the count there.
		await expect(page.locator('h1 .item-count')).toHaveText(/^1 of \d+$/);
	}

	await summary.getByRole('button', { name: 'Clear' }).click();
	await expect(summary).toBeHidden();
	await expect(page).not.toHaveURL(/priority=high/);
	if (!isMobile) await expect(page.locator('h1 .item-count')).toHaveText(/^\d+$/);
	await expect(page.getByText(`T3592 low ${stamp}`)).toBeVisible();
});

test('the view trigger names the active saved view; the summary shows once the panel closes', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the desktop View menu');
	const stamp = `zfv${Date.now().toString(36)}`;
	await seed(request, fixture, stamp);
	const viewName = `Hot ${stamp}`;
	const v = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/views`, {
		headers: authJson(fixture),
		data: { name: viewName, view_type: 'list', config: JSON.stringify({ filters: [{ field: 'priority', op: 'eq', value: 'high' }] }) },
	});
	expect(v.ok(), await v.text()).toBeTruthy();

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks?view=list`);
	const trigger = page.locator('button.view-dd-trigger');
	await expect(trigger).toContainText('List');
	await expect(page.locator('.filter-summary')).toHaveCount(0);

	await trigger.click();
	await page.getByRole('menuitem', { name: viewName }).click();
	await expect(trigger).toContainText(viewName);
	// Applying a view opens the panel; the summary is for when it is closed.
	await expect(page.locator('.filters-panel')).toBeVisible();
	await expect(page.locator('.filter-summary')).toHaveCount(0);
	await page.getByRole('button', { name: 'Toggle filters' }).click();
	await expect(page.locator('.filter-summary')).toContainText('priority: high');

	await page.locator('.filter-summary').getByRole('button', { name: 'Clear' }).click();
	await expect(trigger).toContainText('List');
	await expect(trigger).not.toContainText(viewName);
});
