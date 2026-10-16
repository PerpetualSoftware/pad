// BUG-3850 (lead ruling a6380212): text this tab SENT after the confirmed base
// may already be on the server when the tab dies before the barrier ack. The
// draft counts it as its own change, and at the end of the document an anchor
// that is a substring sends it again: base 'A', draft 'A U1 U2', stored
// 'A U1' restores as 'A U1 U2 U1'. A send after the base flags it stale, so
// the restore takes the conflict notice. A real provider, so the counter the
// tracker reads is the one the send path moves, applier included.
import { describe, it, expect, afterEach, vi } from 'vitest';
import * as Y from 'yjs';
import { Editor } from '@tiptap/core';
import Collaboration from '@tiptap/extension-collaboration';
import { headlessExtensions } from '$lib/collab/materializer/headless';
import { CollabProvider } from '$lib/collab/wsProvider.svelte';
import { applyPatch, type PatchContext } from '$lib/collab/patchApply';
import { createConfirmedBaseTracker } from './confirmedBase';
import { draftEdits, applyDraftEdits, restoreEdits } from './draftPatch';

class FakeSocket extends EventTarget {
	static readonly CONNECTING = 0;
	static readonly OPEN = 1;
	static readonly CLOSING = 2;
	static readonly CLOSED = 3;
	static last: FakeSocket | null = null;
	binaryType = 'arraybuffer';
	readyState = 1;
	sent: unknown[] = [];
	constructor(public url: string) {
		super();
		FakeSocket.last = this;
	}
	send(data: unknown) {
		this.sent.push(data);
	}
	close() {
		this.readyState = 3;
	}
	control(msg: object) {
		this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(msg) }));
	}
}

const ctx: PatchContext = { toCanonical: (md) => md, toEditor: (md) => md };
const live: { editors: Editor[]; providers: CollabProvider[] } = { editors: [], providers: [] };
afterEach(() => {
	for (const ed of live.editors.splice(0)) ed.destroy();
	for (const p of live.providers.splice(0)) p.destroy();
});

// Paragraphs joined as a flush writes them; the cases here are plain paragraphs.
function md(ed: Editor): string {
	const out: string[] = [];
	ed.state.doc.forEach((n) => out.push(n.textContent));
	return out.join('\n\n');
}

function setUp() {
	const doc = new Y.Doc();
	let editor: Editor | null = null;
	const provider = new CollabProvider('item-3850', doc, {
		url: 'ws://test.invalid/api/v1/collab/item-3850',
		WebSocketImpl: FakeSocket as unknown as typeof WebSocket,
		onApplierRequest: vi.fn((markdown: string) => {
			editor!.commands.setContent(markdown.split('\n\n').map((p) => `<p>${p}</p>`).join(''));
			return true;
		}),
		onApplierPatch: (edits, _id, _exp, openBracket) => {
			openBracket();
			const res = applyPatch(editor!, edits, ctx);
			return res.ok ? res : null;
		},
	});
	live.providers.push(provider);
	const sock = FakeSocket.last!;
	sock.dispatchEvent(new Event('open'));
	sock.control({ type: 'op_log_cursor', op_log_id: 0 });
	editor = new Editor({ element: document.createElement('div'), extensions: [...headlessExtensions(), Collaboration.configure({ document: doc, field: 'default' })] });
	live.editors.push(editor);
	editor.commands.setContent('<p>p1</p><p>A</p>');
	const state = { confirmed: true };
	const itemCtx = {};
	// Wired as ItemDetail wires it, over the provider's own counters.
	const tracker = createConfirmedBaseTracker({
		context: () => itemCtx,
		confirmed: () => state.confirmed,
		version: () => provider.docVersion,
		remoteVersion: () => provider.remoteDocVersion,
		sentVersion: () => provider.localSentVersion,
		snapshot: () => md(editor!),
		seq: () => 1,
	});
	tracker.refresh();
	expect(tracker.current(), 'premise: a confirmed base').toEqual({ markdown: 'p1\n\nA', seq: 1, remoteAfter: false, sentAfter: false });
	// From here on nothing is acked: the tab dies before the barrier.
	state.confirmed = false;
	return { provider, sock, editor, tracker };
}
const typeAtEnd = (ed: Editor, text: string) => ed.view.dispatch(ed.state.tr.insertText(text, ed.state.doc.content.size - 1));
const paragraphAtEnd = (ed: Editor, text: string) => ed.commands.insertContentAt(ed.state.doc.content.size, { type: 'paragraph', content: [{ type: 'text', text }] });

// The draft the keeper would store (ItemDetail crashDraftSnapshot's stale rule).
function keptDraft(h: ReturnType<typeof setUp>) {
	const base = h.tracker.current()!;
	return { markdown: md(h.editor), baseBody: base.markdown, ...(base.remoteAfter || base.sentAfter ? { baseStale: true as const } : {}) };
}

describe('BUG-3850: a send after the base flags it stale (lead ruling a6380212)', () => {
	it('own text partly landed on the last line: flagged, so restore takes the conflict path', () => {
		const h = setUp();
		typeAtEnd(h.editor, ' U1');
		typeAtEnd(h.editor, ' U2');
		const d = keptDraft(h);
		expect(d.markdown).toBe('p1\n\nA U1 U2');
		// The premise: unflagged, the patch duplicates the text the server holds.
		expect(applyDraftEdits('p1\n\nA U1', draftEdits(d.baseBody, d.markdown)!)).toBe('p1\n\nA U1 U2 U1');
		expect(d.baseStale).toBe(true);
		expect(restoreEdits(d)).toBeNull();
	});

	it('own new paragraph at the end: flagged, so restore takes the conflict path', () => {
		const h = setUp();
		paragraphAtEnd(h.editor, 'U1');
		paragraphAtEnd(h.editor, 'U2');
		const d = keptDraft(h);
		expect(d.markdown).toBe('p1\n\nA\n\nU1\n\nU2');
		expect(applyDraftEdits('p1\n\nA\n\nU1', draftEdits(d.baseBody, d.markdown)!)).toBe('p1\n\nA\n\nU1\n\nU2\n\nU1');
		expect(d.baseStale).toBe(true);
		expect(restoreEdits(d)).toBeNull();
	});

	it('applier patch text landed: the applier\'s transaction is sent like typing, so the base is flagged', () => {
		const h = setUp();
		const before = h.provider.localSentVersion;
		h.sock.control({ type: 'applier_request', request_id: 'p1', patch_id: 'patch-1', edits: [{ old: 'A', new: 'A X' }], expires_at_millis: Date.now() + 60_000 });
		expect(md(h.editor), 'premise: the patch applied through the editor').toBe('p1\n\nA X');
		expect(h.provider.localSentVersion, 'the applier\'s change went out').toBeGreaterThan(before);
		typeAtEnd(h.editor, ' U');
		const d = keptDraft(h);
		expect(applyDraftEdits('p1\n\nA X', draftEdits(d.baseBody, d.markdown)!)).toBe('p1\n\nA X U X');
		expect(d.baseStale).toBe(true);
		expect(restoreEdits(d)).toBeNull();
	});

	it('applier whole-body request: setContent is sent like typing, so the base is flagged', () => {
		const h = setUp();
		const before = h.provider.localSentVersion;
		h.sock.control({ type: 'applier_request', request_id: 'r1', markdown: 'p1\n\nA X', expires_at_millis: Date.now() + 60_000 });
		expect(md(h.editor)).toBe('p1\n\nA X');
		expect(h.provider.localSentVersion).toBeGreaterThan(before);
		expect(h.tracker.current()!.sentAfter).toBe(true);
	});

	it('edits made with the socket closed are not sent: the base stays unflagged and restore patches', () => {
		const h = setUp();
		h.sock.readyState = 3;
		h.sock.dispatchEvent(new Event('close'));
		const before = h.provider.localSentVersion;
		typeAtEnd(h.editor, ' U');
		expect(h.provider.localSentVersion).toBe(before);
		const d = keptDraft(h);
		expect(d.baseStale).toBeUndefined();
		expect(restoreEdits(d)).toEqual(draftEdits('p1\n\nA', 'p1\n\nA U'));
	});

	it('a remote update does not move the sent count', () => {
		const h = setUp();
		const before = h.provider.localSentVersion;
		const other = new Y.Doc();
		Y.applyUpdate(other, Y.encodeStateAsUpdate(h.provider.ydoc));
		other.getText('t').insert(0, 'x');
		// Applied as a server message is: the provider is the origin.
		Y.applyUpdate(h.provider.ydoc, Y.encodeStateAsUpdate(other, Y.encodeStateVector(h.provider.ydoc)), h.provider);
		expect(h.provider.localSentVersion).toBe(before);
		expect(h.tracker.current()!.remoteAfter).toBe(true);
	});
});
