import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * TASK-2204 (audit C71): every write on the roles board that fails is shown.
 * Role save and delete went only to console.error (the dialog read as frozen);
 * the drag write, the card sort order and the lane order reloaded the board on
 * failure and said nothing, so a move silently undid itself.
 */

const toasts = vi.hoisted(() => [] as Array<{ message: string; kind: string }>);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: {
		show: (message: string, kind: string) => { toasts.push({ message, kind }); return 'id'; },
		dismiss: () => {},
		get toasts() { return []; },
	},
	quietExternalToasts: () => false,
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		get session() { return { user: { id: 'u1' } }; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		clear() {},
	},
}));

const ROLE_A = { id: 'r1', name: 'Implementer', slug: 'implementer', icon: '🔨', description: '', tools: '' };
const ROLE_B = { id: 'r2', name: 'Reviewer', slug: 'reviewer', icon: '🔍', description: '', tools: '' };
const ITEM = {
	id: 'i1', slug: 'i1', title: 'Row', item_number: 1, collection_slug: 'tasks',
	fields: '{}', tags: '[]', agent_role_id: null, assigned_user_id: null, role_sort_order: 3,
};
// Already in role lane A, so a drop has a card to land ahead of: a card alone
// in a lane needs no sort-order write (TASK-2230).
const NEIGHBOR = { ...ITEM, id: 'i2', slug: 'i2', title: 'Neighbour', item_number: 2, agent_role_id: 'r1', role_sort_order: 1 };

vi.mock('$lib/api/client', () => ({
	api: {
		agentRoles: {
			board: vi.fn(async () => ({
				lanes: [
					{ role: null, items: [ITEM] },
					{ role: ROLE_A, items: [NEIGHBOR] },
					{ role: ROLE_B, items: [] },
				],
			})),
			update: vi.fn(async () => ({})),
			create: vi.fn(async () => ({})),
			delete: vi.fn(async () => ({})),
			reorder: vi.fn(async () => ({})),
			reorderLanes: vi.fn(async () => ({})),
		},
		items: { create: vi.fn(async () => ({})), update: vi.fn(async () => ({})) },
		auth: { session: vi.fn(async () => ({ authenticated: true, user: { id: 'u1' } })) },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get isOwner() { return true; },
		get currentRole() { return 'owner'; },
		get current() { return { id: 'ws1', slug: 'ws', name: 'WS' }; },
		get currentMembership() { return { role: 'owner' }; },
		canEditCollection: () => true,
		canEditItem: () => true,
		setCurrent: vi.fn(async () => {}),
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { get collections() { return []; }, loadCollections: vi.fn(async () => {}) },
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { registerCollectionSearch: () => {}, unregisterCollectionSearch: () => {}, onNavigate: () => () => {} },
}));
vi.mock('svelte-dnd-action', () => ({
	dndzone: () => ({ destroy: () => {} }),
	TRIGGERS: { DROPPED_INTO_ZONE: 'droppedIntoZone' },
	SHADOW_ITEM_MARKER_PROPERTY_NAME: '__dndShadow',
	DRAGGED_ELEMENT_ID: 'dnd-action-dragged-el',
}));

import { api } from '$lib/api/client';
vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));
import { page } from '$app/state';
import RolesPage from './+page.svelte';

async function mountPage() {
	toasts.length = 0;
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/roles');
	render(RolesPage);
	await waitFor(() => {
		if (document.querySelectorAll('.lane-items').length < 3) throw new Error('board not rendered yet');
	});
}

function dropIntoRoleLane(): void {
	document.querySelectorAll('.lane-items')[1]!.dispatchEvent(
		new CustomEvent('finalize', { detail: { items: [{ ...ITEM }, { ...NEIGHBOR }], info: { id: ITEM.id, trigger: 'droppedIntoZone' } } })
	);
}

async function openEditModal(): Promise<void> {
	(document.querySelector('.lane-edit-btn') as HTMLButtonElement).click();
	await tick();
}

function button(text: string): HTMLButtonElement {
	const b = [...document.querySelectorAll('button')].find((x) => x.textContent?.trim() === text) as HTMLButtonElement | undefined;
	expect(b, `the "${text}" button did not render`).toBeDefined();
	return b!;
}

const errorToast = (re: RegExp) => toasts.find((t) => t.kind === 'error' && re.test(t.message));

afterEach(() => {
	cleanup();
	vi.mocked(api.agentRoles.board).mockClear();
});

describe('roles board: a failed write is shown (TASK-2204)', () => {
	it('a role save that fails toasts the server message and keeps the dialog open', async () => {
		await mountPage();
		vi.mocked(api.agentRoles.update).mockRejectedValueOnce(new Error('name already taken'));
		await openEditModal();
		button('Save').click();
		await waitFor(() => expect(errorToast(/^Couldn't save the role: name already taken$/)).toBeDefined());
		expect(button('Save')).toBeDefined();
	});

	it('a role delete that fails toasts', async () => {
		await mountPage();
		vi.spyOn(window, 'confirm').mockReturnValue(true);
		vi.mocked(api.agentRoles.delete).mockRejectedValueOnce(new Error('forbidden'));
		await openEditModal();
		button('Delete Role').click();
		await waitFor(() => expect(errorToast(/^Couldn't delete the role: forbidden$/)).toBeDefined());
	});

	it('a drag whose write fails toasts, beside the reload that undoes the move', async () => {
		await mountPage();
		vi.mocked(api.items.update).mockRejectedValueOnce(new Error('409'));
		const boards = vi.mocked(api.agentRoles.board).mock.calls.length;
		dropIntoRoleLane();
		await waitFor(() => expect(errorToast(/^Couldn't move the item: 409$/)).toBeDefined());
		await waitFor(() => expect(vi.mocked(api.agentRoles.board).mock.calls.length).toBe(boards + 1));
	});

	it('a refused card sort order toasts', async () => {
		await mountPage();
		vi.mocked(api.agentRoles.reorder).mockRejectedValueOnce(new Error('403'));
		dropIntoRoleLane();
		await waitFor(() => expect(errorToast(/^Couldn't save the card order: 403$/)).toBeDefined());
	});

	it('a refused lane order toasts', async () => {
		await mountPage();
		vi.mocked(api.agentRoles.reorderLanes).mockRejectedValueOnce(new Error('500'));
		const headers = document.querySelectorAll('.lane-header');
		// headers[0] is the unassigned lane; drag Implementer onto Reviewer
		headers[1]!.dispatchEvent(new Event('dragstart', { bubbles: true }));
		await tick();
		headers[2]!.dispatchEvent(new Event('drop', { bubbles: true, cancelable: true }));
		await waitFor(() => expect(vi.mocked(api.agentRoles.reorderLanes)).toHaveBeenCalled());
		await waitFor(() => expect(errorToast(/^Couldn't save the lane order: 500$/)).toBeDefined());
	});

	it('CONTROL: writes that land toast no error', async () => {
		await mountPage();
		dropIntoRoleLane();
		await waitFor(() => expect(vi.mocked(api.agentRoles.reorder)).toHaveBeenCalled());
		await tick();
		expect(toasts.filter((t) => t.kind === 'error')).toEqual([]);
	});

	it('a failure that lands after navigating to another workspace is not toasted there (codex r1)', async () => {
		await mountPage();
		let reject!: (e: unknown) => void;
		vi.mocked(api.agentRoles.update).mockReturnValueOnce(new Promise((_res, rej) => { reject = rej; }) as never);
		await openEditModal();
		button('Save').click();
		await waitFor(() => expect(vi.mocked(api.agentRoles.update)).toHaveBeenCalled());
		page.params = { username: 'dave', workspace: 'other' };
		reject(new Error('late'));
		await new Promise((r) => setTimeout(r, 50));
		expect(errorToast(/Couldn't save the role/)).toBeUndefined();
	});

	it('nor on another owner\'s board that has the same slug (codex r2)', async () => {
		await mountPage();
		let reject!: (e: unknown) => void;
		vi.mocked(api.agentRoles.delete).mockReturnValueOnce(new Promise((_res, rej) => { reject = rej; }) as never);
		vi.spyOn(window, 'confirm').mockReturnValue(true);
		await openEditModal();
		button('Delete Role').click();
		await waitFor(() => expect(vi.mocked(api.agentRoles.delete)).toHaveBeenCalled());
		page.params = { username: 'erin', workspace: 'ws' };
		reject(new Error('late'));
		await new Promise((r) => setTimeout(r, 50));
		expect(errorToast(/Couldn't delete the role/)).toBeUndefined();
	});
});
