// BUG-2136 U2 (lead ruling, option B): opening an invitation link is not
// consent. A visitor who is signed in, or who signs in from the link, lands
// on an accept/decline card; only REGISTERING a new account through the code
// joins in one step.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	session: vi.fn(),
	preview: vi.fn(),
	accept: vi.fn(),
	decline: vi.fn(),
	login: vi.fn(),
	register: vi.fn(),
	verify2FA: vi.fn(),
	refreshInvitations: vi.fn(async () => {}),
}));

vi.mock('$app/navigation', () => ({ goto: mocks.goto, replaceState: vi.fn() }));
vi.mock('$lib/api/client', () => ({
	api: {
		auth: {
			session: mocks.session,
			login: mocks.login,
			register: mocks.register,
			verify2FA: mocks.verify2FA,
			checkUsername: vi.fn(),
		},
		members: {
			previewInvitation: mocks.preview,
			acceptInvitation: mocks.accept,
			declineInvitation: mocks.decline,
		},
	},
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		cloudMode: false,
		ensureLoaded: vi.fn(async () => {}),
		load: vi.fn(async () => ({ authenticated: true })),
	},
}));
// The "+" badge must not keep counting an invitation this page accepted or
// declined (codex r2): both force a refetch, which also orders out any older one.
vi.mock('$lib/stores/pendingInvitations.svelte', () => ({
	pendingInvitations: { refresh: mocks.refreshInvitations },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { loadAll: vi.fn(async () => {}) } }));

import { page } from '$app/state';
import JoinPage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

const submitBtn = () =>
	Array.from(document.querySelectorAll<HTMLButtonElement>('button')).find((b) =>
		/^(Sign in|Create account & join)$/.test(b.textContent?.trim() ?? '')
	)!;

const byTestId = (id: string) => document.querySelector<HTMLElement>(`[data-testid="${id}"]`);

beforeEach(() => {
	for (const fn of Object.values(mocks)) fn.mockReset();
	mocks.goto.mockResolvedValue(undefined);
	(page as { params: Record<string, string> }).params = { code: 'abc123' };
	page.url = new URL('http://localhost/join/abc123');
	mocks.preview.mockResolvedValue({
		found: true,
		email: 'inv@example.com',
		has_account: true,
		workspace_name: 'Acme',
	});
	mocks.accept.mockResolvedValue({ workspace_slug: 'acme', owner_username: 'o' });
	mocks.decline.mockResolvedValue(undefined);
});

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('/join/[code] asks before joining (BUG-2136)', () => {
	it('a signed-in visitor sees the card, and nothing is accepted until they choose', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		render(JoinPage);
		await settle();
		expect(byTestId('join-accept')).not.toBeNull();
		expect(byTestId('join-decline')).not.toBeNull();
		expect(document.body.textContent).toContain('Acme');
		expect(mocks.accept).not.toHaveBeenCalled();
		expect(mocks.decline).not.toHaveBeenCalled();
	});

	it('Accept accepts and lands in the workspace', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		render(JoinPage);
		await settle();
		await fireEvent.click(byTestId('join-accept')!);
		await settle();
		expect(mocks.accept).toHaveBeenCalledWith('abc123', undefined);
		expect(mocks.goto).toHaveBeenCalledWith('/o/acme', { replaceState: true });
		expect(mocks.refreshInvitations).toHaveBeenCalledWith(true);
		expect(mocks.decline).not.toHaveBeenCalled();
	});

	it('Decline declines, joins nothing and says so', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		render(JoinPage);
		await settle();
		await fireEvent.click(byTestId('join-decline')!);
		await settle();
		expect(mocks.decline).toHaveBeenCalledWith('abc123');
		expect(mocks.refreshInvitations).toHaveBeenCalledWith(true);
		expect(mocks.accept).not.toHaveBeenCalled();
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(document.body.textContent).toContain('Invitation declined');
	});

	it('a failed decline reports the error', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		mocks.decline.mockRejectedValue(new Error('Invitation not found'));
		render(JoinPage);
		await settle();
		await fireEvent.click(byTestId('join-decline')!);
		await settle();
		expect(document.body.textContent).toContain('Invitation not found');
		expect(document.body.textContent).not.toContain('Invitation declined');
	});

	it('signing in from the link lands on the card, not an accept', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.login.mockResolvedValue({});
		render(JoinPage);
		await settle();
		const pw = document.querySelector<HTMLInputElement>('input[type="password"]')!;
		await fireEvent.input(pw, { target: { value: 'password123' } });
		await fireEvent.click(submitBtn());
		await settle();
		expect(mocks.login).toHaveBeenCalledWith('inv@example.com', 'password123');
		expect(byTestId('join-accept')).not.toBeNull();
		expect(mocks.accept).not.toHaveBeenCalled();
	});

	it('signing in with two-factor from the link lands on the card, not an accept (codex r1)', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.login.mockResolvedValue({ requires_2fa: true, challenge_token: 'ch' });
		mocks.verify2FA.mockResolvedValue({});
		render(JoinPage);
		await settle();
		const pw = document.querySelector<HTMLInputElement>('input[type="password"]')!;
		await fireEvent.input(pw, { target: { value: 'password123' } });
		await fireEvent.click(submitBtn());
		await settle();
		const totp = document.querySelector<HTMLInputElement>('input[inputmode="numeric"]')!;
		await fireEvent.input(totp, { target: { value: '123456' } });
		await fireEvent.click(
			Array.from(document.querySelectorAll<HTMLButtonElement>('button')).find((b) => b.textContent?.trim() === 'Verify')!
		);
		await settle();
		expect(mocks.verify2FA).toHaveBeenCalled();
		expect(byTestId('join-accept')).not.toBeNull();
		expect(mocks.accept).not.toHaveBeenCalled();
	});

	it('registering a new account through the code still joins in one step', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockResolvedValue({ found: true, email: 'new@example.com', has_account: false, workspace_name: 'Acme' });
		mocks.register.mockResolvedValue({ accepted_invitation: { workspace_slug: 'acme', owner_username: 'o' } });
		render(JoinPage);
		await settle();
		const name = document.querySelector<HTMLInputElement>('input[placeholder="Name"]')!;
		await fireEvent.input(name, { target: { value: 'New Person' } });
		const pws = document.querySelectorAll<HTMLInputElement>('input[type="password"]');
		await fireEvent.input(pws[0], { target: { value: 'password123' } });
		await fireEvent.input(pws[1], { target: { value: 'password123' } });
		await fireEvent.click(submitBtn());
		await settle();
		expect(mocks.register).toHaveBeenCalled();
		expect(byTestId('join-accept')).toBeNull();
		expect(mocks.goto).toHaveBeenCalledWith('/o/acme', { replaceState: true });
	});
});
