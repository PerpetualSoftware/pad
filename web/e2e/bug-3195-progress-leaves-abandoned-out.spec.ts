import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3195 — an abandoned child (cancelled) is left out of a parent's progress:
 * out of the done count AND the total. One done task and one cancelled task
 * used to read 2/2, 100%.
 *
 * Driven through the embedded web UI, because the numbers a user sees come
 * from the server's /progress and pass through ItemDetail into the ChildItems
 * header. A unit test of either end cannot see that wiring.
 *
 * - mixed plan: done + cancelled → the header reads 1/1 done.
 * - abandoned plan: cancelled only → 0/0, which renders as no progress: the
 *   header shows the child count alone.
 */

const DESKTOP = { width: 1200, height: 900 };

async function create(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collection: string,
	title: string,
	fields: Record<string, unknown>,
): Promise<{ id: string; slug: string }> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collection}/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}` },
		data: { title, fields: JSON.stringify(fields), content: '' },
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return (await resp.json()) as { id: string; slug: string };
}

test('BUG-3195: a cancelled child is out of the parent progress', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one layout is enough for a count');
	await page.setViewportSize(DESKTOP);

	const stamp = Date.now();
	const mixed = await create(fixture, request, 'plans', `B3195 mixed ${stamp}`, {});
	await create(fixture, request, 'tasks', `B3195 done ${stamp}`, { status: 'done', parent: mixed.id });
	await create(fixture, request, 'tasks', `B3195 cancelled ${stamp}`, { status: 'cancelled', parent: mixed.id });
	const abandoned = await create(fixture, request, 'plans', `B3195 abandoned ${stamp}`, {});
	await create(fixture, request, 'tasks', `B3195 only cancelled ${stamp}`, { status: 'cancelled', parent: abandoned.id });

	// The server's own numbers first, so a UI failure below is known to be the
	// wiring and not the count.
	for (const [plan, want] of [
		[mixed, { total: 1, done: 1 }],
		[abandoned, { total: 0, done: 0 }],
	] as const) {
		const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${plan.slug}/progress`, {
			headers: { Authorization: `Bearer ${fixture.apiToken}` },
		});
		expect(resp.ok(), await resp.text()).toBeTruthy();
		expect(await resp.json()).toMatchObject(want);
	}

	await browserLogin(page);
	for (const [plan, header] of [
		[mixed, '1/1 done'],
		[abandoned, '1'],
	] as const) {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/plans/${plan.slug}`);
		const count = page.locator('.child-count').first();
		await expect(count).toHaveText(header);
	}
});
