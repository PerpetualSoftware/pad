// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3590: the mobile quick-capture sheet keeps the user's place. "Add"
// creates the item, stays open with an empty title for the next capture and
// confirms with a toast that links to the item. "Add and open" closes and
// navigates as the sheet always did.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup, fireEvent, screen } from '@testing-library/svelte';
import { tick } from 'svelte';

const created = vi.hoisted(() => ({ n: 0 }));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: { me: vi.fn(async () => ({ role: 'owner' })) },
	collections: { list: vi.fn(async () => [{id: 'tasks', slug:'tasks', name:'Tasks', fields: '[]', settings:'{}'}]) },
	items: {
			create: vi.fn(async (_ws: string, _coll: string, body: { title: string }) => {
				created.n++;
				return { id: `i${created.n}`, slug: `s${created.n}`, title: body.title, collection_prefix: 'TASK', item_number: created.n };
			}),
		},
	},
	isPlanLimitError: () => false,
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditCollection: () => true, workspaces: [{id:'ws',slug:'ws',name:'First',owner_username:'u'},{id:'other',slug:'other',name:'Last',owner_username:'v'}], current: null },
}));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));

import QuickCaptureSheet from './QuickCaptureSheet.svelte';
import { goto } from '$app/navigation';
import { api } from '$lib/api/client';
import { toastStore } from '$lib/stores/toast.svelte';

function title(): HTMLTextAreaElement {
	return screen.getByRole('textbox', { name: 'New item title' }) as HTMLTextAreaElement;
}

async function settle() {
	for (let i = 0; i < 4; i++) await tick();
}

function mount() {
	const onclose = vi.fn();
	render(QuickCaptureSheet, { open: true, onclose, wsSlug: 'ws', wsPrefix: '/u/ws' });
	return onclose;
}

beforeEach(() => {
	cleanup();
	created.n = 0;
	vi.mocked(goto).mockClear();
	vi.mocked(api.items.create).mockClear();
	for (const t of [...toastStore.toasts]) toastStore.dismiss(t.id);
});


import { lastWorkspace } from '$lib/stores/lastWorkspace.svelte';
it('captures into the last workspace and lets a You page change it', async () => {
	lastWorkspace.note('v', 'other');
	render(QuickCaptureSheet, { open: true, onclose: vi.fn(), wsSlug: undefined, wsPrefix: '' }); await settle();
	const select = screen.getByRole('combobox', { name: 'Workspace' });
	expect(select).toHaveValue('other');
	await fireEvent.input(title(), { target: { value: 'Last workspace task' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Add' })); await settle();
	expect(vi.mocked(api.items.create).mock.calls[0][0]).toBe('other');
	await fireEvent.change(select, { target: { value: 'ws' } }); await settle();
	await fireEvent.input(title(), { target: { value: 'First workspace task' } });
	await fireEvent.click(screen.getByRole('button', { name: 'Add' })); await settle();
	expect(vi.mocked(api.items.create).mock.calls[1][0]).toBe('ws');
});
