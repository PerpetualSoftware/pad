// TASK-2216 (audit C98): three pieces of field-editor polish.
//   - A number field given text that is not a number used to keep it on screen
//     and silently never save. It now says it is invalid, sends nothing (and
//     cancels an earlier number still pending), and reverts on blur.
//   - The custom select moved a highlight with the arrow keys that nothing
//     announced: the trigger is now a select-only combobox whose
//     aria-activedescendant names the highlighted option.
//   - The date clear was a span[role=button] nested inside the trigger button;
//     it is now a sibling button.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

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

const DEBOUNCE_MS = 500;
const numberField = { key: 'effort', label: 'Effort', type: 'number' as const };
const selectField = { key: 'status', label: 'Status', type: 'select' as const, options: ['open', 'doing', 'done'] };
const dateField = { key: 'due', label: 'Due', type: 'date' as const };

beforeEach(() => {
	vi.useFakeTimers();
	// jsdom has no scrollIntoView; the dropdown scrolls the highlighted option.
	Element.prototype.scrollIntoView = vi.fn();
});
afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

async function type(el: HTMLInputElement, text: string) {
	el.value = text;
	await fireEvent.input(el);
	await tick();
}

describe('TASK-2216: number entry', () => {
	it('text that is not a number is marked invalid, sends nothing, and reverts on blur', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { field: numberField, value: 3, onchange, ariaLabel: 'Effort' });
		const input = container.querySelector<HTMLInputElement>('input.number-input')!;
		await type(input, 'abc');
		expect(input.getAttribute('aria-invalid')).toBe('true');
		const err = container.querySelector('.number-error');
		expect(err?.textContent).toMatch(/Not a number/);
		expect(input.getAttribute('aria-describedby')).toBe(err?.id);
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS * 2);
		expect(onchange).not.toHaveBeenCalled();
		await fireEvent.blur(input);
		await tick();
		expect(input.value).toBe('3');
		expect(input.getAttribute('aria-invalid')).toBe('false');
		expect(onchange).not.toHaveBeenCalled();
	});

	it('an earlier number still pending is not sent behind invalid text', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { field: numberField, value: 3, onchange, ariaLabel: 'Effort' });
		const input = container.querySelector<HTMLInputElement>('input.number-input')!;
		await type(input, '5');
		await type(input, '5x');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS * 2);
		expect(onchange).not.toHaveBeenCalled();
	});

	it('CONTROL: a number is sent, and a partial like "-" is not flagged while typing', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { field: numberField, value: 3, onchange, ariaLabel: 'Effort' });
		const input = container.querySelector<HTMLInputElement>('input.number-input')!;
		await type(input, '-');
		expect(input.getAttribute('aria-invalid')).toBe('false');
		await type(input, '-4');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS * 2);
		expect(onchange).toHaveBeenCalledWith(-4);
	});
});

describe('TASK-2216: select announces the highlighted option', () => {
	it('arrow keys move aria-activedescendant to an option of the listbox it controls', async () => {
		const { container } = render(FieldEditor, { field: selectField, value: 'open', onchange: vi.fn(), ariaLabel: 'Status' });
		const trigger = container.querySelector<HTMLButtonElement>('.select-trigger')!;
		expect(trigger.getAttribute('role')).toBe('combobox');
		expect(trigger.hasAttribute('aria-activedescendant')).toBe(false);
		await fireEvent.click(trigger);
		await tick();
		const listbox = container.querySelector('[role="listbox"]')!;
		expect(trigger.getAttribute('aria-controls')).toBe(listbox.id);
		await fireEvent.keyDown(trigger, { key: 'ArrowDown' });
		await tick();
		const active = trigger.getAttribute('aria-activedescendant');
		expect(active, 'arrow movement names no option').toBeTruthy();
		const option = container.querySelector(`#${CSS.escape(active!)}`);
		expect(option?.getAttribute('role')).toBe('option');
		expect(option?.textContent?.trim().toLowerCase()).toContain('doing');
	});
});

describe('TASK-2216: the date clear is a sibling of the trigger', () => {
	it('is a real button outside the trigger, and clears', async () => {
		const onchange = vi.fn();
		const { container } = render(FieldEditor, { field: dateField, value: '2026-10-08', onchange, ariaLabel: 'Due' });
		const trigger = container.querySelector('.date-trigger')!;
		expect(trigger.querySelector('[role="button"], button, .clear-btn')).toBeNull();
		const clear = container.querySelector<HTMLButtonElement>('button.date-clear');
		expect(clear?.getAttribute('aria-label')).toBe('Clear date');
		await fireEvent.click(clear!);
		expect(onchange).toHaveBeenCalledWith(null);
	});
});
