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
 * BUG-3052 unit 2: a stored value whose SHAPE does not match its declared type
 * is shown as its raw text with a note, never handed to the type's editor.
 * The ruling: an edit REPLACES it explicitly, never a silent coercion.
 *
 * The defects these legs pin, each of which a strict reading of the type
 * produced: the checkbox read `!!"false"` as CHECKED and its toggle wrote a
 * boolean over the string; the number step turned `"5"` into 6 without asking.
 */

const flag = { key: 'blocked', label: 'Blocked', type: 'checkbox' as const };
const effort = { key: 'effort', label: 'Effort', type: 'number' as const };
const labels = { key: 'labels', label: 'Labels', type: 'multi_select' as const, options: ['a', 'b'] };

afterEach(() => cleanup());

const mismatch = (c: HTMLElement) => c.querySelector('.field-mismatch');
const button = (c: HTMLElement, name: string) =>
	[...c.querySelectorAll('button')].find((b) => b.textContent?.trim() === name) as HTMLButtonElement | undefined;

describe('a value whose shape does not match its field (BUG-3052 unit 2)', () => {
	it('checkbox holding the string "false": shown as its raw text, no switch, nothing written', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: flag, value: 'false', onchange, itemId: 'i1' } });
		expect(mismatch(container)?.textContent).toContain('"false"');
		expect(mismatch(container)?.textContent).toContain("Doesn't match the field type (checkbox)");
		// The switch that read `!!"false"` as checked is not offered at all.
		expect(container.querySelector('[role="switch"]')).toBeNull();
		expect(onchange).not.toHaveBeenCalled();
	});

	it('number holding "5": Replace opens the editor EMPTY, and only a step the user takes writes', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: effort, value: '5', onchange, itemId: 'i1' } });
		expect(container.querySelector('.number-input')).toBeNull();
		button(container, 'Replace')!.click();
		await tick();
		const input = container.querySelector('.number-input') as HTMLInputElement;
		expect(input).not.toBeNull();
		expect(input.value).toBe('');
		expect(onchange).not.toHaveBeenCalled();
		(container.querySelector('[aria-label="Increase"]') as HTMLButtonElement).click();
		await tick();
		// Stepped from nothing, not from the string: never 6.
		expect(onchange.mock.calls).toEqual([[1]]);
	});

	it('Keep the stored value backs out of a replace without writing', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: effort, value: '5', onchange, itemId: 'i1' } });
		button(container, 'Replace')!.click();
		await tick();
		button(container, 'Keep the stored value')!.click();
		await tick();
		expect(mismatch(container)?.textContent).toContain('"5"');
		expect(onchange).not.toHaveBeenCalled();
	});

	it('a replace does not carry over to another item holding the same value', async () => {
		const { container, rerender } = render(FieldEditor, {
			props: { field: effort, value: '5', onchange: vi.fn(), itemId: 'i1' },
		});
		button(container, 'Replace')!.click();
		await tick();
		expect(mismatch(container)).toBeNull();
		await rerender({ field: effort, value: '5', onchange: vi.fn(), itemId: 'i2' });
		expect(mismatch(container)?.textContent).toContain('"5"');
	});

	it('read-only: the same marker, and no Replace', () => {
		const { container } = render(FieldEditor, {
			props: { field: effort, value: { n: 5 }, onchange: vi.fn(), itemId: 'i1', readonly: true },
		});
		expect(mismatch(container)?.textContent).toContain('{"n":5}');
		expect(button(container, 'Replace')).toBeUndefined();
	});

	it('control: well-typed values render their editors, with no marker', () => {
		const a = render(FieldEditor, { props: { field: flag, value: false, onchange: vi.fn(), itemId: 'i1' } });
		expect(mismatch(a.container)).toBeNull();
		expect(a.container.querySelector('[role="switch"]')?.getAttribute('aria-checked')).toBe('false');
		cleanup();
		const b = render(FieldEditor, { props: { field: effort, value: 5, onchange: vi.fn(), itemId: 'i1' } });
		expect(mismatch(b.container)).toBeNull();
		expect((b.container.querySelector('.number-input') as HTMLInputElement).value).toBe('5');
	});
});

describe('multi_select has no editor here yet (BUG-3052 unit 2)', () => {
	it('shows its values read-only with a hint, and no text input that would write a string', () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { props: { field: labels, value: ['a', 'b'], onchange, itemId: 'i1' } });
		expect(container.querySelector('input')).toBeNull();
		expect(container.textContent).toContain('a, b');
		expect(container.textContent).toContain("Can't be edited here yet");
	});
});
