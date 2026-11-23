import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-4745, the login page's pending provider link.
 *
 * An OAuth sign-in whose address matched an existing account without that
 * provider linked lands here with ?error=oauth_provider_not_linked. The page
 * used to say the provider account "isn't linked to a Pad account", which is
 * wrong: an account with that address exists. Now, with a pending link held
 * behind the browser's cookie, the page says so, prefills the address and
 * sends the user's consent (link_pending) with the sign-in. Without one, the
 * fallback names the existing account and the settings route.
 */
const session = vi.hoisted(() => vi.fn());
const pendingLink = vi.hoisted(() => vi.fn());
const login = vi.hoisted(() => vi.fn());
const verify2FA = vi.hoisted(() => vi.fn());
const toastShow = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		auth: {
			session: () => session(),
			pendingLink: () => pendingLink(),
			login: (...a: unknown[]) => login(...a),
			verify2FA: (...a: unknown[]) => verify2FA(...a)
		}
	},
	PadApiError: class extends Error {
		code = '';
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { load: vi.fn().mockResolvedValue(undefined), onIdentityChange: vi.fn() }
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: (...a: unknown[]) => toastShow(...a) } }));
vi.mock('$lib/auth/redirect', async (orig) => ({
	...(await orig<typeof import('$lib/auth/redirect')>()),
	navigateToRedirectTarget: vi.fn().mockResolvedValue(undefined)
}));

const { default: LoginPage } = await import('./login/+page.svelte');

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	flushSync();
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

async function mountAt(url: string) {
	history.replaceState(null, '', url);
	app = mount(LoginPage, { target: host, props: {} }) as Record<string, unknown>;
	await settle();
}

const pending = {
	pending: true,
	provider: 'google',
	email: 'owner@example.com',
	password: true,
	providers: ['github']
};

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	localStorage.clear();
	session.mockResolvedValue({ authenticated: false, cloud_mode: true });
	pendingLink.mockResolvedValue({ pending: false });
	login.mockResolvedValue({ token: 't', user: { id: 'u' } });
	verify2FA.mockResolvedValue({ token: 't', user: { id: 'u' } });
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
	vi.clearAllMocks();
});

async function submitPassword(password: string) {
	const pw = host.querySelector<HTMLInputElement>('#login-password')!;
	pw.value = password;
	pw.dispatchEvent(new Event('input', { bubbles: true }));
	host.querySelector<HTMLFormElement>('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
	await settle();
}

describe('login page pending provider link (TASK-4745)', () => {
	it('the fallback no longer says the provider account is not linked to a Pad account', async () => {
		await mountAt('/login?error=oauth_provider_not_linked&provider=google');
		const text = host.textContent ?? '';
		expect(text).not.toContain("isn't linked to a Pad account");
		expect(text).toContain('An account with this email already exists');
		expect(text).toContain('link Google from account settings');
		expect(host.querySelector('[data-testid="pending-link-banner"]')).toBeNull();
		expect(host.querySelector('input[name="link_pending"]')).toBeNull();
	});

	it('prefills the address and offers the link when one is pending', async () => {
		pendingLink.mockResolvedValue(pending);
		await mountAt('/login?error=oauth_provider_not_linked&provider=google');
		const banner = host.querySelector('[data-testid="pending-link-banner"]');
		expect(banner?.textContent).toContain('You already have a Pad account with this email. Sign in to link Google.');
		expect(host.querySelector<HTMLInputElement>('#login-email')!.value).toBe('owner@example.com');
		const box = host.querySelector<HTMLInputElement>('input[name="link_pending"]');
		expect(box?.checked).toBe(true);
		expect(host.textContent).toContain('Link Google to this account');
		// The other linked provider finishes the link too.
		const via = banner!.querySelector<HTMLAnchorElement>('a[href^="/auth/github?pending_link=1"]');
		expect(via).not.toBeNull();
	});

	it('does not ask the server off cloud mode', async () => {
		session.mockResolvedValue({ authenticated: false, cloud_mode: false });
		await mountAt('/login');
		expect(pendingLink).not.toHaveBeenCalled();
	});

	it('sends link_pending with the sign-in and announces the link', async () => {
		pendingLink.mockResolvedValue(pending);
		login.mockResolvedValue({ token: 't', user: { id: 'u' }, pending_link: { provider: 'google', linked: true } });
		await mountAt('/login');
		await submitPassword('pw-123456789');
		expect(login).toHaveBeenCalledWith('owner@example.com', 'pw-123456789', true);
		expect(toastShow).toHaveBeenCalledWith(expect.stringContaining('Google is now linked'), 'success');
	});

	it('sends link_pending false when unticked, and says nothing', async () => {
		pendingLink.mockResolvedValue(pending);
		login.mockResolvedValue({ token: 't', user: { id: 'u' }, pending_link: { linked: false, code: 'pending_link_not_requested' } });
		await mountAt('/login');
		const box = host.querySelector<HTMLInputElement>('input[name="link_pending"]')!;
		box.click();
		await settle();
		await submitPassword('pw-123456789');
		expect(login).toHaveBeenCalledWith('owner@example.com', 'pw-123456789', false);
		expect(toastShow).not.toHaveBeenCalled();
	});

	it('says when the link failed', async () => {
		pendingLink.mockResolvedValue(pending);
		login.mockResolvedValue({ token: 't', user: { id: 'u' }, pending_link: { linked: false, code: 'oauth_subject_mismatch' } });
		await mountAt('/login');
		await submitPassword('pw-123456789');
		expect(toastShow).toHaveBeenCalledWith(expect.stringContaining("Google couldn't be linked"), 'error');
	});

	it('sends no consent when nothing is pending', async () => {
		await mountAt('/login');
		const em = host.querySelector<HTMLInputElement>('#login-email')!;
		em.value = 'someone@example.com';
		em.dispatchEvent(new Event('input', { bubbles: true }));
		await submitPassword('pw-123456789');
		expect(login).toHaveBeenCalledWith('someone@example.com', 'pw-123456789', undefined);
	});

	it('carries the consent to the 2FA step', async () => {
		pendingLink.mockResolvedValue(pending);
		login.mockResolvedValue({ requires_2fa: true, challenge_token: 'ch' });
		verify2FA.mockResolvedValue({ token: 't', user: { id: 'u' }, pending_link: { provider: 'google', linked: true } });
		await mountAt('/login');
		await submitPassword('pw-123456789');
		expect(host.querySelector<HTMLInputElement>('input[name="link_pending"]')?.checked).toBe(true);
		const code = host.querySelector<HTMLInputElement>('#login-totp')!;
		code.value = '123456';
		code.dispatchEvent(new Event('input', { bubbles: true }));
		host.querySelector<HTMLFormElement>('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
		await settle();
		expect(verify2FA).toHaveBeenCalledWith('ch', '123456', undefined, true);
		expect(toastShow).toHaveBeenCalledWith(expect.stringContaining('Google is now linked'), 'success');
	});
});
