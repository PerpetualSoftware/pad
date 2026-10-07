import { test, expect, type SuiteFixture } from './fixtures';
import { request, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { quietCrossActorToasts } from './fixtures';

/**
 * TASK-2234 (July audit C8 + C45): the command palette, keyboard only, as a
 * screen reader meets it. The input is a combobox controlling a listbox of
 * options; arrowing moves aria-activedescendant and aria-selected; the result
 * count is announced; Enter opens the selected result. With an empty query
 * the recent searches are the listbox, and the arrows and Enter work on them.
 * axe reports nothing on the open palette, with results and with recents.
 */

const PASSWORD = 'Playwright-Tabs-2026!';

interface Account {
	username: string;
	token: string;
	api: APIRequestContext;
}


/** Waits until no CSS transition is running anywhere in the document. */
async function settleTransitions(page: Page) {
	await page.evaluate(async () => {
		for (;;) {
			// Flush style so a change written since the last recalc has started.
			void getComputedStyle(document.documentElement).color;
			const live = document
				.getAnimations()
				.filter((a) => a instanceof CSSTransition && a.playState !== 'finished' && a.playState !== 'idle');
			if (live.length === 0) return;
			// A retargeted transition is cancelled, which rejects `finished`.
			await Promise.all(live.map((a) => a.finished.catch(() => undefined)));
		}
	});
}
async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) throw new Error(`palette seed: ${what} failed (${resp.status()}): ${await resp.text()}`);
	return resp;
}

async function mintAccount(fixture: SuiteFixture, tag: string): Promise<Account> {
	const admin = await request.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` }
	});
	try {
		const username = `cp${tag}`;
		const email = `${username}@example.com`;
		// Admin-created accounts are verified, so they can log in at once.
		await ok(
			await admin.post('/api/v1/auth/register', { data: { email, username, name: `Palette ${tag}`, password: PASSWORD } }),
			'register'
		);
		return await accountFor(fixture, username, email, tag);
	} finally {
		await admin.dispose();
	}
}

/** An API handle for an account that already exists, by signing in as it. */
async function accountFor(fixture: SuiteFixture, username: string, email: string, tag: string): Promise<Account> {
	const session = await request.newContext({ baseURL: fixture.baseURL });
	try {
		await ok(await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login');
		const csrf = (await session.storageState()).cookies.find(
			(c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf'
		);
		if (!csrf) throw new Error('palette seed: login issued no pad_csrf cookie');
		const tokenResp = await ok(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: `cp-${tag}`, expires_in: 1 }
			}),
			'token'
		);
		const { token } = (await tokenResp.json()) as { token: string };
		const api = await request.newContext({
			baseURL: fixture.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${token}` }
		});
		return { username, token, api };
	} finally {
		await session.dispose();
	}
}

async function createWorkspace(account: Account, name: string): Promise<string> {
	const resp = await ok(await account.api.post('/api/v1/workspaces', { data: { name } }), `create ${name}`);
	return ((await resp.json()) as { slug: string }).slug;
}

async function actAs(context: BrowserContext, account: Account) {
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${account.token}` });
	await quietCrossActorToasts(context);
}

test('the command palette is a combobox a keyboard and a screen reader can use', async ({ page, context, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'Cmd+K is the desktop surface');
	const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
	const account = await mintAccount(fixture, tag);
	try {
		const ws = ((await (await ok(await account.api.post('/api/v1/workspaces', { data: { name: `P ${tag}`, template: 'startup' } }), 'ws')).json()) as { slug: string }).slug;
		for (const t of ['Zephyr alpha', 'Zephyr beta', 'Zephyr gamma']) {
			await ok(await account.api.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { data: { title: t, fields: '{"status":"open"}' } }), 'item');
		}
		// Found only by its body: the title does not contain the word.
		await ok(await account.api.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { data: { title: 'Plain title', content: 'a quokkaword lives here', fields: '{"status":"open"}' } }), 'body item');
		await actAs(context, account);
		await page.setViewportSize({ width: 1280, height: 800 });
		await page.goto(`/${account.username}/${ws}`);
		await page.locator('a.nav-item').first().waitFor(); // the app is up and listening for Cmd+K

		// Open by keyboard and type.
		await page.keyboard.press('Control+k');
		const input = page.locator('.palette .search-input');
		await expect(input).toBeFocused();
		await page.keyboard.type('Zephyr');

		await expect(input).toHaveAttribute('role', 'combobox');
		await expect(input).toHaveAttribute('aria-expanded', 'true');
		// aria-controls is an ID LIST: local results and content matches can
		// both be on screen (codex r1).
		const controls = ((await input.getAttribute('aria-controls')) ?? '').split(/\s+/).filter(Boolean);
		expect(controls.length, 'the combobox names the listbox(es) it controls').toBeGreaterThan(0);
		for (const id of controls) await expect(page.locator(`[id="${id}"]`)).toHaveAttribute('role', 'listbox');
		const listbox = page.locator(`[id="${controls[0]}"]`);
		const options = listbox.getByRole('option');
		await expect(options).toHaveCount(3);
		// The count is announced.
		await expect(page.locator('.palette [aria-live="polite"]')).toContainText('3 results');

		// Arrowing moves the active descendant and aria-selected.
		await page.keyboard.press('ArrowDown');
		const first = options.nth(0);
		const firstId = await first.getAttribute('id');
		expect(firstId).toBeTruthy();
		await expect(input).toHaveAttribute('aria-activedescendant', firstId!);
		await expect(first).toHaveAttribute('aria-selected', 'true');
		await page.keyboard.press('ArrowDown');
		await expect(input).toHaveAttribute('aria-activedescendant', (await options.nth(1).getAttribute('id'))!);
		await expect(first).toHaveAttribute('aria-selected', 'false');

		const withResults = await new AxeBuilder({ page }).include('.palette').analyze();
		expect(withResults.violations.map((v) => `${v.id}: ${v.help}`), 'axe on the open palette with results').toEqual([]);
		// And in the dark theme: the status pills' text is a mix with the
		// theme's text colour, which has to clear 4.5:1 in both.
		// The app pins data-theme, which overrides the media query (codex r1),
		// so the dark scan sets the attribute and proves it took.
		await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
		await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
		// The chips fade their colours (`transition: all 0.15s`), so a scan taken
		// as the theme flips reads in-between colours that are neither theme's.
		// That failed CI on #1851 with #9b9baa on #3b3b44 (4.04:1), passing 13/13
		// locally. Scan only once every transition has finished.
		await settleTransitions(page);
		const dark = await new AxeBuilder({ page }).include('.palette').analyze();
		expect(dark.violations.map((v) => `${v.id}: ${v.help}`), 'axe on the open palette with results, dark theme').toEqual([]);
		await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
		await settleTransitions(page);

		// Enter opens the selected result.
		const title = (await options.nth(1).locator('.result-title').textContent())?.trim();
		await page.keyboard.press('Enter');
		await expect(page.locator('.palette')).toHaveCount(0);
		await expect(page.getByText(title!).first()).toBeVisible();
		// The item page focuses its own controls as it mounts; reopen the
		// palette only once it has settled, as a person would, or the late
		// focus lands after the palette opens and takes the keys with it.
		await page.waitForURL(/\/tasks\//);
		await page.waitForTimeout(1500);

		// Recents: an empty query lists them, and the keyboard works on them (C45).
		await page.keyboard.press('Control+k');
		await expect(input).toBeFocused();
		await expect(input).toHaveValue('');
		const recents = page.locator(`[id="${await input.getAttribute('aria-controls')}"]`).getByRole('option');
		await expect(recents.first()).toContainText('Zephyr');
		await page.keyboard.press('ArrowDown');
		await expect(recents.first()).toHaveAttribute('aria-selected', 'true');
		await expect(input).toHaveAttribute('aria-activedescendant', (await recents.first().getAttribute('id'))!);

		const withRecents = await new AxeBuilder({ page }).include('.palette').analyze();
		expect(withRecents.violations.map((v) => `${v.id}: ${v.help}`), 'axe on the open palette with recents').toEqual([]);

		await page.keyboard.press('Enter');
		await expect(input, 'Enter on a recent runs that search').toHaveValue('Zephyr');

		// A match found only in an item's BODY is announced too (codex r1): the
		// local count is 0 there, and the content matches are the results.
		await input.fill('');
		await page.keyboard.type('quokkaword');
		await expect(page.locator('.palette [role="option"]').first()).toBeVisible({ timeout: 10_000 });
		await expect(page.locator('.palette [aria-live="polite"]')).toContainText('1 result');
	} finally {
		await account.api.dispose();
	}
});
