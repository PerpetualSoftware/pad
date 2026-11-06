import { describe, expect, it, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';

/**
 * TASK-4315: a shallow navigation (Chat opening over the page) must not ask the
 * playbook editor's discard question. The editor stays mounted underneath, so
 * there is nothing to discard. A real navigation still asks (control case).
 */
type Nav = { type: string; shallow: boolean; cancel: () => void };
const nav = vi.hoisted(() => ({ guard: null as null | ((n: Nav) => void) }));
vi.mock('$app/navigation', () => ({
	beforeNavigate: (fn: (n: Nav) => void) => {
		nav.guard = fn;
	},
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

const PLAYBOOK = {
	id: 'p1',
	slug: 'ship-it',
	title: 'Ship it',
	content: 'body',
	collection_slug: 'playbooks',
	collection_prefix: 'PLAYB',
	item_number: 7,
	seq: 3,
	fields: JSON.stringify({ status: 'active', trigger: 'manual', scope: 'all', invocation_slug: 'ship' })
};
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: {
		items: {
			get: vi.fn(async () => PLAYBOOK),
			listAllByCollection: vi.fn(async () => []),
			update: vi.fn(async () => PLAYBOOK)
		},
		collections: {
			list: vi.fn(async () => []),
			get: vi.fn(async () => ({ id: 'c', slug: 'playbooks', schema: '{"fields":[]}' }))
		}
	}
}));

import { page } from '$app/state';
import PlaybookEditor from '../../routes/[username]/[workspace]/playbooks/[slug]/+page.svelte';

beforeEach(() => {
	nav.guard = null;
	page.params = { username: 'dave', workspace: 'ws', slug: 'PLAYB-7' };
	page.url = new URL('http://localhost/dave/ws/playbooks/PLAYB-7');
});
afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

async function openAndEdit() {
	render(PlaybookEditor);
	const title = (await screen.findByPlaceholderText('Playbook title')) as HTMLInputElement;
	title.value = 'Ship it, edited';
	title.dispatchEvent(new Event('input', { bubbles: true }));
}

function go(over: Partial<Nav>) {
	let cancelled = false;
	nav.guard!({ type: 'goto', shallow: false, cancel: () => (cancelled = true), ...over });
	return cancelled;
}

describe('TASK-4315: the playbook discard guard and shallow navigation', () => {
	it('a shallow navigation neither prompts nor cancels with unsaved changes', async () => {
		await openAndEdit();
		const ask = vi.spyOn(window, 'confirm').mockReturnValue(false);
		expect(go({ shallow: true })).toBe(false);
		expect(ask).not.toHaveBeenCalled();
	});

	it('control: a real navigation with the same unsaved changes still asks and cancels on No', async () => {
		await openAndEdit();
		const ask = vi.spyOn(window, 'confirm').mockReturnValue(false);
		expect(go({ shallow: false })).toBe(true);
		expect(ask).toHaveBeenCalledTimes(1);
	});
});
