import { afterEach, expect, it, vi } from 'vitest';
import { api } from './client';

afterEach(() => vi.unstubAllGlobals());
const path = '/api/v1/workspaces/a%2Fb/items/X%2FY/commitment/children/C%2FD';
const cases = [
 ['selectMilestoneChild', () => api.selectMilestoneChild('a/b', 'X/Y', 'C/D'), 'PUT', { kind: 'child' }],
 ['waiveMilestoneChild', () => api.waiveMilestoneChild('a/b', 'X/Y', 'C/D', 'not needed'), 'PUT', { kind: 'waiver', reason: 'not needed' }],
 ['removeMilestoneChild', () => api.removeMilestoneChild('a/b', 'X/Y', 'C/D'), 'DELETE', undefined]
] as const;
it.each(cases)('%s encodes the path and sends the exact body', async (_, call, method, body) => {
 const fetch = vi.fn().mockResolvedValue(new Response('{"milestone":{"total":0}}', { status: 200, headers: { 'Content-Type': 'application/json' } }));
 vi.stubGlobal('fetch', fetch);
 const view = await call();
 expect(fetch.mock.calls[0][0]).toBe(path);
 const options = fetch.mock.calls[0][1];
 expect(options.method).toBe(method);
 expect(options.body).toBe(body === undefined ? undefined : JSON.stringify(body));
 expect(view).toEqual({ milestone: { total: 0 } });
});
