// BUG-3039 — a debounced typed edit is cancelled and lost when the PREVIOUS
// keystroke's write echoes back.
//
// The sequence, all inside one 500ms window's worth of typing:
//
//   1. Type `a`. The debounce fires and the parent's onchange sends a PATCH.
//   2. Before it answers, type `b`. The field holds `ab` and arms a new timer.
//   3. The `a` response lands and the parent re-props `value` as `a`.
//   4. The value-track $effect sees `hasPending` and cancels the `ab` timer.
//
// `ab` is never sent and never shown. No ticket is taken for it, so the write
// ordering added in PLAN-2857 U4 cannot see it either — that model orders
// writes that were DISPATCHED, and this one never is.
//
// The effect exists for a real reason: a value arriving from SSE, a collab peer,
// or the parent's own 409 refetch must replace what the field shows. What it
// cannot currently do is tell that case apart from its OWN write echoing back,
// and the two want opposite answers.
//
// This suite mounts FieldEditor DIRECTLY. ItemDetail wraps its fields in
// `{#key itemSlug}` and so remounts them on an item switch — a fact BUG-3039
// originally got wrong, from a stale comment in the component itself — which
// means the item-switch legs below pin the component's own behaviour for a
// caller that does not remount, not a path the pane can reach today.
//
// Written before the fix, per CONVE-29, and confirmed RED against the unfixed
// tree — see the BUG-3039 trail for the run.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';

vi.mock('$lib/api/client', () => ({ api: { search: vi.fn(), items: { create: vi.fn() } } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrapStateFor: vi.fn(() => 'ready'),
		findByIdOrSlug: vi.fn(),
		getByCollection: vi.fn(() => []),
		cursorFor: vi.fn(),
		upsert: vi.fn(),
		scopeEpochFor: vi.fn(),
		pendingResyncFor: vi.fn(),
		resetGenerationFor: vi.fn(),
	},
}));
vi.mock('$lib/stores/localSearch.svelte', () => ({
	localSearch: { search: vi.fn(() => []), epoch: vi.fn(() => 0) },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [], collectionsAreFreshFor: vi.fn(() => true) },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditCollection: vi.fn(() => true) },
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: vi.fn() } }));

import FieldEditor from './FieldEditor.svelte';

// BUG-2858 — the date picker could not be dismissed in Safari. WebKit closes
// its macOS date popover only when a FOCUSED segment of the input blurs, and on
// iOS a date picker is presented only by FOCUS inside the tap; `showPicker()`
// alone never focuses. So the trigger focuses the input, synchronously, before
// `showPicker()`. What these tests can prove is the page's half (Chromium /
// jsdom); the Safari behaviour itself is Dave's verification on real devices.
const field = { key: 'due', label: 'Due', type: 'date' as const };

function mount(onchange: (v: unknown) => void = () => {}) {
	const r = render(FieldEditor, { props: { field, value: null, onchange, itemId: 'item-A' } });
	const trigger = r.container.querySelector('button.date-trigger') as HTMLButtonElement;
	const input = r.container.querySelector('input.date-hidden-input') as HTMLInputElement;
	expect(trigger, 'date trigger renders').toBeTruthy();
	expect(input, 'hidden date input renders').toBeTruthy();
	return { ...r, trigger, input };
}

let calls: string[];
beforeEach(() => {
	calls = [];
	// jsdom has no showPicker; record the ORDER of focus vs showPicker.
	(HTMLInputElement.prototype as unknown as { showPicker: () => void }).showPicker = function (this: HTMLInputElement) {
		calls.push(`showPicker:${document.activeElement === this ? 'focused' : 'unfocused'}`);
	};
	const origFocus = HTMLElement.prototype.focus;
	vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (this: HTMLElement, opts?: FocusOptions) {
		if (this instanceof HTMLInputElement && this.type === 'date') calls.push(`focus:${JSON.stringify(opts ?? null)}`);
		return origFocus.call(this, opts);
	});
});

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	delete (HTMLInputElement.prototype as unknown as { showPicker?: unknown }).showPicker;
});

describe('date picker opens from a FOCUSED input (BUG-2858)', () => {
	it('the trigger focuses the input (preventScroll) BEFORE showPicker, synchronously in the click', () => {
		const { trigger, input } = mount();
		trigger.click();
		// Nothing awaited: both happened before click() returned (iOS presents
		// its picker only for a focus inside the tap).
		expect(calls).toEqual(['focus:{"preventScroll":true}', 'showPicker:focused']);
		expect(document.activeElement).toBe(input);
	});

	it('a showPicker that throws does not undo the focus', () => {
		(HTMLInputElement.prototype as unknown as { showPicker: () => void }).showPicker = () => {
			throw new DOMException('no activation', 'NotAllowedError');
		};
		const { trigger, input } = mount();
		expect(() => trigger.click()).not.toThrow();
		expect(document.activeElement).toBe(input);
	});

	it('is hidden from assistive tech only while NOT focused, and carries the field label', async () => {
		const { trigger, input } = mount();
		expect(input.getAttribute('aria-hidden')).toBe('true');
		trigger.click();
		await Promise.resolve();
		await new Promise((r) => setTimeout(r, 0));
		expect(input.hasAttribute('aria-hidden'), 'a focused element must not be aria-hidden').toBe(false);
		expect(input.getAttribute('aria-label')).toBeTruthy();
		input.blur();
		await new Promise((r) => setTimeout(r, 0));
		expect(input.getAttribute('aria-hidden')).toBe('true');
	});

	it('Escape in the input closes it: blur, focus back to the trigger, and the key is marked handled', () => {
		const { trigger, input } = mount();
		trigger.click();
		const ev = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
		input.dispatchEvent(ev);
		expect(ev.defaultPrevented, 'handled, so the pane/dialog behind skips it').toBe(true);
		expect(document.activeElement).toBe(trigger);
	});

	it('another key in the input is left alone', () => {
		const { trigger, input } = mount();
		trigger.click();
		const ev = new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true, cancelable: true });
		input.dispatchEvent(ev);
		expect(ev.defaultPrevented).toBe(false);
		expect(document.activeElement).toBe(input);
	});
});
