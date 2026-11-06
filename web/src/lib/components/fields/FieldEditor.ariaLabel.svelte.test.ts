import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { flushSync } from 'svelte';

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

afterEach(cleanup);

describe('FieldEditor accessible names (BUG-4336)', () => {
	it.each([
		['select', '.select-trigger', 'open'],
		['date', '.date-trigger', '2026-11-05'],
		['number', '.number-input', 3],
		['url', 'input', 'https://example.com'],
		['text', 'input', 'Draft'],
	] as const)('%s uses the supplied label and preserves unlabeled callers', (type, selector, value) => {
		const field = { key: 'property', label: 'Property', type, options: ['open', 'closed'] };
		const { container, unmount } = render(FieldEditor, { field, value, onchange: vi.fn(), ariaLabel: 'Item property' });
		flushSync();
		expect(container.querySelector(selector)).toHaveAccessibleName('Item property');
		unmount();
		const fallback = render(FieldEditor, { field, value, onchange: vi.fn() });
		flushSync();
		expect(fallback.container.querySelector(selector)).not.toHaveAttribute('aria-label');
	});

	it.each(['Item effort', undefined])('names both number steppers with label %s', (ariaLabel) => {
		const { getByRole } = render(FieldEditor, {
			field: { key: 'effort', label: 'Effort', type: 'number' }, value: 3, onchange: vi.fn(), ariaLabel,
		});
		flushSync();
		for (const action of ['Decrease', 'Increase']) {
			expect(getByRole('button', { name: `${action} ${ariaLabel ?? 'Effort'}` })).toBeInTheDocument();
		}
	});
});
