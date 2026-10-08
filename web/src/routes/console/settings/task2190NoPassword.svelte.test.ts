import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

/**
 * TASK-2190: an account that signed up with Google, GitHub or Apple has no
 * password. The Password card demanded a current one (so it always failed),
 * and disabling 2FA demanded one too, so such an account could never turn 2FA
 * off. It is now offered the reset email that sets a password, and disables
 * 2FA with a code or a recovery code.
 */
const state = vi.hoisted(() => ({ passwordSet: false as boolean, disabled: [] as unknown[], forgot: [] as string[] }));

vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	isPlanLimitError: () => false,
	api: {
		auth: {
			me: vi.fn(async () => ({
				id: 'u1',
				name: 'Pat',
				email: 'pat@example.com',
				username: 'pat',
				password_set: state.passwordSet,
				totp_enabled: true,
				oauth_providers: ['github']
			})),
			forgotPassword: vi.fn(async (email: string) => {
				state.forgot.push(email);
				return { ok: true, message: '' };
			}),
			totp: {
				disable: vi.fn(async (factor: unknown) => {
					state.disabled.push(factor);
					return { enabled: false };
				}),
				setup: vi.fn(),
				verify: vi.fn()
			},
			tokens: { list: vi.fn(async () => []), create: vi.fn(), delete: vi.fn() },
			updateProfile: vi.fn(),
			unlinkProvider: vi.fn(),
			deleteAccount: vi.fn(),
			logout: vi.fn()
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get cloudMode() { return true; },
		get emailConfigured() { return true; },
		get user() { return { id: 'u1', name: 'Pat', email: 'pat@example.com' }; },
		get userId() { return 'u1'; },
		get identityEpoch() { return 0; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		load: vi.fn(async () => {}),
		ensureLoaded: vi.fn(async () => {})
	}
}));

const { default: AccountSettings } = await import('./+page.svelte');

const button = (re: RegExp) => screen.getAllByRole('button').find((b) => re.test(b.textContent ?? ''));

beforeEach(() => {
	state.disabled.length = 0;
	state.forgot.length = 0;
});

describe('TASK-2190: an account with no password', () => {
	it('is offered the email that sets a password, not a change form it cannot pass', async () => {
		state.passwordSet = false;
		render(AccountSettings);
		await waitFor(() => expect(button(/email me a link to set a password/i)).toBeTruthy());
		expect(screen.queryByLabelText('Current password')).toBeNull();
		button(/email me a link to set a password/i)!.click();
		await waitFor(() => expect(state.forgot).toEqual(['pat@example.com']));
		await waitFor(() => expect(screen.getByText(/signs you out everywhere/)).toBeInTheDocument());
	});

	it('disables 2FA with a code from the authenticator, or a recovery code', async () => {
		state.passwordSet = false;
		render(AccountSettings);
		const start = await waitFor(() => {
			const b = button(/disable 2fa|disable/i);
			if (!b) throw new Error('no disable button');
			return b;
		});
		start.click();
		const input = await screen.findByLabelText('Authenticator code or recovery code');
		(input as HTMLInputElement).value = '123456';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		button(/confirm disable/i)!.click();
		await waitFor(() => expect(state.disabled).toEqual([{ code: '123456' }]));
	});

	it('an account WITH a password keeps the password form and sends the password', async () => {
		state.passwordSet = true;
		render(AccountSettings);
		await waitFor(() => expect(screen.getByLabelText('Current password')).toBeInTheDocument());
		expect(button(/email me a link to set a password/i)).toBeUndefined();
		const start = await waitFor(() => {
			const b = button(/disable 2fa|disable/i);
			if (!b) throw new Error('no disable button');
			return b;
		});
		start.click();
		const input = await screen.findByPlaceholderText('Current password');
		(input as HTMLInputElement).value = 'hunter22';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		button(/confirm disable/i)!.click();
		await waitFor(() => expect(state.disabled).toEqual(['hunter22']));
	});
});
