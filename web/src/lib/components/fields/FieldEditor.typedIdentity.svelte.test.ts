// BUG-3130: a typed burst is sent LATER — by the debounce timer or the blur
// flush — and a sign-out and a different sign-in while the field stays mounted
// must not send one user's typing as the next user's edit.
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

const DEBOUNCE_MS = 500;
const field = { key: 'component', label: 'Component', type: 'text' as const };

beforeEach(() => {
	vi.useFakeTimers();
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
	vi.restoreAllMocks();
});

function type(input: HTMLInputElement, text: string) {
	input.value = text;
	input.dispatchEvent(new Event('input', { bubbles: true }));
}

function mount(onchange: (v: unknown) => void) {
	const result = render(FieldEditor, { props: { field, value: '', onchange, itemId: 'item-A' } });
	return result.container.querySelector('input') as HTMLInputElement;
}

describe('a typed burst belongs to the identity that typed it (BUG-3130)', () => {
	it('CONTROL: the timer sends the burst when the identity holds still', async () => {
		// Without this the legs below pass for a field that never sends.
		const onchange = vi.fn();
		const input = mount(onchange);
		type(input, 'secret');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith('secret');
	});

	it('the timer does not send a burst typed under the previous identity', async () => {
		const onchange = vi.fn();
		const input = mount(onchange);
		type(input, 'secret');
		auth.changeIdentity();
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).not.toHaveBeenCalled();
	});

	it('the blur flush does not send it either', async () => {
		const onchange = vi.fn();
		const input = mount(onchange);
		type(input, 'secret');
		auth.changeIdentity();
		input.dispatchEvent(new Event('blur'));
		await tick();
		expect(onchange).not.toHaveBeenCalled();
		// PRECONDITION for the claim: the flush path is what blur runs — a
		// burst the identity still owns goes out on blur with no timer at all.
		type(input, 'mine');
		input.dispatchEvent(new Event('blur'));
		await tick();
		expect(onchange).toHaveBeenCalledWith('mine');
	});

	it('a ±1 step does not build on a burst typed under the previous identity', async () => {
		const onchange = vi.fn();
		const numberField = { key: 'effort', label: 'Effort', type: 'number' as const };
		const { container } = render(FieldEditor, {
			props: { field: numberField, value: 2, onchange, itemId: 'item-A' },
		});
		const input = container.querySelector('input') as HTMLInputElement;
		type(input, '40');
		auth.changeIdentity();
		(container.querySelector('[aria-label="Increase"]') as HTMLButtonElement).click();
		await tick();
		// From the stored 2, not the previous user's typed 40.
		expect(onchange.mock.calls).toEqual([[3]]);
	});

	it('a burst the NEXT identity starts is its own, and is sent', async () => {
		const onchange = vi.fn();
		const input = mount(onchange);
		type(input, 'secret');
		auth.changeIdentity();
		type(input, 'fresh');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange.mock.calls).toEqual([['fresh']]);
	});
});
