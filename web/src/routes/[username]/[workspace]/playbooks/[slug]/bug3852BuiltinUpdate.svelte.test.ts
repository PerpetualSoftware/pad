import { describe, expect, it, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';

/**
 * BUG-3852: the Library's "Update available" link for a built-in playbook
 * opens this editor, so the review and Accept live here too. Accept refuses
 * while the form holds unsaved edits, the form is locked while an accept is in
 * flight (the dialog can be closed mid-request, which hands the page back),
 * and an accepted update reloads the form, so a later Save cannot send the
 * old body back. The e2e test (bug-3852-playbook-builtin-update.spec.ts) walks
 * the same path against the real server.
 */
vi.mock('$app/navigation', () => ({
	beforeNavigate: () => {},
	goto: () => Promise.resolve(),
	afterNavigate: () => {},
	invalidate: () => Promise.resolve(),
	invalidateAll: () => Promise.resolve()
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; }
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => true } }));
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: () => 'id', dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false
}));

const FIELDS = JSON.stringify({ status: 'active', trigger: 'manual', scope: 'all', invocation_slug: 'plan' });
const state = vi.hoisted(() => ({
	item: null as null | Record<string, unknown>,
	release: null as null | (() => void),
	updates: 0
}));
const OLD = { id: 'p1', slug: 'plan', title: 'Plan', content: 'old body', collection_slug: 'playbooks', collection_prefix: 'PLAYB', item_number: 5, seq: 3, fields: '' };
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: {
		items: {
			get: vi.fn(async () => state.item),
			listAllByCollection: vi.fn(async () => []),
			update: vi.fn(async () => state.item)
		},
		collections: {
			list: vi.fn(async () => []),
			get: vi.fn(async () => ({ id: 'c', slug: 'playbooks', schema: '{"fields":[]}' }))
		},
		builtins: {
			get: vi.fn(async () =>
				(state.item as { content: string }).content === 'old body'
					? {
							key: 'plan',
							kind: 'playbook',
							state: 'update_available',
							seq: 3,
							library: { content: 'library body', fields: {} },
							current: { content: 'old body', fields: {} }
						}
					: { key: 'plan', kind: 'playbook', state: 'current', seq: 4 }
			),
			update: vi.fn(
				() =>
					new Promise((resolve) => {
						state.updates++;
						state.release = () => {
							state.item = { ...(state.item as object), content: 'library body', seq: 4 };
							resolve(state.item);
						};
					})
			)
		}
	}
}));

import { page } from '$app/state';
import { api } from '$lib/api/client';
import PlaybookEditor from './+page.svelte';

beforeEach(() => {
	state.item = { ...OLD, fields: FIELDS };
	state.release = null;
	state.updates = 0;
	page.params = { username: 'dave', workspace: 'ws', slug: 'PLAYB-5' };
	page.url = new URL('http://localhost/dave/ws/playbooks/PLAYB-5');
});
afterEach(() => {
	cleanup();
	vi.clearAllMocks();
});

const body = () => document.getElementById('pbe-body') as HTMLTextAreaElement;
const button = (re: RegExp) => screen.getAllByRole('button', { hidden: true }).find((b) => re.test(b.textContent ?? ''))!;

async function openReview() {
	render(PlaybookEditor);
	await screen.findByPlaceholderText('Playbook title');
	await fireEvent.click(await screen.findByRole('button', { name: /Update available/ }));
}

describe('BUG-3852: the playbook editor offers a built-in update', () => {
	it('Accept refuses while the form holds unsaved edits', async () => {
		render(PlaybookEditor);
		await screen.findByPlaceholderText('Playbook title');
		await fireEvent.input(body(), { target: { value: 'my edit' } });
		await fireEvent.click(await screen.findByRole('button', { name: /Update available/ }));
		await fireEvent.click(button(/Accept the library's version/));
		expect(await screen.findByText(/edits that have not reached the server/, {}, { timeout: 2000 })).toBeTruthy();
		expect(api.builtins.update).not.toHaveBeenCalled();
	});

	it('locks the form while an accept is in flight, then reloads the accepted text', async () => {
		await openReview();
		await fireEvent.click(button(/Accept the library's version/));
		await waitFor(() => expect(state.updates).toBe(1));
		// The page behind the dialog is handed back if the dialog is closed;
		// it must not be editable or savable until the update settles.
		expect(body().readOnly).toBe(true);
		expect((screen.getByPlaceholderText('Playbook title') as HTMLInputElement).readOnly).toBe(true);
		expect((button(/^Save$/) as HTMLButtonElement).disabled).toBe(true);
		expect((button(/Save and close/) as HTMLButtonElement).disabled).toBe(true);

		state.release!();
		await waitFor(() => expect(body()?.value).toBe('library body'));
		expect(body().readOnly).toBe(false);
		expect((button(/^Save$/) as HTMLButtonElement).disabled).toBe(false);
		expect(api.items.get).toHaveBeenCalledTimes(2);
	});
});
