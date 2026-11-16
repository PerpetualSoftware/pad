import { beforeEach, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';

const state = vi.hoisted(() => ({ factor: 'password', active: null as string | null, lapse: false, reauth: false }));
vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		constructor(error: { code: string; message: string }) { super(error.message); this.code = error.code; }
	}
	return { PadApiError, isPlanLimitError: () => false, api: {
		calendarFeeds: { list: vi.fn(async () => []) }, workspaces: { list: vi.fn(async () => []) },
		auth: {
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true, totp_enabled: false })),
			tokens: { list: vi.fn(async () => []) },
			stepUp: {
				status: vi.fn(async () => ({ factor: state.factor, active_until: state.active })),
				sendEmailCode: vi.fn(async () => ({ sent: true })),
				verify: vi.fn(async () => ({ active_until: new Date(Date.now() + 600000).toISOString() }))
			},
			totp: {
				setup: vi.fn(async () => {
					if (state.reauth) throw new PadApiError({code: 'reauth_required', message: 'Sign in again, then turn on two-factor authentication within 10 minutes'});
					return { secret: 'pending-secret', url: 'otpauth://test' };
				}),
				verify: vi.fn(async () => {
					if (state.lapse) { state.lapse = false; throw new PadApiError({code: 'step_up_required', message: 'Verify your identity'}); }
					return { enabled: true, recovery_codes: ['recovery'] };
				})
			}
		}
	} };
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
	cloudMode: false, emailConfigured: true, user: { id: 'u1', name: 'Pat', email: 'pat@example.com' }, userId: 'u1', identityEpoch: 0,
	identityFence() { return () => true; }, onIdentityChange() { return () => {}; }, load: vi.fn(), ensureLoaded: vi.fn()
} }));
vi.mock('qrcode', () => ({ default: { toCanvas: vi.fn() } }));
const { api } = await import('$lib/api/client');
const { default: AccountSettings } = await import('./+page.svelte');
beforeEach(() => { vi.clearAllMocks(); Object.assign(state, { factor: 'password', active: null, lapse: false, reauth: false }); });
async function start() {
	render(AccountSettings);
	await fireEvent.click(await screen.findByRole('button', { name: 'Enable 2FA' }));
}
async function confirmPassword() {
	const prompt = await screen.findByRole('heading', {name: 'Verify it’s you'});
	const container = prompt.parentElement!;
	await fireEvent.input(within(container).getByLabelText('Password'), {target:{value:'password'}});
	await fireEvent.click(within(container).getByRole('button', {name:'Verify'}));
}
it.each(['password', 'email'])('checks status and prompts for %s before creating a secret', async (factor) => {
	state.factor = factor;
	await start();
	await waitFor(() => expect(api.auth.stepUp.status).toHaveBeenCalledOnce());
	expect(api.auth.totp.setup).not.toHaveBeenCalled();
	expect(await screen.findByRole('heading', {name: 'Verify it’s you'})).toBeInTheDocument();
	if (factor === 'password') {
		await confirmPassword();
	} else {
		await fireEvent.click(screen.getByRole('button', {name:'Send code'}));
		await waitFor(() => expect(api.auth.stepUp.sendEmailCode).toHaveBeenCalledOnce());
		await fireEvent.input(screen.getByLabelText('Email code'), {target:{value:'123456'}});
		await fireEvent.click(screen.getByRole('button', {name:'Verify'}));
	}
	await waitFor(() => expect(api.auth.totp.setup).toHaveBeenCalledOnce());
});
it('uses a live grant without prompting', async () => {
	state.active = new Date(Date.now()+600000).toISOString();
	await start();
	await waitFor(() => expect(api.auth.totp.setup).toHaveBeenCalledOnce());
	expect(api.auth.stepUp.status).toHaveBeenCalledOnce();
	expect(screen.queryByRole('heading', {name:'Verify it’s you'})).toBeNull();
});
it('shows the server reauthentication message for an unavailable factor', async () => {
	state.factor = 'unavailable'; state.reauth = true;
	await start();
	expect(await screen.findByText('Sign in again, then turn on two-factor authentication within 10 minutes')).toBeInTheDocument();
	expect(api.auth.stepUp.status).toHaveBeenCalledOnce();
});
it('renews an expired grant and retries the same code and secret', async () => {
	state.active = new Date(Date.now()+600000).toISOString(); state.lapse = true;
	await start();
	await fireEvent.input(await screen.findByLabelText('Verification code'), {target:{value:'654321'}});
	state.active = null;
	await fireEvent.click(screen.getByRole('button', {name:'Verify & Enable'}));
	await confirmPassword();
	await waitFor(() => expect(api.auth.totp.verify).toHaveBeenCalledTimes(2));
	expect(api.auth.totp.verify).toHaveBeenNthCalledWith(1, '654321', 'pending-secret');
	expect(api.auth.totp.verify).toHaveBeenNthCalledWith(2, '654321', 'pending-secret');
	expect(api.auth.totp.setup).toHaveBeenCalledOnce();
});
