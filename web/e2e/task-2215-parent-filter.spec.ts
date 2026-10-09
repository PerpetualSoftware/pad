import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2215 (audit C35): the parent filter existed only on a collection
 * slugged `tasks`, listing `plans`. Ideas under a plan (any collection, any
 * hierarchy) now get it too, with the parents those items actually have.
 */

async function create(fixture: SuiteFixture, request: APIRequestContext, coll: string, data: Record<string, unknown>) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return (await resp.json()) as { id: string; slug: string; title: string };
}

test.describe('TASK-2215: a parent filter on any collection', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'filter bar UI; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('ideas under a plan can be filtered by it', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const plan = await create(fixture, request, 'plans', { title: `Parent plan ${stamp}`, fields: '{}' });
		await create(fixture, request, 'ideas', { title: `Child idea ${stamp}`, fields: JSON.stringify({ parent: plan.slug }) });
		await create(fixture, request, 'ideas', { title: `Loose idea ${stamp}`, fields: '{}' });

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/ideas?view=list`);
		await expect(page.locator('.item-card').filter({ hasText: `Child idea ${stamp}` })).toBeVisible();
		await page.getByRole('button', { name: 'Toggle filters' }).click();
		const select = page.getByLabel('Filter by parent');
		await expect(select).toBeVisible();
		await expect(select.locator('option').first()).toHaveText('All plans');
		const option = select.locator('option', { hasText: `Parent plan ${stamp}` });
		await expect(option).toHaveCount(1);

		await select.selectOption({ label: (await option.textContent())!.trim() });
		await expect(page.locator('.item-card').filter({ hasText: `Child idea ${stamp}` })).toBeVisible();
		await expect(page.locator('.item-card').filter({ hasText: `Loose idea ${stamp}` })).toHaveCount(0);
		expect(new URL(page.url()).searchParams.get('parent')).toBe(plan.id);
	});
});
