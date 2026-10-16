import { describe, it, expect } from 'vitest';
import { draftEdits, applyDraftEdits, restoreEdits, MAX_DRAFT_EDITS } from './draftPatch';

const BASE = ['# Title', '', 'Paragraph one.', '', 'Paragraph two.', '', 'Paragraph three.', ''].join('\n');

describe('draftEdits', () => {
	it('round-trips: the edits applied to the base give the draft', () => {
		const draft = BASE.replace('Paragraph three.', 'Paragraph three, edited.');
		const edits = draftEdits(BASE, draft)!;
		expect(edits).toHaveLength(1);
		expect(applyDraftEdits(BASE, edits)).toBe(draft);
	});

	it('a collaborator changed paragraph 1 after the draft changed paragraph 3: both survive', () => {
		const draft = BASE.replace('Paragraph three.', 'Paragraph three, mine.');
		const theirs = BASE.replace('Paragraph one.', 'Paragraph one, theirs.');
		const edits = draftEdits(BASE, draft)!;
		const merged = applyDraftEdits(theirs, edits);
		expect(merged).toContain('Paragraph one, theirs.');
		expect(merged).toContain('Paragraph three, mine.');
	});

	it('a collaborator changed the same paragraph: the anchor is gone, so nothing applies', () => {
		const draft = BASE.replace('Paragraph three.', 'Paragraph three, mine.');
		const theirs = BASE.replace('Paragraph three.', 'Paragraph three, theirs.');
		expect(applyDraftEdits(theirs, draftEdits(BASE, draft)!)).toBeNull();
	});

	it('widens the context until a repeated line is anchored uniquely', () => {
		const base = ['a', 'x', 'b', 'c', 'x', 'd', ''].join('\n');
		const draft = base.replace('c\nx\n', 'c\nY\n');
		const edits = draftEdits(base, draft)!;
		expect(edits).not.toBeNull();
		for (const e of edits) expect(base.split(e.old)).toHaveLength(2);
		expect(applyDraftEdits(base, edits)).toBe(draft);
	});

	it('never sends an anchor the server would refuse as ambiguous, even one that would land right', () => {
		// The change is in the FIRST of two identical blocks: at context 1 the
		// anchor matches twice, and replacing the first match happens to give
		// the draft, so only the uniqueness check stops it from being sent.
		const base = 'p\nx\nq\np\nx\nq\nr\n';
		const draft = 'p\nY\nq\np\nx\nq\nr\n';
		const edits = draftEdits(base, draft)!;
		expect(edits).not.toBeNull();
		for (const e of edits) expect(base.split(e.old)).toHaveLength(2);
		expect(applyDraftEdits(base, edits)).toBe(draft);
	});

	it('keeps an insertion at the start and the end, and a missing final newline', () => {
		const base = 'one\ntwo\nthree';
		for (const draft of ['zero\none\ntwo\nthree', 'one\ntwo\nthree\nfour', 'one\ntwo\nthree\n', 'one\nthree']) {
			const edits = draftEdits(base, draft)!;
			expect(edits.every((e) => e.old !== '')).toBe(true);
			expect(applyDraftEdits(base, edits)).toBe(draft);
		}
	});

	it('several separate changes become separate edits, applied in order', () => {
		const base = Array.from({ length: 40 }, (_, i) => `line ${i}`).join('\n') + '\n';
		const draft = base.replace('line 3\n', 'line 3!\n').replace('line 30\n', 'line 30!\n');
		const edits = draftEdits(base, draft)!;
		expect(edits).toHaveLength(2);
		expect(applyDraftEdits(base, edits)).toBe(draft);
	});

	it('returns [] for an unchanged draft and null for an empty base', () => {
		expect(draftEdits(BASE, BASE)).toEqual([]);
		expect(draftEdits('', 'new text')).toBeNull();
	});

	it('returns null when no context makes the anchors unique', () => {
		// A change inside a long uniform run: every window of 8 lines either side
		// is the same text elsewhere, so nothing anchors it.
		const tall = Array(40).fill('x').join('\n') + '\n';
		const t = tall.split('\n');
		t[20] = 'y';
		expect(draftEdits(tall, t.join('\n'))).toBeNull();
	});

	it('never sends more than the server accepts', () => {
		const base = Array.from({ length: 2000 }, (_, i) => `l${i % 7}`).join('\n') + '\n';
		const draft = base
			.split('\n')
			.map((l, i) => (i % 9 === 0 && l ? l + '!' : l))
			.join('\n');
		const edits = draftEdits(base, draft);
		if (edits !== null) {
			expect(edits.length).toBeLessThanOrEqual(MAX_DRAFT_EDITS);
			expect(applyDraftEdits(base, edits)).toBe(draft);
		}
	});
});

// BUG-3850: a rich crash draft's text is the editor's serialization, and so is
// the live document an open editor applies a restore patch to (the restore
// waits for that editor, lead ruling 0337bf77). The raw row a CLI or MCP
// writer stored need not be: it can end in a newline, or hold a form the
// editor serializes differently.
describe('a rich draft anchored on the editor snapshot (BUG-3850)', () => {
	// As the CLI stores it: a trailing newline and underscore emphasis.
	const RAW = 'An _emphasised_ word first.\n\nParagraph two.\n\nParagraph three.\n';
	// As the editor serializes the same document.
	const EDITOR = 'An *emphasised* word first.\n\nParagraph two.\n\nParagraph three.';
	const DRAFT = EDITOR.replace('Paragraph three.', 'Paragraph three. Typed offline.');
	const DRAFT_P1 = EDITOR.replace('first.', 'first. Mine.');

	it('premise: anchored on the raw row, the draft misses the editor-form document (the reported false overlap)', () => {
		expect(applyDraftEdits(EDITOR, draftEdits(RAW, DRAFT)!)).toBeNull();
	});

	it('anchored on the snapshot, it applies to the editor-form document, at either end', () => {
		expect(applyDraftEdits(EDITOR, draftEdits(EDITOR, DRAFT)!)).toBe(DRAFT);
		expect(applyDraftEdits(EDITOR, draftEdits(EDITOR, DRAFT_P1)!)).toBe(DRAFT_P1);
	});

	it('why the restore needs the open editor: the snapshot anchors on a raw-only form miss the raw row', () => {
		expect(applyDraftEdits(RAW, draftEdits(EDITOR, DRAFT_P1)!)).toBeNull();
	});

	it('a patch and its undo in the open editor: the snapshot lacks the undone line, so only the typing is sent', () => {
		// The agent's patch landed in the row; its undo is only in the editor (and the op-log).
		const row = 'Paragraph one.\n\nAn agent line, undone.\n\nParagraph two.';
		const snapshot = 'Paragraph one.\n\nParagraph two.';
		const draft = 'Paragraph one.\n\nParagraph two. Mine.';
		expect(applyDraftEdits(snapshot, draftEdits(row, draft)!), 'premise: the row base conflicts').toBeNull();
		expect(applyDraftEdits(snapshot, draftEdits(snapshot, draft)!)).toBe(draft);
	});

	it('control: a genuine concurrent edit of the same line still conflicts', () => {
		const theirs = EDITOR.replace('Paragraph three.', 'Paragraph three, theirs.');
		expect(applyDraftEdits(theirs, draftEdits(EDITOR, DRAFT)!)).toBeNull();
	});
});

// Lead ruling baa63dc3: a base taken before a remote update is never patched from.
describe('restoreEdits (BUG-3850)', () => {
	it('a stale base: no edits, so restore takes the conflict notice; premise: the patch would duplicate', () => {
		expect(applyDraftEdits('A C', draftEdits('A', 'A C U')!)).toBe('A C U C');
		expect(restoreEdits({ markdown: 'A C U', baseBody: 'A', baseStale: true })).toBeNull();
	});
	it('otherwise the draft edits on its base', () => {
		expect(restoreEdits({ markdown: 'A C U', baseBody: 'A C' })).toEqual(draftEdits('A C', 'A C U'));
	});
});

