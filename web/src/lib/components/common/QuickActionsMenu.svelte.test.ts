import { describe, it, expect, vi, beforeEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { tick } from 'svelte';
import type { Collection } from '$lib/types';

// BUG-2265 Pattern C: QuickActionsMenu's save must recover from a competing
// RENAME (404 not_found) — not just a 409 — by resolving the collection by its
// STABLE id and retrying against the fresh slug. This mounts the REAL component
// with a mocked api and drives the create-action save through a not_found on
// the FIRST update, asserting the retry targets the renamed slug.

const updateMock = vi.fn();
const listMock = vi.fn();

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			update: (...args: unknown[]) => updateMock(...args),
			list: (...args: unknown[]) => listMock(...args),
		},
	},
	// Real-ish classifier so the component's branch fires for 404/409.
	isConflictOrNotFound: (err: unknown) =>
		err instanceof Error &&
		((err as { code?: string }).code === 'not_found' ||
			(err as { code?: string }).code === 'update_conflict'),
}));

const toastShow = vi.fn();
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (...a: unknown[]) => toastShow(...a) },
}));

vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: { isMobile: false },
}));

// Stub the sub-components rendered inside the create form so the test doesn't
// depend on their internals.
vi.mock('$lib/components/common/BottomSheet.svelte', () => ({ default: noopComponent() }));
vi.mock('$lib/components/common/EmojiPickerButton.svelte', () => ({ default: noopComponent() }));

function noopComponent() {
	// A minimal Svelte-5-compatible mountable component.
	return function () {};
}

const { default: QuickActionsMenu } = await import('./QuickActionsMenu.svelte');

function makeCollection(id: string, slug: string): Collection {
	return {
		id,
		workspace_id: 'ws-1',
		name: 'Tasks',
		slug,
		icon: '',
		description: '',
		schema: '{"fields":[]}',
		settings: '{"quick_actions":[]}',
		sort_order: 0,
		is_default: false,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'TASK',
	} as Collection;
}

function notFound() {
	const e = new Error('gone') as Error & { code: string };
	e.code = 'not_found';
	return e;
}

describe('QuickActionsMenu save retry (BUG-2265 Pattern C)', () => {
	beforeEach(() => {
		updateMock.mockReset();
		listMock.mockReset();
		toastShow.mockReset();
	});

	it('recovers from a 404 (renamed slug) by resolving the collection by id', async () => {
		// First update (against the captured "old" slug) hits a rename → 404.
		// The retry lists collections, finds the SAME id under the new slug, and
		// updates against that new slug.
		updateMock
			.mockRejectedValueOnce(notFound())
			.mockResolvedValueOnce(makeCollection('c1', 'renamed-tasks'));
		listMock.mockResolvedValueOnce([makeCollection('c1', 'renamed-tasks')]);

		const host = document.createElement('div');
		document.body.appendChild(host);
		const onupdated = vi.fn();
		const component = mount(QuickActionsMenu, {
			target: host,
			props: {
				actions: [],
				collection: makeCollection('c1', 'tasks'),
				scope: 'item',
				wsSlug: 'ws-1',
				canEdit: true,
				oncollectionupdated: onupdated,
			},
		});
		flushSync();

		// Open the menu, then the inline create form.
		(host.querySelector('.trigger-btn') as HTMLButtonElement).click();
		flushSync();
		const openFormBtn = [...host.querySelectorAll('button')].find((b) =>
			b.textContent?.includes('New quick action')
		) as HTMLButtonElement;
		openFormBtn.click();
		flushSync();

		// Fill label + prompt.
		const label = host.querySelector('.qa-label-input') as HTMLInputElement;
		const prompt = host.querySelector('.qa-prompt-input') as HTMLTextAreaElement;
		label.value = 'Ship';
		label.dispatchEvent(new Event('input', { bubbles: true }));
		prompt.value = '/pad ship';
		prompt.dispatchEvent(new Event('input', { bubbles: true }));
		flushSync();

		// Click Save.
		const saveBtn = [...host.querySelectorAll('button')].find(
			(b) => b.textContent?.trim() === 'Save'
		) as HTMLButtonElement;
		saveBtn.click();

		// Let the async save + retry settle.
		await vi.waitFor(() => expect(updateMock).toHaveBeenCalledTimes(2));
		await tick();

		// First update used the OLD slug; the retry used the NEW (renamed) slug.
		expect(updateMock.mock.calls[0][1]).toBe('tasks');
		expect(updateMock.mock.calls[1][1]).toBe('renamed-tasks');
		expect(listMock).toHaveBeenCalledTimes(1);
		// The retry succeeded → success toast + callback.
		expect(onupdated).toHaveBeenCalledTimes(1);

		unmount(component);
		host.remove();
	});
});
