import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, isNotFoundError, PadApiError } from './client';

afterEach(() => vi.unstubAllGlobals());

describe('workspaceGroups', () => {
 it('sends CRUD request methods, escaped ids and replacement bodies', async () => {
  const fetch = vi.fn(async () => new Response(JSON.stringify({ groups: [], id: 'g', deleted: true }), {
   status: 200, headers: { 'Content-Type': 'application/json' }
  }));
  vi.stubGlobal('fetch', fetch);
  await api.workspaceGroups.list();
  await api.workspaceGroups.get('id/with space');
  await api.workspaceGroups.create({ name: 'Saved', workspace_ids: ['w'] });
  await api.workspaceGroups.update('g', { workspace_ids: [] });
  await api.workspaceGroups.update('g', {});
  await api.workspaceGroups.remove('g');
  const calls = fetch.mock.calls as unknown as [string, RequestInit][];
  expect(calls.map(([url, init]) => [url, init.method || 'GET', init.body])).toEqual([
   ['/api/v1/me/workspace-groups', 'GET', undefined],
   ['/api/v1/me/workspace-groups/id%2Fwith%20space', 'GET', undefined],
   ['/api/v1/me/workspace-groups', 'POST', JSON.stringify({ name: 'Saved', workspace_ids: ['w'] })],
   ['/api/v1/me/workspace-groups/g', 'PATCH', JSON.stringify({ workspace_ids: [] })],
   ['/api/v1/me/workspace-groups/g', 'PATCH', '{}'],
   ['/api/v1/me/workspace-groups/g', 'DELETE', undefined]
  ]);
 });
 it('surfaces a 404 as the client not-found error', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: { code: 'not_found', message: 'Workspace group not found' } }), { status: 404 })));
  const error = await api.workspaceGroups.get('missing').catch((e: unknown) => e);
  expect(error).toBeInstanceOf(PadApiError);
  expect(isNotFoundError(error)).toBe(true);
 });
});
