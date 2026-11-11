import { afterEach, expect, it, vi } from 'vitest';
import { api } from './client';

afterEach(() => vi.unstubAllGlobals());
const create = { child: 'child', parent_role: 'target' as const, child_role: 'target' as const, offset: { mode: 'elapsed' as const, elapsed_secs: 0 }, cancel_policy: 'waiting_on' as const, preview: true, reason: 'Replan', revision: 'opaque', acknowledge_violations: true };
const update = { offset: create.offset, cancel_policy: create.cancel_policy, preview: true, reason: 'Replan', revision: 'opaque', acknowledge_violations: true };
const preview = { role: 'target' as const, to: '2099-01-01T00:00:00Z', reason: 'Replan' };
const move = { ...preview, revision: 'opaque', acknowledge_violations: true };
const item = '/api/v1/workspaces/a%2Fb/items/X%2FY/commitment';
const ws = '/api/v1/workspaces/a%2Fb';
const cases = [
 ['listCommitmentLinks', () => api.listCommitmentLinks('a/b', 'X/Y'), item + '/links', 'GET', undefined],
 ['createCommitmentLink', () => api.createCommitmentLink('a/b', 'X/Y', create), item + '/links', 'POST', create],
 ['updateCommitmentLink', () => api.updateCommitmentLink('a/b', 'X/Y', update), ws + '/commitment-links/X%2FY', 'PATCH', update],
 ['deleteCommitmentLink', () => api.deleteCommitmentLink('a/b', 'X/Y'), ws + '/commitment-links/X%2FY', 'DELETE', undefined],
 ['previewCommitmentMove', () => api.previewCommitmentMove('a/b', 'X/Y', preview), item + '/move-preview', 'POST', preview],
 ['moveCommitment', () => api.moveCommitment('a/b', 'X/Y', move), item + '/move', 'POST', move],
 ['createCommitmentGate', () => api.createCommitmentGate('a/b', 'X/Y', { blocker: 'B' }), item + '/gates', 'POST', { blocker: 'B' }],
 ['deleteCommitmentGate', () => api.deleteCommitmentGate('a/b', 'X/Y'), ws + '/commitment-gates/X%2FY', 'DELETE', undefined],
 ['getCommitmentGates', () => api.getCommitmentGates('a/b', 'X/Y'), item + '/gates', 'GET', undefined],
 ['renegotiateCommitment', () => api.renegotiateCommitment('a/b', 'X/Y', { reason: 'Replan', preview: true, acknowledge_violations: true }), item + '/renegotiate', 'POST', { reason: 'Replan', preview: true, acknowledge_violations: true }]
] as const;
it.each(cases)('%s encodes paths and sends the exact body', async (_, call, path, method, body) => {
 const fetch = vi.fn().mockResolvedValue(method === 'DELETE' ? new Response(null, { status: 204 }) : new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } }));
 vi.stubGlobal('fetch', fetch);
 await call();
 expect(fetch.mock.calls[0][0]).toBe(path);
 const options = fetch.mock.calls[0][1];
 expect(options.method ?? 'GET').toBe(method);
 expect(options.body).toBe(body === undefined ? undefined : JSON.stringify(body));
});
