import { describe, expect, it } from 'vitest';
import { nextClientWrite } from './clientWrite';

describe('nextClientWrite (BUG-3080)', () => {
	it('keeps ONE tab id for the page load and a counter that only rises', () => {
		const a = nextClientWrite();
		const b = nextClientWrite();
		const c = nextClientWrite();
		expect(a.tab).toBeTruthy();
		expect(b.tab).toBe(a.tab);
		expect(c.tab).toBe(a.tab);
		expect(b.n).toBe(a.n + 1);
		expect(c.n).toBe(b.n + 1);
		expect(a.n).toBeGreaterThanOrEqual(1);
	});

	it('mints a new id per module evaluation — a reload or another tab is another sequence', async () => {
		const { vi } = await import('vitest');
		const first = nextClientWrite().tab;
		vi.resetModules();
		const fresh = await import('./clientWrite');
		const second = fresh.nextClientWrite();
		expect(second.tab).not.toBe(first);
		expect(second.n).toBe(1);
	});
});
