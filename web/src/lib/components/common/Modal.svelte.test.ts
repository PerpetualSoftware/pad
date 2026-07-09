// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`). jsdom
// lacks the native <dialog> top-layer methods, so `src/test/setup-jsdom.ts`
// polyfills `showModal`/`close` to reflect the `open` attribute — enough to
// exercise Modal.svelte's open/close + focus-restore logic.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';
import Modal from './Modal.svelte';

// A snippet the parent would normally write as <Modal> children. One root
// element (required by createRawSnippet) containing the labelled heading and a
// focusable control used by the focus-restore test.
const bodySnippet = createRawSnippet(() => ({
	render: () =>
		`<div><h2 id="modal-title">Dialog heading</h2><button id="inner-btn" type="button">OK</button></div>`
}));

function baseProps(overrides: Record<string, unknown> = {}) {
	return {
		open: true,
		onclose: vi.fn(),
		labelledby: 'modal-title',
		children: bodySnippet,
		...overrides
	};
}

function getDialog(): HTMLDialogElement {
	const el = document.querySelector('dialog.modal');
	if (!el) throw new Error('dialog.modal not found');
	return el as HTMLDialogElement;
}

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('Modal.svelte', () => {
	it('opens on open=true: dialog present, showModal applied, aria + content wired', async () => {
		render(Modal, { props: baseProps({ open: true }) });
		await tick();
		flushSync();

		const dialog = getDialog();
		expect(dialog.open).toBe(true);
		// aria-labelledby points at the heading inside the children.
		expect(dialog.getAttribute('aria-labelledby')).toBe('modal-title');
		// Inner content is gated on `open`, so it renders only while open.
		expect(document.getElementById('modal-title')).not.toBeNull();
	});

	it('closes when open flips to false and tears down the gated content', async () => {
		const { rerender } = render(Modal, { props: baseProps({ open: true }) });
		await tick();
		flushSync();
		expect(getDialog().open).toBe(true);

		await rerender(baseProps({ open: false }));
		await tick();
		flushSync();

		expect(getDialog().open).toBe(false);
		expect(document.getElementById('modal-title')).toBeNull();
	});

	it('fires onclose (and prevents the native close) on Escape/cancel', async () => {
		const onclose = vi.fn();
		render(Modal, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		// Escape on a native <dialog> dispatches a cancelable `cancel` event —
		// the component prevents it and asks the parent to close instead.
		const cancelEvent = new Event('cancel', { cancelable: true });
		getDialog().dispatchEvent(cancelEvent);

		expect(onclose).toHaveBeenCalledTimes(1);
		expect(cancelEvent.defaultPrevented).toBe(true);
	});

	it('fires onclose on a backdrop click (target === dialog)', async () => {
		const onclose = vi.fn();
		render(Modal, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		// A click whose target is the dialog element itself is the backdrop.
		getDialog().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('does NOT close on a backdrop click when closeOnBackdrop=false', async () => {
		const onclose = vi.fn();
		render(Modal, { props: baseProps({ open: true, onclose, closeOnBackdrop: false }) });
		await tick();
		flushSync();

		getDialog().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('restores focus to the previously-focused element on close', async () => {
		const trigger = document.createElement('button');
		trigger.type = 'button';
		trigger.textContent = 'Open';
		document.body.appendChild(trigger);
		trigger.focus();
		expect(document.activeElement).toBe(trigger);

		const { rerender } = render(Modal, { props: baseProps({ open: true }) });
		await tick();
		flushSync();

		// Simulate focus moving into the modal while it's open.
		const inner = document.getElementById('inner-btn') as HTMLButtonElement;
		inner.focus();
		expect(document.activeElement).toBe(inner);

		await rerender(baseProps({ open: false }));
		await tick();
		flushSync();

		// Focus should return to whatever was focused before the modal opened.
		expect(document.activeElement).toBe(trigger);
	});
});
