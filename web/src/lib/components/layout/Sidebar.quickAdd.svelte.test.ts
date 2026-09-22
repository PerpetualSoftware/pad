// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3115 — the sidebar quick-add dialog must not lose a title the user
// typed. The e2e spec (bug-3115-refused-title-keeps-text.spec.ts) covers the
// refusal paths against the real server; this pins the interleaving codex
// round 1 found: dismiss the dialog and reopen it on the SAME collection while
// the first create is still in flight. The old response used to recognise
// "its" dialog by collection, so it closed the new one and cleared the new
// text.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

const pending = vi.hoisted(() => ({
	resolve: null as null | ((v: unknown) => void),
	reject: null as null | ((e: unknown) => void),
}));

vi.mock('$lib/api/client', () => ({
	api: {
		health: vi.fn(async () => ({ version: 'dev', commit: 'abcdef0' })),
		collections: { list: vi.fn(async () => []), update: vi.fn() },
		items: {
			create: vi.fn(
				() =>
					new Promise((resolve, reject) => {
						pending.resolve = resolve;
						pending.reject = reject;
					}),
			),
		},
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn(), beforeNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { current: { slug: 'ws', owner_username: 'u', is_guest: false } },
}));
// A $state-backed double: see the fixture for why a plain array is not one.
vi.mock('$lib/stores/collections.svelte', async () => await import('./sidebarQuickAdd.fixture.svelte'));

import Sidebar from './Sidebar.svelte';
import { uiStore } from '$lib/stores/ui.svelte';

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

async function type(text: string) {
	await fireEvent.input(input(), { target: { value: text } });
}

beforeEach(() => {
	cleanup();
	pending.resolve = pending.reject = null;
});

describe('Sidebar quick-add — an in-flight create belongs to ONE opening of the dialog (BUG-3115)', () => {
	for (const outcome of ['succeeds', 'is refused'] as const) {
		it(`dismissed and reopened on the same collection, the old create ${outcome} without touching the new text`, async () => {
			render(Sidebar);
			await open();
			await type('first title');
			await fireEvent.keyDown(input(), { key: 'Enter' });
			// PREMISE: the create is in flight.
			expect(pending.resolve, 'no create was sent').not.toBeNull();

			await fireEvent.keyDown(input(), { key: 'Escape' });
			await tick();
			expect(document.querySelector('.quick-add-modal'), 'Escape did not dismiss').toBeNull();
			await open();
			await type('second title');

			if (outcome === 'succeeds') pending.resolve!({ id: 'i1', slug: 'first-title', item_number: 1 });
			else pending.reject!(new Error('Title is too long: 300 characters, maximum 255'));
			await tick();
			await tick();

			expect(document.querySelector('.quick-add-modal'), 'the new dialog was closed by the old create').not.toBeNull();
			expect(input().value, 'the new text was cleared by the old create').toBe('second title');
			expect(document.querySelector('#quick-add-error'), 'the old refusal was shown on the new dialog').toBeNull();
		});
	}
});
