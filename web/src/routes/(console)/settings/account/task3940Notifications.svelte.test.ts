import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

/** TASK-3940: the account settings page shows Notifications only where push is available. */
const state = vi.hoisted(() => ({ push: false, cloud: false, gets: 0 }));

vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	isPlanLimitError: () => false,
	api: {
		calendarFeeds: { list: vi.fn(async () => []), create: vi.fn(), revoke: vi.fn() },
		workspaces: { list: vi.fn(async () => []) },
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
		get cloudMode() { return state.cloud; },
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
	state.cloud = false;
	state.gets = 0;
});

describe('TASK-3940: Notifications section gating', () => {
	it('is absent, and loads nothing, when push is not available', async () => {
		render(AccountSettings);
		await waitFor(() => expect(screen.getByRole('heading', { name: 'Keyboard' })).toBeInTheDocument());
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

it.each([false, true])('indexes only rendered account cards, cloud=%s', async (cloud) => {
	state.cloud = cloud;
	state.push = cloud;
	const { container } = render(AccountSettings);
	await waitFor(() => expect(screen.getByRole('link', { name: 'Profile' })).toHaveAttribute('href', '#profile'));
	const cards = [...container.querySelectorAll('.settings-page > section.card')];
	const links = [...screen.getByRole('navigation', { name: 'On this page' }).querySelectorAll('a')];
	expect(links.map(link => link.getAttribute('href'))).toEqual(cards.map(card => '#' + card.id));
	expect(cards.every(card => card.id)).toBe(true);
	for (const id of ['notifications', 'linked-accounts', 'plan']) {
		expect(links.some(link => link.getAttribute('href') === '#' + id)).toBe(cloud);
	}
	expect(links.at(-1)).toHaveAttribute('href', '#danger-zone');
	expect(links.at(-1)).toHaveClass('danger');
});
