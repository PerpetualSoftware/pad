import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
	createCollabFlusher,
	type CollabFlushContext,
	type CollabFlusherConfig,
} from './collabFlush.svelte';

// BUG-3124 unit B: a deduped flush proves the server holds this document's
// markdown, so it hands the page that content to stamp the watermark with;
// settle() gives a cursor advance with no editor update a flush to dedupe.

function ctx(overrides: Partial<CollabFlushContext> = {}): CollabFlushContext {
	return { wsSlug: 'ws', itemId: 'item-1', baseline: 'stored body', seedMd: null, ...overrides };
}

function makeFlusher(overrides: Partial<CollabFlusherConfig> = {}) {
	const save = vi.fn(async () => 'flushed' as const);
	const stampWatermark = vi.fn();
	const config: CollabFlusherConfig = {
		idleMs: 5000,
		isRecovering: () => false,
		normalize: (m) => m,
		serialize: (m) => m,
		readEditorMarkdown: () => 'stored body',
		isActiveItem: () => true,
		save,
		stampWatermark,
		...overrides,
	};
	return { flusher: createCollabFlusher(config), save, stampWatermark };
}

describe('stampWatermark on dedupe', () => {
	it('storage-space dedupe stamps with the content the server holds, and sends no PATCH', async () => {
		const { flusher, save, stampWatermark } = makeFlusher();
		expect(await flusher.flush(ctx(), 'stored body', false)).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
		expect(stampWatermark).toHaveBeenCalledTimes(1);
		expect(stampWatermark).toHaveBeenCalledWith({
			ws: 'ws',
			itemId: 'item-1',
			content: 'stored body',
			keepalive: false,
		});
	});

	it('editor-space dedupe stamps with the BASELINE (the server copy), not the editor-space markdown', async () => {
		// serialize diverges from the seed, so only the editor-space arm can dedupe.
		const { flusher, save, stampWatermark } = makeFlusher({ serialize: () => 'reserialized differently' });
		const c = ctx({ seedMd: 'editor view' });
		expect(await flusher.flush(c, 'editor view', true)).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
		expect(stampWatermark).toHaveBeenCalledWith({
			ws: 'ws',
			itemId: 'item-1',
			content: 'stored body',
			keepalive: true,
		});
	});

	it('after a real flush, a later dedupe stamps with what was FLUSHED, not the stale baseline', async () => {
		const { flusher, stampWatermark } = makeFlusher();
		expect(await flusher.flush(ctx(), 'edited body', false)).toBe('flushed');
		expect(stampWatermark).not.toHaveBeenCalled();
		expect(await flusher.flush(ctx(), 'edited body', false)).toBe('deduped');
		expect(stampWatermark).toHaveBeenCalledWith(expect.objectContaining({ content: 'edited body' }));
	});

	it('a differing document PATCHes and does not stamp', async () => {
		const { flusher, save, stampWatermark } = makeFlusher();
		expect(await flusher.flush(ctx(), 'a real edit', false)).toBe('flushed');
		expect(save).toHaveBeenCalledTimes(1);
		expect(stampWatermark).not.toHaveBeenCalled();
	});

	it('stamps synchronously, before flush() yields, so a pagehide keepalive request starts in time', () => {
		const { flusher, stampWatermark } = makeFlusher();
		void flusher.flush(ctx(), 'stored body', true);
		expect(stampWatermark).toHaveBeenCalledTimes(1);
	});
});

describe('settle()', () => {
	beforeEach(() => vi.useFakeTimers());
	afterEach(() => vi.useRealTimers());

	it('runs an idle flushNow of the live editor markdown, which dedupes and stamps', () => {
		const { flusher, stampWatermark } = makeFlusher();
		flusher.settle(ctx());
		expect(stampWatermark).not.toHaveBeenCalled();
		vi.advanceTimersByTime(5000);
		expect(stampWatermark).toHaveBeenCalledTimes(1);
	});

	it('does not postpone a pending edit flush', () => {
		const { flusher, save } = makeFlusher();
		flusher.schedule(ctx(), 'an edit');
		vi.advanceTimersByTime(4000);
		flusher.settle(ctx()); // must NOT re-arm the timer
		vi.advanceTimersByTime(1000);
		expect(save).toHaveBeenCalledTimes(1);
		expect(save).toHaveBeenCalledWith(expect.objectContaining({ toSave: 'an edit' }));
	});

	it('is inert while force-refresh recovery is in flight', () => {
		const { flusher, stampWatermark, save } = makeFlusher({ isRecovering: () => true });
		flusher.settle(ctx());
		vi.advanceTimersByTime(5000);
		expect(stampWatermark).not.toHaveBeenCalled();
		expect(save).not.toHaveBeenCalled();
	});
});
