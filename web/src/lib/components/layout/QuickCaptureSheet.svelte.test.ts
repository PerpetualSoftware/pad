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
	workspaceStore: { canEditCollection: () => true },
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

describe('QuickCaptureSheet keeps your place (TASK-3590)', () => {
	it('Add creates, stays open with an empty title and confirms with a linked toast', async () => {
		const onclose = mount();
		await tick();
		await fireEvent.input(title(), { target: { value: 'jot this down' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Add' }));
		await settle();

		expect(api.items.create).toHaveBeenCalledTimes(1);
		expect(goto, 'Add navigated away').not.toHaveBeenCalled();
		expect(onclose, 'Add closed the sheet').not.toHaveBeenCalled();
		expect(title().value).toBe('');
		const toast = toastStore.toasts.at(-1);
		expect(toast?.message).toBe('Added TASK-1');
		expect(toast?.link).toBe('/u/ws/tasks/TASK-1?new=1');
		expect(toast?.action?.label).toBe('Open');
	});

	it('two Adds in a row create two items without navigating', async () => {
		mount();
		await tick();
		for (const t of ['first', 'second']) {
			await fireEvent.input(title(), { target: { value: t } });
			await fireEvent.keyDown(title(), { key: 'Enter' });
			await settle();
		}
		expect(vi.mocked(api.items.create).mock.calls.map((c) => (c[2] as { title: string }).title)).toEqual(['first', 'second']);
		expect(goto).not.toHaveBeenCalled();
	});

	it('Add and open closes the sheet and navigates to the new item', async () => {
		const onclose = mount();
		await tick();
		await fireEvent.input(title(), { target: { value: 'open me' } });
		await fireEvent.click(screen.getByRole('button', { name: 'Add and open' }));
		await settle();
		expect(onclose).toHaveBeenCalled();
		expect(goto).toHaveBeenCalledWith('/u/ws/tasks/TASK-1?new=1');
	});

	it('text typed while the create was in flight is kept, not cleared', async () => {
		let resolve!: (v: unknown) => void;
		vi.mocked(api.items.create).mockImplementationOnce(() => new Promise((r) => (resolve = r)));
		mount();
		await tick();
		await fireEvent.input(title(), { target: { value: 'first' } });
		await fireEvent.keyDown(title(), { key: 'Enter' });
		await fireEvent.input(title(), { target: { value: 'next thought' } });
		resolve({ id: 'i1', slug: 's1', title: 'first', collection_prefix: 'TASK', item_number: 1 });
		await settle();
		expect(title().value).toBe('next thought');
	});

	// TASK-3599 (retro of #2067): the same text typed again while the create
	// was in flight is the next capture, not this one.
	it('identical text retyped while the create was in flight is kept', async () => {
		let resolve!: (v: unknown) => void;
		vi.mocked(api.items.create).mockImplementationOnce(() => new Promise((r) => (resolve = r)));
		mount();
		await tick();
		await fireEvent.input(title(), { target: { value: 'call Dana' } });
		await fireEvent.keyDown(title(), { key: 'Enter' });
		await fireEvent.input(title(), { target: { value: '' } });
		await fireEvent.input(title(), { target: { value: 'call Dana' } });
		resolve({ id: 'i1', slug: 's1', title: 'call Dana', collection_prefix: 'TASK', item_number: 1 });
		await settle();
		expect(title().value).toBe('call Dana');
	});

	// TASK-3599: the item is created in the workspace the submit was sent for,
	// so its link points there even if the user switched while it was in flight.
	it("the toast links into the workspace the item was created in, not the one switched to", async () => {
		let resolve!: (v: unknown) => void;
		vi.mocked(api.items.create).mockImplementationOnce(() => new Promise((r) => (resolve = r)));
		const onclose = vi.fn();
		const view = render(QuickCaptureSheet, { open: true, onclose, wsSlug: 'ws', wsPrefix: '/u/ws' });
		await tick();
		await fireEvent.input(title(), { target: { value: 'jot' } });
		await fireEvent.keyDown(title(), { key: 'Enter' });
		await view.rerender({ open: true, onclose, wsSlug: 'other', wsPrefix: '/v/other' });
		resolve({ id: 'i1', slug: 's1', title: 'jot', collection_prefix: 'TASK', item_number: 1 });
		await settle();
		expect(vi.mocked(api.items.create).mock.calls[0][0]).toBe('ws');
		expect(toastStore.toasts.at(-1)?.link).toBe('/u/ws/tasks/TASK-1?new=1');
	});
});
