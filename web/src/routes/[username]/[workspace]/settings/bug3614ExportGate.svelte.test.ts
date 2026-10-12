import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';
import { page } from '$app/state';
import SettingsPage from './+page.svelte';

/**
 * BUG-3614 — the settings page showed the workspace bundle download to
 * editors, and the server answered 403. The page now reads the store's
 * canExportWorkspace (owner with unrestricted access), the server's own rule.
 * This drives the real page, so it fails if the page stops reading that
 * getter (CONVE-19: the unit test vouches for the rule, not its binding).
 */

const me = vi.hoisted(() => ({ value: null as unknown }));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({
				id: 'ws1', slug: page.params.workspace, name: 'WS', context: {},
				created_at: '2026-10-01T00:00:00Z'
			})),
			me: vi.fn(async () => me.value),
			list: vi.fn(async () => [])
		},
		collections: { list: vi.fn(async () => []) },
		members: { list: vi.fn(async () => ({ members: [], invitations: [] })) }
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {}
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} }
}));

const base = { collection_grants: [], item_grants: [] };

// Each case uses its own workspace slug: the store is a module singleton and
// keeps an answered workspace's membership across a second setCurrent.
async function renderAs(slug: string, membership: Record<string, unknown>) {
	me.value = { ...base, ...membership };
	page.params = { username: 'dave', workspace: slug };
	render(SettingsPage);
	// The General tab's workspace card has rendered once the name input is up.
	await screen.findByDisplayValue('WS');
}

describe('BUG-3614: the export download follows the server rule', () => {
	beforeEach(() => {
		window.location.hash = '';
	});

	it('is shown to an owner with access to every collection (control)', async () => {
		await renderAs('ws-owner', { role: 'owner', collection_access: 'all' });
		await waitFor(() => {
			expect(screen.getByRole('link', { name: /Download \.tar\.gz/ })).toBeTruthy();
		});
	});

	it('is not shown to an editor', async () => {
		await renderAs('ws-editor', { role: 'editor', collection_access: 'all' });
		// Membership has landed: the editor-visible Theme section is there.
		await screen.findByText('Theme');
		expect(screen.queryByRole('link', { name: /Download \.tar\.gz/ })).toBeNull();
	});

	it('is not shown to an owner scoped to specific collections', async () => {
		await renderAs('ws-scoped', { role: 'owner', collection_access: 'specific' });
		// Owner chrome rendered (the Danger Zone tab), so membership is known.
		await screen.findByRole('tab', { name: /Danger Zone/ });
		expect(screen.queryByRole('link', { name: /Download \.tar\.gz/ })).toBeNull();
	});
});
