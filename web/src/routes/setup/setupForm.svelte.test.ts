// TASK-2259 + TASK-2236: the setup page is a real form, like the other auth
// pages. The e2e suite cannot reach this form (it renders only on an instance
// with no admin yet), so this renders the page itself: both of its forms, the
// paste-token step and the first-admin step, with labelled inputs inside a
// <form>, the first field focused, and an empty submit refused with an
// announced error.
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';

const session = vi.hoisted(() => ({ setup_method: 'token' as string }));
const calls = vi.hoisted(() => ({ bootstrap: 0, reject: '' }));

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code = '';
	}
	return {
		PadApiError,
		api: {
			auth: {
				bootstrap: vi.fn(async () => {
					calls.bootstrap++;
					if (calls.reject) throw new PadApiError(calls.reject);
					return {};
				})
			}
		}
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		cloudMode: false,
		user: null,
		ensureLoaded: async () => ({ authenticated: false, setup_required: true, setup_method: session.setup_method }),
		load: async () => {}
	}
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(async () => {}) }));

import SetupPage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

/** Every input is inside a form and has exactly one label pointing at it. */
function expectLabelledInForm(container: HTMLElement) {
	const inputs = Array.from(container.querySelectorAll('input'));
	expect(inputs.length).toBeGreaterThan(0);
	for (const input of inputs) {
		expect(input.closest('form'), `${input.id || input.name} is inside a form`).not.toBeNull();
		expect(input.id, 'input has an id').not.toBe('');
		expect(container.querySelectorAll(`label[for="${input.id}"]`).length, `${input.id} has one label`).toBe(1);
	}
}

beforeEach(() => {
	calls.bootstrap = 0;
	calls.reject = '';
	history.replaceState({}, '', '/setup');
});
afterEach(() => {
	cleanup();
});

describe('setup page forms (TASK-2259, TASK-2236)', () => {
	it('the paste-token step is a labelled form, focused, and refuses an empty submit with an alert', async () => {
		session.setup_method = 'token';
		const { container } = render(SetupPage);
		await settle();

		const token = screen.getByLabelText('Bootstrap token');
		expect(document.activeElement).toBe(token);
		expectLabelledInForm(container);
		const form = token.closest('form')!;
		expect(form.getAttribute('method')).toBe('post');
		expect(form.querySelector('button[type="submit"]')).not.toBeNull();

		await fireEvent.submit(form);
		await settle();
		expect(screen.getByRole('alert').textContent?.trim()).not.toBe('');
		expect(calls.bootstrap).toBe(0);
	});

	it('the first-admin step (open setup) is a labelled form focused on Email, and refuses an empty submit', async () => {
		session.setup_method = 'open';
		const { container } = render(SetupPage);
		await settle();

		const email = screen.getByLabelText('Email');
		expect(document.activeElement).toBe(email);
		expect(screen.getByLabelText('Name')).toBeTruthy();
		expect(screen.getByLabelText('Password', { exact: true })).toBeTruthy();
		expect(screen.getByLabelText('Confirm password')).toBeTruthy();
		expectLabelledInForm(container);

		await fireEvent.submit(email.closest('form')!);
		await settle();
		expect(screen.getByRole('alert').textContent?.trim()).not.toBe('');
		expect(calls.bootstrap).toBe(0);
	});

	// TASK-2260: the rule sits under the password, and a refusal for the
	// password, local or from the server, shows there rather than at the top.
	async function fillOpenSetup(password: string) {
		session.setup_method = 'open';
		render(SetupPage);
		await settle();
		await fireEvent.input(screen.getByLabelText('Email'), { target: { value: 'admin@example.com' } });
		await fireEvent.input(screen.getByLabelText('Name'), { target: { value: 'Admin' } });
		await fireEvent.input(screen.getByLabelText('Password', { exact: true }), { target: { value: password } });
		await fireEvent.input(screen.getByLabelText('Confirm password'), { target: { value: password } });
		const input = screen.getByLabelText('Password', { exact: true });
		await fireEvent.submit(input.closest('form')!);
		await settle();
		return input;
	}

	it('states the password rule beside the field (TASK-2260)', async () => {
		session.setup_method = 'open';
		render(SetupPage);
		await settle();
		const input = screen.getByLabelText('Password', { exact: true });
		expect(input.getAttribute('aria-describedby')).toBe('setup-password-rule');
		expect(document.getElementById('setup-password-rule')?.textContent).toContain('At least 8 characters');
		expect(input.getAttribute('aria-invalid')).not.toBe('true');
	});

	it('refuses a short password on the field, without a round trip (TASK-2260)', async () => {
		const input = await fillOpenSetup('short');
		expect(calls.bootstrap).toBe(0);
		expect(screen.getAllByRole('alert')).toHaveLength(1);
		expect(document.getElementById('setup-password-error')?.textContent).toBe('Password must be at least 8 characters.');
		expect(input.getAttribute('aria-invalid')).toBe('true');
		expect(input.getAttribute('aria-describedby')).toBe('setup-password-rule setup-password-error');
	});

	it("shows the server's weak-password refusal on the field (TASK-2260)", async () => {
		calls.reject = 'Password is too weak — try a longer passphrase or add unusual characters';
		const input = await fillOpenSetup('password123');
		expect(calls.bootstrap).toBe(1);
		expect(screen.getAllByRole('alert')).toHaveLength(1);
		expect(document.getElementById('setup-password-error')?.textContent).toBe(calls.reject);
		expect(input.getAttribute('aria-invalid')).toBe('true');
	});

	it('keeps any other server refusal at the top of the form (TASK-2260)', async () => {
		calls.reject = 'Something else went wrong';
		const input = await fillOpenSetup('a long enough passphrase');
		expect(document.getElementById('setup-password-error')).toBeNull();
		expect(screen.getByRole('alert').textContent).toContain('Something else went wrong');
		expect(input.getAttribute('aria-invalid')).not.toBe('true');
	});
});
