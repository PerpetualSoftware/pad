import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { createWorkspace, deleteWorkspace } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2210: "Needs Attention" and "Active Plans" on the dashboard are capped
 * (6 and 5 rows) with a "Show all (N)" toggle that remembers its state per
 * workspace. Before this, both rendered every row (76 and 23 on the docapp
 * workspace), burying Up Next and every later section.
 *
 * Own workspace, so the counts are exactly what this test seeds: 7 active
 * plans (one with a child task, which is what makes the dashboard report
 * parentless tasks) and 8 parentless open tasks, each an attention row.
 */

const DESKTOP = { width: 1200, height: 900 };

function authJson(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function create(
	fixture: SuiteFixture,
	request: APIRequestContext,
	ws: string,
	coll: string,
	data: Record<string, unknown>,
) {
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data,
	});
	if (!resp.ok()) throw new Error(`create in ${coll}: ${resp.status()} ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref?: string };
}

test.describe('dashboard lists are capped (TASK-2210)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'layout concern; one desktop browser is enough');
	});

	test('attention and plans show a capped head, expand to all, and stay expanded across a reload', async ({
		page,
		fixture,
		request,
	}) => {
		const { slug: ws } = await createWorkspace(fixture, request, 'Dash caps');
		try {
			const plans = [];
			for (let i = 1; i <= 7; i++) {
				plans.push(await create(fixture, request, ws, 'plans', { title: `Cap plan ${i}`, fields: JSON.stringify({ status: 'active' }) }));
			}
			await create(fixture, request, ws, 'tasks', {
				title: 'Child of plan 1',
				fields: JSON.stringify({ status: 'open', parent: plans[0].slug }),
			});
			for (let i = 1; i <= 8; i++) {
				await create(fixture, request, ws, 'tasks', { title: `Loose task ${i}`, fields: JSON.stringify({ status: 'open' }) });
			}
			const dash = await request.get(`/api/v1/workspaces/${ws}/dashboard`, { headers: authJson(fixture) });
			const body = (await dash.json()) as { attention: unknown[]; active_plans: unknown[] };
			const nAttention = body.attention.length;
			expect(nAttention, 'precondition: more attention rows than the cap').toBeGreaterThan(6);
			expect(body.active_plans.length, 'precondition: 7 active plans').toBe(7);

			await page.setViewportSize(DESKTOP);
			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}`);

			const attention = page.locator('.attention-card');
			const plansRows = page.locator('.plan-row');
			const showAllAttention = page.getByRole('button', { name: `Show all (${nAttention})` });
			const showAllPlans = page.getByRole('button', { name: 'Show all (7)' });

			// Capped.
			await expect(attention).toHaveCount(6);
			await expect(plansRows).toHaveCount(5);
			await expect(showAllAttention).toHaveAttribute('aria-expanded', 'false');
			await expect(showAllPlans).toHaveAttribute('aria-expanded', 'false');

			// Expanded.
			await showAllAttention.click();
			await expect(attention).toHaveCount(nAttention);
			await showAllPlans.click();
			await expect(plansRows).toHaveCount(7);

			// Kept across a reload, per workspace.
			await page.reload();
			await expect(attention).toHaveCount(nAttention);
			await expect(plansRows).toHaveCount(7);
			const fewer = page.getByRole('button', { name: 'Show fewer' });
			await expect(fewer).toHaveCount(2);

			// And collapsible again.
			await fewer.first().click();
			await fewer.first().click();
			await expect(attention).toHaveCount(6);
			await expect(plansRows).toHaveCount(5);
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});
});
