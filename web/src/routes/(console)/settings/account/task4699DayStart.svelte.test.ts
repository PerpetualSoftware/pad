import { expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/** TASK-4699: the "Day starts at" control on /settings/account shows 09:00 when unset and saves through api.auth.setDayStart. */
const state = vi.hoisted(() => ({
	dayStart: null as string | null, epoch: 0, signalCount: 0, userId: 'u1',
	epochSignal: null as null | { set(key: string, value: number): unknown },
	setDayStart: vi.fn(), storeSet: vi.fn(),
	listeners: new Set<() => void>()
}));
function changeIdentity() { state.epoch++; state.epochSignal?.set('epoch', ++state.signalCount); for (const fn of [...state.listeners]) fn(); }
function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	isPlanLimitError: () => false,
	api: {
		calendarFeeds: { list: vi.fn(async () => []), create: vi.fn(), revoke: vi.fn() },
		workspaces: { list: vi.fn(async () => []) },
		notificationSettings: { get: vi.fn(async () => ({})), put: vi.fn() },
		auth: {
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true, totp_enabled: false, oauth_providers: [], timezone: 'America/New_York', day_start: state.dayStart })),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: { list: vi.fn(async () => []), create: vi.fn(), rotate: vi.fn(), delete: vi.fn() },
			updateProfile: vi.fn(), setTimezone: vi.fn(), setDayStart: state.setDayStart,
			unlinkProvider: vi.fn(), deleteAccount: vi.fn(), logout: vi.fn(), forgotPassword: vi.fn()
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', async () => {
	const { SvelteMap } = await import('svelte/reactivity');
	const signal = new SvelteMap<string, number>();
	state.epochSignal = signal;
	return {
		authStore: {
			get cloudMode() { return false; },
			get emailConfigured() { return true; },
			get pushAvailable() { return false; },
			get user() { return { id: 'u1', name: 'Pat', email: 'pat@example.com' }; },
			get userId() { return state.userId; },
			get identityEpoch() { signal.get('epoch'); return state.epoch; },
			identityFence() { const captured = state.epoch; return () => state.epoch === captured; },
			onIdentityChange(fn: () => void) { state.listeners.add(fn); return () => state.listeners.delete(fn); },
			setTimezone: vi.fn(),
			setDayStart: state.storeSet,
			load: vi.fn(async () => {}),
			ensureLoaded: vi.fn(async () => {})
		}
	};
});

const { default: AccountSettings } = await import('./+page.svelte');

beforeEach(() => {
	state.setDayStart.mockReset(); state.storeSet.mockReset();
	state.dayStart = null; state.epoch = 0; state.listeners.clear(); state.userId = 'u1';
});
afterEach(() => { vi.unstubAllGlobals(); });

it('an unset day start shows 09:00 as the default, beside the time zone', async () => {
	render(AccountSettings);
	const input = await screen.findByLabelText('Day starts at');
	expect(input).toHaveAttribute('type', 'time');
	await waitFor(() => expect(input).toHaveValue('09:00'));
	expect(screen.getByText('Not set, defaults to 09:00')).toBeInTheDocument();
	expect(screen.getByLabelText('Time zone')).toBeInTheDocument();
});

it('a set day start shows the stored value and no default note', async () => {
	state.dayStart = '07:30';
	render(AccountSettings);
	const input = await screen.findByLabelText('Day starts at');
	await waitFor(() => expect(input).toHaveValue('07:30'));
	expect(screen.queryByText('Not set, defaults to 09:00')).toBeNull();
});

it('Save sends day_start through the API and stores the answer', async () => {
	const { api } = await vi.importActual<typeof import('$lib/api/client')>('$lib/api/client');
	state.setDayStart.mockImplementation(api.auth.setDayStart);
	const fetcher = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({ id: 'u1', day_start: '06:45' })));
	vi.stubGlobal('fetch', fetcher);
	render(AccountSettings);
	const input = await screen.findByLabelText('Day starts at');
	await waitFor(() => expect(input).toHaveValue('09:00'));
	await fireEvent.input(input, { target: { value: '06:45' } });
	const save = screen.getByRole('button', { name: 'Save day start' });
	await waitFor(() => expect(save).toBeEnabled());
	await fireEvent.click(save);
	await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
	expect(fetcher.mock.calls[0]).toEqual(['/api/v1/auth/me', expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ day_start: '06:45' }) })]);
	expect(await screen.findByText('Day start updated.')).toBeInTheDocument();
	expect(state.storeSet).toHaveBeenCalledWith('u1', '06:45');
	expect(input).toHaveValue('06:45');
});

it('a failed save shows the error and re-enables the control', async () => {
	state.setDayStart.mockRejectedValue(new Error('`day_start` must be HH:MM'));
	render(AccountSettings);
	const input = await screen.findByLabelText('Day starts at');
	await waitFor(() => expect(input).toHaveValue('09:00'));
	const save = screen.getByRole('button', { name: 'Save day start' });
	await waitFor(() => expect(save).toBeEnabled());
	await fireEvent.click(save);
	expect((await screen.findAllByRole('alert')).some((n) => n.textContent === '`day_start` must be HH:MM')).toBe(true);
	expect(state.storeSet).not.toHaveBeenCalled();
	await waitFor(() => expect(input).toBeEnabled());
});

it('an identity change clears a save in flight for the previous identity', async () => {
	const pending = deferred<unknown>();
	state.setDayStart.mockReturnValue(pending.promise);
	render(AccountSettings);
	const input = await screen.findByLabelText('Day starts at');
	await waitFor(() => expect(input).toHaveValue('09:00'));
	const save = screen.getByRole('button', { name: 'Save day start' });
	await waitFor(() => expect(save).toBeEnabled());
	await fireEvent.click(save);
	await waitFor(() => expect(input).toBeDisabled());
	changeIdentity();
	await waitFor(() => expect(input).toBeEnabled());
	pending.resolve({ id: 'u1', day_start: '05:00' });
	await new Promise((r) => setTimeout(r, 0));
	expect(state.storeSet).not.toHaveBeenCalled();
	expect(screen.queryByText('Day start updated.')).toBeNull();
});
