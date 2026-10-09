import { test, expect } from './fixtures';
import { authJson } from './lib/attachment-viewer';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';
import type { APIRequestContext, Browser } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2249: a share link's two dead ends.
 *  - C13: "Sign in" on a sign-in-required share went to a bare /login, so the
 *    reader landed on /console with the shared item gone. It now carries the
 *    share path, and signing in comes back to the item.
 *  - C85: a revoked (or expired, or used-up) link and a dropped connection both
 *    read as "Unable to load / Not found". A gone link now says so in plain
 *    words; a network failure offers a retry instead of "Failed to fetch".
 */

async function sharedItem(fixture: SuiteFixture, request: APIRequestContext, shareData: Record<string, unknown>) {
	const stamp = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
	const item = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture),
		data: { title: `T2249 ${stamp}`, content: `Shared body ${stamp}`, fields: '{}' }
	});
	expect(item.ok(), await item.text()).toBeTruthy();
	const { slug } = (await item.json()) as { slug: string };
	const link = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}/share-links`, {
		headers: authJson(fixture),
		data: shareData
	});
	expect(link.ok(), await link.text()).toBeTruthy();
	const { token, id } = (await link.json()) as { token: string; id: string };
	return { token, id, body: `Shared body ${stamp}` };
}

async function signedOut(browser: Browser, fixture: SuiteFixture) {
	return browser.newContext({ baseURL: fixture.baseURL, storageState: { cookies: [], origins: [] } });
}

test.describe('share link dead ends (TASK-2249)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough');
	});

	test('signing in from a sign-in-required share comes back to it', async ({ browser, fixture, request }) => {
		const s = await sharedItem(fixture, request, { require_auth: true });
		const context = await signedOut(browser, fixture);
		try {
			const page = await context.newPage();
			await page.goto(`/s/${s.token}`);
			const signIn = page.getByRole('link', { name: 'Sign in' });
			await expect(signIn).toBeVisible();
			expect(await signIn.getAttribute('href')).toBe(`/login?redirect=${encodeURIComponent(`/s/${s.token}`)}`);

			await signIn.click();
			await page.getByLabel('Email', { exact: true }).fill(ADMIN_EMAIL);
			await page.getByLabel('Password', { exact: true }).fill(ADMIN_PASSWORD);
			await page.getByRole('button', { name: 'Sign in' }).click();
			await expect(page).toHaveURL(new RegExp(`/s/${s.token}$`));
			await expect(page.getByText(s.body)).toBeVisible();
		} finally {
			await context.close();
		}
	});

	test('a revoked link says it is gone and what to do, with no retry', async ({ browser, fixture, request }) => {
		const s = await sharedItem(fixture, request, {});
		const del = await request.delete(`/api/v1/workspaces/${fixture.workspaceSlug}/share-links/${s.id}`, { headers: authJson(fixture) });
		expect(del.ok(), await del.text()).toBeTruthy();
		const context = await signedOut(browser, fixture);
		try {
			const page = await context.newPage();
			await page.goto(`/s/${s.token}`);
			const alert = page.getByRole('alert');
			await expect(alert).toContainText("This link isn't available");
			await expect(alert).toContainText('Ask the person who shared it');
			await expect(page.getByRole('button', { name: 'Try again' })).toHaveCount(0);
		} finally {
			await context.close();
		}
	});

	test('a dropped connection offers a retry, and the retry loads the item', async ({ browser, fixture, request }) => {
		const s = await sharedItem(fixture, request, {});
		const context = await signedOut(browser, fixture);
		try {
			const page = await context.newPage();
			let fail = true;
			await page.route(`**/api/v1/s/${s.token}`, (route) => (fail ? route.abort('internetdisconnected') : route.continue()));
			await page.goto(`/s/${s.token}`);
			await expect(page.getByRole('alert')).toContainText("Couldn't reach Pad");
			await expect(page.getByRole('alert')).not.toContainText('Failed to fetch');
			fail = false;
			await page.getByRole('button', { name: 'Try again' }).click();
			await expect(page.getByText(s.body)).toBeVisible();
		} finally {
			await context.close();
		}
	});
});
