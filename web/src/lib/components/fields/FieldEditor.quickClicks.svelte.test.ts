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
 * BUG-3047: two quick clicks on a number step or the checkbox toggle must be
 * two actions. The value prop only catches up after the server round trip, so
 * a control that computes its next value from the PROP builds both clicks on
 * the same stale base and one click is lost.
 *
 * The number step was fixed on the way by BUG-3039 (it steps from what was last
 * SENT); its leg here pins that. The checkbox computed `!value` from the prop,
 * so two toggles both sent the same value and the field ended where it started.
 */

const flag = { key: 'blocked', label: 'Blocked', type: 'checkbox' as const };
const effort = { key: 'effort', label: 'Effort', type: 'number' as const };

/** An onchange whose writes settle only when the test says so. */
function deferredOnchange() {
	const settles: Array<{ resolve: () => void; reject: (e: unknown) => void }> = [];
	const fn = vi.fn(
		() =>
			new Promise<void>((resolve, reject) => {
				settles.push({ resolve, reject });
			}),
	);
	return { fn, settles };
}

afterEach(() => cleanup());

const toggle = (c: HTMLElement) => c.querySelector('[role="switch"]') as HTMLButtonElement;
const shown = (c: HTMLElement) => toggle(c).getAttribute('aria-checked');

describe('two quick clicks are two actions (BUG-3047)', () => {
	it('number: Increase twice from 0 sends 1, then 2', async () => {
		const { fn } = deferredOnchange();
		const { container } = render(FieldEditor, { props: { field: effort, value: 0, onchange: fn, itemId: 'i1' } });
		const inc = container.querySelector('[aria-label="Increase"]') as HTMLButtonElement;
		inc.click();
		inc.click();
		await tick();
		expect(fn.mock.calls).toEqual([[1], [2]]);
	});

	it('checkbox: two toggles from false send true, then false', async () => {
		const { fn } = deferredOnchange();
		const { container } = render(FieldEditor, { props: { field: flag, value: false, onchange: fn, itemId: 'i1' } });
		toggle(container).click();
		toggle(container).click();
		await tick();
		expect(fn.mock.calls).toEqual([[true], [false]]);
	});

	it('checkbox: the switch shows each click at once, before the prop catches up', async () => {
		const { fn } = deferredOnchange();
		const { container } = render(FieldEditor, { props: { field: flag, value: false, onchange: fn, itemId: 'i1' } });
		toggle(container).click();
		await tick();
		expect(shown(container)).toBe('true');
		toggle(container).click();
		await tick();
		expect(shown(container)).toBe('false');
	});

	it('checkbox: an OLDER write coming home does not flip the switch back; the newest one releases it', async () => {
		const { fn } = deferredOnchange();
		const { container, rerender } = render(FieldEditor, {
			props: { field: flag, value: false, onchange: fn, itemId: 'i1' },
		});
		toggle(container).click(); // sends true
		toggle(container).click(); // sends false
		await tick();
		await rerender({ field: flag, value: true, onchange: fn, itemId: 'i1' }); // the first write's echo
		expect(shown(container)).toBe('false');
		await rerender({ field: flag, value: false, onchange: fn, itemId: 'i1' }); // the second's
		expect(shown(container)).toBe('false');
		// Released: an outside change is shown now that nothing of ours is outstanding.
		await rerender({ field: flag, value: true, onchange: fn, itemId: 'i1' });
		expect(shown(container)).toBe('true');
	});

	it('checkbox: a toggle outstanding for ANOTHER item is not a base to toggle from', async () => {
		// Unreachable from ItemDetail, which remounts per item; this pins the
		// sent value for a caller that retargets instead.
		const { fn } = deferredOnchange();
		const { container, rerender } = render(FieldEditor, {
			props: { field: flag, value: false, onchange: fn, itemId: 'i1' },
		});
		toggle(container).click(); // i1: sends true, still outstanding
		await tick();
		// i2's stored value DIFFERS from i1's outstanding one, so the two bases
		// give different toggles (an equal value made this leg unable to tell).
		await rerender({ field: flag, value: false, onchange: fn, itemId: 'i2' });
		toggle(container).click();
		await tick();
		// From i2's stored false, not i1's outstanding true.
		expect(fn.mock.calls).toEqual([[true], [true]]);
	});

	it('checkbox: a REFUSED write releases the switch to the stored value when it settles', async () => {
		const { fn, settles } = deferredOnchange();
		const { container } = render(FieldEditor, { props: { field: flag, value: false, onchange: fn, itemId: 'i1' } });
		toggle(container).click();
		await tick();
		expect(shown(container)).toBe('true');
		settles[0].reject(new Error('refused'));
		await tick();
		await tick();
		expect(shown(container)).toBe('false');
	});
});
