import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';
import type { Page, APIRequestContext, Route } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3050 U3: every page that renders a body the server marked as behind the
 * live document (`content_state: applied_pending_flush`, BUG-3000) says so.
 * The ruled treatments: a one-line notice above a body that is the view's
 * subject (share pages, the read-only item pane, the diff's current side), and
 * a muted dot on MARKED rows only where the body is a snippet among many
 * (conventions, playbooks, the command palette; backlinks is a unit leg).
 *
 * These legs pin the BINDING from each page's API response to its render, so
 * the marker is added to the real response by interception, on one row only:
 * a real marker lasts only until a tab flushes, which no leg can schedule. The
 * components themselves are pinned in src/lib/items/staleBodyRenders.svelte.test.ts.
 */

const MARK = 'applied_pending_flush';
const NOTICE = 'This may not include the latest edits.';

async function create(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string, body: string, fields: Record<string, unknown> = {}) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data: { title, content: body, fields: JSON.stringify(fields) },
	});
	if (!resp.ok()) throw new Error(`create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string; id: string };
}

async function share(fixture: SuiteFixture, request: APIRequestContext, path: string, data: Record<string, unknown> = {}) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/${path}/share-links`, {
		headers: authJson(fixture),
		data,
	});
	if (!resp.ok()) throw new Error(`share failed (${resp.status()}): ${await resp.text()}`);
	return ((await resp.json()) as { token: string }).token;
}

/** Fulfil `route` with the real response, rewritten by `edit`. */
async function rewrite(route: Route, edit: (json: any) => void) {
	const resp = await route.fetch();
	const json = await resp.json();
	edit(json);
	await route.fulfill({ response: resp, json });
}

/** Mark the rows of a JSON array (or `{items}`/`{results}` wrapper) whose id is in `ids`. */
function markRows(json: any, ids: string[]) {
	const rows: any[] = Array.isArray(json) ? json : (json.items ?? json.results ?? []);
	let marked = 0;
	for (const r of rows) {
		const row = r.item ?? r;
		if (ids.includes(row.id)) { row.content_state = MARK; marked++; }
	}
	return marked;
}

/** Mark the item GET for `id`, whatever URL the pane reads it by (ref, slug, query). */
async function markItemReads(page: Page, fixture: SuiteFixture, id: string) {
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/**`, async (r) => {
		if (r.request().method() !== 'GET') return r.continue();
		const resp = await r.fetch();
		const text = await resp.text();
		let json: any;
		try { json = JSON.parse(text); } catch { return r.fulfill({ response: resp, body: text }); }
		if (json && !Array.isArray(json) && json.id === id) json.content_state = MARK;
		await r.fulfill({ response: resp, json });
	});
}

test.describe('stale body renders (BUG-3050 U3)', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a render binding');
	});

	test('share page: a marked item shows the notice above its body, an unmarked one does not (C1)', async ({ browser, fixture, request }) => {
		const stamp = Date.now();
		const item = await create(fixture, request, 'docs', `Shared ${stamp}`, 'Shared body text.');
		const token = await share(fixture, request, `items/${item.slug}`);
		const anon = await browser.newContext();
		try {
			const p = await anon.newPage();
			await p.goto(`/s/${token}`);
			await expect(p.locator('.item-content')).toContainText('Shared body text.');
			await expect(p.getByText(NOTICE)).toHaveCount(0);

			await p.route(`**/api/v1/s/${token}`, (r) => rewrite(r, (j) => { j.item.content_state = MARK; }));
			await p.reload();
			await expect(p.locator('.item-content')).toContainText('Shared body text.');
			await expect(p.getByText(NOTICE)).toBeVisible();
		} finally {
			await anon.close();
		}
	});

	test('share page: the password-unlocked copy carries the marker too (C1)', async ({ browser, fixture, request }) => {
		const stamp = Date.now();
		const item = await create(fixture, request, 'docs', `Locked ${stamp}`, 'Locked body text.');
		const token = await share(fixture, request, `items/${item.slug}`, { password: `pw-${stamp}` });
		const anon = await browser.newContext();
		try {
			const p = await anon.newPage();
			await p.route(`**/api/v1/s/${token}`, (r) => rewrite(r, (j) => { if (j.item) j.item.content_state = MARK; }));
			await p.goto(`/s/${token}`);
			await p.getByPlaceholder(/password/i).fill(`pw-${stamp}`);
			await p.getByRole('button', { name: /View content/ }).click();
			await expect(p.locator('.item-content')).toContainText('Locked body text.');
			await expect(p.getByText(NOTICE)).toBeVisible();
		} finally {
			await anon.close();
		}
	});

	test('shared collection: the inline expansion of a marked item shows the notice, its neighbour\'s does not (C2/C3)', async ({ browser, fixture, request }) => {
		const stamp = Date.now();
		const marked = await create(fixture, request, 'docs', `Coll marked ${stamp}`, `Marked expansion body ${stamp}.`);
		const plain = await create(fixture, request, 'docs', `Coll plain ${stamp}`, `Plain expansion body ${stamp}.`);
		const token = await share(fixture, request, 'collections/docs');
		const anon = await browser.newContext();
		try {
			const p = await anon.newPage();
			// Public share items carry no id (an allow-list payload), so mark by title.
			await p.route(`**/api/v1/s/${token}`, (r) => rewrite(r, (j) => {
				for (const it of j.items ?? []) if (it.title === `Coll marked ${stamp}`) it.content_state = MARK;
			}));
			await p.goto(`/s/${token}`);
			for (const [it, body, noticeCount] of [[plain, `Plain expansion body ${stamp}.`, 0], [marked, `Marked expansion body ${stamp}.`, 1]] as const) {
				await p.getByText(`Coll ${it === marked ? 'marked' : 'plain'} ${stamp}`).first().click();
				const region = p.getByRole('region', { name: new RegExp(`Coll ${it === marked ? 'marked' : 'plain'} ${stamp} details`) });
				await expect(region).toContainText(body);
				await expect(region.getByText(NOTICE)).toHaveCount(noticeCount);
			}
		} finally {
			await anon.close();
		}
	});

	test('item pane: a read-only viewer sees the notice above a marked body (C4)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const item = await create(fixture, request, 'docs', `Viewer ${stamp}`, `Viewer body ${stamp}.`);
		await browserLogin(page);
		// A viewer's membership, on the one call canEdit derives from.
		await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/me`, (r) => rewrite(r, (j) => { j.role = 'viewer'; j.item_grants = []; j.collection_grants = []; }));
		await markItemReads(page, fixture, item.id);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${item.ref}`);
		await expect(page.getByText(`Viewer body ${stamp}.`)).toBeVisible({ timeout: 15_000 });
		await expect(page.getByText(NOTICE)).toBeVisible();
	});

	test('item pane: a read-only viewer on an unmarked item sees no notice (C4 control)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const item = await create(fixture, request, 'docs', `Viewer ctl ${stamp}`, `Viewer ctl body ${stamp}.`);
		await browserLogin(page);
		await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/me`, (r) => rewrite(r, (j) => { j.role = 'viewer'; j.item_grants = []; j.collection_grants = []; }));
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${item.ref}`);
		await expect(page.getByText(`Viewer ctl body ${stamp}.`)).toBeVisible({ timeout: 15_000 });
		await expect(page.getByText(NOTICE)).toHaveCount(0);
	});

	test('versions tab: the diff against a marked current body shows the notice (C5)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const item = await create(fixture, request, 'docs', `Diff ${stamp}`, `First body ${stamp}.`);
		const upd = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${item.slug}`, {
			headers: authJson(fixture),
			data: { content: `Second body ${stamp}.` },
		});
		expect(upd.ok(), await upd.text()).toBeTruthy();
		await browserLogin(page);
		await markItemReads(page, fixture, item.id);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${item.ref}`);
		await page.getByRole('tab', { name: 'Versions' }).click();
		const card = page.locator('.card-header').first();
		await expect(card).toBeVisible({ timeout: 15_000 });
		await card.click();
		await expect(page.locator('.diff-container').first()).toBeVisible();
		await expect(page.locator('.diff-container').first().getByText(NOTICE)).toBeVisible();
	});

	async function dotsIn(page: Page, rowSelector: string, title: string) {
		return page.locator(rowSelector).filter({ hasText: title }).locator('[data-testid="stale-body-dot"]');
	}

	test('conventions list: a dot on the marked row only (C6)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const marked = await create(fixture, request, 'conventions', `Conv marked ${stamp}`, 'Rule body.', { status: 'active', trigger: 'always' });
		await create(fixture, request, 'conventions', `Conv plain ${stamp}`, 'Rule body.', { status: 'active', trigger: 'always' });
		await browserLogin(page);
		await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/collections/conventions/items*`, (r) => rewrite(r, (j) => markRows(j, [marked.id])));
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		await expect(page.getByText(`Conv plain ${stamp}`)).toBeVisible({ timeout: 15_000 });
		await expect(await dotsIn(page, '.convention-row', `Conv marked ${stamp}`)).toHaveCount(1);
		await expect(await dotsIn(page, '.convention-row', `Conv plain ${stamp}`)).toHaveCount(0);
		await expect((await dotsIn(page, '.convention-row', `Conv marked ${stamp}`)).first()).toHaveAttribute('title', NOTICE);
	});

	test('playbooks list: a dot on the marked card only (C7)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const marked = await create(fixture, request, 'playbooks', `PB marked ${stamp}`, '1. Step', { status: 'draft' });
		await create(fixture, request, 'playbooks', `PB plain ${stamp}`, '1. Step', { status: 'draft' });
		await browserLogin(page);
		await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/collections/playbooks/items*`, (r) => rewrite(r, (j) => markRows(j, [marked.id])));
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks`);
		await expect(page.getByText(`PB plain ${stamp}`)).toBeVisible({ timeout: 15_000 });
		await expect(await dotsIn(page, '.card', `PB marked ${stamp}`)).toHaveCount(1);
		await expect(await dotsIn(page, '.card', `PB plain ${stamp}`)).toHaveCount(0);
	});

	test('command palette: a content match from a marked item carries the dot, its neighbour does not (C9)', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const word = `zqpal${stamp.toString(36)}`;
		const marked = await create(fixture, request, 'docs', `Pal marked ${stamp}`, `Body mentions ${word} here.`);
		await create(fixture, request, 'docs', `Pal plain ${stamp}`, `Body mentions ${word} too.`);
		await browserLogin(page);
		await page.route('**/api/v1/search?*', (r) => rewrite(r, (j) => markRows(j, [marked.id])));
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs`);
		const input = page.getByPlaceholder('Search items, collections, docs...');
		// The shortcut is bound once the app shell has loaded; press until the palette opens.
		await expect(async () => {
			if (!(await input.isVisible())) await page.keyboard.press('Control+k');
			await expect(input).toBeVisible({ timeout: 1_000 });
		}).toPass({ timeout: 20_000 });
		await input.fill(word);
		const rows = page.locator('button.result');
		await expect(rows.filter({ hasText: `Pal plain ${stamp}` })).toBeVisible({ timeout: 15_000 });
		await expect(rows.filter({ hasText: `Pal marked ${stamp}` }).locator('[data-testid="stale-body-dot"]')).toHaveCount(1);
		await expect(rows.filter({ hasText: `Pal plain ${stamp}` }).locator('[data-testid="stale-body-dot"]')).toHaveCount(0);
	});
});
