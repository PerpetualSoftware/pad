// BUG-3850 (lead ruling c9db9612): a collaborator's text that arrived while
// this tab was confirmed must be in the crash draft's base before the user's
// first keystroke on top of it. Otherwise restore sends it again, and on the
// last line an anchor that is a substring duplicates it: base 'A', stored
// 'A C', draft 'A C U' restores as 'A C U C'. Two real collab editors.
import { it, expect, describe, afterEach } from 'vitest';
import * as Y from 'yjs';
import { Editor } from '@tiptap/core';
import Collaboration from '@tiptap/extension-collaboration';
import { headlessExtensions } from '$lib/collab/materializer/headless';
import { createConfirmedBaseTracker, registerRefreshBeforeLocalChange } from './confirmedBase';
import { draftEdits, applyDraftEdits, restoreEdits } from './draftPatch';

// Destroyed after each test: a live view's DOMObserver timer firing after the
// jsdom environment is torn down is an unhandled error for the whole run.
const editors: Editor[] = [];
afterEach(() => {
	for (const ed of editors.splice(0)) ed.destroy();
});
function collabEditor(doc: Y.Doc) {
	const ed = new Editor({ element: document.createElement('div'), extensions: [...headlessExtensions(), Collaboration.configure({ document: doc, field: 'default' })] });
	editors.push(ed);
	return ed;
}
// The origin a remote apply carries, as the provider's own `this` does.
const REMOTE = Symbol('remote');
function push(from: Y.Doc, to: Y.Doc) {
	Y.applyUpdate(to, Y.encodeStateAsUpdate(from, Y.encodeStateVector(to)), REMOTE);
}
function typeAtEnd(ed: Editor, text: string) {
	ed.view.dispatch(ed.state.tr.insertText(text, ed.state.doc.content.size - 1));
}

function setUp(hook: 'before' | 'none') {
	const mine = new Y.Doc();
	const theirs = new Y.Doc();
	const me = collabEditor(mine);
	const them = collabEditor(theirs);
	me.commands.setContent('A');
	push(mine, theirs);
	let version = 0;
	let remote = 0;
	mine.on('update', (_u: Uint8Array, origin: unknown) => {
		version++;
		if (origin === REMOTE) remote++;
	});
	const ctx = {};
	const state = { confirmed: true, snapshots: 0 };
	const tracker = createConfirmedBaseTracker({
		context: () => ctx,
		confirmed: () => state.confirmed,
		version: () => version,
		remoteVersion: () => remote,
		// Sends are not modelled here (two bare Y.Docs); confirmedBaseSent covers them.
		sentVersion: () => 0,
		snapshot: () => {
			state.snapshots++;
			return me.state.doc.textContent;
		},
		seq: () => 1,
	});
	tracker.refresh();
	const stop = hook === 'before' ? registerRefreshBeforeLocalChange(me as never, tracker.refresh) : () => {};
	return { mine, theirs, me, them, tracker, state, stop };
}

describe('BUG-3850: the base is refreshed before the first local change after a remote one', () => {
	it('remote append, then typing on the last line: the base holds the remote text, not the typing, and restore does not duplicate', () => {
		const h = setUp('before');
		expect(h.tracker.current()?.markdown).toBe('A');
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		expect(h.me.state.doc.textContent, 'premise: the remote text arrived').toBe('A C');
		const before = h.state.snapshots;
		// The keystroke makes this tab unconfirmed from now on; the base must be
		// taken from the document as it was just before it.
		typeAtEnd(h.me, ' U');
		h.state.confirmed = false;
		const base = h.tracker.current()!.markdown;
		expect(base).toBe('A C');
		expect(h.state.snapshots, 'one serialization, at the local change').toBe(before + 1);
		const draft = h.me.state.doc.textContent;
		expect(draft).toBe('A C U');
		expect(applyDraftEdits('A C', draftEdits(base, draft)!)).toBe('A C U');
		h.stop();
	});

	it('control: without the hook the base lags, and the restore duplicates the collaborator\'s text', () => {
		const h = setUp('none');
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		typeAtEnd(h.me, ' U');
		h.state.confirmed = false;
		h.tracker.refresh();
		const base = h.tracker.current()!.markdown;
		expect(base).toBe('A');
		expect(applyDraftEdits('A C', draftEdits(base, h.me.state.doc.textContent)!)).toBe('A C U C');
	});

	it('a remote change alone serializes nothing', () => {
		const h = setUp('before');
		const before = h.state.snapshots;
		for (const t of [' C', 'D', 'E']) {
			typeAtEnd(h.them, t);
			push(h.theirs, h.mine);
		}
		expect(h.me.state.doc.textContent).toBe('A CDE');
		expect(h.state.snapshots).toBe(before);
		h.stop();
	});

	it('a local change while unconfirmed takes no base', () => {
		const h = setUp('before');
		h.state.confirmed = false;
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		typeAtEnd(h.me, ' U');
		expect(h.tracker.current()?.markdown).toBe('A');
		h.stop();
	});

	it('a selection-only transaction takes no base, and stop unregisters', () => {
		const h = setUp('before');
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		const before = h.state.snapshots;
		h.me.commands.setTextSelection(1);
		expect(h.state.snapshots).toBe(before);
		h.stop();
		typeAtEnd(h.me, ' U');
		expect(h.state.snapshots).toBe(before);
	});

	// Lead ruling baa63dc3, the r4 hunter's trace: a remote update lands while
	// this tab is unconfirmed, so no base can be taken that holds it.
	it('a remote change while unconfirmed: the base stays behind and is flagged, so restore never patches from it', () => {
		const h = setUp('before');
		h.state.confirmed = false;
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		typeAtEnd(h.me, ' U');
		h.tracker.refresh();
		const base = h.tracker.current()!;
		expect(base.markdown).toBe('A');
		expect(base.remoteAfter).toBe(true);
		const draft = h.me.state.doc.textContent;
		expect(draft).toBe('A C U');
		expect(applyDraftEdits('A C', draftEdits(base.markdown, draft)!), "premise: a patch from it duplicates the collaborator's text").toBe('A C U C');
		expect(restoreEdits({ markdown: draft, baseBody: base.markdown, baseStale: true })).toBeNull();
		h.stop();
	});

	it('the r3-L1 order (remote change, then a disconnect, then typing) while still confirmed: the base takes the remote text and is not flagged', () => {
		const h = setUp('before');
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		// A disconnect with nothing pending leaves the tab confirmed: synced persists.
		typeAtEnd(h.me, ' U');
		h.state.confirmed = false;
		const base = h.tracker.current()!;
		expect(base).toEqual({ markdown: 'A C', seq: 1, remoteAfter: false, sentAfter: false });
		expect(applyDraftEdits('A C', restoreEdits({ markdown: h.me.state.doc.textContent, baseBody: base.markdown })!)).toBe('A C U');
		h.stop();
	});

	it('the r3-L1 order when the disconnect left the tab unconfirmed: flagged', () => {
		const h = setUp('before');
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		h.state.confirmed = false;
		typeAtEnd(h.me, ' U');
		const base = h.tracker.current()!;
		expect(base.markdown).toBe('A');
		expect(base.remoteAfter).toBe(true);
		h.stop();
	});

	it('a later confirmed refresh takes the remote text and clears the flag', () => {
		const h = setUp('before');
		h.state.confirmed = false;
		typeAtEnd(h.them, ' C');
		push(h.theirs, h.mine);
		expect(h.tracker.current()!.remoteAfter).toBe(true);
		h.state.confirmed = true;
		h.tracker.refresh();
		expect(h.tracker.current()).toEqual({ markdown: 'A C', seq: 1, remoteAfter: false, sentAfter: false });
		h.stop();
	});

	it('a local change alone never flags the base', () => {
		const h = setUp('before');
		typeAtEnd(h.me, ' U');
		h.state.confirmed = false;
		typeAtEnd(h.me, ' V');
		expect(h.tracker.current()!.remoteAfter).toBe(false);
		h.stop();
	});
});

