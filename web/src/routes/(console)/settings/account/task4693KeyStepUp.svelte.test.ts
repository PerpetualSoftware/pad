import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';

// TASK-4693: creating or rotating a key needs a recent sign-in or a step-up.
// On step_up_required the page shows the step-up prompt the edit flow uses,
// then retries the same request once.
const state = vi.hoisted(() => ({ factor: 'password', verified: false, refuseRetry: false }));
vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		constructor(error: { code: string; message: string }) { super(error.message); this.code = error.code; }
	}
	const refuse = () => {
		if (!state.verified || state.refuseRetry) throw new PadApiError({ code: 'step_up_required', message: 'Sign in again, or verify your identity, then retry within 10 minutes' });
	};
	return { PadApiError, isPlanLimitError: () => false, api: {
		calendarFeeds: { list: vi.fn(async () => []), create: vi.fn(), revoke: vi.fn() },
		workspaces: { list: vi.fn(async () => []) },
		auth: {
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true })),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: {
				list: vi.fn(async () => [{ id: 't1', name: 'ci-runner', scopes: '["*"]', prefix: 'pad_ab', created_at: '2026-10-01T00:00:00Z' }]),
				create: vi.fn(async (name: string) => { refuse(); return { id: 't2', name, prefix: 'pad_cd', created_at: '2026-11-19T00:00:00Z', token: 'pad_cdNEWSECRET' }; }),
				rotate: vi.fn(async (id: string) => { refuse(); return { id, name: 'ci-runner', prefix: 'pad_ef', created_at: '2026-10-01T00:00:00Z', token: 'pad_efROTATEDSECRET' }; }),
				update: vi.fn(), delete: vi.fn(),
			},
			stepUp: {
				status: vi.fn(async () => ({ factor: state.factor, active_until: null })),
				sendEmailCode: vi.fn(),
				verify: vi.fn(async () => { state.verified = true; return { active_until: '2026-11-19T12:10:00Z' }; }),
			},
			updateProfile: vi.fn(), unlinkProvider: vi.fn(), deleteAccount: vi.fn(), logout: vi.fn(), forgotPassword: vi.fn(),
		},
	} };
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
	cloudMode: false, emailConfigured: true, user: { id: 'u1', name: 'Pat', email: 'pat@example.com' }, userId: 'u1',
	identityEpoch: 0,
	identityFence() { return () => true; },
	onIdentityChange() { return () => {}; }, load: vi.fn(), ensureLoaded: vi.fn(),
} }));
const { api } = await import('$lib/api/client');
const { default: AccountSettings } = await import('./+page.svelte');
beforeEach(() => {
	vi.clearAllMocks();
	Object.assign(state, { factor: 'password', verified: false, refuseRetry: false });
});

async function startCreate() {
	render(AccountSettings);
	await waitFor(() => expect(screen.getByText('ci-runner')).toBeInTheDocument());
	await fireEvent.input(screen.getByLabelText('Token name'), { target: { value: 'deploy' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Create' }));
	await waitFor(() => expect(api.auth.stepUp.status).toHaveBeenCalledOnce());
}
async function startRotate() {
	render(AccountSettings);
	await waitFor(() => expect(screen.getByText('ci-runner')).toBeInTheDocument());
	await fireEvent.click(screen.getByRole('button', { name: /^Rotate$/ }));
	await fireEvent.click(await screen.findByRole('button', { name: 'Rotate token' }));
	await waitFor(() => expect(api.auth.stepUp.status).toHaveBeenCalledOnce());
}
async function verifyPassword() {
	await fireEvent.input(await screen.findByLabelText('Password'), { target: { value: 'fresh-password' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Verify' }));
}

describe('TASK-4693: key create and rotate step-up', () => {
	it('create verifies, then retries the same create once', async () => {
		await startCreate();
		await verifyPassword();
		await waitFor(() => expect(api.auth.tokens.create).toHaveBeenCalledTimes(2));
		expect(vi.mocked(api.auth.tokens.create).mock.calls[0]).toEqual(vi.mocked(api.auth.tokens.create).mock.calls[1]);
		expect(await screen.findByText('pad_cdNEWSECRET')).toBeInTheDocument();
	});
	it('the retry sends the refused request, not edits made while the prompt is open', async () => {
		await startCreate();
		await fireEvent.input(screen.getByLabelText('Token name'), { target: { value: 'renamed' } });
		await fireEvent.click(screen.getByRole('radio', { name: /Read only/ }));
		await verifyPassword();
		await waitFor(() => expect(api.auth.tokens.create).toHaveBeenCalledTimes(2));
		const calls = vi.mocked(api.auth.tokens.create).mock.calls;
		expect(calls[0]).toEqual(['deploy', '["*"]']);
		expect(calls[1]).toEqual(calls[0]);
	});
	it('the name and access are locked while the prompt is open; cancel unlocks them', async () => {
		await startCreate();
		expect(screen.getByLabelText('Token name')).toBeDisabled();
		expect(screen.getByRole('radio', { name: /Read only/ })).toBeDisabled();
		expect(screen.getByRole('radio', { name: /Read and write/ })).toBeDisabled();
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
		await waitFor(() => expect(screen.getByLabelText('Token name')).not.toBeDisabled());
		expect(screen.getByRole('radio', { name: /Read only/ })).not.toBeDisabled();
		expect((screen.getByLabelText('Token name') as HTMLInputElement).value).toBe('deploy');
	});
	it('rotate verifies, then retries the same rotate once', async () => {
		await startRotate();
		const dialog = screen.getByRole('dialog');
		await fireEvent.input(await within(dialog).findByLabelText('Password'), { target: { value: 'fresh-password' } });
		await fireEvent.click(within(dialog).getByRole('button', { name: 'Verify' }));
		await waitFor(() => expect(api.auth.tokens.rotate).toHaveBeenCalledTimes(2));
		expect(vi.mocked(api.auth.tokens.rotate).mock.calls[0]).toEqual(['t1']);
		expect(vi.mocked(api.auth.tokens.rotate).mock.calls[1]).toEqual(vi.mocked(api.auth.tokens.rotate).mock.calls[0]);
		expect(await screen.findByText('pad_efROTATEDSECRET')).toBeInTheDocument();
	});
	it('a refused retry is reported without prompting again', async () => {
		state.refuseRetry = true;
		await startCreate();
		await verifyPassword();
		await waitFor(() => expect(api.auth.tokens.create).toHaveBeenCalledTimes(2));
		expect(await screen.findByText(/then retry within 10 minutes/)).toBeInTheDocument();
		expect(api.auth.stepUp.status).toHaveBeenCalledOnce();
	});
	it('an account with no factor is told to sign in again', async () => {
		state.factor = 'unavailable';
		await startCreate();
		expect(await screen.findByText(/Sign out and sign in again, then retry within 10 minutes/)).toBeInTheDocument();
		expect(screen.queryByRole('button', { name: 'Verify' })).toBeNull();
		expect(api.auth.tokens.create).toHaveBeenCalledOnce();
	});
});
