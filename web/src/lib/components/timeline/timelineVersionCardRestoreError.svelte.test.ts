import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import TimelineVersionCard from './TimelineVersionCard.svelte';
import type { Version } from '$lib/types';

// TASK-2205 (audit C37): a restore that fails for any reason other than
// pending edits (BUG-3031) used to be rethrown from the click handler: no
// toast, no inline line, an unhandled rejection, and the card snapped back as
// if nothing had been asked. It now says so on the card, with the server's
// message, and does not report a restore that did not happen.

const { restore } = vi.hoisted(() => ({ restore: vi.fn() }));

vi.mock('$lib/api/client', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/api/client')>();
	return {
		...actual,
		api: {
			...actual.api,
			versions: { ...actual.api.versions, restore, get: vi.fn(async () => ({ content: '' })),
			diff: vi.fn(async () => ({ before: 'old', after: 'now' })) },
		},
	};
});

const version = {
	id: 'v1', item_id: 'i1', content: 'hello', is_diff: false, change_summary: 'edited',
	created_by: 'user', source: 'web', created_at: '2026-07-20T00:00:00Z',
} as unknown as Version;

async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
	flushSync();
}

describe('TimelineVersionCard: a failed restore says so (TASK-2205)', () => {
	let root: HTMLElement;
	let instance: ReturnType<typeof mount>;
	let onRestore: ReturnType<typeof vi.fn>;
	const unhandled: unknown[] = [];
	const onUnhandled = (e: PromiseRejectionEvent) => { unhandled.push(e.reason); e.preventDefault(); };

	beforeEach(() => {
		restore.mockReset();
		unhandled.length = 0;
		window.addEventListener('unhandledrejection', onUnhandled);
		onRestore = vi.fn();
		root = document.body.appendChild(document.createElement('div'));
		instance = mount(TimelineVersionCard, {
			target: root,
			props: { version, wsSlug: 'ws', itemSlug: 'ITEM-1', currentContent: 'now', onRestore },
		});
		flushSync();
		(root.querySelector('.show-changes') as HTMLButtonElement).click();
		flushSync();
		(root.querySelector('.btn-restore') as HTMLButtonElement).click();
		flushSync();
	});

	afterEach(() => {
		window.removeEventListener('unhandledrejection', onUnhandled);
		unmount(instance);
		root.remove();
	});

	const confirm = () => root.querySelector('.btn-restore-confirm') as HTMLButtonElement;
	const alertText = () => root.querySelector('.restore-error')?.textContent?.trim();

	it('shows the server message inline, reports no restore, and leaves nothing unhandled', async () => {
		restore.mockRejectedValueOnce(new Error('You do not have permission to edit this item'));
		confirm().click();
		await settle();
		expect(alertText()).toBe('Restore failed: You do not have permission to edit this item');
		expect(root.querySelector('.restore-error')?.getAttribute('role')).toBe('alert');
		expect(onRestore).not.toHaveBeenCalled();
		await new Promise((r) => setTimeout(r, 0));
		expect(unhandled).toEqual([]);
	});

	it('a retry that lands clears the error and restores', async () => {
		restore.mockRejectedValueOnce(new Error('boom'));
		confirm().click();
		await settle();
		expect(alertText()).toBe('Restore failed: boom');
		restore.mockResolvedValueOnce({ id: 'i1', slug: 'ITEM-1' });
		confirm().click();
		await settle();
		expect(alertText()).toBeUndefined();
		expect(onRestore).toHaveBeenCalledTimes(1);
	});
});
