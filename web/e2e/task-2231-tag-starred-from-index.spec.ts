import type { APIRequestContext, Page, Request } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2231 (audit C114): the tag and starred pages fetched full-content items
 * (and their own collection list) to render card summaries the local index
 * already holds. Against a real server and a real index, this checks that
 * each page renders its items and makes no items or collections request of
 * its own, that a tag matches exactly, and that starred lists in star order.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const stamp = String(Date.now()).slice(-6);
	const tag = `t2231-${stamp}`;
	const ws = fixture.workspaceSlug;
	const create = async (title: string, tags: string[]) => {
		const r = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers: authHeaders(fixture),
			data: { title, fields: JSON.stringify({ status: 'open' }), tags: JSON.stringify(tags), content: `Body of ${title}` },
		});
		if (!r.ok()) throw new Error(`item create: ${r.status()} ${await r.text()}`);
		return (await r.json()) as { slug: string; title: string };
	};
	const exact = await create(`Exact ${stamp}`, [tag]);
	const variant = await create(`Variant ${stamp}`, [tag.toUpperCase()]);
	const second = await create(`Second ${stamp}`, [tag]);
	return { tag, exact, variant, second, stamp };
}

/** An in-app navigation, as a link click is: the workspace layout stays
 *  mounted, so what follows is the PAGE's requests, not a full reload's. */
async function navigate(page: Page, href: string) {
	await page.evaluate((h) => {
		const a = document.createElement('a');
		a.href = h;
		document.body.appendChild(a);
		a.click();
		a.remove();
	}, href);
}

/** Requests the page makes, from the navigation until the page has settled. */
async function apiRequestsDuring(page: Page, act: () => Promise<void>): Promise<string[]> {
	const seen: string[] = [];
	const onReq = (r: Request) => {
		const u = new URL(r.url());
		if (r.method() === 'GET' && u.pathname.startsWith('/api/v1/') && !u.pathname.startsWith('/api/v1/events')) {
			seen.push(`${u.pathname}${u.search}`);
		}
	};
	page.on('request', onReq);
	await act();
	await page.waitForLoadState('networkidle');
	page.off('request', onReq);
	return seen;
}

test('the tag and starred pages render from the index', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a request census');
	test.setTimeout(60_000);
	const { tag, exact, variant, second } = await seed(fixture, request);
	await browserLogin(page);
	const base = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;

	// Land in the workspace first, so the layout's own requests (the index,
	// the collection list, the starred ids) are made before the census.
	await page.goto(`${base}/tasks`);
	await expect(page.getByText(exact.title)).toBeVisible();
	await page.waitForLoadState('networkidle');

	const tagRequests = await apiRequestsDuring(page, async () => {
		await navigate(page, `${base}/tags/${encodeURIComponent(tag)}`);
		await expect(page.getByText(exact.title)).toBeVisible();
	});
	await expect(page.getByText(second.title)).toBeVisible();
	// Exact: the upper-case variant is a different tag, as the tag list counts it.
	await expect(page.getByText(variant.title)).toHaveCount(0);
	await expect(page.getByText('2 items')).toBeVisible();
	testInfo.annotations.push({ type: 'tag-page-requests', description: tagRequests.join(' | ') });
	expect(tagRequests.filter((r) => /\/items(\?|$)|\/collections(\?|$)/.test(r)), 'the tag page fetched items or collections').toEqual([]);

	// Star `second`, then `exact`: star order puts exact first. A star's time
	// is stored to the second, so two stars in one second tie (the server's
	// order between them is then arbitrary); space them.
	for (const [i, it] of [second, exact].entries()) {
		if (i > 0) await page.waitForTimeout(1100);
		const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${it.slug}/star`, { headers: authHeaders(fixture) });
		expect(r.ok()).toBeTruthy();
	}
	// A full load, so the layout's starred store loads these stars, then an
	// in-app navigation to the starred page for the census.
	await page.goto(`${base}/tasks`);
	await expect(page.getByText(exact.title)).toBeVisible();
	await page.waitForLoadState('networkidle');
	const starredRequests = await apiRequestsDuring(page, async () => {
		await navigate(page, `${base}/starred`);
		await expect(page.getByText(exact.title)).toBeVisible();
	});
	await expect(page.getByText(second.title)).toBeVisible();
	const exactBox = await page.getByText(exact.title).boundingBox();
	const secondBox = await page.getByText(second.title).boundingBox();
	expect(exactBox && secondBox && exactBox.y < secondBox.y, 'most recently starred first').toBeTruthy();
	testInfo.annotations.push({ type: 'starred-page-requests', description: starredRequests.join(' | ') });
	expect(
		starredRequests.filter((r) => /\/starred|\/items(\?|$)|\/collections(\?|$)/.test(r)),
		'the starred page fetched starred items, items or collections'
	).toEqual([]);
});
