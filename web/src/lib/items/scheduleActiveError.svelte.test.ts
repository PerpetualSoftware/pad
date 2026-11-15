import { afterEach, expect, it, vi } from 'vitest';
import { PadApiError } from '$lib/api/client';
import {
	isScheduleActiveError,
	isOpenChildrenError,
	confirmOpenChildrenOrThrow
} from './openChildrenError';
import { openChildrenDialog } from '$lib/stores/openChildrenDialog.svelte';

const details = { schedule_id: 's1', next_at: '2099-01-01T09:00:00Z' };
const refusal = (d: unknown = details) =>
	new PadApiError({
		code: 'schedule_active',
		message: 'Recurring schedule',
		details: d as Record<string, unknown>
	});
afterEach(() => openChildrenDialog.abandonAll());
it('detects schedule_active with a next instant or null, rejecting malformed details', () => {
	expect(isScheduleActiveError(refusal())).toBe(true);
	expect(isScheduleActiveError(refusal({ ...details, next_at: null }))).toBe(true);
	for (const d of [null, {}, { ...details, next_at: 42 }, { ...details, next_at: 'invalid' }])
		expect(isScheduleActiveError(refusal(d))).toBe(false);
	expect(isScheduleActiveError(new Error('schedule_active'))).toBe(false);
});
it('confirms through the existing queue and retries only on consent', async () => {
	const retry = vi.fn(async () => 'updated');
	const pending = confirmOpenChildrenOrThrow(refusal(), 'TASK-1', retry);
	void pending.catch(() => {});
	expect(openChildrenDialog.active?.details).toEqual(details);
	openChildrenDialog.confirm();
	await expect(pending).resolves.toBe('updated');
	expect(retry).toHaveBeenCalledTimes(1);
	const cancelled = confirmOpenChildrenOrThrow(refusal(), 'TASK-1', retry);
	openChildrenDialog.cancel();
	await expect(cancelled).resolves.toBeNull();
	expect(retry).toHaveBeenCalledTimes(1);
});

it('validates an optional schedule warning on open_children details', () => {
	const children = {
		open_children: [],
		hidden_blocker_count: 1,
		done_field: 'status',
		attempted_value: 'done'
	};
	const error = (d: Record<string, unknown>) =>
		new PadApiError({ code: 'open_children', message: 'Children still open', details: d });
	expect(isOpenChildrenError(error(children))).toBe(true);
	for (const next_at of [details.next_at, null]) {
		expect(
			isOpenChildrenError(error({ ...children, schedule_active: { ...details, next_at } }))
		).toBe(true);
	}
	for (const schedule_active of [
		null,
		{},
		{ ...details, next_at: 'invalid' },
		{ ...details, schedule_id: '' }
	]) {
		expect(isOpenChildrenError(error({ ...children, schedule_active }))).toBe(false);
	}
});
