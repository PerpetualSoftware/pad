import { afterEach, describe, expect, it, vi } from 'vitest';
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
