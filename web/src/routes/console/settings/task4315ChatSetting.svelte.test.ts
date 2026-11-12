import { expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/** TASK-4315: the Chat card on /console/settings is a labelled per-user toggle that writes through the store. */
const state = vi.hoisted(() => ({
	enabled: false, epoch: 0, signalCount: 0, userId: 'u1',
	epochSignal: null as null | { set(key: string, value: number): unknown },
	set: vi.fn(),
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
			me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true, totp_enabled: false, oauth_providers: [], timezone: null })),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: { list: vi.fn(async () => []), create: vi.fn(), rotate: vi.fn(), delete: vi.fn() },
			updateProfile: vi.fn(), setTimezone: vi.fn(), unlinkProvider: vi.fn(), deleteAccount: vi.fn(), logout: vi.fn(), forgotPassword: vi.fn()
		}
	}
}));
vi.mock('$lib/stores/chatSetting.svelte', async () => {
	const { SvelteMap } = await import('svelte/reactivity');
	const signal = new SvelteMap<string, number>();
	return {
		chatSetting: {
			get enabled() { signal.get('v'); return state.enabled; },
			set: async (next: boolean) => { const v = await state.set(next); state.enabled = v; signal.set('v', (signal.get('v') ?? 0) + 1); return v; }
		}
	};
});
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
			load: vi.fn(async () => {}),
			ensureLoaded: vi.fn(async () => {})
		}
	};
});

const { inputMode } = await import('$lib/chat/inputMode.svelte');
const { default: AccountSettings } = await import('./+page.svelte');

beforeEach(() => {
	localStorage.clear();
	inputMode.reset();
	state.enabled = false; state.epoch = 0; state.userId = 'u1'; state.listeners.clear();
	state.set.mockReset();
});

it('shows a labelled Chat toggle, off by default, with the display-preference note', async () => {
	const { container } = render(AccountSettings);
	const toggle = await screen.findByLabelText('Show Chat');
	expect(toggle).not.toBeChecked();
	expect(container.querySelector('section#chat')).not.toBeNull();
	expect(screen.getByText('Display preference only; messages are kept either way.')).toBeInTheDocument();
	// The Chat card follows the Profile card.
	const titles = [...container.querySelectorAll('h2.card-title')].map((h) => h.textContent);
	expect(titles.indexOf('Chat')).toBe(titles.indexOf('Profile') + 1);
});

it('turning it on writes true and shows the stored value', async () => {
	state.set.mockResolvedValue(true);
	render(AccountSettings);
	const toggle = await screen.findByLabelText('Show Chat');
	await waitFor(() => expect(toggle).toBeEnabled());
	await fireEvent.click(toggle);
	await waitFor(() => expect(state.set).toHaveBeenCalledWith(true));
	await waitFor(() => expect(toggle).toBeChecked());
});

it('is disabled while saving, and a failure shows an error and leaves it off', async () => {
	const d = deferred<boolean>();
	state.set.mockReturnValue(d.promise);
	render(AccountSettings);
	const toggle = await screen.findByLabelText('Show Chat');
	await waitFor(() => expect(toggle).toBeEnabled());
	await fireEvent.click(toggle);
	await waitFor(() => expect(toggle).toBeDisabled());
	expect(screen.getByText('Saving...')).toBeInTheDocument();
	d.reject(new Error('server said no'));
	expect(await screen.findByRole('alert')).toHaveTextContent('server said no');
	await waitFor(() => expect(toggle).toBeEnabled());
	expect(toggle).not.toBeChecked();
});

it('an identity change clears the saving and error state of the previous identity', async () => {
	const d = deferred<boolean>();
	state.set.mockReturnValue(d.promise);
	render(AccountSettings);
	const toggle = await screen.findByLabelText('Show Chat');
	await waitFor(() => expect(toggle).toBeEnabled());
	await fireEvent.click(toggle);
	await waitFor(() => expect(screen.getByText('Saving...')).toBeInTheDocument());
	changeIdentity();
	await waitFor(() => expect(screen.queryByText('Saving...')).toBeNull());
	d.reject(new Error('late failure'));
	await new Promise((r) => setTimeout(r, 0));
	expect(screen.queryByRole('alert')).toBeNull();
});


afterEach(() => Reflect.deleteProperty(window, 'matchMedia'));

it('shows the Enter-sends device preference in the Chat card on a coarse pointer', async () => {
	Object.defineProperty(window, 'matchMedia', {
		configurable: true,
		value: () => ({ matches: false })
	});
	const { container } = render(AccountSettings);
	const toggle = await screen.findByLabelText('Chat: Enter sends (for a hardware keyboard)');
	expect(container.querySelector('section#chat')).toContainElement(toggle);
	expect(toggle).not.toBeChecked();
});
