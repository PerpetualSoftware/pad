// The fact that justifies BUG-3030's once-per-teardown latch: the flusher's
// dedupe does NOT absorb a redundant flush while the save is still in flight.
//
// WHY THIS TEST EXISTS RATHER THAN A COMMENT. BUG-3030's fix registers the
// teardown flush on `pagehide` and `visibilitychange`→hidden as well as
// `beforeunload`, and a desktop tab close fires all three. The obvious
// reasoning — "the dedupe makes a redundant flush a no-op, so firing on more
// events costs nothing" — is TRUE in general and FALSE in exactly the case the
// teardown path runs in. `lastFlushedContent` is seeded only from a RESOLVED
// save (see `flush`), and the teardown call is deliberately fire-and-forget
// under `keepalive`, so on a real teardown nothing has resolved and the compare
// falls back to `ctx.baseline`, which a real edit does not match.
//
// The two legs are the evidence together; neither alone says anything. The
// resolved leg shows the dedupe genuinely works, so the in-flight leg cannot be
// dismissed as a broken dedupe; the in-flight leg shows it does not reach the
// case that matters, so the latch in ItemDetail is not belt-and-braces.
import { describe, expect, it } from 'vitest';
import { createCollabFlusher, type CollabFlushContext, type CollabFlusherConfig } from './collabFlush.svelte';

const EDITED = 'edited markdown';

const ctx: CollabFlushContext = {
	wsSlug: 'w',
	itemId: 'i',
	baseline: 'ORIGINAL',
	seedMd: 'ORIGINAL',
} as CollabFlushContext;

function makeFlusher(opts: { resolveSave: boolean }) {
	const saves: string[] = [];
	const release: Array<() => void> = [];
	const config: CollabFlusherConfig = {
		idleMs: 1000,
		isRecovering: () => false,
		normalize: (m) => m,
		serialize: (m) => m,
		readEditorMarkdown: () => EDITED,
		isActiveItem: () => true,
		save: async (input) => {
			saves.push(input.toSave);
			if (!opts.resolveSave) {
				// Model the real teardown: the keepalive PATCH is dispatched and
				// has NOT resolved when the next lifecycle event fires.
				await new Promise<void>((r) => release.push(r));
			}
			return 'flushed';
		},
	};
	return { flusher: createCollabFlusher(config), saves, release };
}

describe('collab flush dedupe on the teardown path', () => {
	it('does NOT dedupe redundant flushes while the keepalive save is in flight', async () => {
		const { flusher, saves } = makeFlusher({ resolveSave: false });

		// visibilitychange -> pagehide -> beforeunload, as a tab close delivers
		// them, each fire-and-forget exactly as the teardown path calls it.
		flusher.flushNow(ctx, true);
		flusher.flushNow(ctx, true);
		flusher.flushNow(ctx, true);
		await Promise.resolve();

		expect(
			saves.length,
			'if this is 1, the dedupe now absorbs in-flight teardown flushes and the ' +
				'once-latch in ItemDetail is no longer load-bearing — re-derive it before deleting it',
		).toBe(3);
		expect(flusher.lastFlushed, 'nothing resolved, so no baseline can have been recorded').toBeNull();
	});

	it('DOES dedupe once the save resolves between flushes', async () => {
		// The control. Without it, the leg above is equally consistent with a
		// dedupe that never works at all, and would say nothing about teardown.
		const { flusher, saves } = makeFlusher({ resolveSave: true });

		expect(await flusher.flush(ctx, EDITED, true)).toBe('flushed');
		expect(await flusher.flush(ctx, EDITED, true)).toBe('deduped');
		expect(await flusher.flush(ctx, EDITED, true)).toBe('deduped');

		expect(saves.length, 'the dedupe should collapse resolved repeats to one PATCH').toBe(1);
		expect(flusher.lastFlushed).toBe(EDITED);
	});
});
