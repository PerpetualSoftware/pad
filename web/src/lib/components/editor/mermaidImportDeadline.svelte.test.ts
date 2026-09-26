// BUG-3239, the import half: a lazy `import('mermaid')` that never settles (a
// stalled chunk fetch) is bounded, and the jobs queued behind it fail at once
// rather than each waiting out another full deadline on the same fetch.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

const gate = vi.hoisted(() => {
	let open!: () => void;
	const opened = new Promise<void>((r) => (open = r));
	return { open: () => open(), opened, imports: 0 };
});

vi.mock('mermaid', async () => {
	gate.imports++;
	await gate.opened;
	return {
		default: {
			initialize: () => {},
			render: async (_id: string, source: string) => ({ svg: `<svg data-src="${source}"></svg>` }),
		},
	};
});

import * as loader from './mermaidRender';

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

function el(): HTMLElement {
	const d = document.createElement('div');
	document.body.appendChild(d);
	return d;
}

describe('BUG-3239: a stalled mermaid import is bounded', () => {
	it('fails the first diagram at the import deadline, the rest at once, and recovers when the import lands', async () => {
		const reasons: Array<[string, string]> = [];
		const a = el();
		const b = el();
		const c = el();
		loader.queueMermaidRender('graph a', a, (_t, r) => reasons.push(['a', r]));
		loader.queueMermaidRender('graph b', b, (_t, r) => reasons.push(['b', r]));
		loader.queueMermaidRender('graph c', c, (_t, r) => reasons.push(['c', r]));
		await vi.advanceTimersByTimeAsync(0);
		expect(reasons, 'something failed before any deadline').toEqual([]);

		await vi.advanceTimersByTimeAsync(loader.MERMAID_IMPORT_DEADLINE_MS);
		// ONE deadline for all three, not three in a row.
		expect(reasons).toEqual([
			['a', 'timeout'],
			['b', 'timeout'],
			['c', 'timeout'],
		]);

		// The chunk arrives after all: the next diagram draws normally.
		gate.open();
		await vi.advanceTimersByTimeAsync(0);
		const d = el();
		loader.queueMermaidRender('graph d', d);
		await vi.advanceTimersByTimeAsync(0);
		expect(d.querySelector('svg')?.getAttribute('data-src')).toBe('graph d');
	});
});
