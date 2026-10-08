import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/svelte';
import { page } from '$app/state';

/**
 * TASK-2190: the members page let an owner demote themselves with one
 * unconfirmed change, offered Remove on their own row (the server refuses
 * it), and reported every refusal as a bare "Failed to update role", so the
 * server's reason ("a workspace must keep at least one owner") never reached
 * the person. The select also kept showing the refused role.
 */
const calls = vi.hoisted(() => ({ updateRole: [] as Array<[string, string]>, refuseWith: null as null | string }));
const toasts = vi.hoisted(() => [] as string[]);

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {
		code: string;
		constructor(message: string, code: string) {
			super(message);
			this.code = code;
		}
	}
	return {
		PadApiError,
		api: {
			workspaces: {
				get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
				me: vi.fn(async () => ({ role: 'owner', collection_grants: [], item_grants: [] })),
				list: vi.fn(async () => [])
			},
			collections: { list: vi.fn(async () => []) },
			members: {
				list: vi.fn(async () => ({
					members: [
						{ user_id: 'u-me', user_name: 'Me', user_email: 'me@example.com', role: 'owner' },
						{ user_id: 'u-other', user_name: 'Other', user_email: 'other@example.com', role: 'owner' }
					],
					invitations: []
				})),
				updateRole: vi.fn(async (_ws: string, userId: string, role: string) => {
					calls.updateRole.push([userId, role]);
					if (calls.refuseWith) throw new PadApiError(calls.refuseWith, 'last_owner');
					return { user_id: userId, role };
				}),
				remove: vi.fn()
			}
		},
		isPlanLimitError: () => false,
		planLimitMessage: () => ''
	};
});
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (m: string) => { toasts.push(m); return 'id'; }, dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get userId() { return 'u-me'; },
		get identityEpoch() { return 0; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		get user() { return { id: 'u-me', name: 'Me', email: 'me@example.com' }; }
	}
}));

const { default: SettingsPage } = await import('./+page.svelte');

function row(name: string): HTMLElement {
	return screen.getByText(name, { selector: '.member-name' }).closest('.member-row') as HTMLElement;
}

async function openMembers() {
	render(SettingsPage);
	await waitFor(() => expect(screen.getByText('Other', { selector: '.member-name' })).toBeInTheDocument());
}

function choose(select: HTMLSelectElement, value: string) {
	select.value = value;
	select.dispatchEvent(new Event('change', { bubbles: true }));
}

describe('TASK-2190: member changes that can lock an owner out', () => {
	beforeEach(() => {
		calls.updateRole.length = 0;
		calls.refuseWith = null;
		toasts.length = 0;
		page.params = { username: 'dave', workspace: 'ws' };
		window.location.hash = '#members';
	});
	afterEach(() => {
		window.location.hash = '';
		vi.restoreAllMocks();
	});

	it('offers Remove on other rows only', async () => {
		await openMembers();
		expect(within(row('Other')).queryByRole('button', { name: 'Remove' })).not.toBeNull();
		expect(within(row('Me')).queryByRole('button', { name: 'Remove' })).toBeNull();
	});

	it('asks before you demote yourself, and a No changes nothing', async () => {
		await openMembers();
		const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
		const select = row('Me').querySelector<HTMLSelectElement>('.role-select')!;
		choose(select, 'editor');
		expect(confirm).toHaveBeenCalledTimes(1);
		expect(calls.updateRole).toEqual([]);
		expect(select.value).toBe('owner');
	});

	it("demoting someone else is not asked about", async () => {
		await openMembers();
		const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
		choose(row('Other').querySelector<HTMLSelectElement>('.role-select')!, 'editor');
		await waitFor(() => expect(calls.updateRole).toEqual([['u-other', 'editor']]));
		expect(confirm).not.toHaveBeenCalled();
	});

	it("a refusal says the server's reason and puts the stored role back", async () => {
		await openMembers();
		calls.refuseWith = 'A workspace must keep at least one owner; make another member an owner first';
		const select = row('Other').querySelector<HTMLSelectElement>('.role-select')!;
		choose(select, 'viewer');
		await waitFor(() => expect(toasts).toContain(calls.refuseWith));
		expect(select.value).toBe('owner');
	});
});
