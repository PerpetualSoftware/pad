import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';

/**
 * BUG-3201 — syncService.setWorkspace seeds its time cursor from a
 * `/changes?since=<client now>` call. That response holds the changes committed
 * between the request's `since` and the server's clock, and the cursor moves
 * past them, so a later sync cannot return them. It used to be discarded.
 *
 * Measured before the fix, with this spec's own setup: a child renamed inside
 * that window stayed stale in its parent's children panel, 3/3, EVEN ACROSS a
 * tab-resume (the resume's /changes answered caught_up, and ChildItems reloads
 * only on a result that is not caught_up), while the same rename AFTER the seed
 * healed on the resume, 3/3. The collection list is not a witness: it renders
 * from the seq-based local index, which the resume also drains, so it healed
 * in both cases.
 *
 * Made deterministic by holding the seed request at the network layer: its
 * `since` is stamped when the page creates it, so a write committed while it is
 * held lands inside [since, server_time] by construction. SSE is aborted, so
 * nothing but the sync service can deliver the rename.
 */

async function resume(page: Page) {
	await page.evaluate(() => {
		Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
		document.dispatchEvent(new Event('visibilitychange'));
	});
	await page.waitForTimeout(2200);
	await page.evaluate(() => {
		Object.defineProperty(document, 'hidden', { configurable: true, get: () => false });
		document.dispatchEvent(new Event('visibilitychange'));
	});
}

for (const leg of ['inside', 'CONTROL-after'] as const) {
	test(`BUG-3201: a child renamed ${leg === 'inside' ? 'INSIDE' : 'after (CONTROL)'} the seed window reaches the children panel`, async ({
		page,
		request,
		fixture,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
		test.setTimeout(90_000);
		const auth = { Authorization: `Bearer ${fixture.apiToken}` };
		const slug = `b3201-${testInfo.workerIndex}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
		const wsResp = await request.post('/api/v1/workspaces', { headers: auth, data: { name: 'BUG-3201', slug, template: 'startup' } });
		expect(wsResp.ok(), await wsResp.text()).toBeTruthy();
		const ws = ((await wsResp.json()) as { slug: string }).slug;
		try {
			const parentResp = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
				headers: auth,
				data: { title: 'seed-window parent' },
			});
			expect(parentResp.ok(), await parentResp.text()).toBeTruthy();
			const parent = (await parentResp.json()) as { slug: string; id: string };
			const childResp = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
				headers: auth,
				data: { title: 'seed-window child OLD', fields: JSON.stringify({ parent: parent.id }) },
			});
			expect(childResp.ok(), await childResp.text()).toBeTruthy();
			const child = (await childResp.json()) as { slug: string };
			// The creates' second passes, so the rename below is in a later second.
			await page.waitForTimeout(1100);

			await page.route('**/api/v1/events**', (route) => route.abort());
			let releaseSeed: () => void = () => {};
			const seedHeld = new Promise<void>((r) => (releaseSeed = r));
			let seedSeen = false;
			// The first /changes RESPONSE is the seed's.
			const seedDone = page.waitForResponse((r) => r.url().includes(`/workspaces/${ws}/changes?`));
			await page.route(`**/api/v1/workspaces/${ws}/changes?**`, async (route) => {
				if (seedSeen) return route.continue();
				seedSeen = true;
				if (leg === 'inside') await seedHeld;
				await route.continue();
			});

			await page.goto(`/${fixture.adminUsername}/${ws}/tasks/${parent.slug}`);
			const title = (t: string) => page.locator('.child-title', { hasText: t });
			await expect(title('seed-window child OLD').first()).toBeAttached({ timeout: 20_000 });
			expect(seedSeen, 'the seed /changes call was never made — re-point this spec').toBeTruthy();

			const patch = await request.patch(`/api/v1/workspaces/${ws}/items/${child.slug}`, {
				headers: auth,
				data: { title: 'seed-window child NEW' },
			});
			expect(patch.ok(), await patch.text()).toBeTruthy();

			if (leg === 'inside') {
				// A second boundary, so the rename's updated_at second is strictly
				// before the cursor's (the store truncates since and matches >=).
				await page.waitForTimeout(1300);
				releaseSeed();
				await seedDone;
				// THE FIX: the seed's own delta is delivered, so the panel reloads
				// with no resume at all.
				await expect(title('seed-window child NEW').first()).toBeAttached({ timeout: 10_000 });
			} else {
				await seedDone;
				// CONTROL: a rename after the seed is the next sync's to deliver.
				await resume(page);
				await expect(title('seed-window child NEW').first()).toBeAttached({ timeout: 10_000 });
			}
		} finally {
			await request.delete(`/api/v1/workspaces/${ws}`, { headers: auth }).catch(() => {});
		}
	});
}
