// BUG-3382: a verification link opened without a session for the account it
// names does not verify. The page offers sign-in (back to this link), and,
// when the address is unverified, a claim that resets the account and lists
// what the claim removed.
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';

const calls = vi.hoisted(() => ({ verify: [] as string[], claim: [] as string[], logout: 0, goto: [] as string[] }));
const signedIn = vi.hoisted(() => ({ user: null as null | { email: string } }));
const next = vi.hoisted(() => ({ verify: null as unknown, claim: null as unknown }));

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		details?: Record<string, unknown>;
		constructor(o: { code: string; message: string; details?: Record<string, unknown> }) {
			super(o.message);
			this.code = o.code;
			this.details = o.details;
		}
	}
	const settle = (v: unknown) => (v instanceof Error ? Promise.reject(v) : Promise.resolve(v));
	return {
		PadApiError,
		api: {
			auth: {
				verifyEmailToken: vi.fn(async (t: string) => {
					calls.verify.push(t);
					return settle(next.verify);
				}),
				claimByVerification: vi.fn(async (t: string) => {
					calls.claim.push(t);
					return settle(next.claim);
				}),
				resendVerification: vi.fn(async () => ({})),
				logout: vi.fn(async () => {
					calls.logout++;
				}),
			},
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		cloudMode: true,
		get user() {
			return signedIn.user;
		},
		ensureLoaded: async () => {},
		load: async () => {},
	},
}));

vi.mock('$app/navigation', () => ({
	goto: vi.fn(async (url: string) => {
		calls.goto.push(url);
	}),
}));

import { page } from '$app/state';
import { PadApiError } from '$lib/api/client';
import VerifyPage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

const needsSession = (canClaim: boolean) =>
	new (PadApiError as unknown as new (o: object) => Error)({
		code: 'verify_needs_session',
		message: 'Sign in',
		details: { email: 'victim@example.com', can_claim: canClaim },
	});

beforeEach(() => {
	calls.verify.length = 0;
	calls.claim.length = 0;
	calls.logout = 0;
	calls.goto.length = 0;
	signedIn.user = null;
	(page as { params: Record<string, string> }).params = { token: 'tok123' };
});
afterEach(() => cleanup());

describe('verify link without the account’s session (BUG-3382)', () => {
	it('offers sign-in back to this link, and a confirmed claim', async () => {
		next.verify = needsSession(true);
		next.claim = {
			claimed: true,
			reset_path: '/reset-password/rst',
			stripped_workspaces: ['Joined WS'],
			deleted_workspaces: ['Squat WS'],
		};
		render(VerifyPage);
		await settle();

		expect(screen.getByText(/victim@example\.com/)).toBeTruthy();
		const signIn = screen.getByRole('link', { name: 'Sign in to verify' });
		expect(signIn.getAttribute('href')).toBe('/login?redirect=%2Fverify-email%2Ftok123');

		// The first click only explains; nothing is claimed without the second.
		await fireEvent.click(screen.getByRole('button', { name: "I didn't register this account" }));
		await settle();
		expect(calls.claim).toEqual([]);

		await fireEvent.click(screen.getByRole('button', { name: 'Claim this address' }));
		await settle();
		expect(calls.claim).toEqual(['tok123']);
		expect(screen.getByText('Squat WS')).toBeTruthy();
		expect(screen.getByText('Joined WS')).toBeTruthy();
		expect(screen.getByRole('link', { name: 'Set a password' }).getAttribute('href')).toBe(
			'/reset-password/rst',
		);
	});

	it('offers no claim for an already-verified address', async () => {
		next.verify = needsSession(false);
		render(VerifyPage);
		await settle();
		expect(screen.getByRole('link', { name: 'Sign in to verify' })).toBeTruthy();
		expect(screen.queryByRole('button', { name: "I didn't register this account" })).toBeNull();
	});

	it('shows a refused claim and lets it be retried', async () => {
		next.verify = needsSession(true);
		next.claim = new (PadApiError as unknown as new (o: object) => Error)({
			code: 'account_claim_needs_support',
			message: 'Contact support@getpad.dev',
		});
		render(VerifyPage);
		await settle();
		await fireEvent.click(screen.getByRole('button', { name: "I didn't register this account" }));
		await fireEvent.click(screen.getByRole('button', { name: 'Claim this address' }));
		await settle();
		expect(screen.getByRole('alert').textContent).toContain('support@getpad.dev');
		expect(screen.getByRole('button', { name: 'Claim this address' })).toBeTruthy();
	});

	it('signed in as another account: signs out before sending to sign-in', async () => {
		signedIn.user = { email: 'other@example.com' };
		next.verify = needsSession(true);
		render(VerifyPage);
		await settle();
		expect(screen.getByText(/signed in as other@example\.com/)).toBeTruthy();
		// A plain link to /login would bounce back here still signed in.
		expect(screen.queryByRole('link', { name: 'Sign in to verify' })).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Switch account to verify' }));
		await settle();
		expect(calls.logout).toBe(1);
		expect(calls.goto).toEqual(['/login?redirect=%2Fverify-email%2Ftok123']);
	});
});
