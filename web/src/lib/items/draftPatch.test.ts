import { describe, it, expect } from 'vitest';
import { draftEdits, applyDraftEdits, MAX_DRAFT_EDITS } from './draftPatch';

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
