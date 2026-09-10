// BUG-2995: the markdown handed to the designated applier is NOT what a later
// collab-snapshot flush stores in items.content.
//
// This is the measurement the response contract rests on. `handlers_items.go`
// echoes the SENT markdown into the 200 and marks it `applied_pending_flush`
// rather than claiming it is the stored value — and the reason it must be a
// marker rather than a bare echo is the property asserted here: the sent form
// and the stored form are different strings by construction. If a future editor
// or tiptap-markdown change made the round trip lossless, that justification
// would no longer hold and this file is where that shows up.
//
// It reproduces the two halves the real path performs:
//   1. `editor.commands.setContent(markdown)` — the applier handler in
//      ItemDetail.svelte, with the markdown the server hands it;
//   2. `unescapeDocLinks(editor.storage.markdown.getMarkdown())` — what
//      Editor.svelte's onUpdate reads back and feeds to collabFlush.
//
// VANTAGE POINT (CONVE-34), because it bounds what these results mean. Only
// StarterKit (in Editor.svelte's configuration) and tiptap-markdown are loaded,
// not the full production extension list, so every fixture is restricted to a
// construct StarterKit itself owns and a divergence here is one production has
// too. The error direction is one-way: this harness can only UNDERSTATE
// divergence, and could never establish byte-identity for the product, since
// the omitted extensions add transforms rather than remove them. Constructs
// belonging to those extensions — tables, task lists, attachments, SafeLink —
// are deliberately absent: they would measure the harness.
import { describe, it, expect } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { Markdown } from 'tiptap-markdown';
import { unescapeDocLinks } from '$lib/utils/markdown';

function roundTrip(markdown: string): string {
	const element = document.createElement('div');
	const editor = new Editor({
		element,
		extensions: [
			StarterKit.configure({ codeBlock: false, link: false }),
			Markdown.configure({ html: true, transformPastedText: true, transformCopiedText: true }),
		],
		content: '',
	});
	try {
		editor.commands.setContent(markdown);
		return unescapeDocLinks((editor.storage as any).markdown.getMarkdown());
	} finally {
		editor.destroy();
	}
}

// Each entry is [name, sent, stored] — `stored` is what the round trip produced
// when this was measured, recorded so a future change to the normalisation is
// visible as a diff rather than as a silent shift.
const normalising: Array<[string, string, string]> = [
	['setext heading becomes ATX', 'Title\n=====\n\nbody text', '# Title\n\nbody text'],
	['asterisk bullets become dashes', '* one\n* two\n* three', '- one\n- two\n- three'],
	['plus bullets become dashes', '+ one\n+ two', '- one\n- two'],
	['underscore emphasis becomes asterisks', 'an _emphasised_ word', 'an *emphasised* word'],
	['double-underscore bold becomes asterisks', 'a __bold__ word', 'a **bold** word'],
	['lazy ordered list is renumbered', '1. one\n1. two\n1. three', '1. one\n2. two\n3. three'],
	['blank-line runs collapse', 'para one\n\n\n\npara two', 'para one\n\npara two'],
	['trailing-space break becomes a backslash', 'a line with trailing spaces   \nnext line', 'a line with trailing spaces\\\nnext line'],
	['asterisk rule becomes dashes', 'above\n\n***\n\nbelow', 'above\n\n---\n\nbelow'],
	['nested list indent is re-spaced', '- a\n    - b', '- a\n  - b'],
	['lazy blockquote continuation is joined', '> quoted line\ncontinued lazily', '> quoted line continued lazily'],
];

// Fixtures that survive unchanged. They are the reason the claim is "not
// byte-identical" rather than "always rewritten": a caller whose markdown is
// already in the editor's preferred form sees no difference at all, which is
// exactly why the divergence is easy to miss in casual testing.
const stable = ['a perfectly ordinary paragraph', '3. three\n4. four', 'just one line'];

describe('BUG-2995: applier markdown vs flushed markdown', () => {
	// THE PROPERTY the response contract rests on: the sent form is not what comes
	// back. Only inequality is asserted here, so an editor or tiptap-markdown bump
	// that changes HOW a construct is normalised leaves these green — the contract
	// does not depend on the particular output, only on there being a difference.
	for (const [name, sent] of normalising) {
		it(`does not round-trip unchanged: ${name}`, () => {
			expect(roundTrip(sent)).not.toBe(sent);
		});
	}

	// The RECORDED FORMS, kept separate on purpose (codex round 1, nit). These pin
	// exact serializer output and are the brittle half: a dependency bump can change
	// them without touching the property above. When this fails and the block above
	// does not, the contract is intact and the fixtures need re-recording — update
	// the table rather than reaching for the property tests.
	it('recorded stored forms (re-record on a serializer change; not the contract)', () => {
		const actual = normalising.map(([name, sent]) => [name, roundTrip(sent)] as const);
		const expected = normalising.map(([name, , stored]) => [name, stored] as const);
		expect(actual).toEqual(expected);
	});

	for (const sent of stable) {
		it(`round-trips unchanged: ${JSON.stringify(sent)}`, () => {
			expect(roundTrip(sent)).toBe(sent);
		});
	}

	it('the contract claim: at least one ordinary construct is not byte-identical', () => {
		const changed = normalising.filter(([, sent]) => roundTrip(sent) !== sent);
		expect(changed.length).toBeGreaterThan(0);
	});
});
