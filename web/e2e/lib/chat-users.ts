import { request as playwrightRequest, expect, type Page } from '@playwright/test';
import { suiteFixture } from '../fixtures';

/**
 * Throwaway users for the Chat shell specs (TASK-4315 PR 3b): a verified
 * account with Chat turned on, an API session token for seeding its
 * conversations, and a form login for the browser. The admin session mints the
 * account (a bearer API token cannot), the pattern account-delete.spec.ts uses.
 */
export const CHAT_USER_PASSWORD = 'Playwright-Chat-2026!';

export interface ChatUser {
	email: string;
	username: string;
	token: string;
}

export async function createChatUser(tag: string): Promise<ChatUser> {
	const { baseURL, adminSessionToken } = suiteFixture();
	const suffix = `${tag}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`;
	const email = `e2e-chat-${suffix}@example.com`;
	const username = `e2echat${suffix}`;

	const admin = await playwrightRequest.newContext({
		baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${adminSessionToken}` }
	});
	try {
		const made = await admin.post('/api/v1/auth/register', {
			data: { email, username, name: `Chat ${tag}`, password: CHAT_USER_PASSWORD }
		});
		expect(made.ok(), await made.text()).toBe(true);
	} finally {
		await admin.dispose();
	}

	const anon = await playwrightRequest.newContext({ baseURL });
	try {
		const login = await anon.post('/api/v1/auth/login', { data: { email, password: CHAT_USER_PASSWORD } });
		expect(login.ok(), await login.text()).toBe(true);
		const { token } = (await login.json()) as { token: string };
		const authed = await playwrightRequest.newContext({
			baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${token}` }
		});
		try {
			const on = await authed.patch('/api/v1/auth/me', { data: { chat_enabled: true } });
			expect(on.ok(), await on.text()).toBe(true);
		} finally {
			await authed.dispose();
		}
		return { email, username, token };
	} finally {
		await anon.dispose();
	}
}

/** A conversation with an agent, as this user. Returns its number. */
export async function createConversation(user: ChatUser, agent: string): Promise<number> {
	const { baseURL } = suiteFixture();
	const api = await playwrightRequest.newContext({
		baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${user.token}`, 'Content-Type': 'application/json' }
	});
	try {
		const res = await api.post('/api/v1/me/conversations', { data: { agent_key: agent } });
		expect(res.ok(), await res.text()).toBe(true);
		return ((await res.json()) as { conversation: { number: number } }).conversation.number;
	} finally {
		await api.dispose();
	}
}

/** Log in through the real form (the session is minted under the browser's own User-Agent). */
export async function loginViaForm(page: Page, email: string): Promise<void> {
	await page.goto('/login');
	await page.getByLabel('Email', { exact: true }).fill(email);
	await page.getByLabel('Password', { exact: true }).fill(CHAT_USER_PASSWORD);
	await Promise.all([
		page.waitForResponse((r) => r.url().includes('/api/v1/auth/login') && r.request().method() === 'POST'),
		page.getByRole('button', { name: /^sign in$/i }).click()
	]);
}
