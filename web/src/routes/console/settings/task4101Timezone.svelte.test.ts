import { expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/** TASK-4101: independent time zone Save uses the unconditional API path. */
const state = vi.hoisted(() => ({ push: false, gets: 0, zone: null as string | null, set: vi.fn() }));

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
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true, timezone: state.zone })),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: { list: vi.fn(async () => []), create: vi.fn(), rotate: vi.fn(), delete: vi.fn() },
			updateProfile: vi.fn(),
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
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		setTimezone: vi.fn(),
		load: vi.fn(async () => {}),
		ensureLoaded: vi.fn(async () => {})
	}
}));

const { default: AccountSettings } = await import('./+page.svelte');

beforeEach(() => { state.set.mockReset(); });
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
