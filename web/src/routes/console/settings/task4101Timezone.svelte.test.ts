import { expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/** TASK-4101: independent time zone Save uses the unconditional API path. */
const state = vi.hoisted(() => ({
	push: false, gets: 0, zone: null as string | null, set: vi.fn(), update: vi.fn(),
	epoch: 0, signalCount: 0, userId: 'u1',
	// Set by the authStore mock: reading identityEpoch subscribes to it, as the store's $state epoch does.
	epochSignal: null as null | { set(key: string, value: number): unknown },
	// Queued /auth/me answers, consumed in call order; the default answer applies once it is empty.
	meQueue: [] as Array<Promise<unknown>>,
	listeners: new Set<() => void>()
}));
/** An identity change as the store does it: the epoch bumps first, then the listeners run. */
function changeIdentity() { state.epoch++; state.epochSignal?.set('epoch', ++state.signalCount); for (const fn of [...state.listeners]) fn(); }
function deferred<T>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((r) => { resolve = r; });
	return { promise, resolve };
}
const profileOf = (name: string, extra: Record<string, unknown> = {}) => ({ id: 'u1', name, email: 'pat@example.com', username: 'pat', password_set: true, totp_enabled: true, oauth_providers: ['github'], timezone: state.zone, ...extra });

vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	isPlanLimitError: () => false,
	api: {
		notificationSettings: {
			get: vi.fn(async () => {
				state.gets++;
				return {
					push_enabled: true,
					notify_assigned: true,
					notify_comments: true,
					notify_replies: true,
					notify_reminders: true,
					notify_mentions: true,
					include_agent_activity: false
				};
			}),
			put: vi.fn()
		},
		auth: {
			me: vi.fn(async () => state.meQueue.length ? state.meQueue.shift() : profileOf('Pat')),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: { list: vi.fn(async () => []), create: vi.fn(), rotate: vi.fn(), delete: vi.fn() },
			updateProfile: state.update,
			setTimezone: state.set,
			unlinkProvider: vi.fn(),
			deleteAccount: vi.fn(),
			logout: vi.fn(),
			forgotPassword: vi.fn()
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
		get pushAvailable() { return state.push; },
		get user() { return { id: 'u1', name: 'Pat', email: 'pat@example.com' }; },
		get userId() { return state.userId; },
		get identityEpoch() { signal.get('epoch'); return state.epoch; },
		identityFence() { const captured = state.epoch; return () => state.epoch === captured; },
		onIdentityChange(fn: () => void) { state.listeners.add(fn); return () => state.listeners.delete(fn); },
		setTimezone: vi.fn(),
		load: vi.fn(async () => {}),
		ensureLoaded: vi.fn(async () => {})
	}
	};
});

const { default: AccountSettings } = await import('./+page.svelte');

beforeEach(() => {
	state.set.mockReset(); state.update.mockReset();
	state.epoch = 0; state.zone = null; state.meQueue = []; state.listeners.clear(); state.userId = 'u1';
});
afterEach(() => { vi.unstubAllGlobals(); });
it.each([null, 'Asia/Tokyo'])('settings shows current zone %s and saves an unconditional zone', async (zone) => {
 const { api } = await vi.importActual<typeof import('$lib/api/client')>('$lib/api/client');
 state.set.mockImplementation(api.auth.setTimezone);
 const fetcher = vi.fn(async (_url: string, _init?: RequestInit) => new Response(JSON.stringify({ timezone: 'Europe/Paris' })));
 vi.stubGlobal('fetch', fetcher);
 state.zone = zone;
 render(AccountSettings);
 const input = await screen.findByLabelText('Time zone');
 await waitFor(() => expect(input).toHaveValue(zone ?? ''));
 if (zone === null) expect(screen.getByText('Not set, times use UTC')).toBeInTheDocument();
 await fireEvent.input(input, { target: { value: 'Europe/Paris' } });
 await fireEvent.click(screen.getByRole('button', { name: 'Save time zone' }));
 await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
 expect(fetcher.mock.calls[0]).toEqual(['/api/v1/auth/me', expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ timezone: 'Europe/Paris' }) })]);
});

// TASK-4101 PR2a r2 H2-M1: a load that settles after an identity change is dropped and
// the page reloads for the identity signed in now, so no save is built from the old one.
it('a profile load that settles after an identity change is dropped and the new identity is loaded', async () => {
	const { api } = await import('$lib/api/client');
	const first = deferred<unknown>();
	const second = deferred<unknown>();
	state.meQueue = [first.promise, second.promise];
	render(AccountSettings);
	await waitFor(() => expect(api.auth.me).toHaveBeenCalledTimes(1));
	changeIdentity();
	await waitFor(() => expect(api.auth.me).toHaveBeenCalledTimes(2));
	first.resolve(profileOf('Old'));
	await new Promise((r) => setTimeout(r, 0));
	expect(screen.queryByDisplayValue('Old')).toBeNull();
	expect(screen.queryByRole('button', { name: 'Save Changes' })).toBeNull();
	second.resolve(profileOf('New'));
	const name = await screen.findByLabelText('Name');
	await waitFor(() => expect(name).toHaveValue('New'));
	expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled();
});
it('both Save buttons are disabled while the profile is reloading for a new identity', async () => {
	render(AccountSettings);
	const name = await screen.findByLabelText('Name');
	await waitFor(() => expect(name).toHaveValue('Pat'));
	const pending = deferred<unknown>();
	state.meQueue = [pending.promise];
	state.zone = 'Asia/Tokyo';
	changeIdentity();
	await waitFor(() => expect(name).toHaveValue(''));
	await fireEvent.input(screen.getByLabelText('Time zone'), { target: { value: 'Europe/Paris' } });
	expect(screen.getByRole('button', { name: 'Save Changes' })).toBeDisabled();
	expect(screen.getByRole('button', { name: 'Save time zone' })).toBeDisabled();
	pending.resolve(profileOf('New'));
	await waitFor(() => expect(name).toHaveValue('New'));
	expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled();
	expect(screen.getByRole('button', { name: 'Save time zone' })).toBeEnabled();
});

// TASK-4101 PR2a r1 H-L1: a PATCH response is merged into the profile, never replaces it
// (the PATCH answer has no totp_enabled / oauth_providers / password_set).
it('a time zone save keeps the 2FA state the PATCH response omits', async () => {
	state.set.mockResolvedValue({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', timezone: 'Europe/Paris' });
	render(AccountSettings);
	await screen.findByText('Enabled');
	const input = await screen.findByLabelText('Time zone');
	await fireEvent.input(input, { target: { value: 'Europe/Paris' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Save time zone' }));
	await screen.findByText('Time zone updated.');
	expect(screen.getByText('Enabled')).toBeInTheDocument();
});
it('a name save keeps the 2FA state the PATCH response omits', async () => {
	state.update.mockResolvedValue({ id: 'u1', name: 'Pat B', email: 'pat@example.com', username: 'pat', timezone: null });
	render(AccountSettings);
	await screen.findByText('Enabled');
	await fireEvent.click(screen.getByRole('button', { name: 'Save Changes' }));
	await screen.findByText('Profile updated.');
	expect(screen.getByText('Enabled')).toBeInTheDocument();
});
it('a time zone Save counts as an explicit write', async () => {
	const { explicitTimezoneWrites } = await import('$lib/timezone');
	state.set.mockResolvedValue({ id: 'u1', timezone: 'Europe/Paris' });
	render(AccountSettings);
	const input = await screen.findByLabelText('Time zone');
	const before = explicitTimezoneWrites();
	await fireEvent.input(input, { target: { value: 'Europe/Paris' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Save time zone' }));
	await waitFor(() => expect(explicitTimezoneWrites()).toBe(before + 1));
});

// TASK-4101 PR2a r2 H2-M2: a failed explicit write must not count.
it('a time zone Save whose PATCH fails does not count as an explicit write', async () => {
	const { explicitTimezoneWrites } = await import('$lib/timezone');
	state.set.mockRejectedValue(new Error('bad zone'));
	render(AccountSettings);
	const input = await screen.findByLabelText('Time zone');
	const before = explicitTimezoneWrites();
	await fireEvent.input(input, { target: { value: 'Europe/Nowhere' } });
	await waitFor(() => expect(screen.getByRole('button', { name: 'Save time zone' })).toBeEnabled());
	await fireEvent.click(screen.getByRole('button', { name: 'Save time zone' }));
	await screen.findByText('bad zone');
	expect(explicitTimezoneWrites()).toBe(before);
});


// TASK-4101 PR2a r2 (lead ruling on option A): a fenced zone Save that settles after an
// identity change must not leave its busy flag stuck for the new identity.
it('a zone Save in flight across an identity change does not leave Save disabled', async () => {
	const pending = deferred<unknown>();
	state.set.mockReturnValue(pending.promise);
	render(AccountSettings);
	const input = await screen.findByLabelText('Time zone');
	await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('Pat'));
	await fireEvent.input(input, { target: { value: 'Europe/Paris' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Save time zone' }));
	changeIdentity();
	pending.resolve({ id: 'u1', timezone: 'Europe/Paris' });
	await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('Pat'));
	await fireEvent.input(screen.getByLabelText('Time zone'), { target: { value: 'Asia/Tokyo' } });
	await waitFor(() => expect(screen.getByRole('button', { name: 'Save time zone' })).toBeEnabled());
});

// #2342 E2E (account-delete.spec.ts:99): a sign-out's reload 401s raced the delete's navigation.
it('a sign-out does not reload the profile, and the next sign-in does', async () => {
	const { api } = await import('$lib/api/client');
	render(AccountSettings);
	await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('Pat'));
	const calls = vi.mocked(api.auth.me).mock.calls.length;
	state.userId = ''; changeIdentity(); await new Promise((r) => setTimeout(r, 0));
	expect(api.auth.me).toHaveBeenCalledTimes(calls);
	state.userId = 'u1'; changeIdentity();
	await waitFor(() => expect(api.auth.me).toHaveBeenCalledTimes(calls + 1));
	await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('Pat'));
});
