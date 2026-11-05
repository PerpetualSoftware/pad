import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { page } from '$app/state';
import { api } from '$lib/api/client';
import { copyToClipboard } from '$lib/utils/clipboard';
import { toastStore } from '$lib/stores/toast.svelte';
import { bumpEpoch, readEpoch, resetEpoch, writeUserId } from '../../../../test/identityEpochMock.svelte';
import SettingsPage from './+page.svelte';

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
			me: vi.fn(async () => ({ role: 'owner', collection_grants: [], item_grants: [] })),
			list: vi.fn(async () => [])
		},
		collections: { list: vi.fn(async () => []) },
		members: { list: vi.fn(), invite: vi.fn(), cancelInvitation: vi.fn(async () => ({})) }
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {}
}));
vi.mock('$lib/utils/clipboard', () => ({ copyToClipboard: vi.fn() }));
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: vi.fn(), dismiss: vi.fn(), toasts: [] },
	quietExternalToasts: () => false
}));
vi.mock('$lib/stores/confirmDialog.svelte', () => ({
	confirmDialog: { request: vi.fn(async () => true) }
}));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/stores/auth.svelte', async () => {
	const { readEpoch, readUserId } = await import('../../../../test/identityEpochMock.svelte');
	return { authStore: {
		get identityEpoch() { return readEpoch(); },
		get userId() { return readUserId(); },
		identityFence() { const epoch = readEpoch(); return () => readEpoch() === epoch; },
		onIdentityChange() { return () => {}; }
	} };
});

const EMAIL = 'a@x.test';
const URL = 'https://pad.test/join/secret-invite';
const CREATED = `Invitation created for ${EMAIL}.`;
const COPY_FAILED = 'Couldn’t copy the invitation link. Copy it below and send it to the invitee.';
const pending = { id: 'inv1', email: EMAIL, role: 'editor', code: '', created_at: '2026-11-05T00:00:00Z' };

function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((res) => { resolve = res; });
	return { promise, resolve };
}

beforeEach(() => {
	vi.clearAllMocks();
	resetEpoch();
	writeUserId('u1');
	page.params = { username: 'dave', workspace: 'ws' };
	window.location.hash = '#members';
	vi.mocked(api.members.list).mockResolvedValue({ members: [], invitations: [pending] });
	vi.mocked(api.members.invite).mockResolvedValue({ email: EMAIL, role: 'editor', join_url: URL });
	vi.mocked(copyToClipboard).mockReset().mockResolvedValue(true);
});
afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	window.location.hash = '';
});

async function mount() {
	render(SettingsPage);
	await screen.findByRole('textbox', { name: 'Email address to invite' });
}
async function invite(email = EMAIL) {
	await fireEvent.input(screen.getByRole('textbox', { name: 'Email address to invite' }), { target: { value: email } });
	await fireEvent.click(screen.getByRole('button', { name: 'Invite', exact: true }));
}
function linkField() {
	const input = screen.getByRole('textbox', { name: 'Invitation link' }) as HTMLInputElement;
	expect(input.readOnly).toBe(true);
	expect(input.value).toBe(URL);
	return input;
}
function changeIdentity() {
	const before = readEpoch();
	bumpEpoch();
	writeUserId('u2');
	expect(readEpoch()).toBeGreaterThan(before);
}

// Each test drives the actual invite form; refreshed rows deliberately have no shareable secret.
describe('BUG-4335: invitation creation and clipboard feedback', () => {
	it('reports a confirmed copy, keeps a selectable link, and never stores the URL', async () => {
		// jsdom's Storage proxy needs a prototype spy to observe both storage areas.
		const storageWrites = vi.spyOn(Storage.prototype, 'setItem');
		localStorage.setItem('invite-test-probe', 'local');
		sessionStorage.setItem('invite-test-probe', 'session');
		expect(storageWrites).toHaveBeenCalledTimes(2);
		localStorage.removeItem('invite-test-probe');
		sessionStorage.removeItem('invite-test-probe');
		storageWrites.mockClear();
		await mount();
		await invite();
		const message = await screen.findByText(`${CREATED} Link copied`);
		expect(message.classList.contains('invite-success')).toBe(true);
		expect(document.body.textContent).not.toMatch(/\bsent\b|via email|email delivery/i);
		const input = linkField();
		input.focus();
		expect(input.selectionStart).toBe(0);
		expect(input.selectionEnd).toBe(URL.length);
		expect(copyToClipboard).toHaveBeenCalledWith(URL);
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		expect(JSON.stringify(storageWrites.mock.calls)).not.toContain(URL);
	});

	it.each(['false', 'reject'] as const)('recovers from clipboard %s without creating another invitation', async (outcome) => {
		if (outcome === 'reject') vi.mocked(copyToClipboard).mockRejectedValueOnce(new Error('clipboard denied'));
		else vi.mocked(copyToClipboard).mockResolvedValueOnce(false);
		await mount();
		await invite();
		await screen.findByText(`${CREATED} ${COPY_FAILED}`);
		linkField();
		expect(document.body.textContent).not.toContain('Link copied');
		await fireEvent.click(screen.getByRole('button', { name: 'Copy invitation link' }));
		await waitFor(() => expect(toastStore.show).toHaveBeenCalledWith('Link copied', 'success'));
		expect(copyToClipboard).toHaveBeenNthCalledWith(2, URL);
		expect(api.members.invite).toHaveBeenCalledTimes(1);
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
	});

	it.each(['false', 'reject'] as const)('reports retry clipboard %s as an error with manual-copy guidance', async (outcome) => {
		await mount();
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		if (outcome === 'reject') vi.mocked(copyToClipboard).mockRejectedValueOnce(new Error('clipboard denied'));
		else vi.mocked(copyToClipboard).mockResolvedValueOnce(false);
		await fireEvent.click(screen.getByRole('button', { name: 'Copy invitation link' }));
		await waitFor(() => expect(toastStore.show).toHaveBeenCalledWith(COPY_FAILED, 'error'));
		linkField();
	});

	it('waits for clipboard completion before reporting copy success or refreshing members', async () => {
		const copy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(copy.promise);
		await mount();
		await invite();
		await waitFor(() => expect(copyToClipboard).toHaveBeenCalledWith(URL));
		await screen.findByText(CREATED);
		linkField();
		expect((screen.getByRole('textbox', { name: 'Email address to invite' }) as HTMLInputElement).value).toBe('');
		expect(api.members.list).toHaveBeenCalledTimes(1);
		copy.resolve(true);
		await screen.findByText(`${CREATED} Link copied`);
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
	});

	it('clears the link and discards clipboard completion after an identity change', async () => {
		const copy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(copy.promise);
		await mount();
		await invite();
		await waitFor(() => expect(copyToClipboard).toHaveBeenCalledWith(URL));
		linkField();
		changeIdentity();
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		await screen.findByRole('textbox', { name: 'Email address to invite' });
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
		copy.resolve(true);
		await copy.promise;
		await tick();
		expect(document.querySelector('.invite-result')).toBeNull();
		expect(api.members.list).toHaveBeenCalledTimes(2);
	});

	it('discards the retry toast if the identity changes while copying', async () => {
		await mount();
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		const copy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(copy.promise);
		await fireEvent.click(screen.getByRole('button', { name: 'Copy invitation link' }));
		expect(copyToClipboard).toHaveBeenCalledTimes(2);
		changeIdentity();
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(3));
		await screen.findByRole('textbox', { name: 'Email address to invite' });
		copy.resolve(true);
		await copy.promise;
		await tick();
		expect(toastStore.show).not.toHaveBeenCalled();
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
	});

	it('labels a hashed pending invitation without claiming email delivery', async () => {
		await mount();
		const label = screen.getByText('Pending acceptance');
		expect(label.classList.contains('inv-pending-label')).toBe(true);
		expect(document.body.textContent).not.toContain('Sent via email');
		expect(screen.queryByRole('button', { name: 'Copy invite link' })).toBeNull();
	});

	it.each([true, false])('reports the per-row clipboard result %s without promising a missing field', async (ok) => {
		vi.mocked(api.members.list).mockResolvedValue({ members: [], invitations: [{ ...pending, code: 'ABC' }] });
		vi.mocked(copyToClipboard).mockResolvedValueOnce(ok);
		await mount();
		await fireEvent.click(screen.getByRole('button', { name: 'Copy invite link' }));
		await waitFor(() => expect(toastStore.show).toHaveBeenCalledWith(
			ok ? 'Link copied' : 'Couldn’t copy the invitation link.', ok ? 'success' : 'error'
		));
		expect(copyToClipboard).toHaveBeenCalledWith(`${window.location.origin}/join/ABC`);
	});

	it.each([['ABC', `${CREATED} Join code: ABC`], [undefined, CREATED]])('handles a response without a URL (code: %s)', async (code, message) => {
		vi.mocked(api.members.invite).mockResolvedValue({ email: EMAIL, role: 'editor', ...(code ? { code } : {}) });
		await mount();
		await invite();
		await screen.findByText(message);
		expect(document.body.textContent).not.toMatch(/\bsent\b/i);
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
		expect(copyToClipboard).not.toHaveBeenCalled();
	});

	it('clears the retained link when its invitation is cancelled', async () => {
		await mount();
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		linkField();
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel', exact: true }));
		await waitFor(() => expect(api.members.cancelInvitation).toHaveBeenCalledWith('ws', pending.id));
		await waitFor(() => expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull());
	});

	it('clears the previous link as soon as another invitation starts', async () => {
		await mount();
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		linkField();
		const next = deferred<Awaited<ReturnType<typeof api.members.invite>>>();
		vi.mocked(api.members.invite).mockReturnValueOnce(next.promise);
		await invite('b@x.test');
		expect(api.members.invite).toHaveBeenCalledTimes(2);
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
		next.resolve({ email: 'b@x.test', role: 'editor' });
		await screen.findByText('Invitation created for b@x.test.');
	});
});
