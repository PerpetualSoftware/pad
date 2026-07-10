import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
	createCollabFlusher,
	type CollabFlushContext,
	type CollabFlusherConfig,
} from './collabFlush.svelte';

// A ctx helper mirroring the page's activeCollabContext shape.
function ctx(overrides: Partial<CollabFlushContext> = {}): CollabFlushContext {
	return {
		wsSlug: 'ws',
		itemId: 'item-1',
		baseline: 'baseline text',
		seedMd: null,
		...overrides,
	};
}

// Build a flusher with sensible identity defaults; every injected callback is
// overridable per-test. `save` defaults to a spy that resolves 'flushed'.
function makeFlusher(overrides: Partial<CollabFlusherConfig> = {}) {
	const save = vi.fn(
		async (_input: { ws: string; itemId: string; toSave: string; keepalive: boolean }) =>
			'flushed' as const,
	);
	const config: CollabFlusherConfig = {
		idleMs: 5000,
		isRecovering: () => false,
		normalize: (m) => m,
		serialize: (m) => m, // identity: toSave === normalized markdown
		readEditorMarkdown: () => 'editor md',
		isActiveItem: () => true,
		save,
		...overrides,
	};
	return { flusher: createCollabFlusher(config), save, config };
}

describe('createCollabFlusher — scheduling / debounce', () => {
	beforeEach(() => vi.useFakeTimers());
	afterEach(() => vi.useRealTimers());

	it('arms a 5s idle flush that fires the injected save with keepalive=false', () => {
		const { flusher, save } = makeFlusher();
		flusher.schedule(ctx(), 'hello');
		expect(save).not.toHaveBeenCalled();
		vi.advanceTimersByTime(5000);
		expect(save).toHaveBeenCalledTimes(1);
		expect(save).toHaveBeenCalledWith(
			expect.objectContaining({ toSave: 'hello', keepalive: false, itemId: 'item-1' }),
		);
	});

	it('coalesces rapid edits into one flush of the last markdown', () => {
		const { flusher, save } = makeFlusher();
		flusher.schedule(ctx(), 'a');
		vi.advanceTimersByTime(2000);
		flusher.schedule(ctx(), 'ab'); // re-arms, cancelling the first
		vi.advanceTimersByTime(2000);
		flusher.schedule(ctx(), 'abc');
		expect(save).not.toHaveBeenCalled();
		vi.advanceTimersByTime(5000);
		expect(save).toHaveBeenCalledTimes(1);
		expect(save).toHaveBeenCalledWith(expect.objectContaining({ toSave: 'abc' }));
	});

	it('does not schedule while force-refresh recovery is in flight', () => {
		const { flusher, save } = makeFlusher({ isRecovering: () => true });
		flusher.schedule(ctx(), 'hello');
		vi.advanceTimersByTime(10000);
		expect(save).not.toHaveBeenCalled();
	});

	it('does not schedule when there is no active context', () => {
		const { flusher, save } = makeFlusher();
		flusher.schedule(null, 'hello');
		vi.advanceTimersByTime(10000);
		expect(save).not.toHaveBeenCalled();
	});

	it('cancel() disarms a pending scheduled flush (no cross-item bleed on swap)', () => {
		const { flusher, save } = makeFlusher();
		flusher.schedule(ctx(), 'hello');
		flusher.cancel();
		vi.advanceTimersByTime(10000);
		expect(save).not.toHaveBeenCalled();
	});
});

describe('createCollabFlusher — dedupe', () => {
	it("editor-space short-circuit: no-edit view of the session's seed dedupes without serializing or saving", async () => {
		const serialize = vi.fn((m: string) => m);
		const { flusher, save } = makeFlusher({ serialize });
		const result = await flusher.flush(ctx({ seedMd: 'seed text' }), 'seed text', false);
		expect(result).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
		// Short-circuit happens BEFORE serialize (the whole point of BUG-1941).
		expect(serialize).not.toHaveBeenCalled();
	});

	it('storage-space dedupe against the per-item baseline: merely viewing an item is a no-op', async () => {
		const { flusher, save } = makeFlusher();
		const result = await flusher.flush(ctx({ baseline: 'baseline text' }), 'baseline text', false);
		expect(result).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
	});

	it('a real edit (markdown differs from baseline) flushes', async () => {
		const { flusher, save } = makeFlusher();
		const result = await flusher.flush(ctx({ baseline: 'old' }), 'new content', false);
		expect(result).toBe('flushed');
		expect(save).toHaveBeenCalledWith(expect.objectContaining({ toSave: 'new content' }));
	});

	it('records lastFlushed after a successful flush, then dedupes an identical re-flush', async () => {
		const { flusher, save } = makeFlusher();
		await flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(flusher.lastFlushed).toBe('edited');
		save.mockClear();
		// Same content again -> serverContent (lastFlushed) === toSave -> deduped.
		const second = await flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(second).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
	});

	it('does NOT record lastFlushed when the flushed item is no longer active (no stale-page pollution)', async () => {
		const { flusher } = makeFlusher({ isActiveItem: () => false });
		const result = await flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(result).toBe('flushed');
		expect(flusher.lastFlushed).toBeNull();
	});

	it('does NOT record lastFlushed when save reports failed / skipped', async () => {
		const failed = makeFlusher({ save: vi.fn(async () => 'failed' as const) });
		expect(await failed.flusher.flush(ctx({ baseline: 'old' }), 'edited', false)).toBe('failed');
		expect(failed.flusher.lastFlushed).toBeNull();

		const skipped = makeFlusher({ save: vi.fn(async () => 'skipped' as const) });
		expect(await skipped.flusher.flush(ctx({ baseline: 'old' }), 'edited', false)).toBe('skipped');
		expect(skipped.flusher.lastFlushed).toBeNull();
	});
});

describe('createCollabFlusher — recovery gate', () => {
	it('flush() bails to skipped while recovering, without saving', async () => {
		const { flusher, save } = makeFlusher({ isRecovering: () => true });
		const result = await flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(result).toBe('skipped');
		expect(save).not.toHaveBeenCalled();
	});
});

describe('createCollabFlusher — flushNow (teardown / beforeunload)', () => {
	beforeEach(() => vi.useFakeTimers());
	afterEach(() => vi.useRealTimers());

	it('reads live editor markdown, flushes it, and returns true', () => {
		const { flusher, save } = makeFlusher({ readEditorMarkdown: () => 'live editor md' });
		const ok = flusher.flushNow(ctx({ baseline: 'old' }), true);
		expect(ok).toBe(true);
		expect(save).toHaveBeenCalledWith(
			expect.objectContaining({ toSave: 'live editor md', keepalive: true }),
		);
	});

	it('no-ops (returns false) when the editor is unavailable', () => {
		const { flusher, save } = makeFlusher({ readEditorMarkdown: () => null });
		const ok = flusher.flushNow(ctx(), true);
		expect(ok).toBe(false);
		expect(save).not.toHaveBeenCalled();
	});

	it('cancels the pending debounce so it cannot fire a second, older-content flush', () => {
		const { flusher, save } = makeFlusher({ readEditorMarkdown: () => 'live md' });
		flusher.schedule(ctx({ baseline: 'old' }), 'stale debounced md');
		flusher.flushNow(ctx({ baseline: 'old' }), true);
		vi.advanceTimersByTime(10000);
		// Only the flushNow save fired; the debounce was cancelled.
		expect(save).toHaveBeenCalledTimes(1);
		expect(save).toHaveBeenCalledWith(expect.objectContaining({ toSave: 'live md' }));
	});
});

describe('createCollabFlusher — reset during an in-flight flush', () => {
	it('does NOT record lastFlushed if resetDedup() lands while the PATCH is in flight (no stale re-pollution)', async () => {
		// A deferred save so we can interleave a reset before it resolves.
		let resolveSave!: (v: 'flushed') => void;
		const save = vi.fn(
			() =>
				new Promise<'flushed'>((res) => {
					resolveSave = res;
				}),
		);
		const { flusher } = makeFlusher({ save });

		const flushPromise = flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(save).toHaveBeenCalledTimes(1);
		// An out-of-band raw save / item swap resets the dedupe baseline WHILE the
		// collab PATCH is still awaiting.
		flusher.resetDedup();
		expect(flusher.lastFlushed).toBeNull();
		// Now the collab PATCH resolves — its stale snapshot must NOT re-seed.
		resolveSave('flushed');
		const result = await flushPromise;
		expect(result).toBe('flushed');
		expect(flusher.lastFlushed).toBeNull();
	});
});

describe('createCollabFlusher — resetDedup', () => {
	it('clears the recorded baseline so an identical content flushes again (post raw-save)', async () => {
		const { flusher, save } = makeFlusher();
		await flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(flusher.lastFlushed).toBe('edited');
		// Raw save happened out-of-band; reset so the collab dedupe can't skip.
		flusher.resetDedup();
		expect(flusher.lastFlushed).toBeNull();
		save.mockClear();
		// With lastFlushed cleared, serverContent falls back to baseline ('old'),
		// so re-flushing 'edited' is a real PATCH again.
		const result = await flusher.flush(ctx({ baseline: 'old' }), 'edited', false);
		expect(result).toBe('flushed');
		expect(save).toHaveBeenCalled();
	});
});
