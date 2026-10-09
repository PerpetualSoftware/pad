import { expect, test } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import type { Browser } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';
import { suiteFixture } from './fixtures';

/**
 * TASK-3509: the auth pages follow the theme, with a visible light/dark toggle
 * whose choice is the app's own preference ('pad-theme'), so signing in keeps
 * it. The suite's instance is self-hosted, so this also covers the slim
 * self-hosted header: the Pad mark, the host chip and the toggle.
 */

async function signedOut(browser: Browser, theme?: 'light' | 'dark') {
	const { baseURL } = suiteFixture();
	// A dark system preference, so the default is dark and only the toggle (or
	// a stored choice) makes the page light.
	const context = await browser.newContext({ baseURL, colorScheme: 'dark', storageState: { cookies: [], origins: [] } });
	if (theme) {
		await context.addInitScript((t) => {
			try {
				localStorage.setItem('pad-theme', t);
			} catch {
				/* ignore */
			}
		}, theme);
	}
	return context;
}

const htmlTheme = (page: import('@playwright/test').Page) =>
	page.evaluate(() => document.documentElement.getAttribute('data-theme'));

async function signIn(page: import('@playwright/test').Page) {
	await page.getByLabel('Email', { exact: true }).fill(ADMIN_EMAIL);
	await page.getByLabel('Password', { exact: true }).fill(ADMIN_PASSWORD);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).not.toHaveURL(/\/login/);
}

test.describe('auth pages: theme (TASK-3509)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough');
	});

	test('the toggle on /login is the app preference: it survives signing in and a reload', async ({ browser }) => {
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			await page.goto('/login');
			const toggle = page.getByRole('button', { name: 'Switch to light theme' });
			await expect(toggle).toBeVisible();
			expect(await htmlTheme(page), 'dark before the toggle').not.toBe('light');

			await toggle.click();
			expect(await htmlTheme(page)).toBe('light');
			await expect(page.getByRole('button', { name: 'Switch to dark theme' })).toBeVisible();
			expect(await page.evaluate(() => localStorage.getItem('pad-theme'))).toBe('light');

			await signIn(page);
			// A fresh load restores only what was stored, so this is the
			// persistence, not the attribute the toggle left on the DOM.
			await page.reload();
			await expect.poll(() => htmlTheme(page)).toBe('light');
		} finally {
			await context.close();
		}
	});

	test('control: without the toggle, the app opens dark for a dark system preference', async ({ browser }) => {
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			await page.goto('/login');
			await signIn(page);
			await page.reload();
			// Wait for the app shell, then read the theme it settled on.
			await page.waitForLoadState('networkidle');
			expect(await htmlTheme(page)).not.toBe('light');
		} finally {
			await context.close();
		}
	});

	test('self-hosted header: the mark, the host and the toggle; no marketing links', async ({ browser }) => {
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			await page.goto('/login');
			const header = page.locator('header.auth-header');
			await expect(header.locator('img[src="/pad-mark.svg"]')).toBeVisible();
			await expect(header.getByText(new URL(suiteFixture().baseURL).host)).toBeVisible();
			await expect(header.getByRole('button', { name: /Switch to (light|dark) theme/ })).toBeVisible();
			await expect(header.getByRole('link', { name: 'Blog' })).toHaveCount(0);
			// The mark is served (a broken image would still be "visible").
			const ok = await page.evaluate(() => {
				const img = document.querySelector<HTMLImageElement>('header.auth-header img');
				return !!img && img.complete && img.naturalWidth > 0;
			});
			expect(ok, 'pad-mark.svg loaded').toBe(true);
		} finally {
			await context.close();
		}
	});

	for (const theme of ['dark', 'light'] as const) {
		for (const path of ['/login', '/forgot-password']) {
			test(`axe: ${path} in ${theme} has no contrast or label violations`, async ({ browser }) => {
				const context = await signedOut(browser, theme);
				try {
					const page = await context.newPage();
					await page.goto(path);
					await expect(page.locator('header.auth-header')).toBeVisible();
					expect(await htmlTheme(page)).toBe(theme);
					// Where the page has a form, a filled field means an enabled submit
					// button, whose fill is the colour that has to pass (axe skips a
					// disabled one). /forgot-password on this suite's instance has no
					// email provider, so it shows recovery instructions and no form.
					const email = page.getByLabel('Email', { exact: true });
					if (path === '/login') {
						await email.fill('you@example.com');
						await expect(page.locator('button[type="submit"]')).toBeEnabled();
					}
					// The stored theme is applied before first paint of the card,
					// but let any transitions settle before sampling colours.
					await page.evaluate(() => Promise.all(document.getAnimations().map((a) => a.finished)));
					const result = await new AxeBuilder({ page })
						.withRules(['color-contrast', 'label', 'button-name', 'image-alt'])
						.analyze();
					expect(result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)).toEqual([]);
				} finally {
					await context.close();
				}
			});
		}
	}
});
