import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';

/**
 * TASK-2204 (audit C70): "Enable all" in a trigger group wrote each convention
 * back to back, swallowed every failure in an empty catch, kept the failed
 * rows' optimistic state and always toasted the full count. A failure is now
 * reverted on its own row and counted in the toast, and the writes are paced.
 */
const auth = vi.hoisted(() => ({
	get identityEpoch() { return 0; },
	get userId() { return 'u1'; },
	get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
	identityFence() { return () => true; },
	onIdentityChange() { return () => {}; },
	clear() {}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

const writes = vi.hoisted(() => [] as { slug: string; at: number }[]);
const failing = vi.hoisted(() => new Set<string>());
const held = vi.hoisted(() => new Map<string, { release: (ok: boolean) => void }>());
const holdNext = vi.hoisted(() => new Set<string>());
const conv = (n: number) => ({
	id: 'c' + n, slug: 'c' + n, title: 'Rule ' + n, content: '', collection_id: 'conventions', collection_slug: 'conventions',
	item_number: n, fields: JSON.stringify({ status: 'disabled', trigger: 'on-commit', scope: 'all', priority: 'should' })
});
vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			listByCollection: vi.fn(async () => [conv(1), conv(2), conv(3)]),
			update: vi.fn(async (_ws: string, slug: string) => {
				writes.push({ slug, at: Date.now() });
				if (holdNext.has(slug)) {
					holdNext.delete(slug);
					await new Promise<void>((res, rej) => held.set(slug, { release: (ok) => (ok ? res() : rej(new Error('held write failed'))) }));
				}
				if (failing.has(slug)) throw new Error('rate limited');
				return {};
			})
		},
		collections: {
			list: vi.fn(async () => [{ id: 'conventions', slug: 'conventions', name: 'Conventions', workspace_id: 'w1', schema: '{"fields":[]}', settings: '{}', traits: JSON.stringify({ artifact_kind: { kind: 'convention' } }) }]),
			get: vi.fn(async (_ws: string, slug: string) => ({ id: slug, slug, name: slug, schema: '{"fields":[]}', settings: '{}' }))
		}
	},
	isPlanLimitError: () => false
}));
vi.mock('$lib/collections/canCreateIn', () => ({ canCreateIn: () => true }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));
import { page } from '$app/state';
import { collectionStore } from '$lib/stores/collections.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { toastStore } from '$lib/stores/toast.svelte';
import ConventionsPage from './+page.svelte';

afterEach(() => {
	cleanup();
	writes.length = 0;
	failing.clear();
	held.clear();
	holdNext.clear();
	vi.restoreAllMocks();
});

async function openPage() {
	vi.spyOn(workspaceStore, 'canEditItem').mockReturnValue(true);
	await collectionStore.loadCollections('ws1');
	page.params = { username: 'dave', workspace: 'ws1' };
	page.url = new URL('http://localhost/dave/ws1/conventions');
	render(ConventionsPage);
	return screen.findByRole('button', { name: 'Enable all' });
}

describe('Conventions: Enable all (TASK-2204)', () => {
	it('reverts a failed row, reports the true count, and paces the writes', async () => {
		const toast = vi.spyOn(toastStore, 'show');
		failing.add('c2');
		await fireEvent.click(await openPage());
		await waitFor(() => {
			if (!toast.mock.calls.length) throw new Error('no toast yet');
		}, { timeout: 3000 });

		expect(writes.map((w) => w.slug)).toEqual(['c1', 'c2', 'c3']);
		for (let i = 1; i < writes.length; i++) expect(writes[i].at - writes[i - 1].at).toBeGreaterThanOrEqual(200);
		expect(toast).toHaveBeenCalledWith('2 enabled, 1 failed. Try again for the rest.', 'error');
		expect(toast).not.toHaveBeenCalledWith(expect.stringMatching(/^3 conventions/), expect.anything());
		// the failed rule is shown as still off; the other two as on
		expect(screen.getAllByRole('button', { name: 'Disable convention' })).toHaveLength(2);
		expect(screen.getAllByRole('button', { name: 'Enable convention' })).toHaveLength(1);
	});

	it('says so when every write lands', async () => {
		const toast = vi.spyOn(toastStore, 'show');
		await fireEvent.click(await openPage());
		await waitFor(() => {
			if (!toast.mock.calls.length) throw new Error('no toast yet');
		}, { timeout: 3000 });
		expect(toast).toHaveBeenCalledWith('3 conventions enabled', 'success');
		expect(screen.getAllByRole('button', { name: 'Disable convention' })).toHaveLength(3);
	});

	it('a row cannot be toggled by hand while a bulk run owns it (codex r2)', async () => {
		holdNext.add('c1');
		await fireEvent.click(await openPage());
		await waitFor(() => { if (!held.has('c1')) throw new Error('bulk write not held yet'); });
		const sw = screen.getByRole('button', { name: 'Disable convention' }) as HTMLButtonElement;
		expect(sw.disabled).toBe(true);
		await fireEvent.click(sw);
		expect(writes.filter((w) => w.slug === 'c1')).toHaveLength(1);
		held.get('c1')!.release(true);
		await waitFor(() => {
			if ((screen.getAllByRole('button', { name: 'Disable convention' })[0] as HTMLButtonElement).disabled) throw new Error('still busy');
		}, { timeout: 3000 });
	});

	it('a run whose last write lands after navigating to another workspace toasts nothing there (codex r1)', async () => {
		const toast = vi.spyOn(toastStore, 'show');
		holdNext.add('c3');
		await fireEvent.click(await openPage());
		await waitFor(() => { if (!held.has('c3')) throw new Error('last write not held yet'); }, { timeout: 3000 });
		page.params = { username: 'dave', workspace: 'ws2' };
		held.get('c3')!.release(true);
		await new Promise((r) => setTimeout(r, 50));
		expect(toast.mock.calls.filter((c) => /conventions? (enabled|disabled)|failed/.test(String(c[0])))).toEqual([]);
	});
});
