import { test, expect, request as playwrightRequest } from '@playwright/test';
import { suiteFixture } from './fixtures';

/**
 * Danger Zone — "Delete my account" (TASK-1962 / TASK-1963).
 *
 * These specs must NOT run under the shared admin Bearer context: the header
 * from e2e/fixtures.ts would make every request auth as the suite admin, so a
 * real delete would wipe the admin the rest of the suite depends on. Instead
 * each test spins up a CLEAN context (no Bearer) and a throwaway user.
 *
 * Two branches of web/src/routes/console/settings/+page.svelte::deleteAccount
 * are covered:
 *   (b) password branch — the only branch a self-hosted server ever renders
 *       (usePasswordBranch is true whenever cloud_mode is false). Driven
 *       end-to-end against the real server: register → login → delete → the
 *       account is really gone (admin user search no longer lists it).
 *   (c) cloud OAuth-only typed-confirm branch — renders only when
 *       cloud_mode === true AND password_set === false. The self-hosted e2e
 *       binary reports neither, so we patch the two flag-bearing responses
 *       (/auth/session, /auth/me) to flip just those flags, and stub the
 *       delete transport (a self-host server 400s a confirm-only delete —
 *       "password required"). The real server contract for both branches is
 *       pinned in internal/server/handlers_account_test.go; this spec asserts
 *       the UI branch selection + the post-delete /login redirect (d).
 *
 * These run on desktop-chromium only. The delete flow is viewport-agnostic,
 * and running it on both projects doubles the hits to /auth/login and
 * /auth/register — both IP-rate-limited (burst 5; see middleware_ratelimit.go)
 * — which flaked the suite under Playwright's parallel load.
 */

// A password that satisfies validatePasswordStrength (see
// internal/server/handlers_auth.go) and isn't derivable from the email/name.
const THROWAWAY_PASSWORD = 'Playwright-Delete-2026!';

// A collision-safe email + username. The username is passed explicitly to the
// register call: letting the server auto-generate one from the display name
// risked a UNIQUE-constraint 500 across concurrent runs.
function throwawayIdentity(tag: string): { email: string; username: string } {
	const suffix = `${tag}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`;
	return { email: `e2e-del-${suffix}@example.com`, username: `e2edel${suffix}` };
}

// Create a verified, password-holding user via the admin-authenticated
// register endpoint. Admin-created accounts stay verified (handleRegister),
// so the user can log in and reach settings immediately. Bearer auth exempts
// the call from CSRF.
async function createThrowawayUser(
	baseURL: string,
	adminToken: string,
	identity: { email: string; username: string },
	name: string
): Promise<void> {
	const api = await playwrightRequest.newContext({
		baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${adminToken}` }
	});
	try {
		const resp = await api.post('/api/v1/auth/register', {
			data: {
				email: identity.email,
				username: identity.username,
				name,
				password: THROWAWAY_PASSWORD
			}
		});
		if (!resp.ok()) {
			throw new Error(`register throwaway user failed (${resp.status()}): ${await resp.text()}`);
		}
	} finally {
		await api.dispose();
	}
}

// Log in through the real form. Sessions are User-Agent-bound (see
// fixtures.ts); minting the session from the browser itself keeps the UA
// consistent so it isn't silently rejected on the next request.
async function loginViaForm(page: import('@playwright/test').Page, email: string): Promise<void> {
	await page.goto('/login');
	await page.getByPlaceholder('Email').fill(email);
	await page.getByPlaceholder('Password').fill(THROWAWAY_PASSWORD);
	await Promise.all([
		page.waitForResponse(
			(r) => r.url().includes('/api/v1/auth/login') && r.request().method() === 'POST'
		),
		page.getByRole('button', { name: /^sign in$/i }).click()
	]);
}

test.describe('Danger Zone: delete my account', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'Account-deletion flow is viewport-agnostic; run once (desktop) to avoid multiplying auth-endpoint rate-limit pressure across projects.'
		);
	});

	test('password branch removes the account and redirects to /login', async ({ browser }) => {
		const { baseURL, apiToken } = suiteFixture();
		const identity = throwawayIdentity('pw');
		await createThrowawayUser(baseURL, apiToken, identity, 'Delete PW');

		const context = await browser.newContext({ baseURL });
		try {
			const page = await context.newPage();
			await loginViaForm(page, identity.email);

			await page.goto('/console/settings');

			// Reveal the confirm block. Only the reveal button exists at this
			// point (the "Permanently delete…" button appears after reveal), so
			// an exact name match is unambiguous.
			await page.getByRole('button', { name: 'Delete my account', exact: true }).click();

			// Self-host always uses the password branch.
			await page.locator('#delete-password').fill(THROWAWAY_PASSWORD);
			await page.getByRole('button', { name: 'Permanently delete my account' }).click();

			// (d) On success the server clears the cookies and the client
			// hard-navigates to /login.
			await page.waitForURL('**/login', { timeout: 15_000 });
			expect(new URL(page.url()).pathname).toBe('/login');
		} finally {
			await context.close();
		}

		// The account is really gone: an admin user search no longer lists it.
		// (A GET on the generous API limiter, so it adds no /auth rate pressure.)
		const admin = await playwrightRequest.newContext({
			baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${apiToken}` }
		});
		try {
			const resp = await admin.get(
				`/api/v1/admin/users?q=${encodeURIComponent(identity.email)}`
			);
			expect(resp.ok()).toBeTruthy();
			const body = (await resp.json()) as { users?: Array<{ email: string }> };
			const emails = (body.users ?? []).map((u) => u.email);
			expect(emails).not.toContain(identity.email);
		} finally {
			await admin.dispose();
		}
	});

	test('cloud OAuth-only typed-confirm branch redirects to /login', async ({ browser }) => {
		const { baseURL, apiToken } = suiteFixture();
		const identity = throwawayIdentity('cloud');
		await createThrowawayUser(baseURL, apiToken, identity, 'Delete Cloud');

		const context = await browser.newContext({ baseURL });
		try {
			const page = await context.newPage();

			// Real login first, so every un-mocked layout/settings request is
			// genuinely authenticated (no 401 → /login bounce).
			await loginViaForm(page, identity.email);

			// The delete is stubbed (below), so the real session stays valid —
			// which would make the post-delete /login page see authenticated:true
			// and bounce back to /console, racing the assertion. Flip the mocked
			// session to authenticated:false once the delete fires so /login stays.
			let deleted = false;
			let deletePayload: Record<string, unknown> | null = null;

			// Force the cloud OAuth-only shape by patching only the flags that
			// select the typed-confirm branch; every other real field is kept.
			await page.route('**/api/v1/auth/session', async (route) => {
				const resp = await route.fetch();
				const body = await resp.json();
				body.cloud_mode = true;
				if (deleted) {
					body.authenticated = false;
					body.user = null;
				}
				await route.fulfill({
					status: 200,
					contentType: 'application/json',
					body: JSON.stringify(body)
				});
			});
			await page.route('**/api/v1/auth/me', async (route) => {
				const resp = await route.fetch();
				const body = await resp.json();
				body.password_set = false;
				await route.fulfill({
					status: 200,
					contentType: 'application/json',
					body: JSON.stringify(body)
				});
			});

			// A self-host server 400s a confirm-only delete ("password required"),
			// so stub the transport at 200 {ok:true}. The real server contract is
			// covered by the Go handler tests; here we assert the UI branch + the
			// confirm-only payload + the success→/login redirect.
			await page.route('**/api/v1/auth/delete-account', async (route) => {
				deletePayload = route.request().postDataJSON();
				deleted = true;
				await route.fulfill({
					status: 200,
					contentType: 'application/json',
					body: JSON.stringify({ ok: true })
				});
			});

			await page.goto('/console/settings');

			await page.getByRole('button', { name: 'Delete my account', exact: true }).click();

			// Typed-confirm branch: the password input is absent; a text confirm
			// input is shown instead.
			await expect(page.locator('#delete-password')).toHaveCount(0);
			await page.locator('#delete-confirm').fill('DELETE');
			await page.getByRole('button', { name: 'Permanently delete my account' }).click();

			await page.waitForURL('**/login', { timeout: 15_000 });
			expect(new URL(page.url()).pathname).toBe('/login');

			// The OAuth-only account sends confirm:true and NO password.
			expect(deletePayload).toMatchObject({ confirm: true });
			expect(deletePayload?.password).toBeUndefined();
		} finally {
			await context.close();
		}
	});
});
