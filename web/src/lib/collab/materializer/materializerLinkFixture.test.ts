// TASK-2198 U4, leg (i) / (ii): Y.Docs holding internal links, for the Go
// tests that run the op-log recovery with the server's WHOLE-workspace link
// index (internal/server/materialize_recovery_links_test.go).
//
// Each case is a markdown body a tab would hold in its editor AFTER loading:
// the stored [[REF|text]] form has already been rendered by wikiLinksToMarkdown
// into a [text](/owner/ws/collection/REF) link, which is what the Y.Doc
// stores. The case's document is built with the headless editor (the live
// editor's schema, materializerSchema.svelte.test.ts) and encoded as ONE
// y-protocols Update frame, with a fixed client id so regeneration is
// byte-stable.
//
// `raw` is the serializer's output BEFORE the flush pipeline (no link index):
// the Go test uses it as "what the document itself contains", so any title in
// the recovered body that is not in `raw` came from the index.
//
// Regenerate with PAD_GEN_MATERIALIZE_LINK_FIXTURE=1. Without it the test
// checks the committed fixture still decodes to the same `raw`.
import { describe, it, expect } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';
import * as Y from 'yjs';
import { prosemirrorToYXmlFragment } from '@tiptap/y-tiptap';
import { createHeadlessEditor, materialize } from './entry';
import { updateFrame } from './replay';

const FIXTURE = path.resolve(__dirname, '../../../../../internal/server/testdata/materialize_links.json');

// X is SECR-1, "Secret Launch Codename", slug secret-launch-codename, in a
// collection the Go test restricts. Its title appears in a document ONLY in
// case `text-equals-current-title`.
const CASES: Array<{ name: string; markdown: string }> = [
	{ name: 'ref-link-other-text', markdown: 'See [shown text](/alice/ws/secrets/SECR-1) here.' },
	{ name: 'ref-link-old-title', markdown: 'See [Old Codename](/alice/ws/secrets/SECR-1) here.' },
	{ name: 'slug-link-slug-text', markdown: 'See [the plan](/alice/ws/secrets/secret-launch-codename) here.' },
	{ name: 'text-equals-current-title', markdown: 'See [Secret Launch Codename](/alice/ws/secrets/SECR-1) here.' },
	{ name: 'broken-link', markdown: 'See [gone text](broken) here.' },
	{ name: 'cross-workspace', markdown: 'See [xw text](/-/r/other/SECR-1) here.' },
	{ name: 'typed-wiki-literal', markdown: 'Typed \\[\\[SECR-1\\]\\] literal.' },
	{ name: 'public-ref-link', markdown: 'See [the public plan](/alice/ws/public/PUB-2) here.' },
	// BUG-3315 U2: a tab loads a link that follows its target's title with the
	// follows-title marker in the link's title attribute, and the marker lives
	// in the Y.Doc. Recovery must strip it exactly as a tab's save does.
	{ name: 'marker-follows-title', markdown: 'See [Secret Launch Codename](/alice/ws/secrets/SECR-1 "pad-follows-title:Secret Launch Codename") here.' },
	{ name: 'marker-renamed', markdown: 'See [Old Codename](/alice/ws/secrets/SECR-1 "pad-follows-title:Old Codename") here.' },
	{ name: 'marker-external-href', markdown: 'See [Old Codename](https://example.com/x "pad-follows-title:Old Codename") here.' },
];

interface FixtureCase {
	name: string;
	markdown: string;
	rows: string[];
	raw: string;
}

function build(): FixtureCase[] {
	const ed = createHeadlessEditor();
	try {
		return CASES.map((c) => {
			ed.commands.setContent(c.markdown);
			// A marker case is only a marker case if the document holds it.
			if (c.name.startsWith('marker-')) {
				expect((ed.storage as unknown as { markdown: { getMarkdown(): string } }).markdown.getMarkdown()).toContain('pad-follows-title');
			}
			const doc = new Y.Doc();
			doc.clientID = 2198;
			prosemirrorToYXmlFragment(ed.state.doc, doc.getXmlFragment('default'));
			const frame = updateFrame(Y.encodeStateAsUpdate(doc));
			doc.destroy();
			const rows = [Buffer.from(frame).toString('base64')];
			const raw = materialize(JSON.stringify({ rows, link_index: [], workspace_slug: 'ws' }));
			return { name: c.name, markdown: c.markdown, rows, raw };
		});
	} finally {
		ed.destroy();
	}
}

describe('materializer link fixture (TASK-2198 U4)', () => {
	it('is current', () => {
		const cases = build();
		if (process.env.PAD_GEN_MATERIALIZE_LINK_FIXTURE === '1') {
			fs.writeFileSync(FIXTURE, JSON.stringify(cases, null, '\t') + '\n');
		}
		const committed = JSON.parse(fs.readFileSync(FIXTURE, 'utf8')) as FixtureCase[];
		expect(committed).toEqual(cases);
		// Each document holds a link (or the literal), so the fixture is about
		// what it claims to be about.
		for (const c of cases) expect(c.raw).toMatch(/SECR-1|secret-launch-codename|gone text|PUB-2|example\.com/);
	});
});
