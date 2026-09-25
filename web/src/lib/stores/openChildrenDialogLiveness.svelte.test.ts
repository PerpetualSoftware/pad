import { afterEach, describe, expect, it, vi } from 'vitest';
import { openChildrenDialog } from './openChildrenDialog.svelte';
import { confirmOpenChildrenOrThrow } from '$lib/items/openChildrenError';
import { PadApiError } from '$lib/api/client';

/**
 * BUG-3046: a queued open-children confirmation is not withdrawn when its
 * reason disappears (the pane switched items, or a newer edit superseded the
 * one that raised it), so it was SHOWN after the dialog ahead of it closed.
 * The requester now supplies `isLive`, asked when a queued entry would be
 * shown; a dead one resolves as a cancel.
 */

const details = { open_children: [], done_field: 'status', done_value: 'done' } as never;
const settle = () => new Promise((r) => setTimeout(r, 0));

afterEach(() => {
	openChildrenDialog.abandonAll();
});

describe('a queued open-children prompt is asked only while it is live (BUG-3046)', () => {
	it('a request that died while queued is never shown, and resolves as a cancel', async () => {
		let live = true;
		const first = openChildrenDialog.request('TASK-1', details);
		const second = openChildrenDialog.request('TASK-2', details, () => live);
		expect(openChildrenDialog.active?.parentRef).toBe('TASK-1');
		live = false; // e.g. the user switched to another item
		openChildrenDialog.confirm();
		await expect(first).resolves.toBe(true);
		await expect(second).resolves.toBe(false);
		expect(openChildrenDialog.active, 'the stale prompt is not shown').toBeNull();
	});

	it('liveness is asked at SHOW time, not at request time', async () => {
		let live = false;
		openChildrenDialog.request('TASK-1', details);
		const second = openChildrenDialog.request('TASK-2', details, () => live);
		live = true; // dead when queued, live again by the time it would show
		openChildrenDialog.cancel();
		expect(openChildrenDialog.active?.parentRef).toBe('TASK-2');
		openChildrenDialog.confirm();
		await expect(second).resolves.toBe(true);
	});

	it('skips every dead entry in a row and shows the first live one behind them', async () => {
		openChildrenDialog.request('TASK-1', details);
		const dead1 = openChildrenDialog.request('TASK-2', details, () => false);
		const dead2 = openChildrenDialog.request('TASK-3', details, () => false);
		openChildrenDialog.request('TASK-4', details, () => true);
		openChildrenDialog.cancel();
		expect(openChildrenDialog.active?.parentRef).toBe('TASK-4');
		await expect(dead1).resolves.toBe(false);
		await expect(dead2).resolves.toBe(false);
	});

	it('CONTROL: a request with no predicate is shown as before', async () => {
		openChildrenDialog.request('TASK-1', details);
		const second = openChildrenDialog.request('TASK-2', details);
		openChildrenDialog.confirm();
		expect(openChildrenDialog.active?.parentRef).toBe('TASK-2');
		openChildrenDialog.cancel();
		await expect(second).resolves.toBe(false);
	});
});

describe('confirmOpenChildrenOrThrow hands the predicate to the store (BUG-3046 wiring)', () => {
	it('a dead queued prompt returns null and never runs the forced retry', async () => {
		openChildrenDialog.request('TASK-1', details); // something already showing
		const err = new PadApiError({
			code: 'open_children',
			message: 'open children',
			details: { open_children: [], hidden_blocker_count: 0, done_field: 'status', attempted_value: 'done' },
		} as never);
		const retry = vi.fn(async () => 'forced');
		let live = true;
		const pending = confirmOpenChildrenOrThrow(err, 'TASK-2', retry, () => live);
		await settle();
		live = false;
		openChildrenDialog.confirm();
		await expect(pending).resolves.toBeNull();
		expect(retry).not.toHaveBeenCalled();
		expect(openChildrenDialog.active).toBeNull();
	});
});
