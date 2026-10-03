import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';

/**
 * TASK-3376: system collections (Conventions, Playbooks) are ordinary
 * collections for a restricted member. In the member access picker they are
 * real checkboxes, not a disabled always-checked row. They start UNCHECKED
 * when an owner switches a member from All to Specific (TASK-3384, Dave's
 * ruling), like every other collection, and a hint names what leaving them
 * unchecked means: that member's agents won't load the workspace's
 * conventions or playbooks. A member who is ALREADY restricted keeps their
 * saved list as is.
 */

const meCalls: Array<(value: unknown) => void> = [];
const saves: Array<{ mode: string; ids: string[] }> = [];
let memberAccess: { collection_access: string; collection_ids: string[] } = { collection_access: 'all', collection_ids: [] };

const COLLECTIONS = [
	{ id: 'c-tasks', slug: 'tasks', name: 'Tasks', icon: 'T', is_system: false, is_default: true, sort_order: 0 },
	{ id: 'c-conv', slug: 'conventions', name: 'Conventions', icon: 'C', is_system: true, is_default: true, sort_order: 1 },
	{ id: 'c-pb', slug: 'playbooks', name: 'Playbooks', icon: 'P', is_system: true, is_default: true, sort_order: 2 },
];

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: () => 'toast-id', dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false,
}));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
			me: vi.fn(() => new Promise((resolve) => { meCalls.push(resolve); })),
			list: vi.fn(async () => []),
			update: vi.fn(async () => ({})),
			delete: vi.fn(async () => ({})),
			restore: vi.fn(async () => ({ slug: 'ws', name: 'WS' })),
		},
		collections: { list: vi.fn(async () => COLLECTIONS) },
		members: {
			list: vi.fn(async () => ({
				members: [{ user_id: 'u2', user_name: 'Bob', user_email: 'bob@example.com', role: 'editor' }],
				invitations: [],
			})),
			getMemberCollectionAccess: vi.fn(async () => memberAccess),
			setMemberCollectionAccess: vi.fn(async (_ws: string, _u: string, mode: string, ids: string[]) => {
				saves.push({ mode, ids: [...ids] });
				return { collection_access: mode, collection_ids: ids };
			}),
			remove: vi.fn(async () => ({})),
		},
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

vi.mock('$app/navigation', () => ({ goto: () => Promise.resolve() }));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		clear() {},
	},
}));

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };

function checkbox(name: string): HTMLInputElement {
	const label = screen.getByText(name, { selector: '.access-coll-name' }).closest('label');
	expect(label, `no access row for ${name}`).not.toBeNull();
	return label!.querySelector('input[type="checkbox"]') as HTMLInputElement;
}

function setAccessMode(next: 'all' | 'specific') {
	const select = document.querySelector('select[id^="access-mode-"]') as HTMLSelectElement;
	expect(select, 'the access panel is not open').not.toBeNull();
	select.value = next;
	select.dispatchEvent(new Event('change', { bubbles: true }));
}

async function openAccessPanel() {
	render(SettingsPage);
	await waitFor(() => expect(meCalls.length).toBeGreaterThan(0));
	meCalls[0]!(OWNER);
	await waitFor(() => expect(screen.getByRole('tab', { name: /Members/ })).toBeTruthy());
	screen.getByRole('tab', { name: /Members/ }).click();
	await waitFor(() => expect(screen.getByText('Bob')).toBeTruthy());
	screen.getByRole('button', { name: /Manage access/ }).click();
	await waitFor(() => expect(document.querySelector('select[id^="access-mode-"]')).not.toBeNull());
}

describe('TASK-3376: system collections in the member access picker', () => {
	beforeEach(() => {
		meCalls.length = 0;
		saves.length = 0;
		memberAccess = { collection_access: 'all', collection_ids: [] };
		page.params = { username: 'dave', workspace: 'ws' };
		window.location.hash = '';
	});

	afterEach(() => {
		cleanup();
		window.location.hash = '';
	});

	it('All to Specific starts with nothing checked, the system collections included, and the hint names the consequence', async () => {
		await openAccessPanel();
		setAccessMode('specific');
		await waitFor(() => expect(checkbox('Conventions')).toBeTruthy());
		expect(checkbox('Conventions').checked).toBe(false);
		expect(checkbox('Playbooks').checked).toBe(false);
		expect(checkbox('Tasks').checked).toBe(false);
		// Real choices, not the old disabled always-checked row.
		expect(checkbox('Conventions').disabled).toBe(false);
		expect(checkbox('Playbooks').disabled).toBe(false);
		const hint = document.querySelector('.access-coll-hint');
		expect(hint?.textContent).toMatch(/agents won.t load .*conventions.*playbooks/i);

		// Checking one is an ordinary choice, saved like any collection.
		checkbox('Playbooks').click();
		await waitFor(() => expect(checkbox('Playbooks').checked).toBe(true));
		checkbox('Tasks').click();
		await waitFor(() => expect(checkbox('Tasks').checked).toBe(true));
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(saves.length).toBe(1));
		expect([...saves[0].ids].sort()).toEqual(['c-pb', 'c-tasks']);
		expect(saves[0].mode).toBe('specific');
	});

	it('an already-restricted member keeps their saved list; nothing is added for them', async () => {
		memberAccess = { collection_access: 'specific', collection_ids: ['c-tasks'] };
		await openAccessPanel();
		await waitFor(() => expect(checkbox('Tasks').checked).toBe(true));
		expect(checkbox('Conventions').checked).toBe(false);
		expect(checkbox('Playbooks').checked).toBe(false);
		screen.getByRole('button', { name: /^Save$/ }).click();
		await waitFor(() => expect(saves.length).toBe(1));
		expect(saves[0]).toEqual({ mode: 'specific', ids: ['c-tasks'] });
	});
});
