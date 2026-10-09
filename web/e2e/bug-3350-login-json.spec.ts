import { expect, test } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';
import { suiteFixture } from './fixtures';

// BUG-3350: sign-in refuses anything but application/json (and a cross-site
// Origin), which is what stops a cross-site form signing a victim into the
// attacker's account. The web app's own sign-in must therefore send JSON:
// this pins it through the real login form, so a change to the shared fetch
// wrapper that dropped the header would fail here before it locked users out.
test('BUG-3350: the login form signs in with an application/json request', async ({ browser }) => {
	const { baseURL } = suiteFixture();
	const context = await browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } });
	try {
		const page = await context.newPage();
		await page.goto('/login');
		await page.getByLabel('Email', { exact: true }).fill(ADMIN_EMAIL);
		await page.getByLabel('Password', { exact: true }).fill(ADMIN_PASSWORD);
		const [request] = await Promise.all([
			page.waitForRequest((r) => r.url().endsWith('/api/v1/auth/login') && r.method() === 'POST'),
			page.getByLabel('Password', { exact: true }).press('Enter')
		]);
		expect(request.headers()['content-type'] ?? '').toContain('application/json');
		const response = await request.response();
		expect(response?.status()).toBe(200);
	} finally {
		await context.close();
	}
});
