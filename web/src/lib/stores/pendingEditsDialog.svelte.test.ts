// BUG-3050 U1: the keep-or-overwrite choice is the user's; nothing defaults to overwrite.
import { describe, expect, it, vi } from 'vitest';

const identity = vi.hoisted(() => ({ listeners: [] as Array<() => void> }));
vi.mock('./auth.svelte', () => ({
	authStore: { onIdentityChange: (fn: () => void) => identity.listeners.push(fn) },
}));

const { pendingEditsDialog } = await import('./pendingEditsDialog.svelte');

describe('pendingEditsDialog', () => {
	it('resolves true only on Overwrite, false on Keep', async () => {
		const a = pendingEditsDialog.request('PLAYB-1');
		pendingEditsDialog.overwrite();
		expect(await a).toBe(true);
		const b = pendingEditsDialog.request('PLAYB-1');
		pendingEditsDialog.keep();
		expect(await b).toBe(false);
	});

	it('queues requests and answers them in order', async () => {
		const a = pendingEditsDialog.request('A');
		const b = pendingEditsDialog.request('B', 'duplicate');
		expect(pendingEditsDialog.active?.itemRef).toBe('A');
		pendingEditsDialog.keep();
		expect(pendingEditsDialog.active?.itemRef).toBe('B');
		expect(pendingEditsDialog.active?.kind).toBe('duplicate');
		pendingEditsDialog.overwrite();
		expect([await a, await b]).toEqual([false, true]);
		expect(pendingEditsDialog.active).toBeNull();
	});

	it('a stale-body question (BUG-3540) answers reload, overwrite or dismiss; closing dismisses', async () => {
		const a = pendingEditsDialog.requestStale('DOC-1');
		expect(pendingEditsDialog.active?.kind).toBe('stale');
		pendingEditsDialog.reload();
		expect(await a).toBe('reload');
		const b = pendingEditsDialog.requestStale('DOC-1');
		pendingEditsDialog.overwrite();
		expect(await b).toBe('overwrite');
		const c = pendingEditsDialog.requestStale('DOC-1');
		pendingEditsDialog.keep(); // Escape / the close button
		expect(await c).toBe('dismiss');
		expect(pendingEditsDialog.active).toBeNull();
	});

	it('an identity change dismisses a stale-body question, never overwrites', async () => {
		const a = pendingEditsDialog.requestStale('DOC-2');
		identity.listeners.forEach((fn) => fn());
		expect(await a).toBe('dismiss');
	});

	it('an identity change abandons every open question as KEEP, never overwrite', async () => {
		const a = pendingEditsDialog.request('A');
		const b = pendingEditsDialog.request('B');
		identity.listeners.forEach((fn) => fn());
		expect([await a, await b]).toEqual([false, false]);
		expect(pendingEditsDialog.active).toBeNull();
	});
});
