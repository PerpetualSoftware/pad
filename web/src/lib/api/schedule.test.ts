import { afterEach, describe, expect, it, vi } from 'vitest';
import { expectTypeOf } from 'vitest';
import type { CommitmentResolveResult, CommitmentMoveResult } from '$lib/types';
import { api, PadApiError } from './client';

afterEach(() => vi.unstubAllGlobals());
function answer(status: number, body: unknown) {
 const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
 vi.stubGlobal('fetch', fetch);
 return fetch;
}
describe('schedule client', () => {
 it('reads the schedule view using encoded path components', async () => {
  const view = { item_ref: 'EX-30', roles: { target: null, earliest: null, deadline: null, deadline_behavior: null, occupied_until: null, outcome: '', roles_origin: 'native' }, floating: true, next_occurrence: { at: '2026-11-05T17:00:00Z', wall: '2026-11-05T09:00:00' } };
  const fetch = answer(200, view);
  expect(await api.getCommitment('a/b', 'EX-30')).toEqual(view);
  expect(fetch.mock.calls[0][0]).toBe('/api/v1/workspaces/a%2Fb/items/EX-30/commitment');
 });
 it('posts the exact resolve body and returns the result', async () => {
  const result = { commitment: {}, resolution: { occurrence_at: '2026-11-05T17:00:00Z', state: 'done', missed_recorded: 0 } };
  const fetch = answer(200, result); const body = { occurrence_at: result.resolution.occurrence_at, state: 'done' as const };
  expect(await api.resolveCommitment('work', 'EX-30', body)).toEqual(result);
  expect(fetch.mock.calls[0][0]).toBe('/api/v1/workspaces/work/items/EX-30/commitment/resolve');
  expect(fetch.mock.calls[0][1]).toMatchObject({ method: 'POST', body: JSON.stringify(body) });
 });
 it.each([[409, 'stale', { ended: true }], [400, 'validation_error', { field: 'until', code: 'range' }]])('preserves %s errors and details', async (status, code, details) => {
  answer(status as number, { error: { code, message: 'Refused', details } });
  const error = await api.resolveCommitment('work', 'EX-30', { occurrence_at: '2026-11-05T17:00:00Z', state: 'done' }).catch(e => e);
  expect(error).toBeInstanceOf(PadApiError); expect(error).toMatchObject({ code, details });
 });
});

// Labels follow the same deadline-first table as the Go callers.
import { commitmentKind } from '$lib/commitment';
it.each([
 [null, null, 'check'],
 ['2099-01-01', null, 'deadline_hard'],
 [null, '2099-01-02', 'window'],
 ['2099-01-01', '2099-01-02', 'deadline_hard']
] as const)('derives a label from deadline=%s and occupied_until=%s', (deadline, occupied_until, expected) => {
 expect(commitmentKind({ deadline, occupied_until })).toBe(expected);
});

it('reopens the exact resolution through encoded path components', async () => {
 const body = { resolution_id: '11111111-1111-4111-8111-111111111111' };
 const result = { commitment: {}, reopen: { ...body, change_id: 'new', occurrence_key: 'wall', previous_state: 'done', missed_removed: 2, comment_id: 'kept' } };
 const fetch = answer(200, result);
 expect(await api.reopenCommitment('a/b', 'TASK/5', body)).toEqual(result);
 expect(fetch.mock.calls[0][0]).toBe('/api/v1/workspaces/a%2Fb/items/TASK%2F5/commitment/reopen');
 expect(fetch.mock.calls[0][1]).toMatchObject({ method: 'POST', body: JSON.stringify(body) });
});
it.each(['reopen_unavailable', 'reopen_not_supported', 'reopen_conflict', 'reopen_dependency', 'reopen_deadline_passed'])('preserves %s without replaying', async code => {
 const fetch = answer(409, { error: { code, message: 'Refused' } });
 const error = await api.reopenCommitment('work', 'TASK-5', { resolution_id: 'exact' }).catch(e => e);
 expect(error).toBeInstanceOf(PadApiError);
 expect(error).toMatchObject({ code });
 expect(fetch).toHaveBeenCalledTimes(1);
});

// Mutation response types must not advertise eligibility, including the nested view.
it('limits eligibility to commitment reads and reopen results', () => {
 expectTypeOf<CommitmentResolveResult['commitment']>().not.toHaveProperty('last_resolution');
 expectTypeOf<CommitmentResolveResult['resolution']>().not.toHaveProperty('last_resolution');
 expectTypeOf<CommitmentResolveResult['resolution']>().not.toHaveProperty('resolution_id');
 expectTypeOf<CommitmentMoveResult>().not.toHaveProperty('last_resolution');
 expectTypeOf<CommitmentMoveResult>().not.toHaveProperty('resolution_id');
});
it.each([true, false])('preserves the move result for preview=%s', async preview => {
 const result = { revision: 'exact', changes: [] };
 const fetch = answer(200, result);
 const body = { role: 'target' as const, to: '2026-11-20T09:00:00Z' };
 const got = preview ? await api.previewCommitmentMove('work', 'TASK-5', body) : await api.moveCommitment('work', 'TASK-5', body);
 expect(got).toEqual(result);
 expect(got).not.toHaveProperty('last_resolution');
 expect(got).not.toHaveProperty('resolution_id');
 expect(fetch).toHaveBeenCalledTimes(1);
});
