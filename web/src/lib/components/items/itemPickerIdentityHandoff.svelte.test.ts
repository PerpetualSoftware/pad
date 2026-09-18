/**
 * BUG-3105 — the driven leg for `ItemPicker`'s UPWARD HAND-OFF.
 *
 * This member is the inverse of everything else in the family. The other rows
 * are children that forgot to fence their own sends. `invokeCreate` awaits
 * `oncreate(title)` — a callback the PARENT owns, which issues the API call —
 * so the picker cannot fence the request even in principle: it cannot unsend
 * something it never sent. What was wrong was that it left the caller unable to
 * fence either, passing neither a workspace, an epoch, nor a predicate upward.
 *
 * The fix is therefore a CONTRACT change, and this leg tests the contract: the
 * picker hands its caller an identity predicate captured BEFORE the hand-off,
 * and that predicate reports false if the signed-in user changed while the
 * caller's create was in flight.
 *
 * Deliberately a separate file from `ItemPicker.svelte.test.ts`: that suite does
 * not mock `authStore`, so the real store's epoch never moves there and its
 * fence is inert — which is correct for its assertions and useless for these.
 * Mocking auth here keeps that true on both sides.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const { searchApi, localIndexMock, localSearchMock } = vi.hoisted(() => ({
	searchApi: vi.fn(),
	localIndexMock: {
		bootstrapStateFor: vi.fn(),
		cursorFor: vi.fn(),
		getByCollection: vi.fn(),
		findByIdOrSlug: vi.fn(),
		pendingResyncFor: vi.fn(),
	},
	localSearchMock: { search: vi.fn(), epoch: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({ api: { search: (...a: unknown[]) => searchApi(...a) } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/localSearch.svelte', () => ({ localSearch: localSearchMock }));

const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		get identityEpoch() { return epoch; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A' }; },
		get authenticated() { return true; },
		moveIdentity() { epoch += 1; },
		reset() { epoch = 0; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(_fn: (p: string) => void) { return () => {}; },
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import ItemPicker from './ItemPicker.svelte';

function input(): HTMLInputElement {
	return document.querySelector('input') as HTMLInputElement;
}

async function type(value: string) {
	const el = input();
	el.value = value;
	el.dispatchEvent(new Event('input', { bubbles: true }));
	await tick();
}

function createRow(): HTMLElement | null {
	return document.querySelector<HTMLElement>('.picker-create');
}

const baseProps = {
	wsSlug: 'ws',
	collection: 'colors',
	onselect: () => {},
	createLabel: 'Colors',
};

beforeEach(() => {
	auth.reset();
	searchApi.mockReset();
	searchApi.mockResolvedValue({ results: [], limit: 20 });
	localIndexMock.getByCollection.mockReturnValue([]);
	localIndexMock.bootstrapStateFor.mockReturnValue('ready');
	localIndexMock.cursorFor.mockReturnValue(null);
	localIndexMock.pendingResyncFor.mockReturnValue(false);
	localIndexMock.findByIdOrSlug.mockReturnValue(null);
	localSearchMock.search.mockReturnValue([]);
	localSearchMock.epoch.mockReturnValue(1);
});

afterEach(() => {
	cleanup();
	vi.clearAllMocks();
});

describe('ItemPicker upward identity hand-off (BUG-3105)', () => {
	it('PRECONDITION: the create row is reachable, so the legs below drive a real hand-off', async () => {
		render(ItemPicker, { props: { ...baseProps, oncreate: vi.fn() } });
		await tick();
		await type('Purple');
		expect(createRow()).not.toBeNull();
	});

	it('hands the caller an identity predicate alongside the title', async () => {
		const oncreate = vi.fn();
		render(ItemPicker, { props: { ...baseProps, oncreate } });
		await tick();
		await type('Purple');
		createRow()!.click();
		await tick();

		expect(oncreate).toHaveBeenCalled();
		const [title, isSameIdentity] = oncreate.mock.calls[0];
		expect(title).toBe('Purple');
		// The contract is a FUNCTION, not an epoch number: the caller must not
		// have to know how identity is represented, only whether it moved.
		expect(typeof isSameIdentity).toBe('function');
	});

	it('CONTROL: the handed predicate reports TRUE while the identity holds', async () => {
		const oncreate = vi.fn();
		render(ItemPicker, { props: { ...baseProps, oncreate } });
		await tick();
		await type('Purple');
		createRow()!.click();
		await tick();

		const isSameIdentity = oncreate.mock.calls[0][1] as () => boolean;
		expect(isSameIdentity()).toBe(true);
	});

	it('the handed predicate reports FALSE once the identity moves mid-create', async () => {
		// The window this exists for: the caller has issued its create and is
		// awaiting the response when the signed-in user changes. Nothing about
		// the picker's own state moves, and the caller has no other way to find
		// out.
		let observedDuringFlight: boolean | null = null;
		const oncreate = vi.fn(async (_title: string, isSameIdentity: () => boolean) => {
			auth.moveIdentity();
			await Promise.resolve();
			observedDuringFlight = isSameIdentity();
		});

		render(ItemPicker, { props: { ...baseProps, oncreate } });
		await tick();
		await type('Purple');
		createRow()!.click();
		await tick();
		await Promise.resolve();
		await Promise.resolve();

		expect(observedDuringFlight).toBe(false);
	});

	it('the predicate is captured PER INVOCATION, not once per component', async () => {
		// A stale identity must not poison later creates by the NEW user for the
		// life of the picker — the same property row 9 needed on BUG-3095.
		const seen: Array<() => boolean> = [];
		const oncreate = vi.fn((_t: string, f: () => boolean) => { seen.push(f); });

		render(ItemPicker, { props: { ...baseProps, oncreate } });
		await tick();
		await type('Purple');
		createRow()!.click();
		await tick();

		auth.moveIdentity();

		await type('Crimson');
		createRow()!.click();
		await tick();

		expect(seen).toHaveLength(2);
		// The first capture is now stale; the second was taken after the move and
		// is current.
		expect(seen[0]()).toBe(false);
		expect(seen[1]()).toBe(true);
	});
});
