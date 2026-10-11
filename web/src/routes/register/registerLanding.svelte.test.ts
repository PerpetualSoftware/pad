// TASK-3595: a first Cloud signup lands in the workspace the server made for
// it (register's `workspace_path`), not on /console. An explicit ?redirect=
// still wins. The username is a customization behind "Change".
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	register: vi.fn(),
	url: 'http://localhost/register',
}));

vi.mock('$app/state', () => ({ page: { get url() { return new URL(mocks.url); } } }));
vi.mock('$app/navigation', () => ({ goto: mocks.goto }));
vi.mock('$lib/api/client', () => ({
	api: { auth: { register: mocks.register, checkUsername: vi.fn(async () => ({ available: true })) } },
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		cloudMode: true,
		ensureLoaded: vi.fn(async () => ({ authenticated: false })),
		load: vi.fn(async () => {}),
	},
}));

import RegisterPage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

async function mountAndSubmit() {
	render(RegisterPage);
	await settle();
	const field = (id: string) => document.getElementById(id) as HTMLInputElement;
	await fireEvent.input(field('register-name'), { target: { value: 'Ada Lovelace' } });
	await fireEvent.input(field('register-email'), { target: { value: 'ada@example.com' } });
	await fireEvent.input(field('register-password'), { target: { value: 'correct-horse-battery-staple' } });
	await fireEvent.input(field('register-confirm-password'), { target: { value: 'correct-horse-battery-staple' } });
	const submit = Array.from(document.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Create account')!;
	await fireEvent.click(submit);
	await settle();
}

beforeEach(() => {
	cleanup();
	mocks.goto.mockClear();
	mocks.register.mockReset();
	mocks.url = 'http://localhost/register';
});

describe('register landing (TASK-3595)', () => {
	it('a first signup lands in the workspace the server made for it', async () => {
		mocks.register.mockResolvedValue({ user: { email_verified: true }, token: 't', workspace_path: '/ada/adas-workspace' });
		await mountAndSubmit();
		expect(mocks.register).toHaveBeenCalledTimes(1);
		expect(mocks.goto).toHaveBeenCalledWith('/ada/adas-workspace', { replaceState: true });
	});

	it('an explicit ?redirect= still wins', async () => {
		mocks.url = 'http://localhost/register?redirect=%2Fsome%2Fdeep%2Flink';
		mocks.register.mockResolvedValue({ user: { email_verified: true }, token: 't', workspace_path: '/ada/adas-workspace' });
		await mountAndSubmit();
		expect(mocks.goto).toHaveBeenCalledWith('/some/deep/link', { replaceState: true });
	});

	it("an unverified signup's Continue also goes to the workspace", async () => {
		mocks.register.mockResolvedValue({ user: { email_verified: false }, token: 't', workspace_path: '/ada/adas-workspace' });
		await mountAndSubmit();
		expect(mocks.goto, 'navigated before the check-your-email state').not.toHaveBeenCalled();
		const cont = Array.from(document.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Continue to Pad')!;
		await fireEvent.click(cont);
		await settle();
		expect(mocks.goto).toHaveBeenCalledWith('/ada/adas-workspace', { replaceState: true });
	});

	it('the username is a line with Change until asked for', async () => {
			render(RegisterPage);
		await settle();
		expect(document.getElementById('register-username')).toBeNull();
		const change = Array.from(document.querySelectorAll('button')).find((b) => b.textContent?.trim() === 'Change')!;
		await fireEvent.click(change);
		await settle();
		expect(document.getElementById('register-username')).not.toBeNull();
		expect(document.activeElement?.id).toBe('register-username');
	});
});
