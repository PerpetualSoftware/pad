import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import TimelineVersionCard from './TimelineVersionCard.svelte';
import type { Version } from '$lib/types';

// BUG-3031: the server refuses a version restore with `content_pending_flush`
// while the item holds edits another editor session has not saved, because the
// restore would prune them and no version would keep them. The card must not
// treat that as a failure to swallow or a success: it asks again, naming the
// loss, and only a second yes sends `overwrite_pending_edits`.
//
// The real PadApiError and predicate are kept (importActual) so the test
// exercises the same classification the product runs; only the network call is
// stubbed.

const { restore } = vi.hoisted(() => ({ restore: vi.fn() }));

vi.mock('$lib/api/client', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/api/client')>();
	return {
		...actual,
		api: {
			...actual.api,
			versions: { ...actual.api.versions, restore, get: vi.fn(async () => ({ content: '' })) },
		},
	};
});

const { PadApiError } = await import('$lib/api/client');

const version = {
	id: 'v1',
	item_id: 'i1',
	content: 'hello',
	is_diff: false,
	change_summary: 'edited',
	created_by: 'user',
	source: 'web',
	created_at: '2026-07-20T00:00:00Z',
} as unknown as Version;

function pendingRefusal() {
	return new PadApiError({
		code: 'content_pending_flush',
		message: 'ITEM-1 has unsaved edits from another editor session',
		details: { ref: 'ITEM-1', pending_rows: 1 },
	});
}

async function settle() {
	for (let i = 0; i < 5; i++) {
		await Promise.resolve();
	}
	flushSync();
}

describe('TimelineVersionCard pending-edits refusal (BUG-3031)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;
	let onRestore: ReturnType<typeof vi.fn>;

	beforeEach(() => {
		restore.mockReset();
		onRestore = vi.fn();
		root = document.body.appendChild(document.createElement('div'));
		instance = mount(TimelineVersionCard, {
			target: root,
			props: { version, wsSlug: 'ws', itemSlug: 'ITEM-1', currentContent: 'now', onRestore },
		});
		flushSync();
		(root.querySelector('.card-header') as HTMLButtonElement).click();
		flushSync();
		(root.querySelector('.btn-restore') as HTMLButtonElement).click();
		flushSync();
	});

	afterEach(() => {
		if (instance) unmount(instance);
		root?.remove();
		instance = null;
		root = null;
	});

	function confirmButton(): HTMLButtonElement {
		return root!.querySelector('.btn-restore-confirm') as HTMLButtonElement;
	}

	it('asks again, naming the loss, and sends the override only on the second yes', async () => {
		restore.mockRejectedValueOnce(pendingRefusal());
		restore.mockResolvedValueOnce({ id: 'i1' });

		confirmButton().click();
		await settle();

		expect(restore).toHaveBeenCalledTimes(1);
		expect(restore.mock.calls[0]).toEqual(['ws', 'ITEM-1', 'v1']);
		expect(onRestore).not.toHaveBeenCalled();
		const warning = root!.querySelector('.confirm-warning');
		expect(warning?.textContent).toMatch(/unsaved edits/);
		expect(warning?.textContent).toMatch(/discard/);
		expect(confirmButton().textContent).toMatch(/Discard edits and restore/);

		confirmButton().click();
		await settle();

		expect(restore).toHaveBeenCalledTimes(2);
		expect(restore.mock.calls[1]).toEqual(['ws', 'ITEM-1', 'v1', { overwritePendingEdits: true }]);
		expect(onRestore).toHaveBeenCalledTimes(1);
		expect(root!.querySelector('.confirm-warning')).toBeNull();
	});

	it('cancel after the refusal sends nothing and forgets the override', async () => {
		restore.mockRejectedValueOnce(pendingRefusal());
		confirmButton().click();
		await settle();
		expect(root!.querySelector('.confirm-warning')).not.toBeNull();

		(root!.querySelector('.btn-cancel') as HTMLButtonElement).click();
		flushSync();
		(root!.querySelector('.btn-restore') as HTMLButtonElement).click();
		flushSync();

		expect(root!.querySelector('.confirm-warning')).toBeNull();
		restore.mockResolvedValueOnce({ id: 'i1' });
		confirmButton().click();
		await settle();
		// A fresh confirm is a plain restore again: the override is never sticky.
		expect(restore.mock.calls[1]).toEqual(['ws', 'ITEM-1', 'v1']);
	});

	it('a plain success never shows the warning or sends the override', async () => {
		restore.mockResolvedValueOnce({ id: 'i1' });
		confirmButton().click();
		await settle();
		expect(restore.mock.calls[0]).toEqual(['ws', 'ITEM-1', 'v1']);
		expect(onRestore).toHaveBeenCalledTimes(1);
		expect(root!.querySelector('.confirm-warning')).toBeNull();
	});
});
