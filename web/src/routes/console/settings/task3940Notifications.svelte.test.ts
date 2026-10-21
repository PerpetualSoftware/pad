import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

/** TASK-3940: the account settings page shows Notifications only where push is available. */
const state = vi.hoisted(() => ({ push: false, gets: 0 }));

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
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true })),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: { list: vi.fn(async () => []), create: vi.fn(), rotate: vi.fn(), delete: vi.fn() },
			updateProfile: vi.fn(),
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
		load: vi.fn(async () => {}),
		ensureLoaded: vi.fn(async () => {})
	}
}));

const { default: AccountSettings } = await import('./+page.svelte');

beforeEach(() => {
	state.push = false;
	state.gets = 0;
});

describe('TASK-3940: Notifications section gating', () => {
	it('is absent, and loads nothing, when push is not available', async () => {
		render(AccountSettings);
		await waitFor(() => expect(screen.getByText('Keyboard')).toBeInTheDocument());
		expect(screen.queryByRole('heading', { name: 'Notifications' })).toBeNull();
		expect(state.gets).toBe(0);
	});

	it('shows the heading and seven toggles when push is available', async () => {
		state.push = true;
		const { container } = render(AccountSettings);
		await waitFor(() => expect(screen.getByRole('heading', { name: 'Notifications' })).toBeInTheDocument());
		const section = container.querySelector('#notifications') as HTMLElement;
		await waitFor(() => expect(section.querySelectorAll('input[type=checkbox]')).toHaveLength(7));
	});
});
