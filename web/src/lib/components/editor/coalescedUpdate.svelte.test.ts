// TASK-2232 (audit C78): the Editor serialized the whole document to markdown
// on every transaction, to feed a dirty tracker whose save sits behind a 1.2s
// (or 5s collab) debounce. Measured on real 86-101 KB plans: 48-73 ms p95 per
// keystroke, 190-260 ms on a 4x-throttled CPU. The serialization is now
// coalesced; the dirty signal is not.
//
// What is pinned here, through the REAL Editor mount (CONVE-19):
//   - a burst of changes delivers ONE onUpdate, with the final text;
//   - onDirty fires synchronously on every change, before any delivery;
//   - `drain` (handed out with onEditor) delivers at once, and only once;
//   - unmounting delivers a pending change BEFORE the editor is destroyed,
//     which is what the host's teardown flush (TASK-2117) reads;
//   - a change that restores the delivered document reports nothing, as the
//     markdown comparison did.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

vi.mock('$lib/api/client', () => ({
	api: {
		server: { capabilities: () => new Promise(() => {}) },
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
			transform: vi.fn(),
			upload: vi.fn(),
		},
		items: { list: vi.fn(async () => []) },
	},
}));

const { default: BodyEditor } = await import('./Editor.svelte');
const { page } = await import('$app/state');

let target: HTMLElement;
let instance: Record<string, unknown> | null = null;
let tiptap: TiptapEditor | null = null;
let drain: (() => void) | null = null;
let updates: string[] = [];
let dirty = 0;
let settledUnchanged = 0;
// Who saw what at the moment onUpdate fired: whether the editor was already
// destroyed decides whether a teardown reader can still get the text.
let destroyedAtUpdate: boolean[] = [];

/** One keystroke at the end of the text, as a plain ProseMirror transaction
 *  (jsdom has no layout, so nothing here may scroll or focus). */
function type(text: string) {
	const { state, view } = tiptap!;
	view.dispatch(state.tr.insertText(text, state.doc.content.size - 1));
}

beforeEach(() => {
	vi.useFakeTimers();
	(page as { params: Record<string, string> }).params = { workspace: 'ws' };
	target = document.createElement('div');
	document.body.appendChild(target);
	updates = [];
	dirty = 0;
	settledUnchanged = 0;
	destroyedAtUpdate = [];
	instance = mount(BodyEditor, {
		target,
		props: {
			content: 'start',
			itemId: 'item-coalesce',
			hostToken: 'coalesce',
			onUpdate: (md: string) => {
				updates.push(md);
				destroyedAtUpdate.push(!!tiptap?.isDestroyed);
			},
			onDirty: () => {
				dirty++;
			},
			onSettledUnchanged: () => {
				settledUnchanged++;
			},
			onEditor: (e: TiptapEditor, d: () => void) => {
				tiptap = e;
				drain = d;
			},
		},
	}) as Record<string, unknown>;
	flushSync();
});

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	tiptap = null;
	drain = null;
	target.remove();
	vi.useRealTimers();
});

describe('the Editor coalesces its markdown and not its dirty signal (TASK-2232)', () => {
	it('a burst of keys delivers one onUpdate, with the final text, after the window', () => {
		for (const ch of 'hello') type(ch);
		expect(dirty, 'dirty is per change and immediate').toBe(5);
		expect(updates, 'nothing serialized inside the window').toEqual([]);
		vi.advanceTimersByTime(200);
		expect(updates).toHaveLength(1);
		expect(updates[0]).toContain('starthello');
	});

	it('the window trails: a key inside it pushes the delivery back', () => {
		type('a');
		vi.advanceTimersByTime(100);
		type('b');
		vi.advanceTimersByTime(100);
		expect(updates).toEqual([]);
		vi.advanceTimersByTime(100);
		expect(updates).toHaveLength(1);
		expect(updates[0]).toContain('startab');
	});

	it('drain delivers at once, and a second drain or the timer delivers nothing more', () => {
		type('x');
		drain!();
		expect(updates).toHaveLength(1);
		expect(updates[0]).toContain('startx');
		drain!();
		vi.advanceTimersByTime(500);
		expect(updates).toHaveLength(1);
	});

	it('unmounting delivers a pending change before the editor is destroyed', () => {
		type('z');
		expect(updates).toEqual([]);
		unmount(instance!);
		instance = null;
		expect(updates).toHaveLength(1);
		expect(updates[0]).toContain('startz');
		expect(destroyedAtUpdate, 'delivered while the editor was still alive').toEqual([false]);
		// And nothing fires later into a component that is gone.
		vi.advanceTimersByTime(500);
		expect(updates).toHaveLength(1);
	});

	it('a burst undone inside the window reports settled-unchanged, so the host can drop its dirty mark', () => {
		type('k');
		const { state, view } = tiptap!;
		view.dispatch(state.tr.delete(state.doc.content.size - 2, state.doc.content.size - 1));
		expect(dirty).toBeGreaterThan(0);
		vi.advanceTimersByTime(200);
		expect(updates).toEqual([]);
		expect(settledUnchanged).toBe(1);
		// A real change reports no such thing.
		type('m');
		vi.advanceTimersByTime(200);
		expect(updates).toHaveLength(1);
		expect(settledUnchanged).toBe(1);
	});

	it('mounting, and a change that leaves the delivered document as it was, report nothing', () => {
		// Mounting runs a normalizing transaction; it is not an edit.
		vi.advanceTimersByTime(500);
		expect(dirty).toBe(0);
		expect(updates).toEqual([]);
		// Re-setting the same document, with the update event forced on: a
		// transaction a reader cannot tell from no change.
		tiptap!.commands.setContent(tiptap!.state.doc.toJSON(), { emitUpdate: true });
		vi.advanceTimersByTime(500);
		expect(dirty).toBe(0);
		expect(updates).toEqual([]);
	});
});
