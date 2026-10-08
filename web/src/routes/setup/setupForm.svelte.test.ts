// TASK-2259 + TASK-2236: the setup page is a real form, like the other auth
// pages. The e2e suite cannot reach this form (it renders only on an instance
// with no admin yet), so this renders the page itself: both of its forms, the
// paste-token step and the first-admin step, with labelled inputs inside a
// <form>, the first field focused, and an empty submit refused with an
// announced error.
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';

const session = vi.hoisted(() => ({ setup_method: 'token' as string }));
const calls = vi.hoisted(() => ({ bootstrap: 0 }));

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
		expect(screen.getByLabelText('Password (at least 8 characters)')).toBeTruthy();
		expect(screen.getByLabelText('Confirm password')).toBeTruthy();
		expectLabelledInForm(container);

		await fireEvent.submit(email.closest('form')!);
		await settle();
		expect(screen.getByRole('alert').textContent?.trim()).not.toBe('');
		expect(calls.bootstrap).toBe(0);
	});
});
