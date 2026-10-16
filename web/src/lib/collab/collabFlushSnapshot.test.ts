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

// BUG-3850: a raw body in the storage form a flush writes, so restore can ask
// whether the row a crash draft was kept beside is the same document as its
// editor-snapshot base.
describe('collabFlusher.storageForm', () => {
	it("is the editor's canonical form, then normalized and serialized for the given workspace, the snapshot's pipeline", () => {
		const { flusher } = make({ canonicalize: (m) => `c(${m})` });
		expect(flusher.storageForm('raw', 'ws-b')).toBe('s(n(c(raw)),ws-b)');
	});

	it('null when the editor cannot canonicalize it, or there is no canonicalizer', () => {
		expect(make({ canonicalize: () => null }).flusher.storageForm('raw', 'ws-b')).toBeNull();
		expect(make().flusher.storageForm('raw', 'ws-b')).toBeNull();
	});
});
