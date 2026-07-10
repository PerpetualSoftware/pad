// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`) — the
// same setup Modal.svelte.test.ts relies on polyfills showModal/close.
// Covers the migration off the hand-rolled backdrop div onto the shared
// <Modal> primitive: the native dialog drives visibility, aria-labelledby
// points at the visible heading, and the close button asks the parent to
// flip `visible` (single source of truth) rather than closing itself.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';
import KeyboardShortcuts from './KeyboardShortcuts.svelte';

function getDialog(): HTMLDialogElement {
	const el = document.querySelector('dialog.modal');
	if (!el) throw new Error('dialog.modal not found');
	return el as HTMLDialogElement;
}

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('KeyboardShortcuts.svelte', () => {
	it('renders through the shared Modal primitive when visible', async () => {
		render(KeyboardShortcuts, { props: { visible: true, onclose: vi.fn() } });
		await tick();
		flushSync();

		const dialog = getDialog();
		expect(dialog.open).toBe(true);
		// The heading provides the accessible name via aria-labelledby.
		expect(dialog.getAttribute('aria-labelledby')).toBe('keyboard-shortcuts-title');
		expect(document.getElementById('keyboard-shortcuts-title')?.textContent).toBe(
			'Keyboard Shortcuts'
		);
		// The shortcut groups render inside the dialog.
		const text = dialog.textContent ?? '';
		for (const group of ['Global', 'Navigation', 'Item Detail']) {
			expect(text).toContain(group);
		}
	});

	it('stays closed (content torn down) while visible=false', async () => {
		render(KeyboardShortcuts, { props: { visible: false, onclose: vi.fn() } });
		await tick();
		flushSync();

		expect(getDialog().open).toBe(false);
		expect(document.getElementById('keyboard-shortcuts-title')).toBeNull();
	});

	it('close button asks the parent to close instead of closing itself', async () => {
		const onclose = vi.fn();
		render(KeyboardShortcuts, { props: { visible: true, onclose } });
		await tick();
		flushSync();

		const btn = document.querySelector<HTMLButtonElement>('dialog.modal button[aria-label="Close"]');
		expect(btn).not.toBeNull();
		btn?.click();
		expect(onclose).toHaveBeenCalledOnce();
		// `visible` is the single source of truth — until the parent flips it,
		// the dialog itself has not closed.
		expect(getDialog().open).toBe(true);
	});
});
