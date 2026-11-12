import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import type { Workspace, WorkspaceGroup } from '$lib/types';

const h = vi.hoisted(() => ({ create: vi.fn(), update: vi.fn(), remove: vi.fn() }));
vi.mock('$lib/api/client', () => ({ api: { workspaceGroups: { create: h.create, update: h.update, remove: h.remove } } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { identityFence: () => () => true, onIdentityChange: () => () => {} } }));
import WorkspaceGroupsDialog from './WorkspaceGroupsDialog.svelte';
import { confirmDialog } from '$lib/stores/confirmDialog.svelte';

const workspaces = ['Harbor App', 'Marina Ops', 'Quiet Corner', 'Website', 'Fifth'].map((name, index) => ({ id: `ws-${index + 1}`, name, slug: name, owner_username: 'alice' })) as Workspace[];
const named = (id: string, name: string, workspace_ids: string[]): WorkspaceGroup => ({ id, name, kind: 'named', workspace_ids });
const groups: WorkspaceGroup[] = [{ id: 'all', name: 'All', kind: 'all', workspace_ids: [] }, named('g1', 'Client work', ['ws-1', 'ws-2']), named('g2', 'Everything', ['ws-1', 'ws-2', 'ws-3', 'ws-4', 'ws-5']), named('g3', 'Empty', [])];
const settle = async () => { await new Promise(r => setTimeout(r, 0)); flushSync(); };
function mount(extra: Record<string, unknown> = {}) {
	const onclose = vi.fn(), onchanged = vi.fn().mockResolvedValue(undefined);
	const view = render(WorkspaceGroupsDialog, { groups, workspaces, mobile: false, onclose, onchanged, ...extra });
	return { onclose, onchanged, view, dialog: screen.getByRole('dialog', { name: 'Workspace groups' }) };
}
const primaries = () => [...screen.getByRole('dialog').querySelectorAll('button.btn.primary')].map(button => button.textContent?.trim());
const button = (name: string) => screen.getByRole('button', { name });
beforeEach(() => { vi.resetAllMocks(); h.create.mockResolvedValue({ id: 'g9' }); h.update.mockResolvedValue({}); h.remove.mockResolvedValue({}); });
afterEach(() => { confirmDialog.abandonAll(); cleanup(); });

describe('WorkspaceGroupsDialog list', () => {
	it('lists named groups with their members, the first three then +N', () => {
		const { dialog } = mount();
		expect([...dialog.querySelectorAll('[data-group]')].map(row => row.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
			'Client work Harbor App, Marina Ops', 'Everything Harbor App, Marina Ops, Quiet Corner +2', 'Empty No workspaces'
		]);
		for (const name of ['Edit Client work', 'Delete Client work', 'Edit Empty', 'Delete Empty']) expect(button(name)).toBeInTheDocument();
	});
	it('has New group on the left, one primary Done that closes, and no other Close button', async () => {
		const { onclose, dialog } = mount();
		expect(primaries()).toEqual(['Done']); expect(button('New group')).toHaveClass('secondary');
		expect(within(dialog).getAllByRole('button', { name: /close/i })).toHaveLength(1);
		await fireEvent.click(button('Done')); expect(onclose).toHaveBeenCalledTimes(1);
	});
	it('deletes through the confirmation dialog, then focuses New group', async () => {
		const { onchanged } = mount();
		await fireEvent.click(button('Delete Client work'));
		expect(confirmDialog.active).toMatchObject({ title: 'Delete Client work?', confirmLabel: 'Delete group', danger: true });
		expect(h.remove).not.toHaveBeenCalled();
		confirmDialog.confirm();
		await waitFor(() => expect(onchanged).toHaveBeenCalledWith('g1')); expect(h.remove).toHaveBeenCalledWith('g1');
		await waitFor(() => expect(document.activeElement).toBe(button('New group')));
	});
	it('deletes nothing when the confirmation is cancelled', async () => {
		mount(); await fireEvent.click(button('Delete Client work')); confirmDialog.cancel(); await settle();
		expect(h.remove).not.toHaveBeenCalled();
	});
});

describe('WorkspaceGroupsDialog edit step', () => {
	it('edits a group: back button, name, a checklist with a count, and Save group as the one primary', async () => {
		const { dialog } = mount(); await fireEvent.click(button('Edit Client work'));
		expect(screen.getByRole('dialog', { name: 'Workspace groups' })).toBe(dialog);
		expect(button('Back to groups')).toBeInTheDocument();
		expect((within(dialog).getByRole('textbox', { name: 'Name' }) as HTMLInputElement).value).toBe('Client work');
		expect(within(dialog).getAllByRole('checkbox')).toHaveLength(5); expect(within(dialog).getByText('2 of 5 selected')).toBeInTheDocument();
		await fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Quiet Corner' })); expect(within(dialog).getByText('3 of 5 selected')).toBeInTheDocument();
		expect(primaries()).toEqual(['Save group']); expect(button('Delete group')).toHaveClass('danger'); expect(button('Cancel')).toHaveClass('secondary');
		expect(screen.queryByRole('button', { name: 'Done' })).toBeNull();
	});
	it.each(['Cancel', 'Back to groups'])('%s returns to the list without closing, with focus on that group\'s Edit button', async name => {
		const { onclose } = mount(); await fireEvent.click(button('Edit Everything')); await fireEvent.click(button(name));
		expect(onclose).not.toHaveBeenCalled(); expect(h.update).not.toHaveBeenCalled();
		await waitFor(() => expect(document.activeElement).toBe(button('Edit Everything')));
	});
	it('saves, returns to the list and focuses that group\'s Edit button', async () => {
		const { onchanged } = mount(); await fireEvent.click(button('Edit Client work'));
		await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: ' Clients ' } });
		await fireEvent.click(button('Save group'));
		await waitFor(() => expect(h.update).toHaveBeenCalledWith('g1', { name: 'Clients', workspace_ids: ['ws-1', 'ws-2'] }));
		await waitFor(() => expect(onchanged).toHaveBeenCalledTimes(1));
		await waitFor(() => expect(document.activeElement).toBe(button('Edit Client work')));
	});
	it('creates a group as the one primary, with no Delete, then focuses the new group\'s Edit button', async () => {
		const { view, onchanged } = mount(); await fireEvent.click(button('New group'));
		expect(primaries()).toEqual(['Create group']); expect(screen.queryByRole('button', { name: 'Delete group' })).toBeNull();
		expect(button('Create group')).toBeDisabled();
		await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: '   ' } }); expect(button('Create group')).toBeDisabled();
		await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: ' Fresh ' } });
		onchanged.mockImplementation(async () => { await view.rerender({ groups: [...groups, named('g9', 'Fresh', [])] }); });
		await fireEvent.click(button('Create group'));
		await waitFor(() => expect(h.create).toHaveBeenCalledWith({ name: 'Fresh', workspace_ids: [] }));
		await waitFor(() => expect(document.activeElement).toBe(button('Edit Fresh')));
	});
	it('cannot create while the workspace list is missing, but can rename', async () => {
		mount({ workspaces: null }); await fireEvent.click(button('New group'));
		await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'Fresh' } });
		expect(button('Create group')).toBeDisabled(); expect(screen.getByText('Could not load workspaces.')).toBeInTheDocument();
		await fireEvent.click(button('Cancel')); await fireEvent.click(button('Edit Empty')); expect(button('Save group')).not.toBeDisabled();
	});
	it('opens straight at the edit step of the group it is given', () => {
		mount({ initialEdit: 'g3' });
		expect((screen.getByRole('textbox', { name: 'Name' }) as HTMLInputElement).value).toBe('Empty'); expect(primaries()).toEqual(['Save group']);
	});
	it('deletes from the edit step through the confirmation dialog', async () => {
		const { onchanged } = mount(); await fireEvent.click(button('Edit Client work')); await fireEvent.click(button('Delete group'));
		expect(confirmDialog.active).toMatchObject({ title: 'Delete Client work?', confirmLabel: 'Delete group', danger: true });
		confirmDialog.confirm();
		await waitFor(() => expect(onchanged).toHaveBeenCalledWith('g1'));
		await waitFor(() => expect(document.activeElement).toBe(button('New group')));
	});
	it('keeps the edit step and shows the failure inside it, and ignores Escape while saving', async () => {
		let fail!: (reason: unknown) => void; h.update.mockReturnValue(new Promise((_, reject) => { fail = reject; }));
		const { dialog, onclose } = mount(); await fireEvent.click(button('Edit Client work')); await fireEvent.click(button('Save group'));
		await waitFor(() => expect(button('Cancel')).toBeDisabled());
		await fireEvent(dialog, new Event('cancel', { cancelable: true })); expect(onclose).not.toHaveBeenCalled();
		fail({ message: 'Name is already used.' });
		expect(await within(dialog).findByRole('alert')).toHaveTextContent('Name is already used.'); expect(button('Save group')).not.toBeDisabled();
	});
});
