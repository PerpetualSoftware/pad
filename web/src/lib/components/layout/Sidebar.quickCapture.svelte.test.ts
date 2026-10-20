// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3590: sidebar quick-add keeps the user's place. Plain Enter (and the
// Add button) creates the item, clears the input for the next capture and
// confirms with a toast that links to the item. Ctrl/Cmd+Enter is "add and
// open", which navigates as quick-add always did.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const created = vi.hoisted(() => ({ n: 0 }));

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []), update: vi.fn() },
		items: {
			create: vi.fn(async (_ws: string, _coll: string, body: { title: string }) => {
				created.n++;
				return { id: `i${created.n}`, slug: `s${created.n}`, title: body.title, collection_prefix: 'TASK', item_number: created.n };
			}),
		},
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn(), beforeNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		current: { slug: 'ws', owner_username: 'u', is_guest: false },
		canEditCollection: () => true,
		membershipKnown: true
	},
}));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));

import Sidebar from './Sidebar.svelte';
import { goto } from '$app/navigation';
import { api } from '$lib/api/client';
import { uiStore } from '$lib/stores/ui.svelte';
import { toastStore } from '$lib/stores/toast.svelte';

function input(): HTMLTextAreaElement {
	const el = document.querySelector<HTMLTextAreaElement>('.quick-add-modal textarea.quick-add-input');
	if (!el) throw new Error('quick-add dialog is not open');
	return el;
}

async function open() {
	uiStore.requestQuickAdd('tasks');
	await tick();
	await tick();
}

async function settle() {
	for (let i = 0; i < 4; i++) await tick();
}

beforeEach(() => {
	cleanup();
	created.n = 0;
	vi.mocked(goto).mockClear();
	vi.mocked(api.items.create).mockClear();
	for (const t of [...toastStore.toasts]) toastStore.dismiss(t.id);
});

describe('Sidebar quick-add keeps your place (TASK-3590)', () => {
	it('Enter adds, stays put, clears the input and confirms with a linked toast', async () => {
		render(Sidebar);
		await open();
		await fireEvent.input(input(), { target: { value: 'jot this down' } });
		await fireEvent.keyDown(input(), { key: 'Enter' });
		await settle();

		expect(api.items.create).toHaveBeenCalledTimes(1);
		expect(goto, 'Add navigated away').not.toHaveBeenCalled();
		expect(document.querySelector('.quick-add-modal'), 'Add closed the dialog').not.toBeNull();
		expect(input().value).toBe('');
		const toast = toastStore.toasts.at(-1);
		expect(toast?.message).toBe('Added TASK-1');
		expect(toast?.link).toBe('/u/ws/tasks/TASK-1?new=1');
		expect(toast?.action?.label).toBe('Open');
		toast!.action!.onAction();
		expect(goto).toHaveBeenCalledWith('/u/ws/tasks/TASK-1?new=1');
	});

	it('two Adds in a row create two items without navigating', async () => {
		render(Sidebar);
		await open();
		for (const t of ['first', 'second']) {
			await fireEvent.input(input(), { target: { value: t } });
			await fireEvent.keyDown(input(), { key: 'Enter' });
			await settle();
		}
		expect(vi.mocked(api.items.create).mock.calls.map((c) => (c[2] as { title: string }).title)).toEqual(['first', 'second']);
		expect(goto).not.toHaveBeenCalled();
	});

	it('the Add button stays too', async () => {
		render(Sidebar);
		await open();
		await fireEvent.input(input(), { target: { value: 'by button' } });
		await fireEvent.click(document.querySelector<HTMLButtonElement>('.quick-add-submit')!);
		await settle();
		expect(api.items.create).toHaveBeenCalledTimes(1);
		expect(goto).not.toHaveBeenCalled();
	});

	it('the Add and open button adds and opens the new item (TASK-3972)', async () => {
		render(Sidebar);
		await open();
		await fireEvent.input(input(), { target: { value: 'open by button' } });
		await fireEvent.click(document.querySelector<HTMLButtonElement>('.quick-add-open')!);
		await settle();
		expect(api.items.create).toHaveBeenCalledTimes(1);
		expect(goto).toHaveBeenCalledWith('/u/ws/tasks/TASK-1?new=1');
		expect(document.querySelector('.quick-add-modal'), 'add and open left the dialog up').toBeNull();
	});

	it('the Cancel button closes the dialog without creating anything (TASK-3972)', async () => {
		render(Sidebar);
		await open();
		await fireEvent.input(input(), { target: { value: 'never mind' } });
		await fireEvent.click(document.querySelector<HTMLButtonElement>('.quick-add-cancel')!);
		await settle();
		expect(document.querySelector('.quick-add-modal'), 'Cancel left the dialog up').toBeNull();
		expect(api.items.create).not.toHaveBeenCalled();
		await open();
		expect(input().value, 'the cancelled text came back on reopen').toBe('');
	});

	it('Add and open is disabled until there is a title, like Add (TASK-3972)', async () => {
		render(Sidebar);
		await open();
		const openBtn = () => document.querySelector<HTMLButtonElement>('.quick-add-open')!;
		expect(openBtn().disabled).toBe(true);
		expect(document.querySelector<HTMLButtonElement>('.quick-add-cancel')!.disabled).toBe(false);
		await fireEvent.input(input(), { target: { value: 'x' } });
		expect(openBtn().disabled).toBe(false);
	});

	for (const mod of ['ctrlKey', 'metaKey'] as const) {
		it(`${mod}+Enter adds and opens the new item`, async () => {
			render(Sidebar);
			await open();
			await fireEvent.input(input(), { target: { value: 'open me' } });
			await fireEvent.keyDown(input(), { key: 'Enter', [mod]: true });
			await settle();
			expect(goto).toHaveBeenCalledWith('/u/ws/tasks/TASK-1?new=1');
			expect(document.querySelector('.quick-add-modal'), 'add and open left the dialog up').toBeNull();
		});
	}
});
