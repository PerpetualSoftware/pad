import { describe, it, expect } from 'vitest';
import { draftEdits, applyDraftEdits, planDraftRestore, MAX_DRAFT_EDITS } from './draftPatch';

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
// any body a flush writes. The raw row a CLI or MCP writer stored need not be:
// it can end in a newline, or hold a form the editor serializes differently.
describe('planDraftRestore (BUG-3850)', () => {
	// As the CLI stores it: a trailing newline and a raw-only autolink.
	const RAW = 'See <https://example.com> first.\n\nParagraph two.\n\nParagraph three.\n';
	// As the editor serializes the same document, and so as a flush stores it.
	const EDITOR = 'See https://example.com first.\n\nParagraph two.\n\nParagraph three.';
	const DRAFT = EDITOR.replace('Paragraph three.', 'Paragraph three. Typed offline.');

	it('premise: the draft anchored on the raw row misses the body a flush wrote (the reported false overlap)', () => {
		expect(applyDraftEdits(EDITOR, draftEdits(RAW, DRAFT)!)).toBeNull();
	});

	it('anchored on the editor snapshot, the draft applies to the flushed body', () => {
		const plan = planDraftRestore({ markdown: DRAFT, baseBody: EDITOR, rawBaseBody: RAW }, EDITOR);
		expect(applyDraftEdits(EDITOR, plan.edits!)).toBe(DRAFT);
		expect(plan.rawRetry, 'the stored body is no longer the raw row: no retry').toBeNull();
	});

	it('a patch and its undo in the open editor: the snapshot lacks the undone line, so only the typing is sent', () => {
		// The agent's patch landed in the row; its undo is only in the editor (and the op-log).
		const row = 'Paragraph one.\n\nAn agent line, undone.\n\nParagraph two.';
		const snapshot = 'Paragraph one.\n\nParagraph two.';
		const draft = 'Paragraph one.\n\nParagraph two. Mine.';
		const flushed = snapshot;
		expect(applyDraftEdits(flushed, draftEdits(row, draft)!), 'premise: the row base conflicts').toBeNull();
		const plan = planDraftRestore({ markdown: draft, baseBody: snapshot, rawBaseBody: row }, flushed);
		expect(applyDraftEdits(flushed, plan.edits!)).toBe(draft);
	});

	it('control: a genuine concurrent edit of the same line still conflicts on the snapshot path', () => {
		const theirs = EDITOR.replace('Paragraph three.', 'Paragraph three, theirs.');
		const plan = planDraftRestore({ markdown: DRAFT, baseBody: EDITOR, rawBaseBody: RAW }, theirs);
		expect(applyDraftEdits(theirs, plan.edits!)).toBeNull();
		expect(plan.rawRetry).toBeNull();
	});

	// Typing in the paragraph that holds the raw-only form puts that form
	// inside the anchor. (Typing elsewhere needs no retry: an editor anchor is
	// often a substring of the raw body, 'Paragraph three.' of 'Paragraph three.\n'.)
	const DRAFT_P1 = EDITOR.replace('first.', 'first. Mine.');

	it('the stored body still byte-equal to the raw row: one retry anchored on it', () => {
		const plan = planDraftRestore({ markdown: DRAFT_P1, baseBody: EDITOR, rawBaseBody: RAW }, RAW);
		expect(applyDraftEdits(RAW, plan.edits!), 'premise: the editor anchors miss the raw body').toBeNull();
		expect(plan.rawRetry).not.toBeNull();
	});

	it('the retry rewrites the raw-only forms to the editor\'s, as any editor save would, and nothing more', () => {
		const plan = planDraftRestore({ markdown: DRAFT_P1, baseBody: EDITOR, rawBaseBody: RAW }, RAW);
		const result = applyDraftEdits(RAW, plan.rawRetry!);
		// Exactly the draft: the autolink and the trailing newline, at the end
		// the user never touched, take the editor's form; no other text changes.
		expect(result).toBe(DRAFT_P1);
		expect(result).toContain('See https://example.com first. Mine.');
		expect(result!.endsWith('Paragraph three.')).toBe(true);
	});

	it('no retry unless the stored body is byte-equal to the raw row, even one byte off', () => {
		for (const stored of [RAW.slice(0, -1), RAW + '\n', RAW.replace('two', 'Two'), EDITOR]) {
			expect(planDraftRestore({ markdown: DRAFT_P1, baseBody: EDITOR, rawBaseBody: RAW }, stored).rawRetry, JSON.stringify(stored)).toBeNull();
		}
	});

	it('control: a concurrent edit the client has not seen still conflicts on the retry path', () => {
		// The pane's row read is stale: the server body moved under the same line.
		const plan = planDraftRestore({ markdown: DRAFT_P1, baseBody: EDITOR, rawBaseBody: RAW }, RAW);
		const server = RAW.replace('first.', 'first, theirs.');
		expect(applyDraftEdits(server, plan.rawRetry!)).toBeNull();
	});

	it('a draft kept without a raw row (raw mode, older entries) has no retry', () => {
		expect(planDraftRestore({ markdown: DRAFT, baseBody: RAW }, RAW).rawRetry).toBeNull();
		expect(planDraftRestore({ markdown: DRAFT, baseBody: RAW, rawBaseBody: RAW }, RAW).rawRetry).toBeNull();
	});
});
