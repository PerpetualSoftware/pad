import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { APIToken } from '$lib/types';

vi.mock('$lib/api/client', () => ({
	PadApiError: class PadApiError extends Error { code = 'step_up_required'; },
	api: { auth: {
		tokens: { update: vi.fn() },
		stepUp: { status: vi.fn(), verify: vi.fn(), sendEmailCode: vi.fn() }
	} }
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { identityFence: () => () => true } }));
const { api, PadApiError } = await import('$lib/api/client');
const { default: EditTokenAccessDialog } = await import('./EditTokenAccessDialog.svelte');
const token: APIToken = { id: 'key1', workspace_id: '', name: 'Build key', scopes: '["read"]', prefix: 'pad_test', created_at: '2026-11-01T00:00:00Z' };
beforeEach(() => { vi.resetAllMocks(); });
function mount() {
	const onclose = vi.fn(); const onupdated = vi.fn();
	render(EditTokenAccessDialog, { token, onclose, onupdated });
	return { onclose, onupdated };
}
async function openStepUp() {
	vi.mocked(api.auth.tokens.update).mockRejectedValueOnce(new PadApiError({ code: 'step_up_required', message: 'Verify identity' }));
	vi.mocked(api.auth.stepUp.status).mockResolvedValue({ factor: 'email', active_until: null });
	const callbacks = mount();
	await fireEvent.click(screen.getByRole('radio', { name: 'Read and write' }));
	await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
	await screen.findByRole('button', { name: 'Verify' });
	return callbacks;
}

describe('EditTokenAccessDialog', () => {
	it('uses a primary Save changes action', () => {
		mount();
		expect(screen.getByRole('button', { name: 'Save changes' })).toHaveClass('primary');
	});
	it('puts Cancel then Save in one footer and names the key in the subject', () => {
		mount();
		const dialog = screen.getByRole('dialog', { name: 'Edit API key access' });
		const footer = dialog.querySelector('footer');
		expect(footer).not.toBeNull();
		expect([...footer!.querySelectorAll('button')].map((button) => button.textContent)).toEqual(['Cancel', 'Save changes']);
		expect(dialog.querySelector('.subject')).toHaveTextContent('Build key');
	});
	it('ignores Escape during a save and reports the result when it finishes', async () => {
		let resolve!: (value: APIToken) => void;
		vi.mocked(api.auth.tokens.update).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
		const { onclose, onupdated } = mount();
		await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
		await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledOnce());
		await fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
		expect(onclose).not.toHaveBeenCalled();
		expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
		resolve(token);
		await waitFor(() => expect(onupdated).toHaveBeenCalledWith(token));
		expect(onclose).toHaveBeenCalledOnce();
	});
	it('puts primary Verify next to Cancel in the footer and preserves input and send focus', async () => {
		await openStepUp();
		const verify = screen.getByRole('button', { name: 'Verify' });
		expect(verify).toHaveClass('primary');
		const footer = verify.closest('footer');
		expect(footer).not.toBeNull();
		expect([...footer!.querySelectorAll('button')].map((button) => button.textContent)).toEqual(['Cancel', 'Verify']);
		expect(verify).toBeDisabled();
		await waitFor(() => expect(screen.getByRole('button', { name: 'Send code' })).toHaveFocus());
		await fireEvent.input(screen.getByLabelText('Email code'), { target: { value: '123456' } });
		expect(verify).toBeEnabled();
	});
	it.each(['send', 'verify'])('ignores Escape while step-up %s is in flight', async (action) => {
		let reject!: (reason: Error) => void;
		if (action === 'send') vi.mocked(api.auth.stepUp.sendEmailCode).mockReturnValueOnce(new Promise((_, fail) => { reject = fail; }));
		else vi.mocked(api.auth.stepUp.verify).mockReturnValueOnce(new Promise((_, fail) => { reject = fail; }));
		const { onclose } = await openStepUp();
		if (action === 'verify') await fireEvent.input(screen.getByLabelText('Email code'), { target: { value: '123456' } });
		await fireEvent.click(screen.getByRole('button', { name: action === 'send' ? 'Send code' : 'Verify' }));
		await fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
		expect(onclose).not.toHaveBeenCalled();
		expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
		reject(new Error('Try again'));
		expect(await screen.findByRole('alert')).toHaveTextContent('Try again');
		expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();
	});
	it('passes the unavailable reason to the step-up prompt (TASK-4602)', async () => {
		vi.mocked(api.auth.tokens.update).mockRejectedValueOnce(new PadApiError({ code: 'step_up_required', message: 'Verify identity' }));
		vi.mocked(api.auth.stepUp.status).mockResolvedValue({ factor: 'unavailable', unavailable_reason: 'sign_in_again', active_until: null });
		mount();
		await fireEvent.click(screen.getByRole('radio', { name: 'Read and write' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
		expect(await screen.findByText('This session started before you turned on two-factor authentication. Sign out and sign in again, then retry within 10 minutes.')).toBeInTheDocument();
	});
});
