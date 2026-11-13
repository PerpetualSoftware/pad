import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';

// TASK-4516: the step-up prompt replaces the edit form, so it must take focus
// itself (never leave it on <body>), carry its own heading, and tie an error
// to the field it is about.
vi.mock('$lib/api/client', () => ({ api: { auth: { stepUp: {
	sendEmailCode: vi.fn(async () => ({ sent: true, expires_at: '2026-11-12T12:10:00Z' })),
	verify: vi.fn(async () => { throw new Error('Incorrect password'); }),
} } } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { identityFence: () => () => true } }));
const { default: StepUpPrompt } = await import('./StepUpPrompt.svelte');

beforeEach(() => { vi.clearAllMocks(); });
const mount = (factor: 'password' | 'totp' | 'email' | 'unavailable') => render(StepUpPrompt, { factor, onverified: vi.fn() });

describe('TASK-4516: step-up prompt focus and labelling', () => {
	it('password: focuses the password input and labels the form by its heading', async () => {
		mount('password');
		const heading = screen.getByRole('heading', { name: 'Verify it’s you' });
		const form = screen.getByRole('form', { name: 'Verify it’s you' });
		expect(form.getAttribute('aria-labelledby')).toBe(heading.id);
		await waitFor(() => expect(screen.getByLabelText('Password')).toHaveFocus());
	});
	it('totp: focuses the code input, and the recovery toggle refocuses it', async () => {
		mount('totp');
		await waitFor(() => expect(screen.getByLabelText('Authenticator code')).toHaveFocus());
		const toggle = screen.getByRole('button', { name: 'Use a recovery code' });
		toggle.focus();
		await fireEvent.click(toggle);
		await waitFor(() => expect(screen.getByLabelText('Recovery code')).toHaveFocus());
	});
	it('email: focuses Send code, then the code input once the code is sent', async () => {
		const { api } = await import('$lib/api/client');
		mount('email');
		const send = screen.getByRole('button', { name: 'Send code' });
		await waitFor(() => expect(send).toHaveFocus());
		await fireEvent.click(send);
		await waitFor(() => expect(api.auth.stepUp.sendEmailCode).toHaveBeenCalledOnce());
		await waitFor(() => expect(screen.getByLabelText('Email code')).toHaveFocus());
	});
	it('unavailable: focuses the heading', async () => {
		mount('unavailable');
		const heading = screen.getByRole('heading', { name: 'Verify it’s you' });
		await waitFor(() => expect(heading).toHaveFocus());
		expect(heading.getAttribute('tabindex')).toBe('-1');
	});
	it('an error marks the input invalid and describes it', async () => {
		mount('password');
		const input = screen.getByLabelText('Password');
		expect(input.getAttribute('aria-invalid')).not.toBe('true');
		await fireEvent.input(input, { target: { value: 'wrong' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Verify' }));
		const alert = await screen.findByRole('alert');
		expect(alert).toHaveTextContent('Incorrect password');
		expect(input.getAttribute('aria-invalid')).toBe('true');
		expect(input.getAttribute('aria-describedby')).toBe(alert.id);
		expect(alert.id).not.toBe('');
	});
	it('a failed verify returns focus to the field', async () => {
		mount('password');
		const input = screen.getByLabelText('Password');
		await fireEvent.input(input, { target: { value: 'wrong' } });
		const verify = screen.getByRole('button', { name: 'Verify' });
		// A click moves focus to Verify, which busy then disables.
		verify.focus();
		await fireEvent.click(verify);
		await screen.findByRole('alert');
		await waitFor(() => expect(input).toHaveFocus());
	});
});
