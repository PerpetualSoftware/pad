import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { Page } from '@playwright/test';

/**
 * BUG-3318: over plain HTTP (HTTP/1.1, which is what a self-hosted pad serves
 * without TLS) a browser allows 6 connections per host, and pad tabs held them
 * with long-lived streams: every tab opened its own
 * `/api/v1/events/stream?access=true` EventSource (accessStream.svelte.ts,
 * deliberately per tab) and the workspace SSE leader one more. With enough pad
 * tabs open, a new page's fetches queued in the browser forever and the page
 * sat on its skeleton: `GET /collections` never reached the server.
 *
 * Measured on 7ba82300 in Playwright's Chromium: 4 open tabs, then a 5th load
 * never finishes `GET /collections` (20s) and stays a skeleton. Dave's Brave hit
 * it at 3. This drives the same shape and requires the 5th load to complete.
 *
 * The e2e server is plain http://localhost, so HTTP/1.1 and its 6-per-host
 * limit apply here exactly as on a self-hosted instance.
 */

const OPEN_TABS = 4;

async function openPad(page: Page, path: string) {
	await page.goto(path, { waitUntil: 'domcontentloaded', timeout: 20_000 });
	// Long enough for the tab to open its streams (access stream, leader SSE).
	await page.waitForTimeout(2_000);
}

test.describe('BUG-3318: open pad tabs must not starve a new page of connections', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'a connection-pool check; one project is enough');
		test.setTimeout(120_000);
	});

	test(`with ${OPEN_TABS} pad tabs open, a new tab still loads an item`, async ({ page, context, fixture, request }) => {
		const ws = fixture.workspaceSlug;
		const created = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
			data: { title: `BUG-3318 target ${Date.now()}`, content: 'body' }
		});
		expect(created.ok(), await created.text()).toBeTruthy();
		const item = (await created.json()) as { slug: string; title: string };

		await browserLogin(page);
		const paths = [`/${fixture.adminUsername}/${ws}`, `/${fixture.adminUsername}/${ws}/tasks`];
		await openPad(page, paths[0]);
		for (let i = 1; i < OPEN_TABS; i++) await openPad(await context.newPage(), paths[i % paths.length]);

		const next = await context.newPage();
		const collections = next.waitForResponse((r) => /\/api\/v1\/workspaces\/[^/]+\/collections$/.test(new URL(r.url()).pathname), {
			timeout: 20_000
		});
		await next.goto(`/${fixture.adminUsername}/${ws}/tasks/${item.slug}`, { waitUntil: 'commit', timeout: 20_000 });
		expect((await collections).ok(), 'the new page never got its collections list: the browser had no connection to send it on').toBe(
			true
		);
		// The item page finishes loading: its tab strip renders past the skeleton.
		await expect(next.getByRole('tab', { name: 'Details' })).toBeVisible({ timeout: 20_000 });
	});
});
