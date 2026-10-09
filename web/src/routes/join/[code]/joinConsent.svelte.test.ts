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
	clearProof: vi.fn(),
}));

// Reactive, so a leg can change the code under a mounted page, as an SPA
// navigation from one /join link to another does (codex r4).
vi.mock('$app/state', async () => ({ page: (await import('../../../test/mocks/reactivePage.svelte')).page }));
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
vi.mock('$lib/invitations/proof', () => ({
	captureInvitationProof: () => '',
	clearInvitationProof: mocks.clearProof,
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
		// Announced: the focused buttons are gone, so the result must be spoken
		// (codex r4).
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Invitation declined');
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
		expect(document.querySelector('[role="alert"]')?.textContent).toContain('Invitation not found');
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

	it('a registration kept without its invitation says so and lands nowhere (BUG-3438)', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockResolvedValue({ found: true, email: 'new@example.com', has_account: false, workspace_name: 'Acme' });
		mocks.register.mockResolvedValue({
			user: { id: 'u', email: 'new@example.com' },
			token: 't',
			invitation_not_joined: {
				code: 'invitation_gone',
				message: 'This invitation is no longer valid. Your account was created, but you were not added to the workspace.',
			},
		});
		render(JoinPage);
		await settle();
		await fireEvent.input(document.querySelector<HTMLInputElement>('#join-name')!, { target: { value: 'New' } });
		const pws = document.querySelectorAll<HTMLInputElement>('input[type="password"]');
		await fireEvent.input(pws[0], { target: { value: 'password123' } });
		await fireEvent.input(pws[1], { target: { value: 'password123' } });
		await fireEvent.click(submitBtn());
		await settle();
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(document.querySelector('[role="alert"]')?.textContent).toContain('This invitation is no longer valid');
		const toPad = Array.from(document.querySelectorAll<HTMLAnchorElement>('a')).find((a) => a.getAttribute('href') === '/console');
		expect(toPad).toBeTruthy();
	});

	it('registering a new account through the code still joins in one step', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockResolvedValue({ found: true, email: 'new@example.com', has_account: false, workspace_name: 'Acme' });
		mocks.register.mockResolvedValue({ accepted_invitation: { workspace_slug: 'acme', owner_username: 'o' } });
		render(JoinPage);
		await settle();
		const name = document.querySelector<HTMLInputElement>('#join-name')!;
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

	it('a different /join code under the mounted page shows and acts on THAT invitation (codex r4)', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		render(JoinPage);
		await settle();
		expect(document.body.textContent).toContain('Acme');
		mocks.preview.mockResolvedValue({ found: true, email: 'inv@example.com', has_account: true, workspace_name: 'Beta Co' });
		page.params = { code: 'def456' };
		await settle();
		expect(mocks.preview).toHaveBeenLastCalledWith('def456');
		expect(document.body.textContent).toContain('Beta Co');
		expect(document.body.textContent).not.toContain('Acme');
		await fireEvent.click(byTestId('join-accept')!);
		await settle();
		expect(mocks.accept).toHaveBeenCalledWith('def456', undefined);
	});

	it('a preview for the previous code that answers late does not replace the current one (codex r4)', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		let answerA!: (v: unknown) => void;
		mocks.preview.mockReturnValueOnce(new Promise((r) => (answerA = r)));
		render(JoinPage);
		await settle();
		mocks.preview.mockResolvedValue({ found: true, email: 'inv@example.com', has_account: true, workspace_name: 'Beta Co' });
		page.params = { code: 'def456' };
		await settle();
		answerA({ found: true, email: 'inv@example.com', has_account: true, workspace_name: 'Acme' });
		await settle();
		expect(document.body.textContent).toContain('Beta Co');
		expect(document.body.textContent).not.toContain('Acme');
	});

	// codex r5: an action still in flight when the code changes must not land on
	// the new code's page.
	const BETA_PREVIEW = { found: true, email: 'inv@example.com', has_account: true, workspace_name: 'Beta Co' };

	it('an accept that answers after the code changed neither lands nor touches the new card', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		let answer!: (v: unknown) => void;
		mocks.accept.mockReturnValue(new Promise((r) => (answer = r)));
		render(JoinPage);
		await settle();
		await fireEvent.click(byTestId('join-accept')!);
		await settle();
		mocks.preview.mockResolvedValue(BETA_PREVIEW);
		page.params = { code: 'def456' };
		await settle();
		answer({ workspace_slug: 'acme', owner_username: 'o' });
		await settle();
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(document.body.textContent).toContain('Beta Co');
		expect(byTestId('join-accept')).not.toBeNull();
	});

	it('a decline that answers after the code changed does not report the new invitation declined', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		let answer!: (v: unknown) => void;
		mocks.decline.mockReturnValue(new Promise((r) => (answer = r)));
		render(JoinPage);
		await settle();
		await fireEvent.click(byTestId('join-decline')!);
		await settle();
		mocks.preview.mockResolvedValue(BETA_PREVIEW);
		page.params = { code: 'def456' };
		await settle();
		answer(undefined);
		await settle();
		expect(document.body.textContent).not.toContain('Invitation declined');
		expect(byTestId('join-accept')).not.toBeNull();
	});

	it('a sign-in that completes after the code changed lands on the NEW card, signed in', async () => {
		// Signed out for A. Signing in is pending when the code changes; the new
		// code's own session probe is still out when the sign-in lands, and
		// answers "signed out" (it was sent before the cookie was set).
		mocks.session.mockResolvedValueOnce({ authenticated: false });
		let signedIn!: (v: unknown) => void;
		mocks.login.mockReturnValue(new Promise((r) => (signedIn = r)));
		render(JoinPage);
		await settle();
		const pw = document.querySelector<HTMLInputElement>('input[type="password"]')!;
		await fireEvent.input(pw, { target: { value: 'password123' } });
		await fireEvent.click(submitBtn());
		await settle();
		let probeB!: (v: unknown) => void;
		mocks.session.mockReturnValueOnce(new Promise((r) => (probeB = r)));
		mocks.session.mockResolvedValue({ authenticated: true });
		mocks.preview.mockResolvedValue(BETA_PREVIEW);
		page.params = { code: 'def456' };
		await settle();
		signedIn({});
		await settle();
		probeB({ authenticated: false });
		await settle();
		expect(byTestId('join-accept')).not.toBeNull();
		expect(document.body.textContent).toContain('Beta Co');
		expect(mocks.accept).not.toHaveBeenCalled();
	});

	// codex r6
	it('a code change empties the form it carried, so B is never submitted with what was typed for A', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockResolvedValue({ found: true, email: 'new@example.com', has_account: false, workspace_name: 'Acme' });
		render(JoinPage);
		await settle();
		const typed = (id: string, v: string) =>
			fireEvent.input(document.querySelector<HTMLInputElement>(`#${id}`)!, { target: { value: v } });
		await typed('join-name', 'For A');
		await typed('join-password', 'password123');
		await typed('join-confirm-password', 'password123');
		mocks.preview.mockResolvedValue({ found: true, email: 'other@example.com', has_account: false, workspace_name: 'Beta Co' });
		page.params = { code: 'def456' };
		await settle();
		for (const id of ['join-name', 'join-username', 'join-password', 'join-confirm-password']) {
			expect(document.querySelector<HTMLInputElement>(`#${id}`)!.value, id).toBe('');
		}
	});

	it('a sign-in still out when the code changes keeps the form busy, so a second one cannot start', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		let signedIn!: (v: unknown) => void;
		mocks.login.mockReturnValue(new Promise((r) => (signedIn = r)));
		render(JoinPage);
		await settle();
		await fireEvent.input(document.querySelector<HTMLInputElement>('input[type="password"]')!, {
			target: { value: 'password123' },
		});
		await fireEvent.click(submitBtn());
		await settle();
		mocks.preview.mockResolvedValue(BETA_PREVIEW);
		page.params = { code: 'def456' };
		await settle();
		const busy = Array.from(document.querySelectorAll<HTMLButtonElement>('button')).find((b) =>
			/^Signing in/.test(b.textContent?.trim() ?? '')
		);
		expect(busy?.disabled).toBe(true);
		mocks.session.mockResolvedValue({ authenticated: true });
		signedIn({});
		await settle();
		expect(byTestId('join-accept')).not.toBeNull();
		expect(mocks.login).toHaveBeenCalledTimes(1);
	});

	it('a signup that completes after the code changed still clears its own spent proof', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockResolvedValue({ found: true, email: 'new@example.com', has_account: false, workspace_name: 'Acme' });
		let registered!: (v: unknown) => void;
		mocks.register.mockReturnValue(new Promise((r) => (registered = r)));
		render(JoinPage);
		await settle();
		await fireEvent.input(document.querySelector<HTMLInputElement>('#join-name')!, { target: { value: 'New' } });
		const pws = document.querySelectorAll<HTMLInputElement>('input[type="password"]');
		await fireEvent.input(pws[0], { target: { value: 'password123' } });
		await fireEvent.input(pws[1], { target: { value: 'password123' } });
		await fireEvent.click(submitBtn());
		await settle();
		mocks.preview.mockResolvedValue(BETA_PREVIEW);
		page.params = { code: 'def456' };
		await settle();
		registered({ accepted_invitation: { workspace_slug: 'acme', owner_username: 'o' } });
		await settle();
		expect(mocks.clearProof).toHaveBeenCalledWith('abc123');
		expect(mocks.goto).not.toHaveBeenCalled();
	});
});

// TASK-2251: a preview that ANSWERED found:false is an invalid link, said so
// with no form and no accept card; a preview REQUEST that failed keeps the
// register fallback (BUG-1930), since the code may be fine.
describe('invalid invitation link (TASK-2251)', () => {
	it('signed out, found:false: the invalid state, and no form', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockResolvedValue({ found: false });
		render(JoinPage);
		await settle();
		expect(byTestId('join-invalid')?.textContent).toContain('invalid or has expired');
		expect(document.querySelectorAll('input').length).toBe(0);
	});

	it('signed in, found:false: the invalid state, and no accept card', async () => {
		mocks.session.mockResolvedValue({ authenticated: true });
		mocks.preview.mockResolvedValue({ found: false });
		render(JoinPage);
		await settle();
		expect(byTestId('join-invalid')).not.toBeNull();
		expect(byTestId('join-accept')).toBeNull();
		expect(mocks.accept).not.toHaveBeenCalled();
	});

	it('a preview request that FAILED keeps the register form, not the invalid state', async () => {
		mocks.session.mockResolvedValue({ authenticated: false });
		mocks.preview.mockRejectedValue(new TypeError('Failed to fetch'));
		render(JoinPage);
		await settle();
		expect(byTestId('join-invalid')).toBeNull();
		expect(document.querySelector('#join-name')).not.toBeNull();
	});
});
