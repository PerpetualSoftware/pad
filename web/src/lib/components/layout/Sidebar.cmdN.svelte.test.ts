// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3258 — Cmd-N opens quick-add only on a collection the caller may create
// in. A press while membership is still loading is consumed and does nothing;
// holding it was withdrawn (see the comment on the effect in Sidebar.svelte).
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []), update: vi.fn() },
		items: { create: vi.fn() },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn(), beforeNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', async () => await import('./sidebarCmdN.fixture.svelte'));
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));

import Sidebar from './Sidebar.svelte';
import { uiStore } from '$lib/stores/ui.svelte';
import { ws } from './sidebarCmdN.fixture.svelte';

const dialog = () => document.querySelector('.quick-add-modal');

async function settle() {
	await tick();
	await tick();
}

beforeEach(() => {
	cleanup();
	uiStore.clearQuickAddRequest();
	ws.current = { slug: 'ws-a', owner_username: 'u', is_guest: false };
	ws.membershipKnown = false;
	ws.editable = true;
});

describe('Sidebar Cmd-N — only on a creatable collection (BUG-3258)', () => {
	it('control: with membership known, Cmd-N opens the dialog', async () => {
		ws.membershipKnown = true;
		render(Sidebar);
		uiStore.requestQuickAdd();
		await settle();
		expect(dialog()).not.toBeNull();
	});

	it('a press while membership loads is consumed and opens nothing, even after it settles', async () => {
		render(Sidebar);
		uiStore.requestQuickAdd();
		await settle();
		expect(uiStore.quickAddRequested).toBe(false);
		expect(dialog()).toBeNull();

		ws.membershipKnown = true;
		await settle();
		expect(dialog()).toBeNull();

		// The next press, with membership known, opens it.
		uiStore.requestQuickAdd();
		await settle();
		expect(dialog()).not.toBeNull();
	});

	it('with membership known and nothing creatable, Cmd-N does nothing', async () => {
		ws.membershipKnown = true;
		ws.editable = false;
		render(Sidebar);
		uiStore.requestQuickAdd();
		await settle();
		expect(dialog()).toBeNull();
	});
});
