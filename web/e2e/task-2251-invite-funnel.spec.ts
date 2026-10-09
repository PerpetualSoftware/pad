import { expect, test } from '@playwright/test';
import { request, type Browser } from '@playwright/test';
import { suiteFixture } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2251: the invitation link's funnel does not leak.
 *  - An unknown code says the link is invalid, instead of offering a signup
 *    that can only fail at the end.
 *  - A signed-out invitee is told WHICH workspace invited them.
 *  - The code survives a trip through /login: a failed accept's "Go to login"
 *    carries it, and signing in there comes back to this invitation.
 */

const PASSWORD = 'correct-horse-battery-staple-2251';

async function signedOut(browser: Browser) {
	const { baseURL } = suiteFixture();
	return browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } });
}

/** A workspace, an invitee account, and a pending invitation for it. */
async function seed(tag: string) {
	const fixture = suiteFixture();
	const admin = await request.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` }
	});
	const ws = await admin.post('/api/v1/workspaces', { data: { name: `Funnel ${tag}` } });
	expect(ws.ok(), await ws.text()).toBe(true);
	const { slug, name } = (await ws.json()) as { slug: string; name: string };
	const email = `funnel${tag}@example.com`;
	const reg = await admin.post('/api/v1/auth/register', {
		data: { email, username: `funnel${tag}`, name: `Funnel ${tag}`, password: PASSWORD }
	});
	expect(reg.ok(), await reg.text()).toBe(true);
	const inv = await admin.post(`/api/v1/workspaces/${slug}/members/invite`, { data: { email, role: 'editor' } });
	expect(inv.ok(), await inv.text()).toBe(true);
	const { code } = (await inv.json()) as { code: string };
	return {
		slug,
		name,
		email,
		code,
		cleanup: async () => {
			await admin.delete(`/api/v1/workspaces/${slug}`);
			await admin.dispose();
		}
	};
}

test.describe('invitation funnel (TASK-2251)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough');
	});

	test('an unknown code says the link is invalid, with no signup form', async ({ browser }) => {
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			await page.goto('/join/not-a-real-invitation-code-2251');
			await expect(page.getByTestId('join-invalid')).toContainText('invalid or has expired');
			await expect(page.locator('form')).toHaveCount(0);
			await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0);
		} finally {
			await context.close();
		}
	});

	test('a signed-out invitee is told which workspace invited them', async ({ browser }) => {
		const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
		const world = await seed(tag);
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			await page.goto(`/join/${world.code}`);
			await expect(page.getByText(`You've been invited to ${world.name}`)).toBeVisible();
		} finally {
			await context.close();
			await world.cleanup();
		}
	});

	test("a failed accept's login link carries the code, and signing in comes back to the invitation", async ({ browser }) => {
		const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
		const world = await seed(tag);
		// Signed in as the ADMIN, who is not the invited address: the accept is
		// refused, which is the error state whose login link used to drop the code.
		const wrongUser = await browser.newContext({ baseURL: suiteFixture().baseURL });
		const invitee = await signedOut(browser);
		try {
			const page = await wrongUser.newPage();
			await browserLogin(page);
			await page.goto(`/join/${world.code}`);
			await page.getByTestId('join-accept').click();
			const login = page.getByRole('link', { name: 'Go to login' });
			await expect(login).toBeVisible();
			const href = await login.getAttribute('href');
			expect(href).toBe(`/login?redirect=${encodeURIComponent(`/join/${world.code}`)}`);

			// Follow that link as the invitee, sign in, and land back on this
			// invitation's accept card.
			const p2 = await invitee.newPage();
			await p2.goto(href!);
			await p2.getByLabel('Email', { exact: true }).fill(world.email);
			await p2.getByLabel('Password', { exact: true }).fill(PASSWORD);
			await p2.getByRole('button', { name: 'Sign in' }).click();
			await expect(p2).toHaveURL(new RegExp(`/join/${world.code}$`));
			await expect(p2.getByTestId('join-accept')).toBeVisible();
		} finally {
			await wrongUser.close();
			await invitee.close();
			await world.cleanup();
		}
	});
});
