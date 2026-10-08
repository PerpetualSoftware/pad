import { expect, test } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import type { Browser, Page } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';
import { suiteFixture } from './fixtures';

/**
 * TASK-2259 + TASK-2236: the auth pages are real forms. Every field sits in a
 * <form> with a label, errors are announced (role=alert), the first field has
 * focus, and submitting goes through the form's submit event, not a keydown
 * handler. Password managers key on that structure: a username field and a
 * current-password field in one form.
 *
 * Signed out throughout (an empty storage state). The setup page is not
 * covered here: it only renders a form on an instance with no admin yet, and
 * the suite's instance has one.
 */

const LABEL_RULES = ['label', 'label-title-only', 'form-field-multiple-labels', 'aria-input-field-name'];

async function signedOut(browser: Browser) {
	const { baseURL } = suiteFixture();
	return browser.newContext({ baseURL, storageState: { cookies: [], origins: [] } });
}

/** Every visible input on the card is inside a form, and axe finds no label problems. */
async function expectRealForm(page: Page, what: string) {
	const loose = await page.locator('input:visible').evaluateAll((els) => els.filter((e) => !e.closest('form')).length);
	expect(loose, `${what}: inputs outside a <form>`).toBe(0);
	expect(await page.locator('form').count(), `${what}: has a form`).toBeGreaterThan(0);
	const result = await new AxeBuilder({ page }).withRules(LABEL_RULES).analyze();
	expect(result.violations.map((v) => `${v.id}: ${v.help} (${v.nodes.length})`), `${what}: axe label rules`).toEqual([]);
}

test.describe('auth pages are real forms (TASK-2259, TASK-2236)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'markup and focus; one browser is enough');
	});

	test('login: labelled, focused, autofill-shaped, and it signs in by submitting the form', async ({ browser }) => {
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			await page.goto('/login');
			const email = page.getByLabel('Email');
			const password = page.getByLabel('Password');
			await expect(email).toBeVisible();
			await expect(email, 'the first field has focus on load').toBeFocused();
			await expectRealForm(page, 'login');

			// The pairing a password manager looks for: an identifier and a
			// current password in the SAME form.
			const sameForm = await page.evaluate(() => {
				const user = document.querySelector('input[autocomplete="username"]');
				const pass = document.querySelector('input[type="password"][autocomplete="current-password"]');
				return !!user && !!pass && user.closest('form') !== null && user.closest('form') === pass.closest('form');
			});
			expect(sameForm, 'username + current-password share one form').toBe(true);

			// An empty submit is refused with an announced error.
			await page.locator('form').evaluate((f) => (f as HTMLFormElement).requestSubmit());
			await expect(page.getByRole('alert')).toContainText('Please enter your email.');

			// Submit THROUGH the form: requestSubmit fires the submit event a
			// keydown handler would never see.
			await email.fill(ADMIN_EMAIL);
			await password.fill(ADMIN_PASSWORD);
			const [response] = await Promise.all([
				page.waitForResponse((r) => r.url().endsWith('/api/v1/auth/login') && r.request().method() === 'POST'),
				page.locator('form').evaluate((f) => (f as HTMLFormElement).requestSubmit())
			]);
			expect(response.status()).toBe(200);
			// No native navigation happened: the password never reached a URL.
			expect(page.url()).not.toContain('password');
			await expect(page).not.toHaveURL(/\/login/);
		} finally {
			await context.close();
		}
	});

	test('register, forgot and reset: labelled forms with the first field focused', async ({ browser }) => {
		const context = await signedOut(browser);
		try {
			const page = await context.newPage();
			for (const [path, first] of [
				['/register', 'Name'],
				['/reset-password/not-a-real-token', 'New password']
			] as const) {
				await page.goto(path);
				const field = page.getByLabel(first, { exact: true });
				await expect(field, `${path}: ${first} is labelled`).toBeVisible();
				await expect(field, `${path}: ${first} has focus`).toBeFocused();
				await expectRealForm(page, path);
			}

			// The forgot page shows its form only when the server can send
			// mail; the suite's instance has no email provider, so it shows
			// the host-recovery instructions instead. Report email as
			// configured on the real session answer, and nothing else.
			await page.route('**/api/v1/auth/session', async (route) => {
				const real = await route.fetch();
				const body = { ...(await real.json()), email_configured: true };
				await route.fulfill({ response: real, json: body });
			});
			await page.goto('/forgot-password');
			const forgotEmail = page.getByLabel('Email', { exact: true });
			await expect(forgotEmail, '/forgot-password: Email is labelled').toBeVisible();
			await expect(forgotEmail, '/forgot-password: Email has focus').toBeFocused();
			await expectRealForm(page, '/forgot-password');
			// It submits through the form too, and an empty submit is announced.
			await page.locator('form').evaluate((f) => (f as HTMLFormElement).requestSubmit());
			await expect(page.getByRole('alert')).toBeVisible();
		} finally {
			await context.close();
		}
	});

	test('join: an invitation link shows a labelled form, focused on its first field', async ({ browser }) => {
		const fixture = suiteFixture();
		const auth = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
		const context = await signedOut(browser);
		const api = context.request;
		const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
		const ws = await api.post('/api/v1/workspaces', { headers: auth, data: { name: `Auth forms ${tag}` } });
		expect(ws.ok(), await ws.text()).toBe(true);
		const slug = ((await ws.json()) as { slug: string }).slug;
		try {
			const inv = await api.post(`/api/v1/workspaces/${slug}/members/invite`, {
				headers: auth,
				data: { email: `forms${tag}@example.com`, role: 'editor' }
			});
			expect(inv.ok(), await inv.text()).toBe(true);
			const { code } = (await inv.json()) as { code: string };

			const page = await context.newPage();
			await page.goto(`/join/${code}`);
			const first = page.locator('form input:visible').first();
			await expect(first).toBeVisible();
			await expect(first, 'the first editable field has focus').toBeFocused();
			await expectRealForm(page, '/join');
		} finally {
			await api.delete(`/api/v1/workspaces/${slug}`, { headers: auth });
			await context.close();
		}
	});
});
