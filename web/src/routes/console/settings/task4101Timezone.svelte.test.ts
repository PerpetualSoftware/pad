import { expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/** TASK-4101: independent time zone Save uses the unconditional API path. */
const state = vi.hoisted(() => ({ push: false, gets: 0, zone: null as string | null, set: vi.fn(), sameIdentity: true, update: vi.fn() }));

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
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true, totp_enabled: true, oauth_providers: ['github'], timezone: state.zone })),
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
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get cloudMode() { return false; },
		get emailConfigured() { return true; },
		get pushAvailable() { return state.push; },
		get user() { return { id: 'u1', name: 'Pat', email: 'pat@example.com' }; },
		get userId() { return 'u1'; },
		get identityEpoch() { return 0; },
		identityFence() { return () => state.sameIdentity; },
		onIdentityChange() { return () => {}; },
		setTimezone: vi.fn(),
		load: vi.fn(async () => {}),
		ensureLoaded: vi.fn(async () => {})
	}
}));

const { default: AccountSettings } = await import('./+page.svelte');

beforeEach(() => { state.set.mockReset(); state.update.mockReset(); state.sameIdentity = true; state.zone = null; });
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

// TASK-4101 PR2a r1 C-M1: the onMount /auth/me result is identity-fenced.
it('a profile load that settles after an identity change is not adopted', async () => {
	state.zone = 'Asia/Tokyo';
	state.sameIdentity = false;
	render(AccountSettings);
	const input = await screen.findByLabelText('Time zone');
	await new Promise((r) => setTimeout(r, 0));
	expect(input).toHaveValue('');
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
