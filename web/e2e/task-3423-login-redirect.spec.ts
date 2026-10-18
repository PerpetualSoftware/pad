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

	// Each `value` is what validateRedirect receives: the spec URL-encodes it
	// once, and searchParams decodes it once. The encoded-slash case is a
	// value that still holds `%2f` after that decode, which a later decode
	// would turn into `//evil.example`; the guard refuses it by its %2f rule.
	const offSite = [
		{ label: 'absolute URL', value: 'https://evil.example/x' },
		{ label: 'protocol-relative', value: '//evil.example/x' },
		{ label: 'encoded slash surviving one decode', value: '/%2fevil.example/x' },
	];
	// Same origin, and past /login: one poll, so an intermediate URL cannot
	// satisfy half of it.
	const landedHere = (url: string, origin: string) => {
		const at = new URL(url);
		return at.origin === origin && at.pathname !== '/login' && !at.pathname.includes('evil.example');
	};

	for (const { label, value } of offSite) {
		test(`a signed-in visit with an off-site target (${label}) stays on this origin`, async ({ page, baseURL }) => {
			await browserLogin(page);
			await page.goto(`/login?redirect=${encodeURIComponent(value)}`);
			await expect.poll(() => landedHere(page.url(), new URL(baseURL!).origin)).toBe(true);
		});
	}

	test('signing in with the form and an off-site target stays on this origin', async ({ browser, baseURL }) => {
		const ctx = await browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } });
		const page = await ctx.newPage();
		try {
			await page.goto(`/login?redirect=${encodeURIComponent('//evil.example/x')}`);
			await page.locator('#login-email').fill(ADMIN_EMAIL);
			await page.locator('#login-password').fill(ADMIN_PASSWORD);
			await page.locator('form button[type="submit"]').last().click();
			await expect.poll(() => landedHere(page.url(), new URL(baseURL!).origin)).toBe(true);
		} finally {
			await ctx.close();
		}
	});
});
