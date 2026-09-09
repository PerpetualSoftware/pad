import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';

/**
 * BUG-2978 — the owner-only settings tab was lost whenever the workspace
 * permission went known -> unknown -> known.
 *
 * `workspaceStore.setCurrent` clears `currentMembership` to null before `/me`
 * resolves, and the permission helpers treat unknown as no-access by design.
 * The settings route calls `setCurrent` twice per load (workspace layout, then
 * the page's own `load()`), so an owner sees `canEditWorkspace` read
 * true -> false -> true. During the false window the Danger Zone tab left the
 * tab set, the hash-restoration effect's snap-back moved `activeTab` off it,
 * and `pendingHash` had already been consumed — so nothing restored it.
 *
 * This test drives that sequence DETERMINISTICALLY by controlling when each
 * `/me` resolves, which is the reason it lives here rather than in e2e: on the
 * e2e fixture both `setCurrent` calls land inside a single unresolved window,
 * so the flicker never occurs and the spec there passes on a broken build. The
 * browser measurement against a real workspace (0/10 deep links before, 40/40
 * after) is on the trail; this is the part that fails in CI without the fix.
 */

const meCalls: Array<(value: unknown) => void> = [];
const meRejects: Array<(reason: unknown) => void> = [];

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
			me: vi.fn(() => new Promise((resolve, reject) => {
				meCalls.push(resolve);
				meRejects.push(reject);
			})),
			list: vi.fn(async () => []),
		},
		collections: { list: vi.fn(async () => []) },
		members: { list: vi.fn(async () => ({ members: [], invitations: [] })) },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };

/** Resolve the Nth outstanding `/me`, waiting for it to have been issued. */
async function resolveMe(index: number, value: unknown) {
	await waitFor(() => expect(meCalls.length).toBeGreaterThan(index));
	meCalls[index](value);
}

describe('BUG-2978: settings permissions survive the /me window', () => {
	beforeEach(() => {
		meCalls.length = 0;
		meRejects.length = 0;
		page.params = { username: 'dave', workspace: 'ws' };
		window.location.hash = '#danger';
	});

	afterEach(() => {
		window.location.hash = '';
		// Deliberately NO `vi.resetModules()`: it hands the second test a fresh
		// module graph including a second copy of the Svelte runtime, whose
		// `$effect` does not recognise the first copy's component context, and
		// the remount dies with `effect_orphan`. The store is a module-scoped
		// singleton, and each test re-establishes its state through `setCurrent`.
	});

	it('keeps the deep-linked owner-only tab selected when membership goes known -> unknown -> known', async () => {
		render(SettingsPage);

		// First /me resolves as owner: the Danger Zone tab appears and the
		// pending hash is applied.
		await resolveMe(0, OWNER);
		await waitFor(() => {
			expect(screen.getByRole('tab', { name: /Danger Zone/ })).toHaveAttribute(
				'aria-selected',
				'true',
			);
		});

		// A SECOND setCurrent — what the page's own load() does after the
		// layout's — clears membership to null before its /me resolves. This is
		// the window the bug lived in.
		const second = workspaceStore.setCurrent('ws');
		await waitFor(() => expect(workspaceStore.currentMembership).toBeNull());

		await resolveMe(1, OWNER);
		await second;

		// Non-vacuity: the tab must still exist, or "not selected" would be the
		// correct answer and this assertion would prove nothing.
		const danger = await screen.findByRole('tab', { name: /Danger Zone/ });
		expect(danger).toHaveAttribute('aria-selected', 'true');
	});

	it('drops owner-only chrome when membership becomes a definitive denial', async () => {
		// The complement of the test above, and the reason the cache is gated on
		// `membershipKnown` rather than on `currentMembership !== null` (codex
		// round 1): null means BOTH "not fetched yet" and "no access". A cache
		// that ignores every null holds the last good answer forever, so an owner
		// removed from the workspace — or a `/me` that 403s — would keep the
		// owner-only tab and the delete controls on screen indefinitely.
		render(SettingsPage);

		await resolveMe(0, OWNER);
		await waitFor(() => {
			expect(screen.getByRole('tab', { name: /Danger Zone/ })).toBeInTheDocument();
		});

		// Now the answer changes to "no access", delivered the way the store
		// delivers it: a rejected `/me`, which leaves membership null.
		const second = workspaceStore.setCurrent('ws');
		await waitFor(() => expect(meCalls.length).toBeGreaterThan(1));
		meRejects[1]?.(new Error('403'));
		await second;

		await waitFor(() => {
			expect(screen.queryByRole('tab', { name: /Danger Zone/ })).not.toBeInTheDocument();
		});
	});
});
