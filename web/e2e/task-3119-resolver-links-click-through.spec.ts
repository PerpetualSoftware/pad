import { test, expect } from './fixtures';
import type { APIRequestContext } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-3119: `/-/r/{workspace}/{ref}` is a SERVER redirect route with no client
 * route behind it. A plain anchor to it let the SvelteKit router take the
 * click and render it as `[username]=-` / `[workspace]=r`: "Could not load
 * item / Workspace not found". Found by clicking the live "Possibly breaks
 * CONVE-N" chip on :7777; the unit tests only read its href. The same plain
 * anchor carried every cross-workspace wiki-link rendered from markdown.
 *
 * These CLICK each link and assert where the browser lands.
 */

async function created<T>(res: Awaited<ReturnType<APIRequestContext['post']>>): Promise<T> {
	expect(res.ok(), await res.text()).toBeTruthy();
	return (await res.json()) as T;
}

type Item = { ref: string; slug: string; title: string };

test.describe('TASK-3119: links to the /-/r/ resolver open their target', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'navigation check; one project is enough');
		test.setTimeout(90_000);
	});

	test('the "Possibly breaks" convention chip opens the convention', async ({ page, fixture, request }) => {
		const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
		const ws = fixture.workspaceSlug;
		const stamp = `${test.info().workerIndex}-${Date.now().toString(36)}`;
		// Any item stands in for the convention: the chip's link resolves a ref,
		// and the decision provider is not configured under e2e.
		const target = await created<Item>(
			await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
				headers: h,
				data: { title: `T3119 rule ${stamp}` },
			}),
		);
		const subject = await created<Item>(
			await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
				headers: h,
				data: { title: `T3119 subject ${stamp}` },
			}),
		);
		// The decisions the chip reads, as the server serialises them.
		await page.route(`**/api/v1/workspaces/${ws}/items/*/decisions`, (route) =>
			route.fulfill({
				json: {
					ref: subject.ref,
					decisions: [
						{
							id: 'd1',
							item_id: 'x',
							question_set: 'conventions',
							question_key: `conv:${target.ref}`,
							kind: 'noul',
							answer: { type: 'noul', noul: 0.97 },
							confidence: null,
							provider: 'typesafe',
							model: 'jev-1.13.0',
							evaluated_at: new Date().toISOString(),
							current: true,
						},
					],
				},
			}),
		);

		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${ws}/tasks/${subject.ref}`);
		const chip = page.locator('a.convention-chip');
		await expect(chip).toHaveText(`Possibly breaks ${target.ref}`, { timeout: 20_000 });
		await chip.click();

		await page.waitForURL((u) => !u.pathname.startsWith('/-/r/') && u.pathname.endsWith(`/${target.ref}`), {
			timeout: 20_000,
		});
		await expect(page.getByText(target.title).first()).toBeVisible({ timeout: 20_000 });
		await expect(page.getByText('Could not load item')).toHaveCount(0);
	});

	test('a cross-workspace wiki-link in a comment opens the other workspace\'s item', async ({ page, fixture, request }) => {
		const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
		const ws = fixture.workspaceSlug;
		const other = `t3119-${test.info().workerIndex}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
		await created(
			await request.post('/api/v1/workspaces', { headers: h, data: { name: 'T3119 other', slug: other, template: 'startup' } }),
		);
		try {
			const target = await created<Item>(
				await request.post(`/api/v1/workspaces/${other}/collections/tasks/items`, {
					headers: h,
					data: { title: `T3119 elsewhere ${other}` },
				}),
			);
			const subject = await created<Item>(
				await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
					headers: h,
					data: { title: `T3119 linker ${other}` },
				}),
			);
			await created(
				await request.post(`/api/v1/workspaces/${ws}/items/${subject.slug}/comments`, {
					headers: h,
					data: { body: `See [[${other}::${target.ref}]] for the details.` },
				}),
			);

			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}/tasks/${subject.ref}`);
			const link = page.locator(`.comment-body a.cross-workspace[href="/-/r/${other}/${target.ref}"]`);
			await expect(link).toBeVisible({ timeout: 20_000 });
			await link.click();

			await page.waitForURL(
				(u) => u.pathname.startsWith(`/${fixture.adminUsername}/${other}/`) && u.pathname.endsWith(`/${target.ref}`),
				{ timeout: 20_000 },
			);
			await expect(page.getByText(target.title).first()).toBeVisible({ timeout: 20_000 });
			await expect(page.getByText('Could not load item')).toHaveCount(0);
		} finally {
			await request.delete(`/api/v1/workspaces/${other}`, { headers: h }).catch(() => {});
		}
	});
});
