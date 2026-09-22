import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Page, Route } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3115 — a title the server refuses must not take the typed text with it.
 *
 * Two doors lost it. The sidebar quick-add dialog closed and cleared itself
 * BEFORE the create request, so a refusal toasted over nothing; the item
 * title editor closed before the PATCH, and reopening it re-seeds from the
 * stored title. Both now keep the text on screen, editable, with the reason
 * beside it, and both refuse a too-long title before sending anything.
 *
 * Two legs per door:
 *  - LOCAL: a 256-character title never reaches the network.
 *  - SERVER: a title the client accepts is lengthened IN FLIGHT by the test
 *    (route.continue with a rewritten body), so the REAL server refuses it
 *    with its real message. The client check and the server check agree by
 *    construction (testdata/item_title_limit.json), so no typed title reaches
 *    the refusal path without the rewrite; what is under test is what the
 *    client does with a real refusal.
 */

const DESKTOP = { width: 1200, height: 900 };
const LIMIT = 255;
const TOO_LONG = 'x'.repeat(LIMIT + 1);
const PADDED = 'y'.repeat(300);
const SERVER_REASON = `Title is too long: ${PADDED.length} characters, maximum ${LIMIT}`;
const LOCAL_REASON = `Title is too long: ${TOO_LONG.length} characters, maximum ${LIMIT}`;

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	// No trailing "s": the sidebar's + button is titled "New <name minus s>".
	const collName = `B3115 titles ${Date.now()}`;
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: collName, prefix: 'BTL' },
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = await coll.json();
	const title = `B3115 stored ${Date.now()}`;
	const created = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{ headers: authHeaders(fixture), data: { title, content: '' } },
	);
	expect(created.ok(), await created.text()).toBeTruthy();
	const item = (await created.json()) as { id: string; slug: string };
	return { collSlug, collName, item, title };
}

/** Rewrite the outgoing title so the real server refuses it. */
function padTitle(route: Route) {
	const body = JSON.parse(route.request().postData() ?? '{}');
	body.title = PADDED;
	return route.continue({ postData: JSON.stringify(body) });
}

function titleWrites(page: Page, method: 'POST' | 'PATCH', urlPart: string) {
	const seen: string[] = [];
	page.on('request', (r) => {
		if (r.method() === method && r.url().includes(urlPart)) seen.push(r.postData() ?? '');
	});
	return seen;
}

async function openQuickAdd(page: Page, collName: string) {
	const add = page.locator(`button.nav-quick-add[title="New ${collName}"]`);
	await add.hover({ force: true });
	await add.click({ force: true });
	const dialog = page.locator('.quick-add-modal');
	await expect(dialog).toBeVisible();
	return { dialog, input: dialog.locator('textarea.quick-add-input') };
}

async function storedTitle(fixture: SuiteFixture, request: APIRequestContext, id: string) {
	const res = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`, {
		headers: authHeaders(fixture),
	});
	return ((await res.json()) as { title: string }).title;
}

test.describe('BUG-3115: a refused title keeps the typed text', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a client-side disposition');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('quick-add: a too-long title is refused locally, dialog and text stay', async ({ page, fixture, request }) => {
		const { collSlug, collName, item } = await seed(fixture, request);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		const creates = titleWrites(page, 'POST', `/collections/`);

		const { dialog, input } = await openQuickAdd(page, collName);
		await input.fill(TOO_LONG);
		await input.press('Enter');

		await expect(dialog.locator('#quick-add-error')).toHaveText(LOCAL_REASON);
		await expect(dialog, 'the dialog closed').toBeVisible();
		await expect(input, 'the typed title is gone').toHaveValue(TOO_LONG);
		expect(creates.filter((b) => b.includes('"title"')), 'a create was sent').toEqual([]);
	});

	test('quick-add: a server refusal leaves the dialog open on the typed text', async ({ page, fixture, request }) => {
		const { collSlug, collName, item } = await seed(fixture, request);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		await page.route(`**/api/v1/workspaces/*/collections/*/items`, (route) =>
			route.request().method() === 'POST' ? padTitle(route) : route.fallback(),
		);

		const typed = `B3115 quick-add ${Date.now()}`;
		const { dialog, input } = await openQuickAdd(page, collName);
		await input.fill(typed);
		const refused = page.waitForResponse(
			(r) => r.request().method() === 'POST' && /\/collections\/[^/]+\/items$/.test(new URL(r.url()).pathname),
		);
		await input.press('Enter');
		const res = await refused;
		// PREMISE: the REAL server refused it, for the length.
		expect(res.status()).toBe(400);
		expect(await res.text()).toContain(SERVER_REASON);

		await expect(dialog.locator('#quick-add-error')).toHaveText(SERVER_REASON);
		await expect(dialog, 'BUG-3115: the dialog closed on the refusal').toBeVisible();
		await expect(input, 'BUG-3115: the typed title is gone').toHaveValue(typed);
	});

	test('item title: a too-long title is refused locally, editor and text stay', async ({ page, fixture, request }) => {
		const { collSlug, item, title } = await seed(fixture, request);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		const host = page.locator('.item-page-host > .item-page');
		const patches = titleWrites(page, 'PATCH', `/items/${item.id}`);

		await host.locator('button.title', { hasText: title }).click();
		const editor = host.locator('textarea.title-input');
		await editor.fill(TOO_LONG);
		await editor.press('Enter');

		await expect(host.locator('#item-title-error')).toHaveText(LOCAL_REASON);
		await expect(editor, 'the typed title is gone').toHaveValue(TOO_LONG);
		expect(patches.filter((b) => b.includes('"title"')), 'a title PATCH was sent').toEqual([]);
		expect(await storedTitle(fixture, request, item.id)).toBe(title);
	});

	test('item title: a server refusal reopens the editor on the typed text', async ({ page, fixture, request }) => {
		const { collSlug, item, title } = await seed(fixture, request);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
		await page.route(`**/api/v1/workspaces/*/items/${item.id}`, (route) =>
			route.request().method() === 'PATCH' && (route.request().postData() ?? '').includes('"title"')
				? padTitle(route)
				: route.fallback(),
		);
		const host = page.locator('.item-page-host > .item-page');

		await host.locator('button.title', { hasText: title }).click();
		const editor = host.locator('textarea.title-input');
		const typed = `B3115 renamed ${Date.now()}`;
		await editor.fill(typed);
		const refused = page.waitForResponse(
			(r) => r.request().method() === 'PATCH' && r.url().includes(`/items/${item.id}`),
		);
		await editor.press('Enter');
		const res = await refused;
		expect(res.status()).toBe(400);
		expect(await res.text()).toContain(SERVER_REASON);

		await expect(host.locator('#item-title-error')).toHaveText(SERVER_REASON);
		await expect(editor, 'BUG-3115: the typed title is gone').toHaveValue(typed);
		expect(await storedTitle(fixture, request, item.id)).toBe(title);
	});

	// Lead NO-GO on #1437: the server re-validates a title only when it CHANGES
	// (handlers_items.go's early refusal; the store's echo comparison), so a
	// playbook whose title predates the bound stays saveable as long as the
	// editor sends that title back unchanged — and save() always sends it.
	// No door can STORE an over-limit title any more, so the page is SHOWN one:
	// the item GET is answered with a legacy title, and the PATCH carrying it
	// back is forwarded with the title dropped. The server's half — accepting
	// that echo — is pinned over HTTP by TestPatchItemVerbatimEchoOfALegacyTitleIsANoOp;
	// this leg pins the client's half: it SENDS the save instead of refusing it.
	test('playbook editor: a legacy over-limit title that is not changed does not block the save', async ({
		page,
		fixture,
		request,
	}) => {
		const created = await request.post(
			`/api/v1/workspaces/${fixture.workspaceSlug}/collections/playbooks/items`,
			{
				headers: authHeaders(fixture),
				data: { title: `B3115 playbook ${Date.now()}`, content: '1. step', fields: JSON.stringify({ status: 'draft' }) },
			},
		);
		expect(created.ok(), await created.text()).toBeTruthy();
		const pb = (await created.json()) as { id: string; slug: string };
		const legacy = 'L'.repeat(LIMIT + 45);

		await page.route(`**/api/v1/workspaces/*/items/${pb.slug}*`, async (route) => {
			if (route.request().method() !== 'GET') return route.fallback();
			const real = await route.fetch();
			const body = await real.json();
			return route.fulfill({ response: real, json: { ...body, title: legacy } });
		});
		const sent: Array<Record<string, unknown>> = [];
		await page.route(`**/api/v1/workspaces/*/items/${pb.slug}`, (route) => {
			if (route.request().method() !== 'PATCH') return route.fallback();
			const body = JSON.parse(route.request().postData() ?? '{}');
			sent.push({ ...body });
			delete body.title;
			return route.continue({ postData: JSON.stringify(body) });
		});

		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${pb.slug}`);
		// PREMISE: the editor is showing the legacy title.
		await expect(page.locator('input.title-input')).toHaveValue(legacy);

		const saved = page.waitForResponse(
			(r) => r.request().method() === 'PATCH' && r.url().includes(`/items/${pb.slug}`),
		);
		await page.getByRole('button', { name: 'Save', exact: true }).click();
		const res = await saved;
		expect(res.status(), await res.text()).toBe(200);
		expect(sent, 'the save was refused locally, or sent more than once').toHaveLength(1);
		expect(sent[0].title, 'the save did not echo the stored title').toBe(legacy);
		await expect(page.getByText(`Title is too long`)).toHaveCount(0);
	});
});
