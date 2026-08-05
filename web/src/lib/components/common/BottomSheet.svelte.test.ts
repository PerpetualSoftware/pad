// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`). Covers
// BottomSheet.svelte's focus behavior (BUG-2130): move focus INTO the sheet on
// open, trap Tab within it, and restore focus to the trigger on close. The
// Tab-cycle *math* lives in — and is exhaustively tested by — paneFocus.ts
// (`nextTrapTarget` / `paneFocusables`); here we assert the component wires it
// up and the open/close focus bookkeeping.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';
import BottomSheet from './BottomSheet.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

// Two focusable controls so the Tab-trap wrap has a first/last to cycle between.
const bodySnippet = createRawSnippet(() => ({
	render: () =>
		`<div><button id="first-btn" type="button">First</button><button id="last-btn" type="button">Last</button></div>`
}));

function baseProps(overrides: Record<string, unknown> = {}) {
	return {
		open: true,
		onclose: vi.fn(),
		title: 'Sheet',
		children: bodySnippet,
		...overrides
	};
}

function getSheet(): HTMLElement {
	const el = document.querySelector('.bs-sheet');
	if (!el) throw new Error('.bs-sheet not found');
	return el as HTMLElement;
}

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('BottomSheet.svelte', () => {
	it('renders a labelled role="dialog" and moves focus onto the panel on open', async () => {
		render(BottomSheet, { props: baseProps({ open: true }) });
		await tick();
		flushSync();

		const sheet = getSheet();
		expect(sheet.getAttribute('role')).toBe('dialog');
		expect(sheet.getAttribute('aria-modal')).toBe('true');
		// Focus moved into the sheet (onto the tabindex=-1 panel) rather than
		// staying on whatever triggered it.
		expect(document.activeElement).toBe(sheet);
	});

	it('is not rendered while closed', async () => {
		render(BottomSheet, { props: baseProps({ open: false }) });
		await tick();
		flushSync();
		expect(document.querySelector('.bs-sheet')).toBeNull();
	});

	it('fires onclose on Escape', async () => {
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('fires onclose on a backdrop (overlay) click', async () => {
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		const overlay = document.querySelector('.bs-overlay') as HTMLElement;
		overlay.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('traps Tab: forward Tab off the last control wraps to the first', async () => {
		// jsdom has no layout, so paneFocusables' default visibility check
		// (offsetParent / getClientRects) would filter everything out. Make the
		// controls report as on-screen for this assertion.
		vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([
			{ width: 1, height: 1 } as DOMRect
		] as unknown as DOMRectList);

		// No title → no header close button, so the two body buttons are the
		// only focusables and are unambiguously first/last.
		render(BottomSheet, { props: baseProps({ open: true, title: undefined }) });
		await tick();
		flushSync();

		const first = document.getElementById('first-btn') as HTMLButtonElement;
		const last = document.getElementById('last-btn') as HTMLButtonElement;
		last.focus();
		expect(document.activeElement).toBe(last);

		const evt = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true });
		window.dispatchEvent(evt);

		expect(document.activeElement).toBe(first);
		expect(evt.defaultPrevented).toBe(true);
	});

	it('traps Tab: Shift+Tab off the first control wraps to the last', async () => {
		vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([
			{ width: 1, height: 1 } as DOMRect
		] as unknown as DOMRectList);

		render(BottomSheet, { props: baseProps({ open: true, title: undefined }) });
		await tick();
		flushSync();

		const first = document.getElementById('first-btn') as HTMLButtonElement;
		const last = document.getElementById('last-btn') as HTMLButtonElement;
		first.focus();

		const evt = new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, cancelable: true });
		window.dispatchEvent(evt);

		expect(document.activeElement).toBe(last);
		expect(evt.defaultPrevented).toBe(true);
	});

	it('a sheet containing a nested open sheet stays out of Escape (only the inner closes)', async () => {
		// Reproduces the nested case (Quick Actions sheet → emoji-picker sheet):
		// the inner sheet renders DOM-nested inside the outer's content. Both
		// listen on window, so a naive handler would close BOTH on one Escape.
		// Here the outer's children include a nested `.bs-sheet`, so the outer
		// must NOT fire its onclose — the innermost sheet owns Escape.
		const nested = createRawSnippet(() => ({
			render: () =>
				`<div><div class="bs-overlay"><div class="bs-sheet" role="dialog" aria-modal="true" tabindex="-1"><button type="button">Inner</button></div></div></div>`
		}));
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose, children: nested }) });
		await tick();
		flushSync();

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		// Outer sheet stayed out: its onclose did not fire (the inner sheet, were
		// it a real BottomSheet, would have closed via its own window listener).
		expect(onclose).not.toHaveBeenCalled();
	});

	it('only the topmost of sibling sheets handles Escape (last in document order)', async () => {
		// Two open sheets that do NOT contain each other (sibling topology). Both
		// listen on window; only the one painted on top (last in document order at
		// the shared z-index) should act on Escape.
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		// A later sibling sheet appended after this one → this one is no longer
		// frontmost and must stay out of Escape.
		const sibling = document.createElement('div');
		sibling.className = 'bs-sheet';
		document.body.appendChild(sibling);

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		expect(onclose).not.toHaveBeenCalled();

		// Remove the sibling → this sheet is frontmost again and handles Escape.
		sibling.remove();
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('restores focus to the previously-focused trigger on close', async () => {
		const trigger = document.createElement('button');
		trigger.type = 'button';
		document.body.appendChild(trigger);
		trigger.focus();
		expect(document.activeElement).toBe(trigger);

		const { rerender } = render(BottomSheet, { props: baseProps({ open: true }) });
		await tick();
		flushSync();
		// Focus is now inside the sheet.
		expect(document.activeElement).toBe(getSheet());

		await rerender(baseProps({ open: false }));
		await tick();
		flushSync();

		expect(document.activeElement).toBe(trigger);
	});
});

// ── TASK-2430: front layer wins ──────────────────────────────────────────
//
// `isFrontmostSheet()` only arbitrates between SHEETS. A body-portaled viewer
// sits above every sheet and is invisible to it, so the sheet also has to
// consult the shared arbitration helper. Each case is paired with an
// EMPTY-STACK REGRESSION: with no lease held, Escape and the Tab trap must
// behave exactly as they did before.
describe('BottomSheet.svelte — defers to a frontmost viewer (TASK-2430)', () => {
	/** A body-portaled viewer root, exactly the shape `acquire` expects. */
	function mountViewer(): HTMLElement {
		const root = document.createElement('div');
		root.className = 'attachment-viewer';
		root.setAttribute('role', 'dialog');
		document.body.appendChild(root);
		return root;
	}

	afterEach(() => {
		__resetViewerBackdropForTests();
	});

	it('EMPTY-STACK REGRESSION: still closes on Escape with no lease held', async () => {
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('declines Escape while a viewer lease is frontmost, and takes it back on release', async () => {
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		const lease = acquire(mountViewer());
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		expect(onclose).not.toHaveBeenCalled();

		lease.release();
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('OWNER ARGUMENT: an Escape ORIGINATING inside the viewer is still declined', async () => {
		// The discriminating case for "the argument is the SURFACE ASKING TO ACT,
		// not `event.target`". The blocked test above dispatches on `window`,
		// where the target is outside the viewer too, so it would ALSO pass with
		// the wrong argument. This is the realistic press — the viewer holds
		// focus, so the target is a viewer control, and an `e.target` owner would
		// answer "not blocked" and close the sheet underneath it.
		const onclose = vi.fn();
		render(BottomSheet, { props: baseProps({ open: true, onclose }) });
		await tick();
		flushSync();

		const viewer = mountViewer();
		const btn = document.createElement('button');
		viewer.appendChild(btn);
		acquire(viewer);

		btn.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('EMPTY-STACK REGRESSION: still traps Tab with no lease held, and stops trapping under a viewer', async () => {
		// Same visibility stub the positive Tab tests above use: without it jsdom
		// reports every control as invisible, `paneFocusables` returns [], and the
		// trap falls back to focusing the sheet container — which would still
		// satisfy the assertions below without ever exercising the real wrap.
		vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([
			{ width: 1, height: 1 } as DOMRect
		] as unknown as DOMRectList);
		render(BottomSheet, { props: baseProps({ open: true }) });
		await tick();
		flushSync();

		const last = document.getElementById('last-btn') as HTMLButtonElement;
		last.focus();
		const trapped = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true });
		window.dispatchEvent(trapped);
		// The trap wrapped focus to the sheet's FIRST focusable — the header close
		// button — and consumed the key.
		expect(trapped.defaultPrevented).toBe(true);
		expect(document.activeElement).toBe(getSheet().querySelector('.bs-close'));

		// Under a viewer the sheet must NOT pull focus back into itself — the
		// viewer runs its own trap.
		acquire(mountViewer());
		last.focus();
		const passed = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true });
		window.dispatchEvent(passed);
		expect(passed.defaultPrevented).toBe(false);
		expect(document.activeElement).toBe(last);
	});
});
