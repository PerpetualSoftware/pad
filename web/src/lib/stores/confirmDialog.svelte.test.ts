// TASK-2221 (audit C39): the shared confirmation that replaces native confirm().
import { describe, expect, it, vi } from 'vitest';

const identity = vi.hoisted(() => ({ listeners: [] as Array<() => void> }));
vi.mock('./auth.svelte', () => ({
	authStore: { onIdentityChange: (fn: () => void) => identity.listeners.push(fn) },
}));

const { confirmDialog } = await import('./confirmDialog.svelte');

const req = (title: string) => ({ title, message: 'm', confirmLabel: 'Do it' });

describe('confirmDialog', () => {
	it('resolves true on confirm and false on cancel', async () => {
		const a = confirmDialog.request(req('A'));
		expect(confirmDialog.active?.title).toBe('A');
		confirmDialog.confirm();
		expect(await a).toBe(true);
		const b = confirmDialog.request(req('B'));
		confirmDialog.cancel();
		expect(await b).toBe(false);
		expect(confirmDialog.active).toBeNull();
	});

	it('queues requests and answers them in order', async () => {
		const a = confirmDialog.request(req('A'));
		const b = confirmDialog.request(req('B'));
		expect(confirmDialog.active?.title).toBe('A');
		confirmDialog.cancel();
		expect(confirmDialog.active?.title).toBe('B');
		confirmDialog.confirm();
		expect([await a, await b]).toEqual([false, true]);
		expect(confirmDialog.active).toBeNull();
	});

	it('an answer with nothing open is ignored', () => {
		expect(() => confirmDialog.confirm()).not.toThrow();
		expect(confirmDialog.active).toBeNull();
	});

	it('an identity change answers every open question NO', async () => {
		const a = confirmDialog.request(req('A'));
		const b = confirmDialog.request(req('B'));
		identity.listeners.forEach((fn) => fn());
		expect([await a, await b]).toEqual([false, false]);
		expect(confirmDialog.active).toBeNull();
	});
});
