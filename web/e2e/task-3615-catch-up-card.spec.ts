import { request as pwRequest } from '@playwright/test';
import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { createWorkspace, deleteWorkspace } from './lib/attachment-viewer';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-3615 PR B: the catch-up card on the workspace home, end to end against
 * the real binary. The digest leaves out the viewer's own actions, so a
 * TEAMMATE seeds the work: the admin then sees it under "New and reopened",
 * follows See everything into Activity (Audit, since chip), comes back, marks
 * caught up, and the card collapses and stays collapsed across a reload,
 * because the cursor now lives on the server.
 */

const PASSWORD = 'Playwright-CatchUp-2026!';

function authJson(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) throw new Error(`${what}: ${resp.status()} ${await resp.text()}`);
	return resp;
}

/** Register a teammate, invite them to `ws`, accept as them, and return their bearer context. */
async function teammate(fixture: SuiteFixture, request: APIRequestContext, ws: string): Promise<APIRequestContext> {
	const username = `catchup${Date.now()}`;
	const email = `${username}@example.com`;
	// Account creation is platform administration: the admin's SESSION, not
	// its API token, which is refused 403 session_required (BUG-3361).
	const admin = await pwRequest.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` },
	});
	try {
		await ok(
			await admin.post('/api/v1/auth/register', {
				data: { email, username, name: 'Catch-up Teammate', password: PASSWORD },
			}),
			'register teammate'
		);
	} finally {
		await admin.dispose();
	}
	const session = await pwRequest.newContext({ baseURL: fixture.baseURL });
	let token = '';
	try {
		await ok(await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login teammate');
		const csrf = (await session.storageState()).cookies.find((c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf');
		if (!csrf) throw new Error('teammate login issued no pad_csrf cookie');
		const resp = await ok(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: 'catch-up-e2e', expires_in: 1 },
			}),
			'teammate token'
		);
		token = ((await resp.json()) as { token: string }).token;
	} finally {
		await session.dispose();
	}
	await ok(
		await request.post(`/api/v1/workspaces/${ws}/members/invite`, { headers: authJson(fixture), data: { email, role: 'editor' } }),
		'invite teammate'
	);
	const mate = await pwRequest.newContext({ baseURL: fixture.baseURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
	const list = (await (await ok(await mate.get('/api/v1/me/invitations'), 'list invitations')).json()) as {
		invitations: { id: string; workspace_slug: string }[];
	};
	const inv = list.invitations.find((i) => i.workspace_slug === ws);
	if (!inv) throw new Error(`no pending invitation to ${ws}`);
	await ok(await mate.post(`/api/v1/me/invitations/${inv.id}/accept`), 'accept invitation');
	return mate;
}

test.describe('catch-up card (TASK-3615)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one desktop browser is enough for the card flow');
	});

	test('a teammate’s new work shows, See everything opens the window in Audit, and Mark caught up sticks', async ({ page, fixture, request }) => {
		const { slug: ws } = await createWorkspace(fixture, request, 'Catch up');
		let mate: APIRequestContext | undefined;
		try {
			mate = await teammate(fixture, request, ws);
			for (const title of ['Catch-up alpha', 'Catch-up beta']) {
				await ok(await mate.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { data: { title } }), `create ${title}`);
			}
			// The window's floor is inclusive at second precision, so a mark in
			// the same second as these writes would show them again. Server and
			// browser share this box's clock (TASK-3615 PR B r1).
			const madeInSecond = Math.floor(Date.now() / 1000);

			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}`);
			const card = page.getByTestId('catch-up-card');
			await expect(card).toBeVisible();
			const fresh = page.getByTestId('catch-up-section-new_and_reopened');
			await expect(fresh).toContainText('Catch-up alpha');
			await expect(fresh).toContainText('Catch-up beta');

			await page.getByTestId('catch-up-see-everything').click();
			await expect(page).toHaveURL(/\/activity\?since=.+&view=audit$/);
			await expect(page.getByTestId('activity-since-chip')).toBeVisible();
			await expect(page.getByRole('button', { name: 'Audit' })).toHaveClass(/selected/);
			// Clearing the chip drops since from the URL, so a reload stays cleared.
			await page.getByRole('button', { name: 'Remove the since filter' }).click();
			await expect(page).not.toHaveURL(/since=/);
			// Assert on the reloaded feed request itself: a chip's absence passes
			// before a late effect could add it (PR B r3).
			const reloadedFeed = page.waitForRequest((r) => /\/api\/v1\/workspaces\/[^/]+\/activity(\?|$)/.test(r.url()));
			await page.reload();
			expect(new URL((await reloadedFeed).url()).searchParams.has('since')).toBe(false);
			await expect(page).not.toHaveURL(/since=/);
			await expect(page.getByRole('button', { name: 'Audit' })).toBeVisible();
			await expect(page.getByTestId('activity-since-chip')).toHaveCount(0);

			await page.goto(`/${fixture.adminUsername}/${ws}`);
			await expect.poll(() => Math.floor(Date.now() / 1000), { timeout: 3000 }).toBeGreaterThan(madeInSecond);
			await page.getByTestId('catch-up-mark').click();
			await expect(page.getByTestId('catch-up-caught-up')).toContainText("You're caught up · nothing new since");
			await expect(page.getByTestId('catch-up-section-new_and_reopened')).toHaveCount(0);

			await page.reload();
			await expect(page.getByTestId('catch-up-caught-up')).toBeVisible();
			await expect(page.getByTestId('catch-up-section-new_and_reopened')).toHaveCount(0);
		} finally {
			await mate?.dispose();
			await deleteWorkspace(fixture, request, ws);
		}
	});
});

