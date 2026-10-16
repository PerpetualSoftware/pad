import { describe, it, expect, vi } from 'vitest';
import { createCollabFlusher, type CollabFlushContext, type CollabFlusherConfig } from './collabFlush.svelte';

// BUG-3571 part 2: the text a flush-now would send, for keeping in this
// browser when it cannot be sent under its author's session (ruling R1).
const ctx: CollabFlushContext = { wsSlug: 'ws-a', itemId: 'item-1', baseline: 'b', seedMd: null };

function make(overrides: Partial<CollabFlusherConfig> = {}) {
	const save = vi.fn(async () => 'flushed' as const);
	const config: CollabFlusherConfig = {
		idleMs: 5000,
		isRecovering: () => false,
		normalize: (m) => `n(${m})`,
		serialize: (m, ws) => `s(${m},${ws})`,
		readEditorMarkdown: () => 'md',
		isActiveItem: () => true,
		save,
		...overrides,
	};
	return { flusher: createCollabFlusher(config), save };
}

describe('collabFlusher.snapshot', () => {
	it('is the storage form a flush would PATCH: normalized, then serialized for the captured workspace', () => {
		const { flusher, save } = make();
		expect(flusher.snapshot(ctx)).toBe('s(n(md),ws-a)');
		expect(save).not.toHaveBeenCalled();
	});

	it('does not dedupe: text equal to the baseline is still returned', () => {
		const { flusher } = make({ normalize: (m) => m, serialize: (m) => m, readEditorMarkdown: () => 'b' });
		expect(flusher.snapshot(ctx)).toBe('b');
	});

	it('null when the editor is unreadable or recovery is in flight', () => {
		expect(make({ readEditorMarkdown: () => null }).flusher.snapshot(ctx)).toBeNull();
		expect(make({ isRecovering: () => true }).flusher.snapshot(ctx)).toBeNull();
	});
});
