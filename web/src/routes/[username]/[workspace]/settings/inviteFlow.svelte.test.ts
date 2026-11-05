import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
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
// A reactive page, so a workspace switch reaches the component's derived workspace slug.
vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));
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
		const failed = await screen.findByText(`${CREATED} ${COPY_FAILED}`);
		// A failed copy is an error state, never styled as success.
		expect(failed.classList.contains('invite-error')).toBe(true);
		expect(failed.classList.contains('invite-success')).toBe(false);
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

	it('reports copy success only after the clipboard write completes, and does not hold the members refresh behind it', async () => {
		const copy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(copy.promise);
		await mount();
		await invite();
		await waitFor(() => expect(copyToClipboard).toHaveBeenCalledWith(URL));
		await screen.findByText(CREATED);
		linkField();
		expect((screen.getByRole('textbox', { name: 'Email address to invite' }) as HTMLInputElement).value).toBe('');
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		expect(screen.queryByText(/Link copied/)).toBeNull();
		copy.resolve(true);
		await screen.findByText(`${CREATED} Link copied`);
		expect(api.members.list).toHaveBeenCalledTimes(2);
	});

	it('clears the link and discards clipboard completion after an identity change', async () => {
		const copy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(copy.promise);
		await mount();
		await invite();
		await waitFor(() => expect(copyToClipboard).toHaveBeenCalledWith(URL));
		linkField();
		// The members refresh already ran (it does not wait for the clipboard); the identity change adds the page's own reload.
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		changeIdentity();
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(3));
		await screen.findByRole('textbox', { name: 'Email address to invite' });
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
		copy.resolve(true);
		await copy.promise;
		await tick();
		expect(document.querySelector('.invite-result')).toBeNull();
		expect(api.members.list).toHaveBeenCalledTimes(3);
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

	it('puts the result line in a live region that exists before the result does', async () => {
		await mount();
		const region = document.querySelector('.invite-live');
		expect(region).not.toBeNull();
		expect(region!.getAttribute('role')).toBe('status');
		expect(region!.getAttribute('aria-live')).toBe('polite');
		expect(region!.textContent?.trim()).toBe('');
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		expect(document.querySelector('.invite-live')).toBe(region);
		expect(region!.textContent).toContain(`${CREATED} Link copied`);
	});

	it('drops an invite response that lands after the page moved to another workspace', async () => {
		const slow = deferred<{ email: string; role: string; join_url: string }>();
		vi.mocked(api.members.invite).mockReturnValueOnce(slow.promise as never);
		await mount();
		await invite();
		page.params = { username: 'dave', workspace: 'other' };
		flushSync();
		await tick();
		// Non-vacuity: the page really moved to the other workspace before the invite answered.
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.some(([ws]) => ws === 'other')).toBe(true));
		slow.resolve({ email: EMAIL, role: 'editor', join_url: URL });
		for (let i = 0; i < 10; i++) { await Promise.resolve(); await tick(); }
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
		expect(screen.queryByText(new RegExp(`Invitation created for ${EMAIL}`))).toBeNull();
		expect(copyToClipboard).not.toHaveBeenCalled();
		expect(toastStore.show).not.toHaveBeenCalled();
	});

	it('keeps the invite outcome and the link when only the members reload fails', async () => {
		await mount();
		vi.mocked(api.members.list).mockRejectedValueOnce(new Error('network blip'));
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		linkField();
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		expect(document.body.textContent).not.toContain('network blip');
		expect(document.body.textContent).not.toContain('Failed to invite');
		expect(api.members.invite).toHaveBeenCalledTimes(1);
	});

	it('reloads the members list without waiting for a clipboard write that has not settled', async () => {
		const pendingCopy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(pendingCopy.promise);
		await mount();
		await invite();
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		linkField();
		pendingCopy.resolve(true);
		await screen.findByText(`${CREATED} Link copied`);
	});

	it('writes no copy result when the page moved to another workspace during the clipboard write', async () => {
		const pendingCopy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(pendingCopy.promise);
		await mount();
		await invite();
		await waitFor(() => expect(api.members.list).toHaveBeenCalledTimes(2));
		page.params = { username: 'dave', workspace: 'other' };
		flushSync();
		await tick();
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.some(([ws]) => ws === 'other')).toBe(true));
		pendingCopy.resolve(true);
		for (let i = 0; i < 10; i++) { await Promise.resolve(); await tick(); }
		expect(screen.queryByText(/Link copied/)).toBeNull();
		expect(screen.queryByRole('textbox', { name: 'Invitation link' })).toBeNull();
	});

	it('shows no copy toast when the page moved to another workspace during Copy invitation link', async () => {
		vi.mocked(copyToClipboard).mockResolvedValueOnce(false);
		await mount();
		await invite();
		await screen.findByText(`${CREATED} ${COPY_FAILED}`);
		const pendingCopy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(pendingCopy.promise);
		await fireEvent.click(screen.getByRole('button', { name: 'Copy invitation link' }));
		page.params = { username: 'dave', workspace: 'other' };
		flushSync();
		await tick();
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.some(([ws]) => ws === 'other')).toBe(true));
		vi.mocked(toastStore.show).mockClear();
		pendingCopy.resolve(true);
		for (let i = 0; i < 10; i++) { await Promise.resolve(); await tick(); }
		expect(toastStore.show).not.toHaveBeenCalled();
	});

	it('a cancellation that finishes after a workspace switch does not clear the other workspace link', async () => {
		const cancel = deferred<Record<string, never>>();
		vi.mocked(api.members.cancelInvitation).mockReturnValueOnce(cancel.promise as never);
		await mount();
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		await waitFor(() => expect(screen.getByText('Pending acceptance')).toBeInTheDocument());
		await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
		await waitFor(() => expect(api.members.cancelInvitation).toHaveBeenCalledTimes(1));
		page.params = { username: 'dave', workspace: 'other' };
		flushSync();
		await tick();
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.some(([ws]) => ws === 'other')).toBe(true));
		await invite();
		await screen.findByText(`${CREATED} Link copied`);
		linkField();
		cancel.resolve({});
		for (let i = 0; i < 10; i++) { await Promise.resolve(); await tick(); }
		linkField();
	});

	it('shows no copy toast from a pending-row Copy invite link after the page moved to another workspace', async () => {
		vi.mocked(api.members.list).mockResolvedValue({
			members: [],
			invitations: [{ id: 'inv2', email: 'b@x.test', role: 'editor', code: 'CODE1', created_at: '2026-11-05T00:00:00Z' }]
		});
		await mount();
		const pendingCopy = deferred<boolean>();
		vi.mocked(copyToClipboard).mockReturnValueOnce(pendingCopy.promise);
		await fireEvent.click(await screen.findByRole('button', { name: 'Copy invite link' }));
		page.params = { username: 'dave', workspace: 'other' };
		flushSync();
		await tick();
		await waitFor(() => expect(vi.mocked(api.members.list).mock.calls.some(([ws]) => ws === 'other')).toBe(true));
		vi.mocked(toastStore.show).mockClear();
		pendingCopy.resolve(true);
		for (let i = 0; i < 10; i++) { await Promise.resolve(); await tick(); }
		expect(toastStore.show).not.toHaveBeenCalled();
	});
});
