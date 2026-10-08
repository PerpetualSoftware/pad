import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2197, the audit's own repro with a deadline: the browser loses the
 * network, so "Live" must drop at once instead of persisting; a change made on
 * the server during the outage must show once the network returns (the
 * reconnect arms the catch-up delta, BUG-2540). The 75s silence watchdog for a
 * connection that dies WITHOUT an offline event is covered by
 * src/lib/services/sseLiveness.svelte.test.ts.
 */
test('the Live badge drops when the network goes, and a change made meanwhile shows after it returns', async ({
	page,
	context,
	fixture,
	request
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the stream logic is viewport-agnostic');
	test.setTimeout(90_000);

	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const ws = fixture.workspaceSlug;
	const before = `t2197 before ${Date.now()}`;
	const created = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
		headers,
		data: { title: before, fields: '{}', content: '' }
	});
	expect(created.ok()).toBe(true);
	const item = (await created.json()) as { slug: string };

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/tasks`);
	await expect(page.getByText(before).first()).toBeVisible();
	await expect(page.locator('.sse-state-connected').first()).toBeAttached({ timeout: 20_000 });

	await context.setOffline(true);
	// Before the fix: "Live" for the whole outage (150s measured).
	await expect(page.locator('.sse-state-reconnecting').first()).toBeAttached({ timeout: 5_000 });

	// A change on the server while this tab cannot hear about it.
	const after = `t2197 renamed while offline ${Date.now()}`;
	const patched = await request.patch(`/api/v1/workspaces/${ws}/items/${item.slug}`, {
		headers,
		data: { title: after }
	});
	expect(patched.ok()).toBe(true);

	await context.setOffline(false);
	await expect(page.locator('.sse-state-connected').first()).toBeAttached({ timeout: 20_000 });
	// Before the fix: still the old title more than 120s after the reconnect.
	await expect(page.getByText(after).first()).toBeVisible({ timeout: 15_000 });
});
