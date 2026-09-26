import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

// A real epoch behind a mock, so the fence the component captures is the one an
// identity change actually moves.
const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		changeIdentity() {
			epoch++;
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

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

/**
 * IDEA-3223: the item pane's multi_select editor. It used to fall to the text
 * input, which wrote a STRING the server refuses for a multi_select, and
 * BUG-3052 unit 2 made it read-only until this editor existed.
 */

const labels = { key: 'labels', label: 'Labels', type: 'multi_select' as const, options: ['a', 'b', 'c'] };

/** An onchange whose writes settle only when the test says so. */
function deferredOnchange() {
	const settles: Array<() => void> = [];
	const fn = vi.fn(() => new Promise<void>((resolve) => settles.push(resolve)));
	return { fn, settles };
}

afterEach(() => cleanup());

// jsdom has no scrollIntoView; the editor scrolls the focused option into view.
if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {};

const trigger = (c: HTMLElement) => c.querySelector('.select-trigger') as HTMLButtonElement;
const option = (c: HTMLElement, name: string) =>
	[...c.querySelectorAll('[role="option"]')].find((o) => o.textContent?.includes(name)) as HTMLButtonElement;
const selected = (c: HTMLElement) =>
	[...c.querySelectorAll('[role="option"][aria-selected="true"]')].map((o) => o.textContent?.replace('✓', '').trim());

describe('multi_select editor (IDEA-3223)', () => {
	it('shows the selection, and never offers the text input that wrote a string', () => {
		const { container } = render(FieldEditor, { props: { field: labels, value: ['a', 'c'], onchange: vi.fn(), itemId: 'i1' } });
		expect(container.querySelector('input')).toBeNull();
		expect(trigger(container).textContent).toContain('A, C');
	});

	it('a toggle sends the whole array, adding or removing that one value', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: labels, value: ['a'], onchange, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		const list = container.querySelector('[role="listbox"]');
		expect(list?.getAttribute('aria-multiselectable')).toBe('true');
		expect(selected(container)).toEqual(['A']);
		option(container, 'B').click();
		await tick();
		expect(onchange.mock.calls).toEqual([[['a', 'b']]]);
		// The list stays open for a second choice.
		expect(container.querySelector('[role="listbox"]')).not.toBeNull();
	});

	it('removing the last value sends an empty array', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: labels, value: ['a'], onchange, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		option(container, 'A').click();
		await tick();
		expect(onchange.mock.calls).toEqual([[[]]]);
	});

	it('two quick toggles are two changes, each built on the last one sent (the BUG-3047 rule)', async () => {
		const { fn, settles } = deferredOnchange();
		const { container } = render(FieldEditor, { props: { field: labels, value: [], onchange: fn, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		option(container, 'A').click();
		option(container, 'B').click();
		await tick();
		expect(fn.mock.calls).toEqual([[['a']], [['a', 'b']]]);
		// Shown at once, before the prop catches up.
		expect(selected(container)).toEqual(['A', 'B']);
		settles.forEach((s) => s());
	});

	it('the echo of the newest write is recognised, so a later toggle builds on it', async () => {
		const { fn, settles } = deferredOnchange();
		const { container, rerender } = render(FieldEditor, { props: { field: labels, value: [], onchange: fn, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		option(container, 'A').click();
		await tick();
		// The echo arrives as a NEW array with the same content, which ends our
		// hold; then someone else's write lands. The next toggle must build on
		// THAT, not on our old send. Compared by identity, the echo was never
		// recognised, the hold outlived it, and the outside write was ignored.
		await rerender({ field: labels, value: ['a'], onchange: fn, itemId: 'i1' });
		await rerender({ field: labels, value: ['b'], onchange: fn, itemId: 'i1' });
		option(container, 'C').click();
		await tick();
		expect(fn.mock.calls).toEqual([[['a']], [['b', 'c']]]);
		settles.forEach((s) => s());
	});

	it('a stored value that is no longer an option is listed and can be removed', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: labels, value: ['a', 'gone'], onchange, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		expect(selected(container)).toEqual(['A', 'Gone']);
		option(container, 'Gone').click();
		await tick();
		expect(onchange.mock.calls).toEqual([[['a']]]);
	});

	it('keyboard: ArrowDown then Enter toggles the focused option and keeps the list open', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: labels, value: [], onchange, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		trigger(container).dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
		trigger(container).dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
		await tick();
		expect(onchange.mock.calls).toEqual([[['a']]]);
		expect(container.querySelector('[role="listbox"]')).not.toBeNull();
	});

	it('read-only: the labels, joined, and no control', () => {
		const { container } = render(FieldEditor, {
			props: { field: labels, value: ['a', 'b'], onchange: vi.fn(), itemId: 'i1', readonly: true },
		});
		expect(container.querySelector('.select-trigger')).toBeNull();
		expect(container.textContent).toContain('A, B');
	});
});

describe('multi_select editor, review round 1 (IDEA-3223)', () => {
	it('arrow keys move focus once it is inside the list, and Enter toggles once', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: labels, value: [], onchange, itemId: 'i1' } });
		trigger(container).click();
		await tick();
		const opts = [...container.querySelectorAll<HTMLButtonElement>('[role="option"]')];
		opts[0].focus();
		opts[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
		await tick();
		expect(document.activeElement).toBe(opts[1]);
		// Enter on a focused option is the button's own click, once.
		opts[1].dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
		opts[1].click();
		await tick();
		expect(onchange.mock.calls).toEqual([[['b']]]);
	});

	it('a display hold that is not an array (another field type\'s text) does not throw', async () => {
		const text = { key: 'labels', label: 'Labels', type: 'text' as const };
		const { container, rerender } = render(FieldEditor, { props: { field: text, value: '', onchange: vi.fn(), itemId: 'i1' } });
		const input = container.querySelector('input') as HTMLInputElement;
		input.value = '[';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		await tick();
		await rerender({ field: labels, value: ['a'], onchange: vi.fn(), itemId: 'i1' });
		expect(trigger(container).textContent).toContain('A');
	});
});
