import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

/**
 * TASK-3505: a Rotate button on each API token. The server and CLI already
 * rotate (same name, scopes and workspace, a new secret, the old one dead at
 * once); the console had no way to. Rotate asks first, says the old key stops
 * working now, shows the new key once with Copy, and shows a failure.
 */
const state = vi.hoisted(() => ({
	deleted: [] as string[],
	failDelete: false,
	copied: [] as string[],
	epoch: 0,
	rotated: [] as string[],
	failRotate: false,
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
				rotate: vi.fn(async (id: string) => {
					if (state.failRotate) throw new Error('Rotate refused');
					state.rotated.push(id);
					return { id, name: 'ci-runner', prefix: 'pad_ef', created_at: '2026-10-01T00:00:00Z', token: 'pad_efROTATEDSECRET' };
				}),
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
	state.rotated.length = 0;
	state.failRotate = false;
});

async function openRotate() {
	render(AccountSettings);
	await waitFor(() => expect(screen.getByText('ci-runner')).toBeInTheDocument());
	button(/^rotate$/i)!.click();
	await waitFor(() => expect(screen.getByText(/Rotate the token “ci-runner”\?/)).toBeInTheDocument());
	expect(screen.getByText(/stops working immediately/)).toBeInTheDocument();
}

describe('TASK-3505: rotating an API token', () => {
	it('asks first; Cancel rotates nothing', async () => {
		await openRotate();
		button(/^cancel$/i)!.click();
		await waitFor(() => expect(screen.queryByText(/Rotate the token/)).toBeNull());
		expect(state.rotated).toEqual([]);
	});

	it('shows the new key once, with Copy, and keeps the token listed under its name', async () => {
		await openRotate();
		button(/^rotate token$/i)!.click();
		await waitFor(() => expect(state.rotated).toEqual(['t1']));
		await waitFor(() => expect(screen.getByText('pad_efROTATEDSECRET')).toBeInTheDocument());
		expect(screen.getByText(/The new key for “ci-runner”/)).toBeInTheDocument();
		expect(screen.getAllByText('ci-runner').length).toBeGreaterThan(0);
		expect(screen.getByText(/pad_ef\.\.\./)).toBeInTheDocument(); // the row shows the new prefix
		button(/^copy$/i)!.click();
		await waitFor(() => expect(state.copied).toEqual(['pad_efROTATEDSECRET']));
	});

	it('a failed rotate says so and shows no key', async () => {
		state.failRotate = true;
		await openRotate();
		button(/^rotate token$/i)!.click();
		await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Rotate refused'));
		expect(screen.queryByText(/Copy this token now/)).toBeNull();
	});
});
