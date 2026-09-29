import { test, expect, request as playwrightRequest, type Page } from '@playwright/test';
import { suiteFixture } from './fixtures';

/**
 * PLAN-3291 DR-7 / TASK-3293 (BUG-3290): with the mobile shells' user agent,
 * no upgrade copy, pricing or billing link is reachable in the web app. The
 * mobile apps render this web app, so any of it would be a purchase call to
 * action inside them.
 *
 * This checks what RENDERS, not source strings (the vitest guard does that).
 * Every assertion runs twice: once with the shell UA, where it must find
 * nothing, and once with an ordinary browser UA on the same pages, where it
 * must find the surface (CONVE-12: an absence proves nothing if the surface
 * never rendered at all).
 *
 * The e2e server is self-hosted, so the session is patched to cloud_mode and
 * billing_available (every commerce surface on); a plan-limit refusal is
 * stubbed at the create-workspace door. Desktop only: UA, not viewport, is the
 * variable, and each leg registers and logs in a user (IP rate-limited).
 */

const SHELL_UA =
	'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/129.0 Mobile Safari/537.36 PadShell/1';
const BROWSER_UA =
	'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36';
const PASSWORD = 'Playwright-NoCommerce-2026!';

const COMMERCE_LINKS =
	'a[href*="/console/billing"], a[href*="/billing/portal"], a[href*="stripe.com"], a[href*="/pricing"], a[href^="mailto:info@"]';
const COMMERCE_COPY = /Upgrade to Pro|View Plans|Manage Billing|Compare plans|coming soon/i;

async function register(baseURL: string, adminToken: string, tag: string): Promise<string> {
	const suffix = `${tag}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`;
	const email = `e2e-nc-${suffix}@example.com`;
	const api = await playwrightRequest.newContext({
		baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${adminToken}` }
	});
	try {
		const resp = await api.post('/api/v1/auth/register', {
			data: { email, username: `e2enc${suffix}`, name: 'No Commerce', password: PASSWORD }
		});
		if (!resp.ok()) throw new Error(`register failed (${resp.status()}): ${await resp.text()}`);
	} finally {
		await api.dispose();
	}
	return email;
}

async function openAsCloudUser(page: Page, email: string): Promise<void> {
	await page.goto('/login');
	await page.getByPlaceholder('Email').fill(email);
	await page.getByPlaceholder('Password').fill(PASSWORD);
	await Promise.all([
		page.waitForResponse((r) => r.url().includes('/api/v1/auth/login') && r.request().method() === 'POST'),
		page.getByRole('button', { name: /^sign in$/i }).click()
	]);
	await page.route('**/api/v1/auth/session', async (route) => {
		const resp = await route.fetch();
		const body = await resp.json();
		body.cloud_mode = true;
		body.billing_available = true;
		await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
	});
	await page.route('**/api/v1/workspaces', async (route) => {
		if (route.request().method() !== 'POST') return route.fallback();
		await route.fulfill({
			status: 403,
			contentType: 'application/json',
			body: JSON.stringify({
				error: {
					code: 'plan_limit_exceeded',
					message: "You've reached the 1-workspace limit on the free plan.",
					details: { feature: 'workspaces', limit: 1, current: 1, plan: 'free' }
				}
			})
		});
	});
}

async function commerceOn(page: Page): Promise<{ links: number; copy: boolean }> {
	const links = await page.locator(COMMERCE_LINKS).count();
	const copy = COMMERCE_COPY.test(await page.locator('body').innerText());
	return { links, copy };
}

for (const leg of [
	{ name: 'app (PadShell UA) renders no commerce', ua: SHELL_UA, shell: true },
	{ name: 'browser control renders the commerce surfaces', ua: BROWSER_UA, shell: false }
]) {
	test(`TASK-3293: ${leg.name}`, async ({ browser }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'UA is the variable, not viewport; one project keeps auth rate-limit pressure down.');
		const { baseURL, apiToken } = suiteFixture();
		const email = await register(baseURL, apiToken, leg.shell ? 's' : 'b');
		const context = await browser.newContext({ baseURL, userAgent: leg.ua });
		try {
			const page = await context.newPage();
			await openAsCloudUser(page, email);

			// Settings: the Plan card renders in both; its buttons only in the browser.
			await page.goto('/console/settings');
			await expect(page.getByRole('heading', { name: 'Plan', exact: true })).toBeVisible();
			let seen = await commerceOn(page);
			if (leg.shell) {
				expect(seen).toEqual({ links: 0, copy: false });
			} else {
				// the settings card's Upgrade button and the console nav's Billing link
				expect(seen.links).toBeGreaterThanOrEqual(2);
				expect(seen.copy).toBe(true);
			}

			// Billing, by deep link: plan and limits only in the app.
			await page.goto('/console/billing');
			await expect(page.getByRole('heading', { name: 'Current Plan' })).toBeVisible();
			seen = await commerceOn(page);
			if (leg.shell) {
				await expect(page.getByTestId('plan-limits-card')).toBeVisible();
				expect(seen).toEqual({ links: 0, copy: false });
				await expect(page.getByRole('button', { name: /upgrade/i })).toHaveCount(0);
			} else {
				await expect(page.getByRole('heading', { name: 'Compare plans' })).toBeVisible();
				await expect(page.getByRole('button', { name: 'Upgrade to Pro' }).first()).toBeVisible();
			}

			// The checkout return deep links: web-checkout states, dropped in the app.
			for (const q of ['success', 'cancelled']) {
				await page.goto(`/console/billing?checkout=${q}`);
				await expect(page.getByRole('heading', { name: 'Current Plan' })).toBeVisible();
				const banner = page.locator('.upgrade-banner');
				if (leg.shell) {
					await expect(banner).toHaveCount(0);
					await expect(page).not.toHaveURL(/checkout=/);
				} else {
					await expect(banner).toBeVisible();
				}
			}

			// A plan-limit refusal: the toast states the limit; only the browser adds the upgrade.
			await page.goto('/console');
			await page.getByRole('button', { name: 'Create Workspace', exact: true }).click();
			await page.getByPlaceholder('Workspace name').fill('Over the limit');
			await Promise.all([
				page.waitForResponse((r) => r.url().endsWith('/api/v1/workspaces') && r.request().method() === 'POST'),
				page.getByRole('button', { name: 'Create Workspace', exact: true }).last().click()
			]);
			const toast = page.locator('.toast-error').filter({ hasText: '1-workspace limit' });
			await expect(toast).toBeVisible();
			if (leg.shell) {
				await expect(toast).not.toContainText(/upgrade/i);
				await expect(toast).not.toHaveAttribute('role', 'button');
			} else {
				await expect(toast).toContainText('Upgrade to Pro');
				await expect(toast).toHaveAttribute('role', 'button');
			}
		} finally {
			await context.close();
		}
	});
}
