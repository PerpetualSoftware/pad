import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

/**
 * TASK-2193: deleting an API token fired on the first click and swallowed a
 * failure ("Silent failure acceptable for delete"), and the token shown once
 * at creation had no copy control. Delete now asks, naming the token, says
 * when it fails, and the new token has a Copy button.
 */
const state = vi.hoisted(() => ({
	deleted: [] as string[],
	failDelete: false,
	copied: [] as string[],
	epoch: 0,
	holdDelete: null as null | { release: () => void }
}));

vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	isPlanLimitError: () => false,
	api: {
		auth: {
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true })),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: {
				list: vi.fn(async () => [{ id: 't1', name: 'ci-runner', prefix: 'pad_ab', created_at: '2026-10-01T00:00:00Z' }]),
				create: vi.fn(async (name: string) => ({ id: 't2', name, prefix: 'pad_cd', created_at: '2026-10-08T00:00:00Z', token: 'pad_cdSECRETTOKEN' })),
				delete: vi.fn(async (id: string) => {
					if (state.holdDelete) await new Promise<void>((r) => (state.holdDelete!.release = r));
					if (state.failDelete) throw new Error('Server unavailable');
					state.deleted.push(id);
				})
			},
			updateProfile: vi.fn(),
			unlinkProvider: vi.fn(),
			deleteAccount: vi.fn(),
			logout: vi.fn(),
			forgotPassword: vi.fn()
		}
	}
}));
vi.mock('$lib/utils/clipboard', () => ({
	copyToClipboard: vi.fn(async (text: string) => {
		state.copied.push(text);
		return true;
	})
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get cloudMode() { return false; },
		get emailConfigured() { return true; },
		get user() { return { id: 'u1', name: 'Pat', email: 'pat@example.com' }; },
		get userId() { return 'u1'; },
		get identityEpoch() { return 0; },
		identityFence() {
			const captured = state.epoch;
			return () => state.epoch === captured;
		},
		onIdentityChange() { return () => {}; },
		load: vi.fn(async () => {}),
		ensureLoaded: vi.fn(async () => {})
	}
}));

const { default: AccountSettings } = await import('./+page.svelte');

const button = (re: RegExp) => screen.getAllByRole('button').find((b) => re.test(b.textContent ?? ''));

beforeEach(() => {
	state.deleted.length = 0;
	state.copied.length = 0;
	state.failDelete = false;
	state.epoch = 0;
	state.holdDelete = null;
});

async function openDeleteConfirm() {
	render(AccountSettings);
	await waitFor(() => expect(screen.getByText('ci-runner')).toBeInTheDocument());
	button(/^delete$/i)!.click();
	await waitFor(() => expect(screen.getByText(/Delete the token “ci-runner”\?/)).toBeInTheDocument());
}

describe('TASK-2193: API tokens', () => {
	it('Delete asks first, naming the token; Cancel deletes nothing', async () => {
		await openDeleteConfirm();
		expect(state.deleted).toEqual([]);
		button(/^cancel$/i)!.click();
		await waitFor(() => expect(screen.queryByText(/Delete the token/)).toBeNull());
		expect(state.deleted).toEqual([]);
		expect(screen.getByText('ci-runner')).toBeInTheDocument();
	});

	it('confirming deletes the token', async () => {
		await openDeleteConfirm();
		button(/^delete token$/i)!.click();
		await waitFor(() => expect(state.deleted).toEqual(['t1']));
		await waitFor(() => expect(screen.queryByText('ci-runner')).toBeNull());
	});

	it('a failed delete says so and keeps the token listed', async () => {
		state.failDelete = true;
		await openDeleteConfirm();
		button(/^delete token$/i)!.click();
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Server unavailable'));
		expect(screen.getAllByText('ci-runner').length).toBeGreaterThan(0);
	});

	it('the token shown once has a Copy button', async () => {
		render(AccountSettings);
		const input = await screen.findByPlaceholderText('Token name');
		(input as HTMLInputElement).value = 'laptop';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await waitFor(() => expect(button(/^create$/i)).toBeTruthy());
		button(/^create$/i)!.click();
		await waitFor(() => expect(screen.getByText('pad_cdSECRETTOKEN')).toBeInTheDocument());
		button(/^copy$/i)!.click();
		await waitFor(() => expect(state.copied).toEqual(['pad_cdSECRETTOKEN']));
		await waitFor(() => expect(button(/^copied$/i)).toBeTruthy());
	});

	it('a delete answered after a sign-in change edits nothing (BUG-3105 fence)', async () => {
		await openDeleteConfirm();
		state.holdDelete = { release: () => {} };
		button(/^delete token$/i)!.click();
		await waitFor(() => expect(button(/deleting/i)).toBeTruthy());
		state.epoch += 1; // another identity signed in
		state.holdDelete.release();
		await new Promise((r) => setTimeout(r, 20));
		expect(state.deleted).toEqual(['t1']); // the request went out
		expect(screen.getAllByText('ci-runner').length).toBeGreaterThan(0); // the list was not edited
	});
});
