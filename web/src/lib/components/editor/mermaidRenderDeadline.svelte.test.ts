// BUG-3239: the ONE mermaid render queue is a promise chain, and one render
// that never settles used to stall every diagram queued after it, on every
// editor and share page, until reload. Each job is now bounded.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

type Pending = { resolve: (v: { svg: string }) => void; reject: (e: unknown) => void };

const mm = vi.hoisted(() => ({
	hung: [] as Array<{ resolve: (v: { svg: string }) => void; reject: (e: unknown) => void }>,
	initialize: () => {},
	render: async (_id: string, source: string): Promise<{ svg: string }> => {
		if (source.includes('HANG')) {
			return new Promise((resolve, reject) => mm.hung.push({ resolve, reject }));
		}
		if (source.includes('INVALID')) throw new Error('parse error');
		return { svg: `<svg data-src="${source}"></svg>` };
	},
}));
vi.mock('mermaid', () => ({ default: mm }));

type Loader = typeof import('./mermaidRender');
let loader: Loader;

beforeEach(async () => {
	vi.useFakeTimers();
	mm.hung.length = 0;
	// A FRESH module per test: the queue is module state, so a job left hanging
	// by one test would otherwise sit in front of the next test's renders.
	vi.resetModules();
	loader = await import('./mermaidRender');
});

afterEach(() => {
	vi.useRealTimers();
});

function el(): HTMLElement {
	const d = document.createElement('div');
	document.body.appendChild(d);
	return d;
}

/** Let the queue drain whatever is runnable without moving the clock. */
async function drain() {
	await vi.advanceTimersByTimeAsync(0);
}

describe('BUG-3239: a mermaid render that never settles does not stall the queue', () => {
	it('CONTROL: two renders queued back to back both draw', async () => {
		const a = el();
		const b = el();
		loader.queueMermaidRender('graph A', a);
		loader.queueMermaidRender('graph B', b);
		await drain();
		expect(a.querySelector('svg')?.getAttribute('data-src')).toBe('graph A');
		expect(b.querySelector('svg')?.getAttribute('data-src')).toBe('graph B');
	});

	it('the diagram queued after a hung render draws once the deadline passes', async () => {
		const hung = el();
		const next = el();
		loader.queueMermaidRender('HANG', hung);
		loader.queueMermaidRender('graph after', next);
		await drain();
		expect(mm.hung).toHaveLength(1);
		expect(next.querySelector('svg'), 'drew before the deadline, so the hang was not in front of it').toBeNull();

		await vi.advanceTimersByTimeAsync(loader.MERMAID_RENDER_DEADLINE_MS);
		expect(next.querySelector('svg')?.getAttribute('data-src')).toBe('graph after');
	});

	it('the hung diagram is failed as a TIMEOUT, not reported as invalid syntax', async () => {
		const hung = el();
		const reasons: string[] = [];
		loader.queueMermaidRender('HANG', hung, (_t, reason) => reasons.push(reason));
		await drain();
		await vi.advanceTimersByTimeAsync(loader.MERMAID_RENDER_DEADLINE_MS);
		expect(reasons).toEqual(['timeout']);
	});

	it("the editor's default marker says the diagram timed out rather than that its syntax is invalid", async () => {
		const hung = el();
		loader.queueMermaidRender('HANG', hung);
		await drain();
		await vi.advanceTimersByTimeAsync(loader.MERMAID_RENDER_DEADLINE_MS);
		expect(hung.classList.contains('mermaid-error')).toBe(true);
		expect(hung.textContent).not.toMatch(/Invalid Mermaid syntax/);
		expect(hung.textContent).toMatch(/too long/);
	});

	it('CONTROL: a parse failure is still reported as invalid syntax', async () => {
		const bad = el();
		const reasons: string[] = [];
		loader.queueMermaidRender('INVALID', bad, (_t, reason) => reasons.push(reason));
		await drain();
		expect(reasons).toEqual(['invalid']);
	});

	it('a render that settles AFTER its deadline writes nothing', async () => {
		const hung = el();
		const later = el();
		loader.queueMermaidRender('HANG', hung, () => {});
		await drain();
		await vi.advanceTimersByTimeAsync(loader.MERMAID_RENDER_DEADLINE_MS);
		// The target has moved on: a newer render drew into it.
		loader.queueMermaidRender('graph newer', hung);
		loader.queueMermaidRender('graph other', later);
		await drain();
		expect(hung.querySelector('svg')?.getAttribute('data-src')).toBe('graph newer');

		(mm.hung[0] as Pending).resolve({ svg: '<svg data-src="STALE"></svg>' });
		await drain();
		expect(hung.querySelector('svg')?.getAttribute('data-src')).toBe('graph newer');
	});

	it('a callback that throws does not poison the queue for the diagrams after it (codex r1)', async () => {
		const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
		const a = el();
		const b = el();
		const c = el();
		loader.queueMermaidRender('graph a', a, () => {}, () => {
			throw new Error('onRendered blew up');
		});
		loader.queueMermaidRender('INVALID', b, () => {
			throw new Error('onError blew up');
		});
		loader.queueMermaidRender('graph c', c);
		await drain();
		expect(c.querySelector('svg')?.getAttribute('data-src')).toBe('graph c');
		// ONE report: `onRendered` runs inside the job's own try, so its throw
		// is routed to that job's `onError` (a no-op here), which predates this
		// fix. Only the throwing `onError` reaches the queue's guard.
		expect(spy).toHaveBeenCalledTimes(1);
		spy.mockRestore();
	});

	it('a render just under the deadline still draws', async () => {
		const slow = el();
		loader.queueMermaidRender('HANG', slow);
		await drain();
		await vi.advanceTimersByTimeAsync(loader.MERMAID_RENDER_DEADLINE_MS - 1);
		(mm.hung[0] as Pending).resolve({ svg: '<svg data-src="slow"></svg>' });
		await drain();
		expect(slow.querySelector('svg')?.getAttribute('data-src')).toBe('slow');
	});
});
