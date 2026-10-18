import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';

/**
 * TASK-3423: the login page reads ?redirect= through $app/state's `page`
 * (it used the $page store). That value is the open-redirect guard's input
 * (validateRedirect), so a read that came back empty, stale or unvalidated
 * would either drop the user's destination or send them off-site. Nothing
 * else exercises the read: the share and invite specs check only the link
 * that points at /login.
 */
test.describe('TASK-3423: login honours ?redirect= and still refuses off-site targets', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a URL read');
	});

	test('signing in with the form lands on the redirect target', async ({ browser, baseURL, fixture }) => {
		// A signed-out context: the default one carries the fixture's session,
		// and /login forwards a signed-in visitor before the form is filled.
		const ctx = await browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } });
		const page = await ctx.newPage();
		try {
			const target = `/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`;
			await page.goto(`/login?redirect=${encodeURIComponent(target)}`);
			await page.locator('#login-email').fill(ADMIN_EMAIL);
			await page.locator('#login-password').fill(ADMIN_PASSWORD);
			await page.locator('form button[type="submit"]').last().click();
			await expect.poll(() => new URL(page.url()).pathname).toBe(target);
		} finally {
			await ctx.close();
		}
	});

	test('an already signed-in visit forwards to the redirect target', async ({ page, fixture }) => {
		await browserLogin(page);
		const target = `/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`;
		await page.goto(`/login?redirect=${encodeURIComponent(target)}`);
		await expect.poll(() => new URL(page.url()).pathname).toBe(target);
	});

	for (const evil of ['https://evil.example/x', '//evil.example/x', '/%2fevil.example/x']) {
		test(`an off-site target (${evil}) stays on this origin`, async ({ page, baseURL }) => {
			await browserLogin(page);
			await page.goto(`/login?redirect=${encodeURIComponent(evil)}`);
			// It leaves /login for the default landing, on this origin.
			await expect.poll(() => new URL(page.url()).pathname).not.toBe('/login');
			const at = new URL(page.url());
			expect(at.origin).toBe(new URL(baseURL!).origin);
			expect(at.pathname).not.toContain('evil.example');
		});
	}
});
