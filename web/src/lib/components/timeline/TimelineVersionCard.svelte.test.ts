import { describe, it, expect, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import TimelineVersionCard from './TimelineVersionCard.svelte';
import type { Version } from '$lib/types';

// BUG-2263 / Codex P1 + P2 — REAL-component coverage for the version-restore
// exception. Unlike the other REST surfaces, version restore REST-writes this
// item's `items.content` directly (colliding with the retained Y.Doc on a peeking
// side), so it stays FROZEN while peeking. ItemDetail wires that as
// `restoreFrozen={peeking}` → ItemTimeline passes `frozen={frozen || restoreFrozen}`
// → TimelineVersionCard's `frozen`. This mounts the ACTUAL card (not a probe) to
// lock the terminal gate: the restore control is present iff NOT frozen.

function target(): HTMLElement {
	return document.body.appendChild(document.createElement('div'));
}

// Minimal non-diff version so no mount-time API fetch fires (is_diff=false → the
// content-resolve effect returns early) and the card renders its restore area.
const version = {
	id: 'v1',
	item_id: 'i1',
	content: 'hello',
	is_diff: false,
	change_summary: 'edited',
	created_by: 'user',
	source: 'web',
	created_at: '2026-07-20T00:00:00Z',
} as unknown as Version;

describe('TimelineVersionCard restore gate (BUG-2263)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;

	function render(frozen: boolean) {
		root = target();
		instance = mount(TimelineVersionCard, {
			target: root,
			props: { version, wsSlug: 'ws', itemSlug: 'ITEM-1', currentContent: 'now', onRestore: () => {}, frozen },
		});
		flushSync();
		// The restore-area lives inside the expanded card body — expand it first.
		(root.querySelector('.card-header') as HTMLButtonElement).click();
		flushSync();
		return root;
	}

	afterEach(() => {
		if (instance) unmount(instance);
		root?.remove();
		instance = null;
		root = null;
	});

	it('shows the restore control when NOT frozen (active side)', () => {
		const r = render(false);
		expect(r.querySelector('.restore-area')).not.toBeNull();
	});

	it('HIDES the restore control when frozen (peeking side — same-item content collision)', () => {
		const r = render(true);
		expect(r.querySelector('.restore-area')).toBeNull();
	});
});
