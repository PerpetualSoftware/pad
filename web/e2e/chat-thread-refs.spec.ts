import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { seedPermissionWalk } from './lib/permission-walk';

/**
 * TASK-4044 PR 3c (rulings 687eb810, 463abd0c): how /messages/[n] renders the
 * `[[REF]]` and `[[workspace::REF]]` links in a message body, against the REAL
 * server. Each case asserts the API's `state` for the ref FIRST, then the DOM:
 * the server decides, the page only draws.
 *
 * The cases are N9 (a)-(e), not the first E7 (463abd0c): an invented REF in a
 * workspace the reader can see is `restricted`, not `unresolved`, because the
 * server answers absence and denial with one projection (TASK-4039). Only a
 * bare `[[REF]]` with no visible context workspace is `unresolved`.
 *
 * VISIBILITY GATE (687eb810): the rendered node for a ref in an EXISTING
 * workspace the reader cannot see (b) and for an invented REF (c) must be
 * byte-identical, or the page would tell the two apart.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };
type Ref = { workspace: string; ref: string; state: string; title?: string; collection_slug?: string };
type Row = { seq: number; body: string; deleted: boolean; context_workspace?: { slug: string } };
type Page2 = { messages: Row[]; refs: Ref[] };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

const uniqueAgent = (tag: string) =>
	`e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;

async function createDM(request: APIRequestContext, f: Fixture, agent: string): Promise<number> {
	const res = await request.post('/api/v1/me/conversations', { headers: headers(f), data: { agent_key: agent } });
	expect(res.ok(), await res.text()).toBe(true);
	return ((await res.json()) as { conversation: { number: number } }).conversation.number;
}

/** A send; `context_workspace` rides in the POST body exactly as a client sends it. */
async function send(
	request: APIRequestContext,
	f: Fixture,
	n: number,
	body: string,
	opts: { agent?: string; context_workspace?: string } = {}
): Promise<Row> {
	const res = await request.post(`/api/v1/me/conversations/${n}/messages`, {
		headers: headers(f, opts.agent),
		data: { body, ...(opts.context_workspace ? { context_workspace: opts.context_workspace } : {}) }
	});
	expect(res.status(), await res.text()).toBe(201);
	return ((await res.json()) as { message: Row }).message;
}

async function listPage(request: APIRequestContext, f: Fixture, n: number): Promise<Page2> {
	const res = await request.get(`/api/v1/me/conversations/${n}/messages?limit=200`, { headers: headers(f) });
	expect(res.ok(), await res.text()).toBe(true);
	return (await res.json()) as Page2;
}

async function createTask(request: APIRequestContext, f: Fixture, title: string): Promise<{ ref: string; title: string }> {
	const res = await request.post(`/api/v1/workspaces/${f.workspaceSlug}/collections/tasks/items`, {
		headers: headers(f),
		data: { title }
	});
	expect(res.status(), await res.text()).toBe(201);
	const item = (await res.json()) as { ref?: string; issue_id?: string; item_number?: number; title: string };
	const ref = item.ref ?? item.issue_id ?? '';
	expect(ref, JSON.stringify(item)).toMatch(/^[A-Z]+-\d+$/);
	return { ref, title: item.title };
}

const bubble = (page: Page, seq: number) => page.locator(`article[data-seq="${seq}"]`);

async function waitForThread(page: Page, n: number, rowsWanted: number) {
	await page.goto(`/messages/${n}`);
	await expect(page.locator('article.bubble')).toHaveCount(rowsWanted);
}

test.describe('E7: refs in message bodies', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	});

	test('(a) a visible ref is a link and the click lands on the item', async ({ page, request, fixture }) => {
		const task = await createTask(request, fixture, `E7 target ${Date.now()}`);
		const agent = uniqueAgent('refa');
		const n = await createDM(request, fixture, agent);
		const row = await send(request, fixture, n, `see [[${fixture.workspaceSlug}::${task.ref}]] please`, { agent });

		const api = (await listPage(request, fixture, n)).refs.find((r) => r.ref === task.ref);
		expect(api).toMatchObject({ workspace: fixture.workspaceSlug, state: 'visible', title: task.title });

		await waitForThread(page, n, 1);
		const link = bubble(page, row.seq).locator('.body a');
		await expect(link).toHaveCount(1);
		await expect(link).toHaveAttribute('href', `/-/r/${fixture.workspaceSlug}/${task.ref}`);
		await expect(link).toHaveAttribute('title', task.title);
		await expect(link).toHaveText(task.ref);
		await expect(bubble(page, row.seq).locator('.body')).toHaveText(`see ${task.ref} please`);

		await link.click();
		await page.waitForURL((u) => u.pathname.startsWith(`/${fixture.adminUsername}/${fixture.workspaceSlug}/`));
		await expect(page.locator('body')).toContainText(task.title);
	});

	test('(b) and (c): a denied-workspace ref and an invented ref render identical markup', async ({
		page,
		request,
		fixture
	}) => {
		const walk = await seedPermissionWalk(); // a workspace the fixture admin is NOT in
		const foreign = `${walk.workspaceSlug}::${walk.tasks[0].ref}`;
		const invented = `${fixture.workspaceSlug}::TASK-999999`;
		const agent = uniqueAgent('refbc');
		const n = await createDM(request, fixture, agent);
		const b = await send(request, fixture, n, `[[${foreign}]]`, { agent });
		const c = await send(request, fixture, n, `[[${invented}]]`, { agent });

		const refs = (await listPage(request, fixture, n)).refs;
		expect(refs.find((r) => r.workspace === walk.workspaceSlug)).toEqual({
			workspace: walk.workspaceSlug,
			ref: walk.tasks[0].ref,
			state: 'restricted'
		});
		expect(refs.find((r) => r.ref === 'TASK-999999')).toEqual({
			workspace: fixture.workspaceSlug,
			ref: 'TASK-999999',
			state: 'restricted'
		});

		await waitForThread(page, n, 2);
		const nodeB = bubble(page, b.seq).locator('.body .ref-restricted');
		const nodeC = bubble(page, c.seq).locator('.body .ref-restricted');
		await expect(nodeB).toHaveCount(1);
		await expect(nodeC).toHaveCount(1);
		const htmlB = await nodeB.evaluate((e) => e.outerHTML);
		const htmlC = await nodeC.evaluate((e) => e.outerHTML);
		console.log(`E7 (b) outerHTML: ${htmlB}`);
		console.log(`E7 (c) outerHTML: ${htmlC}`);
		expect(htmlB).toBe(htmlC);
		// The whole body too: nothing about the ref survives in either bubble.
		const bodyB = await bubble(page, b.seq).locator('.body').evaluate((e) => e.outerHTML);
		const bodyC = await bubble(page, c.seq).locator('.body').evaluate((e) => e.outerHTML);
		expect(bodyB).toBe(bodyC);
		for (const node of [nodeB, nodeC]) {
			await expect(node).toHaveText('restricted item');
			expect(await node.evaluate((e) => e.getAttributeNames())).toEqual(['class']);
		}
		await expect(page.locator('.body a')).toHaveCount(0);
		const shown = (await page.locator('article.bubble').allInnerTexts()).join('\n');
		expect(shown).not.toContain(walk.workspaceSlug);
		expect(shown).not.toContain('TASK-999999');
	});

	test('(d) a bare ref with no context workspace is unresolved and stays literal', async ({ page, request, fixture }) => {
		const agent = uniqueAgent('refd');
		const n = await createDM(request, fixture, agent);
		const row = await send(request, fixture, n, 'maybe [[TASK-1]] or not', { agent });
		expect(row.context_workspace).toBeUndefined();

		const api = (await listPage(request, fixture, n)).refs.find((r) => r.ref === 'TASK-1');
		expect(api).toEqual({ workspace: '', ref: 'TASK-1', state: 'unresolved' });

		await waitForThread(page, n, 1);
		const body = bubble(page, row.seq).locator('.body');
		await expect(body).toHaveText('maybe [[TASK-1]] or not');
		await expect(body.locator('a, span')).toHaveCount(0);
	});

	test('(e) a ref in a deleted message renders nothing', async ({ page, request, fixture }) => {
		const task = await createTask(request, fixture, `E7 deleted ${Date.now()}`);
		const agent = uniqueAgent('refe');
		const n = await createDM(request, fixture, agent);
		const row = await send(request, fixture, n, `gone [[${fixture.workspaceSlug}::${task.ref}]]`, {
			agent,
			context_workspace: fixture.workspaceSlug
		});
		const del = await request.delete(`/api/v1/me/conversations/${n}/messages/${row.seq}`, {
			headers: headers(fixture, agent)
		});
		expect(del.ok(), await del.text()).toBe(true);

		const after = await listPage(request, fixture, n);
		expect(after.messages.find((m) => m.seq === row.seq)).toMatchObject({ deleted: true, body: '' });
		expect(after.refs).toEqual([]);

		await waitForThread(page, n, 1);
		const article = bubble(page, row.seq);
		await expect(article).toContainText('Message deleted');
		await expect(article).not.toContainText(task.ref);
		await expect(article.locator('a, .ref-restricted')).toHaveCount(0);
	});
});
