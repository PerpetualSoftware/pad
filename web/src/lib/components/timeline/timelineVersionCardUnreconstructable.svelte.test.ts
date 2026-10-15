import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import TimelineVersionCard from './TimelineVersionCard.svelte';
import { PadApiError } from '$lib/api/client';
import type { Version } from '$lib/types';

// BUG-3820: a version whose body the history cannot rebuild or verify is
// refused by the diff and restore endpoints (409 version_unreconstructable).
// The card used to read that as "Couldn't load this edit's changes." It now
// says what the refusal means. Restore stays offered either way: the diff also
// reads the NEXT newer row, so a verified, restorable version whose neighbour
// failed is refused by the diff and restored fine (r3 R3-1). The restore's own
// gate decides, and its refusal shows on the card. These legs mock the
// server's answers; the server half is pinned in
// internal/server/bug3820_unverified_version_test.go.

const { diff, restore } = vi.hoisted(() => ({ diff: vi.fn(), restore: vi.fn() }));

vi.mock('$lib/api/client', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/api/client')>();
	return {
		...actual,
		api: {
			...actual.api,
			versions: { ...actual.api.versions, diff, restore, get: vi.fn(async () => ({ content: '' })) },
		},
	};
});

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		identityEpoch: 0,
		userId: 'u1',
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
	},
}));

const version = {
	id: 'v1', item_id: 'i1', content: '', is_diff: true, change_summary: 'edited',
	created_by: 'user', source: 'web', created_at: '2026-07-20T00:00:00Z',
} as unknown as Version;

async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
	flushSync();
}

describe('TimelineVersionCard: an unreconstructable version (BUG-3820)', () => {
	let root: HTMLElement;
	let instance: ReturnType<typeof mount>;

	beforeEach(() => {
		diff.mockReset();
		restore.mockReset();
		root = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		unmount(instance);
		root.remove();
	});

	async function expand(onRestore = vi.fn()) {
		instance = mount(TimelineVersionCard, {
			target: root,
			props: { version, wsSlug: 'ws', itemSlug: 'ITEM-1', currentContent: 'now', onRestore },
		});
		flushSync();
		(root.querySelector('.show-changes') as HTMLButtonElement).click();
		await settle();
	}

	async function confirmRestore() {
		(root.querySelector('.btn-restore') as HTMLButtonElement).click();
		flushSync();
		(root.querySelector('.btn-restore-confirm') as HTMLButtonElement).click();
		await settle();
	}

	it('says the version cannot be rebuilt and still offers restore', async () => {
		diff.mockRejectedValue(new PadApiError({ code: 'version_unreconstructable', message: 'refused' }));
		await expand();
		expect(diff).toHaveBeenCalledTimes(1);
		expect(root.querySelector('.diff-status.unreconstructable')?.textContent).toContain("changes can't be shown");
		expect(root.textContent).not.toContain("Couldn't load");
		expect(root.querySelector('.btn-restore')).not.toBeNull();
	});

	// R3-1 (a), as the server answers it: the diff is refused, the restore
	// succeeds (a verified V whose NEWER neighbour failed).
	it('a verified version whose newer neighbour failed is restored', async () => {
		diff.mockRejectedValue(new PadApiError({ code: 'version_unreconstructable', message: 'refused' }));
		const updated = { id: 'i1', content: 'restored' };
		restore.mockResolvedValue(updated);
		const onRestore = vi.fn();
		await expand(onRestore);
		await confirmRestore();
		expect(restore).toHaveBeenCalledWith('ws', 'ITEM-1', 'v1');
		expect(onRestore).toHaveBeenCalledWith(updated);
		expect(root.querySelector('.restore-error')).toBeNull();
	});

	// R3-1 (b), as the server answers it: the diff and the restore are both
	// refused (V itself is unverified). The card shows the server's message
	// instead of snapping back.
	it('an unverified version: the restore is refused and says why', async () => {
		diff.mockRejectedValue(new PadApiError({ code: 'version_unreconstructable', message: 'refused' }));
		restore.mockRejectedValue(
			new PadApiError({ code: 'version_unreconstructable', message: 'this version cannot be rebuilt from history' })
		);
		const onRestore = vi.fn();
		await expand(onRestore);
		await confirmRestore();
		expect(restore).toHaveBeenCalledTimes(1);
		expect(onRestore).not.toHaveBeenCalled();
		expect(root.querySelector('.restore-error')?.textContent).toBe(
			'Restore failed: this version cannot be rebuilt from history'
		);
	});

	// A collapse and re-expand fetches again (diffPair is still null). When
	// the version resolves this time (its history was repaired, or the card
	// now shows another item's version), the refusal from the first fetch
	// must not linger over the diff and hide Restore.
	it('a later successful fetch clears the refusal', async () => {
		diff.mockRejectedValueOnce(new PadApiError({ code: 'version_unreconstructable', message: 'refused' }));
		diff.mockResolvedValueOnce({ before: 'old', after: 'now' });
		await expand();
		expect(root.querySelector('.diff-status.unreconstructable')).not.toBeNull();
		(root.querySelector('.show-changes') as HTMLButtonElement).click();
		flushSync();
		(root.querySelector('.show-changes') as HTMLButtonElement).click();
		await settle();
		expect(diff).toHaveBeenCalledTimes(2);
		expect(root.querySelector('.diff-status.unreconstructable')).toBeNull();
		expect(root.querySelector('.btn-restore')).not.toBeNull();
	});

	it('any other failure is still a load failure, with restore offered', async () => {
		diff.mockRejectedValue(new PadApiError({ code: 'internal_error', message: 'boom' }));
		await expand();
		expect(root.textContent).toContain("Couldn't load this edit's changes.");
		expect(root.querySelector('.diff-status.unreconstructable')).toBeNull();
		expect(root.querySelector('.btn-restore')).not.toBeNull();
	});
});
