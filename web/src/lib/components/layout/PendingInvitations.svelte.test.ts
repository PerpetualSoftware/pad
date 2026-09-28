// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3277 (PLAN-3002 U4b): the Invitations section of the "+" surface. It
// lists the caller's pending invitations and accepts one by id, landing on an
// EPHEMERAL tab (Q5).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	listMyInvitations: vi.fn(),
	acceptMyInvitation: vi.fn(),
	loadAll: vi.fn(async () => {}),
	tabsOpen: vi.fn(async () => {}),
	toast: vi.fn(),
	calls: [] as string[],
}));

vi.mock('$app/navigation', () => ({ goto: mocks.goto }));
vi.mock('$lib/api/client', () => ({
	api: {
		members: {
			listMyInvitations: mocks.listMyInvitations,
			acceptMyInvitation: mocks.acceptMyInvitation,
		},
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { loadAll: mocks.loadAll } }));
vi.mock('$lib/stores/tabs.svelte', () => ({ tabsStore: { open: mocks.tabsOpen } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { identityFence: () => () => true } }));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: mocks.toast } }));

import PendingInvitations from './PendingInvitations.svelte';

const inv = (id: string, name: string, extra: Record<string, unknown> = {}) => ({
	id,
	role: 'editor',
	workspace_slug: `${id}-ws`,
	workspace_name: name,
	workspace_owner_username: 'alice',
	invited_by_name: 'Alice',
	created_at: '2026-09-28T00:00:00Z',
	...extra,
});

async function settle() {
	for (let i = 0; i < 5; i++) {
		await tick();
		await Promise.resolve();
	}
}

async function mount(list: ReturnType<typeof inv>[], verified = true) {
	mocks.listMyInvitations.mockResolvedValue({ invitations: list, email_verified: verified });
	const onaccepted = vi.fn(() => mocks.calls.push('onaccepted'));
	const r = render(PendingInvitations, { props: { active: true, onaccepted } });
	await settle();
	return { ...r, onaccepted };
}

beforeEach(() => {
	mocks.calls.length = 0;
	for (const f of [mocks.goto, mocks.listMyInvitations, mocks.acceptMyInvitation, mocks.loadAll, mocks.tabsOpen, mocks.toast]) {
		f.mockReset();
	}
	mocks.goto.mockImplementation(async () => {
		mocks.calls.push('goto');
	});
	mocks.loadAll.mockImplementation(async () => {
		mocks.calls.push('loadAll');
	});
	mocks.tabsOpen.mockImplementation(async () => {
		mocks.calls.push('tabsOpen');
	});
});

afterEach(() => cleanup());

describe('PendingInvitations', () => {
	it('renders nothing when there are no invitations', async () => {
		const { container } = await mount([]);
		expect(mocks.listMyInvitations).toHaveBeenCalledTimes(1);
		expect(container.querySelector('.invitations-section')).toBeNull();
	});

	it('renders nothing when the fetch fails', async () => {
		mocks.listMyInvitations.mockRejectedValue(new Error('boom'));
		const { container } = render(PendingInvitations, { props: { active: true } });
		await settle();
		expect(container.querySelector('.invitations-section')).toBeNull();
	});

	it('lists each invitation with its inviter and role', async () => {
		const { getByRole, getByText } = await mount([inv('a', 'Alpha'), inv('b', 'Beta', { invited_by_name: '' })]);
		expect(getByRole('region', { name: 'Invitations' })).toBeTruthy();
		expect(getByText('Alpha')).toBeTruthy();
		expect(getByText('Alice · editor')).toBeTruthy();
		expect(getByRole('button', { name: 'Accept the invitation to Beta' })).toBeTruthy();
	});

	it('accepts by id, then reloads workspaces, opens an EPHEMERAL tab and lands', async () => {
		mocks.acceptMyInvitation.mockResolvedValue({
			accepted: true,
			workspace_id: 'w',
			role: 'editor',
			workspace_slug: 'a-ws',
			owner_username: 'alice',
		});
		const { getByRole, onaccepted } = await mount([inv('a', 'Alpha')]);
		await fireEvent.click(getByRole('button', { name: 'Accept the invitation to Alpha' }));
		await settle();

		expect(mocks.acceptMyInvitation).toHaveBeenCalledWith('a');
		expect(onaccepted).toHaveBeenCalledTimes(1);
		expect(mocks.tabsOpen).toHaveBeenCalledWith('a-ws', true);
		expect(mocks.goto).toHaveBeenCalledWith('/alice/a-ws');
		// The workspace list is reloaded before the tab opens, so the new
		// workspace is in it when you land.
		expect(mocks.calls).toEqual(['onaccepted', 'loadAll', 'tabsOpen', 'goto']);
	});

	it('still lands when opening the tab fails', async () => {
		mocks.acceptMyInvitation.mockResolvedValue({ accepted: true, workspace_id: 'w', role: 'editor' });
		mocks.tabsOpen.mockRejectedValue(new Error('tab refused'));
		const { getByRole } = await mount([inv('a', 'Alpha')]);
		await fireEvent.click(getByRole('button', { name: 'Accept the invitation to Alpha' }));
		await settle();
		// Falls back to the listed slug and owner when the body lacks them.
		expect(mocks.goto).toHaveBeenCalledWith('/alice/a-ws');
	});

	it('lands on /console when the owner has no username', async () => {
		mocks.acceptMyInvitation.mockResolvedValue({ accepted: true, workspace_id: 'w', role: 'editor', workspace_slug: 'a-ws', owner_username: '' });
		const { getByRole } = await mount([inv('a', 'Alpha', { workspace_owner_username: '' })]);
		await fireEvent.click(getByRole('button', { name: 'Accept the invitation to Alpha' }));
		await settle();
		expect(mocks.goto).toHaveBeenCalledWith('/console');
	});

	it('a failed accept toasts, does not navigate, and refreshes the list', async () => {
		mocks.acceptMyInvitation.mockRejectedValue(new Error('This invitation has expired.'));
		const { getByRole, onaccepted } = await mount([inv('a', 'Alpha')]);
		expect(mocks.listMyInvitations).toHaveBeenCalledTimes(1);
		await fireEvent.click(getByRole('button', { name: 'Accept the invitation to Alpha' }));
		await settle();
		expect(mocks.toast).toHaveBeenCalledWith('This invitation has expired.', 'error');
		expect(onaccepted).not.toHaveBeenCalled();
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(mocks.tabsOpen).not.toHaveBeenCalled();
		expect(mocks.listMyInvitations).toHaveBeenCalledTimes(2);
	});

	it('a second click while accepting does not send a second accept', async () => {
		let release!: (v: unknown) => void;
		mocks.acceptMyInvitation.mockReturnValue(new Promise((r) => (release = r)));
		const { getByRole } = await mount([inv('a', 'Alpha'), inv('b', 'Beta')]);
		const a = getByRole('button', { name: 'Accept the invitation to Alpha' });
		await fireEvent.click(a);
		await fireEvent.click(a);
		await fireEvent.click(getByRole('button', { name: 'Accept the invitation to Beta' }));
		expect(mocks.acceptMyInvitation).toHaveBeenCalledTimes(1);
		release({ accepted: true, workspace_id: 'w', role: 'editor' });
		await waitFor(() => expect(mocks.goto).toHaveBeenCalledTimes(1));
	});
});
