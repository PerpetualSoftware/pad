// BUG-3244 ruling 1: the pending-edits dialog is the main door that tells a
// user what to do about edits their write would replace. For edits an editor
// upgrade SET ASIDE, opening the item does not store them, so the set-aside
// copy must never say it will. Every kind is rendered for BOTH reasons, so a
// branch that never fires (or fires for both) fails.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { onIdentityChange: () => {}, identityFence: () => () => true, user: null },
}));

const { pendingEditsDialog } = await import('$lib/stores/pendingEditsDialog.svelte');
const { default: PendingEditsDialog } = await import('./PendingEditsDialog.svelte');

let root: HTMLElement | null = null;
let instance: ReturnType<typeof mount> | null = null;

afterEach(() => {
	pendingEditsDialog.abandonAll();
	flushSync();
	if (instance) unmount(instance);
	root?.remove();
	root = null;
	instance = null;
});

function renderFor(kind: 'save' | 'duplicate' | 'excerpt', reason: 'pending' | 'set_aside'): string {
	root = document.body.appendChild(document.createElement('div'));
	instance = mount(PendingEditsDialog, { target: root });
	void pendingEditsDialog.request('TASK-7', kind, reason);
	flushSync();
	return document.body.textContent ?? '';
}

describe('PendingEditsDialog', () => {
	for (const kind of ['save', 'duplicate', 'excerpt'] as const) {
		it(`${kind}: set-aside copy names the upgrade and never offers opening the item`, () => {
			const text = renderFor(kind, 'set_aside');
			expect(text).toMatch(/earlier editor version|editor upgrade/);
			expect(text).toMatch(/TASK-7/);
			expect(text).not.toMatch(/open tab|Open the item|opening the item so/i);
		});

		it(`${kind}: pending copy is unchanged`, () => {
			const text = renderFor(kind, 'pending');
			expect(text).toMatch(/open tab/);
			expect(text).not.toMatch(/earlier editor version/);
		});
	}

	it('save: the set-aside choice is to discard, not to overwrite a tab', () => {
		const text = renderFor('save', 'set_aside');
		expect(text).toMatch(/Discard them/);
		expect(text).toMatch(/Keep those edits/);
	});
});
