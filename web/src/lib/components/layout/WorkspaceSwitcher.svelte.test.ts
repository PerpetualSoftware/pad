import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent, screen } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	workspaces: [] as { id: string; slug: string; name: string; owner_username: string }[],
	tabs: { list: vi.fn(), open: vi.fn(), close: vi.fn(), reorder: vi.fn(), update: vi.fn() },
	listDeleted: vi.fn(async () => []),
	listMyInvitations: vi.fn(),
	acceptMyInvitation: vi.fn(),
}));

vi.mock('$app/navigation', () => ({ goto: mocks.goto }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: {
		workspaces: { tabs: mocks.tabs, listDeleted: mocks.listDeleted, restore: vi.fn() },
		members: { listMyInvitations: mocks.listMyInvitations, acceptMyInvitation: mocks.acceptMyInvitation },
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get workspaces() { return mocks.workspaces; },
		current: null,
		loadAll: vi.fn(async () => {}),
	},
}));

import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';
import { tabsStore } from '$lib/stores/tabs.svelte';
import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';

const invitation = {
	id: 'invite-1', role: 'editor', workspace_slug: 'invited', workspace_name: 'Invited Workspace',
	workspace_owner_username: 'alice', invited_by_name: 'Alice', created_at: '2026-11-14T00:00:00Z',
};

const ws = (slug: string, name: string) => ({ id: slug, slug, name, owner_username: 'u' });
const tab = (slug: string, name: string, ephemeral = false, last_route?: string) => ({
	slug, name, owner_username: 'u', is_guest: false, ephemeral,
	position: 0, created_at: '', updated_at: '', ...(last_route ? { last_route } : {}),
});

// The real tab store rejects responses older than its last committed revision.
let revision = 0;
async function mount(openTabs: ReturnType<typeof tab>[]) {
	mocks.tabs.list.mockResolvedValueOnce({ revision: ++revision, tabs: openTabs });
	await tabsStore.load();
	render(WorkspaceSwitcher, { props: { mobile: true } });
	await fireEvent.click(document.querySelector<HTMLButtonElement>('.switcher .current')!);
	await tick();
}

const rows = () => Array.from(document.querySelectorAll<HTMLButtonElement>('.sheet-body .item[data-ws-slug]'));

beforeEach(() => {
	pendingInvitations.set([], pendingInvitations.reserve());
	mocks.listMyInvitations.mockReset().mockResolvedValue({ invitations: [], email_verified: true });
	mocks.acceptMyInvitation.mockReset().mockResolvedValue({ accepted: true });
	mocks.goto.mockClear();
	for (const fn of Object.values(mocks.tabs)) fn.mockReset();
	mocks.workspaces = [ws('alpha', 'Alpha'), ws('beta', 'Beta'), ws('gamma', 'Gamma'), ws('delta', 'Delta')];
	localStorage.clear();
});

afterEach(() => cleanup());

describe('mobile WorkspaceSwitcher', () => {
	it('shows invitation actions in the sheet and closes it when accepting into the workspace', async () => {
		pendingInvitations.set([invitation], pendingInvitations.reserve());
		mocks.listMyInvitations.mockResolvedValue({ invitations: [invitation], email_verified: true });
		await mount([]);
		const accept = screen.getByRole('button', { name: 'Accept the invitation to Invited Workspace' });
		expect(screen.getByRole('button', { name: 'Decline the invitation to Invited Workspace' })).toBeInTheDocument();
		expect(mocks.listMyInvitations).toHaveBeenCalledTimes(1);
		mocks.tabs.open.mockResolvedValue({ revision: ++revision, tabs: [tab('invited', 'Invited Workspace', true)] });
		await fireEvent.click(accept);
		await vi.waitFor(() => expect(mocks.goto).toHaveBeenCalledWith('/alice/invited'));
		expect(mocks.acceptMyInvitation).toHaveBeenCalledWith('invite-1');
		expect(screen.queryByRole('dialog', { name: 'Switch workspace' })).not.toBeInTheDocument();
	});

	it('shows no Invitations section when the sheet refresh returns no invitations', async () => {
		await mount([]);
		expect(mocks.listMyInvitations).toHaveBeenCalledTimes(1);
		expect(screen.queryByRole('region', { name: 'Invitations' })).not.toBeInTheDocument();
		expect(screen.queryByText('Invitations')).not.toBeInTheDocument();
	});

	it('shows open tabs in bar order, then a divider and remaining workspaces in membership order without duplicates', async () => {
		await mount([tab('gamma', 'Gamma'), tab('alpha', 'Alpha', true)]);
		expect(rows().map((row) => row.dataset.wsSlug)).toEqual(['gamma', 'alpha', 'beta', 'delta']);
		const divider = document.querySelector('.sheet-body .workspace-divider')!;
		expect(divider.previousElementSibling).toBe(rows()[1]);
		expect(divider.nextElementSibling).toBe(rows()[2]);
		expect(getComputedStyle(rows()[1]).fontStyle).toBe('italic');
	});

	it('omits the divider when no tabs are open', async () => {
		await mount([]);
		expect(rows().map((row) => row.dataset.wsSlug)).toEqual(['alpha', 'beta', 'gamma', 'delta']);
		expect(document.querySelector('.sheet-body .workspace-divider')).toBeNull();
	});

	it('an already-open tab only navigates: tapping an ephemeral one does not keep it', async () => {
		await mount([tab('alpha', 'Alpha'), tab('beta', 'Beta', true, '/u/beta/docs')]);
		await fireEvent.click(rows()[1]);
		await tick();
		expect(mocks.tabs.open).not.toHaveBeenCalled();
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta/docs');
	});

	it('opens a workspace from the rest as a durable tab before restoring its route', async () => {
		await mount([tab('alpha', 'Alpha')]);
		mocks.tabs.open.mockResolvedValueOnce({ revision: ++revision, tabs: [tab('alpha', 'Alpha'), tab('beta', 'Beta', false, '/u/beta/docs')] });
		await fireEvent.click(rows()[1]);
		await tick();
		expect(mocks.tabs.open).toHaveBeenCalledWith('beta', false);
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta/docs');
	});
});


it('offers Create workspace in the phone switcher and opens the create modal', async () => {
	const { uiStore } = await import('$lib/stores/ui.svelte');
	const create = vi.spyOn(uiStore, 'openCreateWorkspace');
	await mount([]);
	await fireEvent.click(screen.getByRole('button', { name: /Create workspace/i }));
	expect(create).toHaveBeenCalledTimes(1);
	create.mockRestore();
});

it.each([1, 4])('formats the workspace card role and %i workspace count separately', async count => {
	mocks.workspaces = Array.from({ length: count }, (_, i) => ws(String(i), 'Workspace'));
	render(WorkspaceSwitcher, { mobile: true, card: true, current: { ...ws('alpha', 'Alpha'), is_guest: true } as never });
	expect(document.querySelector('.workspace-meta small')).toHaveTextContent(`Guest · ${count} ${count === 1 ? 'workspace' : 'workspaces'}`);
});
