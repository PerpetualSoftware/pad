import { expect, request as playwrightRequest, type Page } from '@playwright/test';
import { suiteFixture } from '../fixtures';

/**
 * Workspace tab bar helpers, shared by the TASK-3306 and TASK-3312 specs.
 * Tabs are per user, and other specs move the admin's, so a tab spec works as
 * its own registered user with its own workspaces.
 */

export const PASSWORD = 'Playwright-Tabs-3306!';
export const NAMES = [
	'Pad', 'Docs Site', 'Mobile App', 'Hiring Pipeline 2026', 'Research Notes', 'Q4 Marketing Launch Plan',
	'Infra', 'A Very Long Workspace Name For Truncation Testing', 'Design System', 'Customer Interviews',
	'Ops', 'Personal', 'Book Club', 'Home Renovation Project', 'Side Project Alpha'
];

export type Box = { x: number; y: number; width: number; height: number };

export async function asNewUser(page: Page): Promise<{ username: string; slugs: string[] }> {
	const { baseURL, apiToken } = suiteFixture();
	const suffix = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 7)}`;
	const email = `e2e-tabs-${suffix}@example.com`;
	const username = `e2etabs${suffix}`;
	const api = await playwrightRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${apiToken}` } });
	try {
		const r = await api.post('/api/v1/auth/register', { data: { email, username, name: 'Tabs', password: PASSWORD } });
		if (!r.ok()) throw new Error(`register failed (${r.status()}): ${await r.text()}`);
	} finally {
		await api.dispose();
	}
	await page.goto('/login');
	await page.getByPlaceholder('Email').fill(email);
	await page.getByPlaceholder('Password').fill(PASSWORD);
	await Promise.all([
		page.waitForResponse((r) => r.url().includes('/api/v1/auth/login') && r.request().method() === 'POST'),
		page.getByRole('button', { name: /^sign in$/i }).click()
	]);
	await page.waitForURL((u) => !u.pathname.startsWith('/login'));
	// Workspace slugs are unique across the instance, and parallel workers
	// creating the same name raced to a 500; a per-run suffix keeps names
	// distinct (it lands past the ellipsis on the long ones).
	const runNames = NAMES.map((n) => `${n} ${suffix.slice(-4)}`);
	const slugs: string[] = await page.evaluate(async (names) => {
		const csrf = document.cookie.match(/pad_csrf=([^;]+)/)?.[1] ?? '';
		const out: string[] = [];
		for (const name of names) {
			const r = await fetch('/api/v1/workspaces', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
				body: JSON.stringify({ name: `${name}` })
			});
			if (!r.ok) throw new Error(`create ${name}: ${r.status} ${await r.text()}`);
			out.push((await r.json()).slug);
		}
		return out;
	}, runNames);
	return { username, slugs };
}

// Make exactly the first n workspaces the open set, in order.
export async function openTabs(page: Page, slugs: string[], n: number) {
	await page.evaluate(
		async ([all, n]) => {
			const csrf = document.cookie.match(/pad_csrf=([^;]+)/)?.[1] ?? '';
			const h = { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf };
			const cur = (await (await fetch('/api/v1/me/workspace-tabs')).json()).tabs as { slug: string }[];
			for (const t of cur) await fetch(`/api/v1/me/workspace-tabs/${t.slug}`, { method: 'DELETE', headers: h });
			for (const s of (all as string[]).slice(0, n as number))
				await fetch('/api/v1/me/workspace-tabs', { method: 'POST', headers: h, body: JSON.stringify({ slug: s, ephemeral: false }) });
		},
		[slugs, n] as const
	);
}

export async function tabBoxes(page: Page): Promise<Box[]> {
	return page.locator('.workspace-tab').evaluateAll((els) =>
		els.map((e) => {
			const r = e.getBoundingClientRect();
			return { x: r.x, y: r.y, width: r.width, height: r.height };
		})
	);
}

export async function show(page: Page, username: string, slug: string, count: number) {
	await page.goto(`/${username}/${slug}`);
	await expect(page.locator('.workspace-tab')).toHaveCount(count, { timeout: 15_000 });
	await expect(page.locator(`.workspace-tab.active[data-ws-slug="${slug}"]`)).toBeAttached();
	await page.waitForTimeout(300); // the active-tab scroll and the fades settle
}
