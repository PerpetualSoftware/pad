import { describe, it, expect, vi } from 'vitest';
import {
	createCollabFlusher,
	type CollabFlushContext,
	type CollabFlusherConfig,
} from './collabFlush.svelte';

// BUG-3197: a no-edit view of a body the editor does not reproduce byte for
// byte (here `* one` read back as `- one`) must dedupe, not PATCH. The flusher
// asks the page for the editor's canonical form of the seed, memoizes it on the
// context, and compares against it too.

const STORED = '* one';
const CANONICAL = '- one';

function ctx(overrides: Partial<CollabFlushContext> = {}): CollabFlushContext {
	return { wsSlug: 'ws', itemId: 'item-1', baseline: STORED, seedMd: STORED, ...overrides };
}

function makeFlusher(overrides: Partial<CollabFlusherConfig> = {}) {
	const save = vi.fn(async () => 'flushed' as const);
	const stampWatermark = vi.fn();
	const canonicalize = vi.fn((md: string) => md.replace(/^\* /gm, '- '));
	const config: CollabFlusherConfig = {
		idleMs: 5000,
		isRecovering: () => false,
		normalize: (m) => m,
		serialize: (m) => m,
		readEditorMarkdown: () => CANONICAL,
		isActiveItem: () => true,
		save,
		stampWatermark,
		canonicalize,
		...overrides,
	};
	return { flusher: createCollabFlusher(config), save, stampWatermark, canonicalize };
}

describe('collab flush: canonical-seed dedupe (BUG-3197)', () => {
	it('a view whose editor output is the canonical form of the stored body sends no PATCH and stamps the baseline', async () => {
		const { flusher, save, stampWatermark } = makeFlusher();
		expect(await flusher.flush(ctx(), CANONICAL, false)).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
		expect(stampWatermark).toHaveBeenCalledWith(expect.objectContaining({ content: STORED }));
	});

	it('control: without canonicalize the same view PATCHes the normalised body (the defect)', async () => {
		const { flusher, save } = makeFlusher({ canonicalize: undefined });
		expect(await flusher.flush(ctx(), CANONICAL, false)).toBe('flushed');
		expect(save).toHaveBeenCalledWith(expect.objectContaining({ toSave: CANONICAL }));
	});

	it('a real edit still PATCHes', async () => {
		const { flusher, save } = makeFlusher();
		expect(await flusher.flush(ctx(), '- one\n- two', false)).toBe('flushed');
		expect(save).toHaveBeenCalledTimes(1);
	});

	it('memoizes the canonical form on the context, so a teardown flush after the editor is gone reuses it', async () => {
		let editorAlive = true;
		const canonicalize = vi.fn((md: string) => (editorAlive ? md.replace(/^\* /gm, '- ') : null));
		const { flusher, save } = makeFlusher({ canonicalize });
		const c = ctx();
		expect(await flusher.flush(c, CANONICAL, false)).toBe('deduped');
		editorAlive = false;
		expect(await flusher.flush(c, CANONICAL, true)).toBe('deduped');
		expect(canonicalize).toHaveBeenCalledTimes(1);
		expect(c.seedCanonical).toBe(CANONICAL);
		expect(save).not.toHaveBeenCalled();
	});

	it('a failed canonicalization is not memoized: it falls back to the old compare and a later flush retries', async () => {
		let fail = true;
		const canonicalize = vi.fn((md: string) => (fail ? null : md.replace(/^\* /gm, '- ')));
		const save = vi.fn(async () => 'failed' as const);
		const { flusher } = makeFlusher({ canonicalize, save });
		const c = ctx();
		expect(await flusher.flush(c, CANONICAL, false)).toBe('failed');
		expect(c.seedCanonical).toBeUndefined();
		fail = false;
		expect(await flusher.flush(c, CANONICAL, false)).toBe('deduped');
		expect(canonicalize).toHaveBeenCalledTimes(2);
		expect(save).toHaveBeenCalledTimes(1);
	});

	it('is not consulted when the exact seed compare already matches', async () => {
		const { flusher, canonicalize } = makeFlusher();
		expect(await flusher.flush(ctx({ seedMd: 'plain', baseline: 'plain' }), 'plain', false)).toBe('deduped');
		expect(canonicalize).not.toHaveBeenCalled();
	});

	it('revert-safety: after a real flush this session, returning to the canonical seed PATCHes', async () => {
		const { flusher, save } = makeFlusher();
		const c = ctx();
		expect(await flusher.flush(c, '- one\n- two', false)).toBe('flushed');
		expect(await flusher.flush(c, CANONICAL, false)).toBe('flushed');
		expect(save).toHaveBeenCalledTimes(2);
	});

	it('compares the canonical form in the flush normalized space (the serializer escapes [[, normalize undoes it)', async () => {
		const unescape = (m: string) => m.replace(/\\\[\\\[([^\]]+)\\\]\\\]/g, '[[$1]]');
		const { flusher, save } = makeFlusher({
			normalize: unescape,
			canonicalize: (md) => md.replace(/^\* /gm, '- ').replace(/\[\[([^\]]+)\]\]/g, '\\[\\[$1\\]\\]'),
		});
		const c = ctx({ seedMd: '* see [[TASK-5]]', baseline: '* see [[TASK-5]]' });
		expect(await flusher.flush(c, '- see \\[\\[TASK-5\\]\\]', false)).toBe('deduped');
		expect(save).not.toHaveBeenCalled();
	});

	it('prime() memoizes while the editor is alive, so a teardown flush with the editor gone never canonicalizes', async () => {
		let editorAlive = true;
		const canonicalize = vi.fn((md: string) => (editorAlive ? md.replace(/^\* /gm, '- ') : null));
		const { flusher, save } = makeFlusher({ canonicalize });
		const c = ctx();
		flusher.prime(c);
		expect(c.seedCanonical).toBe(CANONICAL);
		editorAlive = false;
		expect(await flusher.flush(c, CANONICAL, true)).toBe('deduped');
		expect(canonicalize).toHaveBeenCalledTimes(1);
		expect(save).not.toHaveBeenCalled();
	});

	it('prime() is a no-op without a seed, and once memoized', () => {
		const { flusher, canonicalize } = makeFlusher();
		flusher.prime(ctx({ seedMd: null }));
		expect(canonicalize).not.toHaveBeenCalled();
		const c = ctx();
		flusher.prime(c);
		flusher.prime(c);
		expect(canonicalize).toHaveBeenCalledTimes(1);
	});
});
