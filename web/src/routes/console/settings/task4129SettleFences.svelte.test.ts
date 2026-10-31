import { expect, it, vi, beforeEach, afterEach, describe } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

/**
 * TASK-4129: every settings-page settle that writes identity-bound state is fenced on the identity
 * it started under, and an identity change resets the page's per-identity form state. Each test
 * holds a settle, changes identity, then lets the settle land and checks that none of its writes
 * (messages, secrets, recovery codes) reached the page and that the buttons are usable again.
 */
const state = vi.hoisted(() => ({
	epoch: 0, signalCount: 0, userId: 'u1', cloud: false, planLimit: false,
	extra: {} as Record<string, unknown>,
	epochSignal: null as null | { set(key: string, value: number): unknown },
	meQueue: [] as Array<Promise<unknown>>,
	listeners: new Set<(previousUserId: string) => void>(),
	toCanvas: vi.fn(),
	exportData: vi.fn(),
	planToast: vi.fn(),
	clear: vi.fn(),
	load: vi.fn(async () => {}),
	update: vi.fn()
}));
function changeIdentity(previousUserId = 'u1') {
	state.epoch++; state.epochSignal?.set('epoch', ++state.signalCount);
	for (const fn of [...state.listeners]) fn(previousUserId);
}
function deferred<T = unknown>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}
// Captured before any test intercepts setTimeout.
const realSetTimeout = globalThis.setTimeout;
const flush = () => new Promise((r) => setTimeout(r, 30));
const profileOf = (name: string, extra: Record<string, unknown> = {}) => ({ id: 'u1', name, email: 'pat@example.com', username: 'pat', password_set: true, totp_enabled: true, oauth_providers: ['github'], timezone: null, ...state.extra, ...extra });

vi.mock('qrcode', () => ({ default: { toCanvas: state.toCanvas } }));
vi.mock('$lib/utils/artifacts', () => ({ exportAndDownloadAccountData: state.exportData }));
vi.mock('$lib/billing/planLimitToast', () => ({ showPlanLimitToast: state.planToast }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	isPlanLimitError: () => state.planLimit,
	api: {
		notificationSettings: {
			get: vi.fn(async () => ({ push_enabled: true, notify_assigned: true, notify_comments: true, notify_replies: true, notify_reminders: true, notify_mentions: true, include_agent_activity: false })),
			put: vi.fn()
		},
		auth: {
			me: vi.fn(async () => state.meQueue.length ? state.meQueue.shift() : profileOf('Pat')),
			totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
			tokens: { list: vi.fn(async () => []), create: vi.fn(), rotate: vi.fn(), delete: vi.fn() },
			updateProfile: state.update,
			setTimezone: vi.fn(),
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
			get cloudMode() { return state.cloud; },
			get emailConfigured() { return true; },
			get pushAvailable() { return false; },
			get user() { return { id: 'u1', name: 'Pat', email: 'pat@example.com' }; },
			get userId() { return state.userId; },
			get identityEpoch() { signal.get('epoch'); return state.epoch; },
			identityFence() { const captured = state.epoch; return () => state.epoch === captured; },
			onIdentityChange(fn: (previousUserId: string) => void) { state.listeners.add(fn); return () => state.listeners.delete(fn); },
			setTimezone: vi.fn(),
			clear: state.clear,
			load: state.load,
			ensureLoaded: vi.fn(async () => {})
		}
	};
});

const { default: AccountSettings } = await import('./+page.svelte');
const { api } = await import('$lib/api/client');
const mocked = {
	unlink: vi.mocked(api.auth.unlinkProvider),
	forgot: vi.mocked(api.auth.forgotPassword),
	setup: vi.mocked(api.auth.totp.setup),
	verify: vi.mocked(api.auth.totp.verify),
	disable: vi.mocked(api.auth.totp.disable),
	create: vi.mocked(api.auth.tokens.create),
	del: vi.mocked(api.auth.deleteAccount),
	me: vi.mocked(api.auth.me)
};

beforeEach(() => {
	for (const m of [mocked.unlink, mocked.forgot, mocked.setup, mocked.verify, mocked.disable, mocked.create, mocked.del, state.update, state.exportData, state.toCanvas, state.planToast, state.clear, state.load]) m.mockReset();
	state.load.mockImplementation(async () => {});
	state.epoch = 0; state.userId = 'u1'; state.cloud = false; state.planLimit = false; state.extra = {};
	state.meQueue = []; state.listeners.clear();
});
afterEach(() => { vi.restoreAllMocks(); window.history.replaceState({}, '', '/'); });

async function ready() {
	render(AccountSettings);
	await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('Pat'));
}
/** An identity change, then the reload for the identity signed in now. */
async function bump(previousUserId = 'u1') {
	changeIdentity(previousUserId);
	await flush();
	await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('Pat'));
}
const text = () => document.body.textContent ?? '';
const button = (name: string) => screen.getByRole('button', { name });

describe('profile writers', () => {
	it('a failed unlink that lands after an identity change shows no error', async () => {
		state.cloud = true;
		const held = deferred();
		mocked.unlink.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Unlink'));
		await bump();
		held.reject(new Error('Unlink failed for old'));
		await flush();
		expect(screen.queryByText('Unlink failed for old')).toBeNull();
	});
	it('an unlink that lands after an identity change writes no message', async () => {
		state.cloud = true;
		const held = deferred();
		mocked.unlink.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Unlink'));
		await bump();
		held.resolve(undefined);
		await flush();
		expect(screen.queryByText('GitHub unlinked.')).toBeNull();
	});
	it('an unlink whose profile refresh lands after an identity change does not replace the profile', async () => {
		state.cloud = true;
		const stale = deferred();
		mocked.unlink.mockResolvedValueOnce(undefined as never);
		await ready();
		state.meQueue = [stale.promise];
		await fireEvent.click(button('Unlink'));
		await waitFor(() => expect(state.meQueue.length).toBe(0));
		await bump();
		stale.resolve(profileOf('Old', { oauth_providers: [] }));
		await flush();
		expect(screen.queryByText('GitHub unlinked.')).toBeNull();
		expect(screen.getByRole('button', { name: 'Unlink' })).toBeInTheDocument();
	});

	it('a profile save that lands after an identity change writes nothing, skips the store reload, and leaves Save idle', async () => {
		const held = deferred();
		state.update.mockReturnValueOnce(held.promise);
		await ready();
		await fireEvent.click(button('Save Changes'));
		await bump();
		expect(button('Save Changes')).toBeEnabled();
		held.resolve({ id: 'u1', name: 'Old', email: 'pat@example.com', username: 'old' });
		await flush();
		expect(screen.queryByText('Profile updated.')).toBeNull();
		expect(state.load).not.toHaveBeenCalled();
		expect(button('Save Changes')).toBeEnabled();
	});
	it('a failed profile save that lands after an identity change shows no error', async () => {
		const held = deferred();
		state.update.mockReturnValueOnce(held.promise);
		await ready();
		await fireEvent.click(button('Save Changes'));
		await bump();
		held.reject(new Error('Username taken'));
		await flush();
		expect(screen.queryByText('Username taken')).toBeNull();
	});
	it('a profile save the old identity left late does not clear the new identity\'s own Saving state', async () => {
		const a = deferred(); const b = deferred();
		state.update.mockReturnValueOnce(a.promise).mockReturnValueOnce(b.promise);
		await ready();
		await fireEvent.click(button('Save Changes'));
		await bump();
		await fireEvent.click(button('Save Changes'));
		expect(button('Saving...')).toBeDisabled();
		a.resolve({ id: 'u1', name: 'Old', email: 'pat@example.com', username: 'old' });
		await flush();
		expect(button('Saving...')).toBeDisabled();
		b.resolve({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat' });
		await waitFor(() => expect(button('Save Changes')).toBeEnabled());
	});
});

describe('password writers', () => {
	it('a failed set-password email that lands after an identity change shows no error', async () => {
		state.extra = { password_set: false };
		const held = deferred();
		mocked.forgot.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Email me a link to set a password'));
		await bump();
		held.reject(new Error('Send failed for old'));
		await flush();
		expect(screen.queryByText('Send failed for old')).toBeNull();
	});
	it('a set-password email that lands after an identity change writes no message and leaves the button idle', async () => {
		state.extra = { password_set: false };
		const held = deferred();
		mocked.forgot.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Email me a link to set a password'));
		await bump();
		expect(button('Email me a link to set a password')).toBeEnabled();
		held.resolve(undefined);
		await flush();
		expect(text()).not.toContain('We sent a link');
		expect(button('Email me a link to set a password')).toBeEnabled();
	});
	it('a set-password email the old identity left late does not clear the new identity\'s Sending state', async () => {
		state.extra = { password_set: false };
		const a = deferred(); const b = deferred();
		mocked.forgot.mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Email me a link to set a password'));
		await bump();
		await fireEvent.click(button('Email me a link to set a password'));
		expect(button('Sending...')).toBeDisabled();
		a.resolve(undefined);
		await flush();
		expect(button('Sending...')).toBeDisabled();
		b.resolve(undefined);
		await waitFor(() => expect(button('Email me a link to set a password')).toBeEnabled());
	});

	async function fillPasswords() {
		await fireEvent.input(document.getElementById('current-pw')!, { target: { value: 'old-pass-phrase-1' } });
		await fireEvent.input(document.getElementById('new-pw')!, { target: { value: 'Brand-new-phrase-2026!' } });
		await fireEvent.input(document.getElementById('confirm-pw')!, { target: { value: 'Brand-new-phrase-2026!' } });
	}
	it('a password change that lands after an identity change writes no message, and the typed passwords are gone', async () => {
		const held = deferred();
		state.update.mockReturnValueOnce(held.promise);
		await ready();
		await fillPasswords();
		await fireEvent.click(button('Change Password'));
		await bump();
		expect(button('Change Password')).toBeEnabled();
		expect(document.getElementById('current-pw')).toHaveValue('');
		expect(document.getElementById('new-pw')).toHaveValue('');
		held.resolve({});
		await flush();
		expect(screen.queryByText('Password changed successfully.')).toBeNull();
		expect(button('Change Password')).toBeEnabled();
	});
	it('a failed password change that lands after an identity change shows no error', async () => {
		const held = deferred();
		state.update.mockReturnValueOnce(held.promise);
		await ready();
		await fillPasswords();
		await fireEvent.click(button('Change Password'));
		await bump();
		held.reject(new Error('Current password is wrong'));
		await flush();
		expect(screen.queryByText('Current password is wrong')).toBeNull();
	});
	it('a password change the old identity left late does not clear the new identity\'s Changing state', async () => {
		const a = deferred(); const b = deferred();
		state.update.mockReturnValueOnce(a.promise).mockReturnValueOnce(b.promise);
		await ready();
		await fillPasswords();
		await fireEvent.click(button('Change Password'));
		await bump();
		await fillPasswords();
		await fireEvent.click(button('Change Password'));
		expect(button('Changing...')).toBeDisabled();
		a.resolve({});
		await flush();
		expect(button('Changing...')).toBeDisabled();
		b.resolve({});
		await waitFor(() => expect(button('Change Password')).toBeEnabled());
	});
});

const SETUP = { secret: 'SECRET123', url: 'otpauth://totp/Pad:pat?secret=SECRET123' };
describe('2FA writers', () => {
	it('a 2FA setup that lands after an identity change shows no secret and leaves Enable idle', async () => {
		state.extra = { totp_enabled: false };
		const held = deferred();
		mocked.setup.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await bump();
		expect(button('Enable 2FA')).toBeEnabled();
		held.resolve(SETUP);
		await flush();
		expect(text()).not.toContain('SECRET123');
		expect(text()).not.toContain('otpauth://');
		expect(button('Enable 2FA')).toBeEnabled();
		expect(state.toCanvas).not.toHaveBeenCalled();
	});
	it('a failed 2FA setup that lands after an identity change shows no error', async () => {
		state.extra = { totp_enabled: false };
		const held = deferred();
		mocked.setup.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await bump();
		held.reject(new Error('2FA is unavailable'));
		await flush();
		expect(screen.queryByText('2FA is unavailable')).toBeNull();
	});
	/**
	 * Click Enable 2FA and hold the page's QR phase at its 50ms wait. The page does `await import('qrcode')`
	 * and then `setTimeout(r, 50)`; this intercepts that one timer and keeps its callback. Waiting for the
	 * callback to be captured proves the import is done and the page is parked on the timer, so the test
	 * can change identity (or not) before firing it. Every other timer, including waitFor's, runs for real.
	 */
	async function toHeldQrTimer() {
		state.extra = { totp_enabled: false };
		// The reset would hide the canvas; give the render a canvas so only the fence can stop it.
		const canvas = document.createElement('canvas');
		const real = document.getElementById.bind(document);
		vi.spyOn(document, 'getElementById').mockImplementation((id) => id === 'qr-canvas' ? canvas : real(id));
		const held: Array<() => void> = [];
		vi.spyOn(globalThis, 'setTimeout').mockImplementation(((fn: () => void, ms?: number, ...rest: unknown[]) => {
			if (ms === 50) { held.push(fn); return 0; }
			return realSetTimeout(fn, ms, ...rest);
		}) as typeof setTimeout);
		mocked.setup.mockResolvedValueOnce(SETUP as never);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await waitFor(() => expect(held.length).toBe(1));
		expect(state.toCanvas).not.toHaveBeenCalled();
		return held[0];
	}
	it('a QR render that wakes after an identity change does not draw the old secret', async () => {
		const wake = await toHeldQrTimer();
		changeIdentity();
		wake();
		await flush();
		expect(state.toCanvas).not.toHaveBeenCalled();
	});
	it('control: the same held-timer path with no identity change draws once', async () => {
		const wake = await toHeldQrTimer();
		wake();
		await waitFor(() => expect(state.toCanvas).toHaveBeenCalledOnce());
		expect(state.toCanvas.mock.calls[0][1]).toBe(SETUP.url);
	});
	it('a QR render with no identity change still draws', async () => {
		state.extra = { totp_enabled: false };
		mocked.setup.mockResolvedValueOnce(SETUP as never);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await waitFor(() => expect(state.toCanvas).toHaveBeenCalledOnce());
		expect(state.toCanvas.mock.calls[0][1]).toBe(SETUP.url);
	});
	it('a 2FA setup the old identity left late does not clear the new identity\'s Setting up state', async () => {
		state.extra = { totp_enabled: false };
		const a = deferred(); const b = deferred();
		mocked.setup.mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await bump();
		await fireEvent.click(button('Enable 2FA'));
		expect(button('Setting up...')).toBeDisabled();
		a.resolve(SETUP);
		await flush();
		expect(button('Setting up...')).toBeDisabled();
		expect(text()).not.toContain('SECRET123');
		b.resolve({ secret: 'OTHER456', url: 'otpauth://totp/B?secret=OTHER456' });
		await screen.findByText('OTHER456');
	});

	/** Walk to the verify step of a 2FA setup, with the code typed. */
	async function toVerifyStep() {
		state.extra = { totp_enabled: false };
		mocked.setup.mockResolvedValueOnce(SETUP as never);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await screen.findByText('SECRET123');
		await fireEvent.input(document.getElementById('totp-verify')!, { target: { value: '123456' } });
	}
	it('a verify that lands after an identity change shows no recovery codes', async () => {
		const held = deferred();
		mocked.verify.mockReturnValueOnce(held.promise as Promise<never>);
		await toVerifyStep();
		await fireEvent.click(button('Verify & Enable'));
		await bump();
		expect(button('Enable 2FA')).toBeEnabled();
		held.resolve({ recovery_codes: ['RC-1', 'RC-2'] });
		await flush();
		expect(text()).not.toContain('RC-1');
		expect(button('Enable 2FA')).toBeEnabled();
	});
	it('a failed verify that lands after an identity change shows no error', async () => {
		const held = deferred();
		mocked.verify.mockReturnValueOnce(held.promise as Promise<never>);
		await toVerifyStep();
		await fireEvent.click(button('Verify & Enable'));
		await bump();
		held.reject(new Error('Invalid code from old'));
		await flush();
		expect(screen.queryByText('Invalid code from old')).toBeNull();
	});
	it('a verify whose profile refresh lands after an identity change does not replace the profile', async () => {
		mocked.verify.mockResolvedValueOnce({ recovery_codes: ['RC-1'] } as never);
		await toVerifyStep();
		const stale = deferred();
		state.meQueue = [stale.promise];
		await fireEvent.click(button('Verify & Enable'));
		await waitFor(() => expect(state.meQueue.length).toBe(0));
		await bump();
		stale.resolve(profileOf('Old', { totp_enabled: true }));
		await flush();
		expect(text()).not.toContain('RC-1');
		expect(screen.queryByText('Enabled')).toBeNull();
	});
	it('a verify the old identity left late does not clear the new identity\'s Verifying state', async () => {
		const a = deferred(); const b = deferred();
		mocked.verify.mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
		await toVerifyStep();
		await fireEvent.click(button('Verify & Enable'));
		await bump();
		mocked.setup.mockResolvedValueOnce(SETUP as never);
		await fireEvent.click(button('Enable 2FA'));
		await screen.findByText('SECRET123');
		await fireEvent.input(document.getElementById('totp-verify')!, { target: { value: '654321' } });
		await fireEvent.click(button('Verify & Enable'));
		expect(button('Verifying...')).toBeDisabled();
		a.resolve({ recovery_codes: ['RC-1'] });
		await flush();
		expect(button('Verifying...')).toBeDisabled();
		expect(text()).not.toContain('RC-1');
		b.resolve({ recovery_codes: ['RC-B'] });
		await screen.findByText('RC-B');
	});

	async function toDisableConfirm() {
		await ready();
		await fireEvent.click(button('Disable 2FA'));
		await fireEvent.input(screen.getByPlaceholderText('Current password'), { target: { value: 'pw' } });
	}
	it('a disable that lands after an identity change writes no message', async () => {
		const held = deferred();
		mocked.disable.mockReturnValueOnce(held.promise as Promise<never>);
		await toDisableConfirm();
		await fireEvent.click(button('Confirm Disable'));
		await bump();
		expect(button('Disable 2FA')).toBeEnabled();
		held.resolve(undefined);
		await flush();
		expect(screen.queryByText('Two-factor authentication has been disabled.')).toBeNull();
		expect(button('Disable 2FA')).toBeEnabled();
	});
	it('a failed disable that lands after an identity change shows no error', async () => {
		const held = deferred();
		mocked.disable.mockReturnValueOnce(held.promise as Promise<never>);
		await toDisableConfirm();
		await fireEvent.click(button('Confirm Disable'));
		await bump();
		held.reject(new Error('Wrong password for old'));
		await flush();
		expect(screen.queryByText('Wrong password for old')).toBeNull();
	});
	it('a disable whose profile refresh lands after an identity change does not replace the profile', async () => {
		mocked.disable.mockResolvedValueOnce(undefined as never);
		await toDisableConfirm();
		const stale = deferred();
		state.meQueue = [stale.promise];
		await fireEvent.click(button('Confirm Disable'));
		await waitFor(() => expect(state.meQueue.length).toBe(0));
		await bump();
		stale.resolve(profileOf('Old', { totp_enabled: false }));
		await flush();
		expect(screen.getByText('Enabled')).toBeInTheDocument();
		expect(screen.queryByText('Two-factor authentication has been disabled.')).toBeNull();
	});
	it('a disable the old identity left late does not clear the new identity\'s Disabling state', async () => {
		const a = deferred(); const b = deferred();
		mocked.disable.mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
		await toDisableConfirm();
		await fireEvent.click(button('Confirm Disable'));
		await bump();
		await toDisableConfirmAgain();
		await fireEvent.click(button('Confirm Disable'));
		expect(button('Disabling...')).toBeDisabled();
		a.resolve(undefined);
		await flush();
		expect(button('Disabling...')).toBeDisabled();
		b.resolve(undefined);
		await screen.findByText('Two-factor authentication has been disabled.');
	});
	async function toDisableConfirmAgain() {
		await fireEvent.click(button('Disable 2FA'));
		await fireEvent.input(screen.getByPlaceholderText('Current password'), { target: { value: 'pw' } });
	}
});

describe('token and danger-zone writers', () => {
	const tokenOf = { id: 't9', name: 'ci', prefix: 'pad_xx', created_at: '2026-10-01T00:00:00Z', token: 'pad_SECRETTOKEN' };
	async function nameToken(name = 'ci') {
		await fireEvent.input(screen.getByLabelText('Token name'), { target: { value: name } });
	}
	it('a token create that lands after an identity change shows no secret and adds no row', async () => {
		const held = deferred();
		mocked.create.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await nameToken();
		await fireEvent.click(button('Create'));
		await bump();
		held.resolve(tokenOf);
		await flush();
		expect(text()).not.toContain('pad_SECRETTOKEN');
		expect(text()).not.toContain('pad_xx...');
		await nameToken('again');
		expect(button('Create')).toBeEnabled();
	});
	it('a plan-limit refusal that lands after an identity change raises no toast and no error', async () => {
		state.planLimit = true;
		const held = deferred();
		mocked.create.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await nameToken();
		await fireEvent.click(button('Create'));
		await bump();
		held.reject(new Error('Token limit reached'));
		await flush();
		expect(state.planToast).not.toHaveBeenCalled();
		expect(screen.queryByText('Token limit reached')).toBeNull();
	});
	it('a failed token create that lands after an identity change shows no error', async () => {
		const held = deferred();
		mocked.create.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await nameToken();
		await fireEvent.click(button('Create'));
		await bump();
		held.reject(new Error('Create failed for old'));
		await flush();
		expect(screen.queryByText('Create failed for old')).toBeNull();
	});
	it('a token create the old identity left late does not clear the new identity\'s Creating state', async () => {
		const a = deferred(); const b = deferred();
		mocked.create.mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
		await ready();
		await nameToken();
		await fireEvent.click(button('Create'));
		await bump();
		await nameToken('b-key');
		await fireEvent.click(button('Create'));
		expect(button('Creating...')).toBeDisabled();
		a.resolve(tokenOf);
		await flush();
		expect(button('Creating...')).toBeDisabled();
		expect(text()).not.toContain('pad_SECRETTOKEN');
		b.resolve({ ...tokenOf, id: 't10', name: 'b-key', token: 'pad_BSECRET' });
		await screen.findByText('pad_BSECRET');
	});

	const rowToken = { id: 't9', name: 'ci', prefix: 'pad_xx', created_at: '2026-10-01T00:00:00Z' };
	describe('token delete and rotate settles', () => {
		beforeEach(() => { vi.mocked(api.auth.tokens.list).mockResolvedValue([rowToken] as never); });
		afterEach(() => { vi.mocked(api.auth.tokens.list).mockResolvedValue([]); });
		it('a token delete the old identity left late does not clear the new identity\'s Deleting state', async () => {
			const a = deferred(); const b = deferred();
			vi.mocked(api.auth.tokens.delete).mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
			await ready();
			await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
			await fireEvent.click(button('Delete token'));
			await bump();
			await fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
			await fireEvent.click(button('Delete token'));
			expect(button('Deleting…')).toBeDisabled();
			a.resolve(undefined);
			await flush();
			expect(button('Deleting…')).toBeDisabled();
			b.resolve(undefined);
			await waitFor(() => expect(screen.queryByRole('button', { name: 'Deleting…' })).toBeNull());
		});
		it('a token rotate the old identity left late does not clear the new identity\'s Rotating state', async () => {
			const a = deferred(); const b = deferred();
			vi.mocked(api.auth.tokens.rotate).mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
			await ready();
			await fireEvent.click(await screen.findByRole('button', { name: 'Rotate' }));
			await fireEvent.click(button('Rotate token'));
			await bump();
			await fireEvent.click(await screen.findByRole('button', { name: 'Rotate' }));
			await fireEvent.click(button('Rotate token'));
			expect(button('Rotating…')).toBeDisabled();
			a.resolve({ ...rowToken, token: 'pad_AOLD' });
			await flush();
			expect(button('Rotating…')).toBeDisabled();
			expect(text()).not.toContain('pad_AOLD');
			b.resolve({ ...rowToken, token: 'pad_BNEW' });
			await screen.findByText('pad_BNEW');
		});
	});

	async function toDeleteConfirm() {
		await fireEvent.click(button('Delete my account'));
		await fireEvent.input(document.getElementById('delete-password')!, { target: { value: 'pw' } });
		await fireEvent.input(document.getElementById('delete-totp')!, { target: { value: '123456' } });
	}
	it('a failed account delete that lands after an identity change shows no error and leaves the form usable', async () => {
		const held = deferred();
		mocked.del.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await toDeleteConfirm();
		await fireEvent.click(button('Permanently delete my account'));
		await bump();
		held.reject(new Error('Your account was NOT deleted'));
		await flush();
		expect(screen.queryByText('Your account was NOT deleted')).toBeNull();
		await fireEvent.click(button('Delete my account'));
		expect(button('Permanently delete my account')).toBeEnabled();
	});
	it('a failed account delete the old identity left late does not clear the new identity\'s Deleting state or show its error', async () => {
		const a = deferred(); const b = deferred();
		mocked.del.mockReturnValueOnce(a.promise as Promise<never>).mockReturnValueOnce(b.promise as Promise<never>);
		await ready();
		await toDeleteConfirm();
		await fireEvent.click(button('Permanently delete my account'));
		await bump();
		await toDeleteConfirm();
		await fireEvent.click(button('Permanently delete my account'));
		expect(button('Deleting...')).toBeDisabled();
		a.reject(new Error('Old delete failed'));
		await flush();
		expect(button('Deleting...')).toBeDisabled();
		expect(screen.queryByText('Old delete failed')).toBeNull();
	});
	it('an account delete that succeeds after an identity change still signs out (the account is gone server-side)', async () => {
		const held = deferred();
		mocked.del.mockReturnValueOnce(held.promise as Promise<never>);
		await ready();
		await toDeleteConfirm();
		await fireEvent.click(button('Permanently delete my account'));
		await bump();
		const loc = { href: '/console/settings' };
		vi.stubGlobal('location', loc);
		try {
			held.resolve(undefined);
			await waitFor(() => expect(state.clear).toHaveBeenCalledOnce());
			expect(loc.href).toBe('/login');
		} finally {
			vi.unstubAllGlobals();
		}
	});

	it('an export that lands after an identity change writes no message and leaves Export idle', async () => {
		const held = deferred();
		state.exportData.mockReturnValueOnce(held.promise);
		await ready();
		await fireEvent.click(button('Export my data'));
		await bump();
		expect(button('Export my data')).toBeEnabled();
		held.resolve(undefined);
		await flush();
		expect(screen.queryByText('Your data export has downloaded.')).toBeNull();
		expect(button('Export my data')).toBeEnabled();
	});
	it('a failed export that lands after an identity change shows no error', async () => {
		const held = deferred();
		state.exportData.mockReturnValueOnce(held.promise);
		await ready();
		await fireEvent.click(button('Export my data'));
		await bump();
		held.reject(new Error('Export failed for old'));
		await flush();
		expect(screen.queryByText('Export failed for old')).toBeNull();
	});
	it('an export the old identity left late does not clear the new identity\'s Exporting state', async () => {
		const a = deferred(); const b = deferred();
		state.exportData.mockReturnValueOnce(a.promise).mockReturnValueOnce(b.promise);
		await ready();
		await fireEvent.click(button('Export my data'));
		await bump();
		await fireEvent.click(button('Export my data'));
		expect(button('Exporting...')).toBeDisabled();
		a.resolve(undefined);
		await flush();
		expect(button('Exporting...')).toBeDisabled();
		b.resolve(undefined);
		await screen.findByText('Your data export has downloaded.');
	});
});

describe('per-identity form state reset', () => {
	it('a profile message does not survive an identity change', async () => {
		state.update.mockResolvedValueOnce({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat' });
		await ready();
		await fireEvent.click(button('Save Changes'));
		await screen.findByText('Profile updated.');
		await bump();
		expect(screen.queryByText('Profile updated.')).toBeNull();
	});
	it('a profile error does not survive an identity change', async () => {
		state.update.mockRejectedValueOnce(new Error('Username taken'));
		await ready();
		await fireEvent.click(button('Save Changes'));
		await screen.findByText('Username taken');
		await bump();
		expect(screen.queryByText('Username taken')).toBeNull();
	});
	it('a revealed token secret does not survive an identity change', async () => {
		mocked.create.mockResolvedValueOnce({ id: 't9', name: 'ci', prefix: 'pad_xx', created_at: '2026-10-01T00:00:00Z', token: 'pad_SECRETTOKEN' } as never);
		await ready();
		await fireEvent.input(screen.getByLabelText('Token name'), { target: { value: 'ci' } });
		await fireEvent.click(button('Create'));
		await screen.findByText('pad_SECRETTOKEN');
		await bump();
		expect(text()).not.toContain('pad_SECRETTOKEN');
	});
	it('shown recovery codes and a typed 2FA code do not survive an identity change', async () => {
		state.extra = { totp_enabled: false };
		mocked.setup.mockResolvedValueOnce(SETUP as never);
		mocked.verify.mockResolvedValueOnce({ recovery_codes: ['RC-1'] } as never);
		await ready();
		await fireEvent.click(button('Enable 2FA'));
		await screen.findByText('SECRET123');
		await fireEvent.input(document.getElementById('totp-verify')!, { target: { value: '123456' } });
		await fireEvent.click(button('Verify & Enable'));
		await screen.findByText('RC-1');
		await bump();
		expect(text()).not.toContain('RC-1');
		expect(button('Enable 2FA')).toBeEnabled();
	});
	it('a typed delete password and the open confirm do not survive an identity change', async () => {
		await ready();
		await fireEvent.click(button('Delete my account'));
		await fireEvent.input(document.getElementById('delete-password')!, { target: { value: 'hunter2' } });
		await bump();
		expect(document.getElementById('delete-password')).toBeNull();
	});
	it('an export message does not survive an identity change', async () => {
		state.exportData.mockResolvedValueOnce(undefined);
		await ready();
		await fireEvent.click(button('Export my data'));
		await screen.findByText('Your data export has downloaded.');
		await bump();
		expect(screen.queryByText('Your data export has downloaded.')).toBeNull();
	});
	it('an OAuth link banner from the page load survives the first sign-in (previous user none)', async () => {
		state.cloud = true;
		window.history.replaceState({}, '', '/console/settings?linked=github');
		render(AccountSettings);
		await screen.findByText('GitHub linked.');
		changeIdentity('');
		await flush();
		expect(screen.getByText('GitHub linked.')).toBeInTheDocument();
	});
	it('an OAuth link banner does not survive a change from one signed-in user to another', async () => {
		state.cloud = true;
		window.history.replaceState({}, '', '/console/settings?linked=github');
		render(AccountSettings);
		await screen.findByText('GitHub linked.');
		changeIdentity('u1');
		await flush();
		expect(screen.queryByText('GitHub linked.')).toBeNull();
	});
});
