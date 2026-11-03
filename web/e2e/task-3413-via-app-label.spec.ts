import { expect, type APIRequestContext, type Route } from '@playwright/test';
import { test, type SuiteFixture } from './fixtures';

/**
 * TASK-3413 (SPEC-6 U9c): the "via <App>" label and the members list's
 * separate Apps section, in a real browser against the real server.
 *
 * WHAT IS REAL AND WHAT IS NOT. The e2e server is plain http, so apps are
 * unavailable there and nothing in it was written through an app. The item,
 * its comment and the members list are REAL rows the real server returns;
 * `page.route` passes each response through (`route.fetch`) and adds only the
 * fields an app write would carry (`via_app`, `via_app_name`, and the members
 * `apps` array). The server half, that those fields are emitted, is covered by
 * the Go tests of #1787.
 */

const APP = 'Support Portal';
const TITLE = `Written through the installed app ${APP}`;

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `Via label ${Date.now()}`, prefix: 'VIA' }
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = await coll.json();
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
		headers: authHeaders(fixture),
		data: { title: `Via label item ${Date.now()}`, content: 'body' }
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = (await created.json()) as { id: string; slug: string };
	const comment = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}/comments`, {
		headers: authHeaders(fixture),
		data: { body: 'filed through the portal' }
	});
	expect(comment.ok(), await comment.text()).toBeTruthy();
	return { collSlug, item };
}

/** Add an app's attribution to whatever in a JSON body is this item or its comments. */
function stamp(value: unknown, itemId: string, name = APP): void {
	if (Array.isArray(value)) {
		value.forEach((v) => stamp(v, itemId, name));
		return;
	}
	if (!value || typeof value !== 'object') return;
	const o = value as Record<string, unknown>;
	if (o.id === itemId || (typeof o.body === 'string' && o.item_id === itemId)) {
		o.via_app = 'inst-e2e';
		o.via_app_name = name;
	}
	for (const v of Object.values(o)) stamp(v, itemId, name);
}

/**
 * This route handles every workspace GET, background ones included. When the
 * test ends, its browser context closes; a handler still between
 * `route.fetch()` and reading the body then gets "Response has been disposed"
 * (Playwright maps the closed target to that message), and the throw was
 * reported against a test whose assertions had all passed: CI runs
 * 37315527477 and 37409948924. Reproduced by closing the context while a
 * handler holds a fetched response. Nobody is waiting for that request, so
 * the handler drops it. Only those two messages: "Route is already handled"
 * would mean two handlers raced for one route, a real defect, so it and any
 * other error still fail the test.
 */
function requestAbandoned(err: unknown): boolean {
	return /Response has been disposed|Target page, context or browser has been closed/.test(
		String((err as Error)?.message ?? err)
	);
}

async function rewriteJson(route: Route, edit: (json: unknown) => void): Promise<void> {
	try {
		const response = await route.fetch();
		const type = response.headers()['content-type'] ?? '';
		if (!type.includes('application/json')) return await route.fulfill({ response });
		const json = await response.json();
		edit(json);
		await route.fulfill({ response, json });
	} catch (err) {
		if (requestAbandoned(err)) return;
		throw err;
	}
}

test('TASK-3413 U9c: an item and its comment read "via <App>" beside their author', async ({ page, fixture, request }) => {
	const { collSlug, item } = await seed(fixture, request);
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/**`, async (route: Route) => {
		if (route.request().method() !== 'GET') return route.fallback();
		await rewriteJson(route, (json) => stamp(json, item.id));
	});

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
	const meta = page.locator('.meta-via-app');
	await expect(meta).toHaveText(`via ${APP}`);
	await expect(meta).toHaveAttribute('title', TITLE);

	const commentLabel = page.locator('.via-app-marker').first();
	await expect(commentLabel).toHaveText(`via ${APP}`);
	await expect(commentLabel).toHaveAttribute('title', TITLE);
});

test('TASK-3413 U9c: the members list shows apps in their own section, not as members', async ({ page, fixture }) => {
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/members`, async (route: Route) => {
		if (route.request().method() !== 'GET') return route.fallback();
		await rewriteJson(route, (json) => {
			(json as { apps: unknown[] }).apps = [{ id: 'bot-e2e', display_name: 'portal-bot', app_name: APP, role: 'editor' }];
		});
	});
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/settings#members`);
	const section = page.getByTestId('members-apps');
	await expect(section).toBeVisible();
	await expect(section).toContainText(APP);
	await expect(section).toContainText('They are not seats');
	// Not listed among people.
	await expect(page.locator('.members-list')).not.toContainText(APP);
	// An owner can jump to the Apps tab from here.
	await section.getByRole('link', { name: 'Manage apps' }).click();
	await expect(page.getByRole('tab', { name: /Apps/ })).toHaveAttribute('aria-selected', 'true');
});

test('TASK-3413 U9c: a long unbroken app name wraps inside the viewport (codex r1)', async ({ page, fixture, request }) => {
	const long = 'Portal' + 'x'.repeat(120);
	const { collSlug, item } = await seed(fixture, request);
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/**`, async (route: Route) => {
		if (route.request().method() !== 'GET') return route.fallback();
		await rewriteJson(route, (json) => stamp(json, item.id, long));
	});
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
	await expect(page.locator('.meta-via-app')).toContainText(long);
	await expect(page.locator('.via-app-marker').first()).toContainText(long);
	// An ancestor clips rather than scrolls, so the page's own width says
	// nothing: measured without the fix, a phone's label ran to x=724 on a
	// 412-wide viewport, cut off. Each label must end inside the viewport.
	const overruns = await page.evaluate(() =>
		['.meta-via-app', '.via-app-marker'].flatMap((sel) => {
			const r = document.querySelector(sel)!.getBoundingClientRect();
			return r.right > window.innerWidth ? [`${sel} ends at ${Math.round(r.right)} of ${window.innerWidth}`] : [];
		})
	);
	expect(overruns, 'a via label runs past the viewport').toEqual([]);
});
