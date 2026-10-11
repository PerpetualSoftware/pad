// BUG-3568: ProseMirror let a mark sit on an inline atom (a hard break, an
// inline attachment), and y-tiptap never writes such a mark to the Y.Doc. The
// tab that applied the mark held a document its own Y.Doc did not, so two
// live tabs flushed different bodies. The fix forbids marks on inline atoms
// (the InlineAtomNoMarks plugin, inlineAtomNoMarks.ts; a node spec's `marks` cannot do it). The lead's ruling names four conditions,
// each a test here:
//   (a) a Y.Doc produced on main loads byte-identically under the new schema;
//   (b) markdown with a mark spanning a hard break round-trips unchanged;
//   (c) the one-tab repro no longer diverges the editor from its Y.Doc;
//   (d) the parity seeds that diverged no longer do (in the parity harness:
//       see the item for the measured seeds).
import { describe, it, expect, vi } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import fs from 'node:fs';
import path from 'node:path';
import * as Y from 'yjs';
import { Awareness } from 'y-protocols/awareness';
import { yXmlFragmentToProseMirrorRootNode } from '@tiptap/y-tiptap';
import type { Editor as TiptapEditor } from '@tiptap/core';
import { replayFrames } from './replay';
import { withUpstreamAtBlank } from './atBlank';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

vi.mock('$lib/api/client', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/api/client')>();
	return {
		api: {
			server: { capabilities: () => new Promise(() => {}) },
			attachments: { downloadUrl: real.api.attachments.downloadUrl, transform: vi.fn(), upload: vi.fn() },
			items: { list: vi.fn(async () => []) },
		},
	};
});

const { default: BodyEditor } = await import('$lib/components/editor/Editor.svelte');
const { page } = await import('$app/state');

const tick = () => new Promise<void>((r) => setTimeout(r, 0));

function mountEditor(doc?: Y.Doc) {
	let editor: TiptapEditor | null = null;
	const target = document.body.appendChild(document.createElement('div'));
	const instance = mount(BodyEditor, {
		target,
		props: {
			content: '',
			itemId: 'item-3568',
			hostToken: `b3568-${Math.random()}`,
			...(doc ? { ydoc: doc, awareness: new Awareness(doc), collabUser: { name: 'T', color: '#123456' } } : {}),
			onEditor: (e: TiptapEditor) => {
				editor = e;
			},
		},
	}) as Record<string, unknown>;
	flushSync();
	if (!editor) throw new Error('Editor.svelte did not construct a Tiptap editor');
	return {
		editor: editor as TiptapEditor,
		destroy: () => {
			unmount(instance);
			target.remove();
		},
	};
}

/** The tab's PM document, and the one its own Y.Doc rebuilds to. */
function pmAndY(editor: TiptapEditor, doc: Y.Doc) {
	const copy = new Y.Doc();
	Y.applyUpdate(copy, Y.encodeStateAsUpdate(doc));
	return {
		pm: JSON.stringify(editor.state.doc.toJSON()),
		y: JSON.stringify(yXmlFragmentToProseMirrorRootNode(copy.getXmlFragment('default'), editor.schema).toJSON()),
	};
}

const markdownOf = (editor: TiptapEditor) =>
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	withUpstreamAtBlank(() => (editor.storage as any).markdown.getMarkdown() as string);

describe('BUG-3568: no marks on inline atoms', () => {
	page.params.workspace = 'ws';

	it('the live editor registers the inline-atom guard', () => {
		const { editor, destroy } = mountEditor(new Y.Doc());
		try {
			expect(editor.extensionManager.extensions.some((e) => e.name === 'inlineAtomNoMarks')).toBe(true);
		} finally {
			destroy();
		}
	});

	// (c) Rook's one-tab repro: apply a mark over a range containing an atom.
	for (const [name, apply] of [
		['bold', (e: TiptapEditor, r: { from: number; to: number }) => e.chain().setTextSelection(r).toggleBold().run()],
		['italic', (e: TiptapEditor, r: { from: number; to: number }) => e.chain().setTextSelection(r).toggleItalic().run()],
		['strike', (e: TiptapEditor, r: { from: number; to: number }) => e.chain().setTextSelection(r).toggleStrike().run()],
		['link', (e: TiptapEditor, r: { from: number; to: number }) => e.chain().setTextSelection(r).setLink({ href: 'https://example.com' }).run()],
	] as const) {
		it(`(c) ${name} over a hard break and an inline image leaves the editor equal to its Y.Doc`, async () => {
			const doc = new Y.Doc();
			const { editor, destroy } = mountEditor(doc);
			try {
				editor.commands.setContent('alpha beta gamma');
				editor.chain().setTextSelection(6).setHardBreak().run();
				editor.commands.insertContentAt(12, { type: 'attachmentImage', attrs: { uuid: '00000000-0000-4000-8000-000000000001', alt: 'x' } });
				apply(editor, { from: 1, to: editor.state.doc.content.size - 1 });
				await tick();
				const { pm, y } = pmAndY(editor, doc);
				expect(pm, 'premise: the mark was applied to the text').toMatch(/"marks"/);
				expect(pm).toBe(y);
			} finally {
				destroy();
			}
		});
	}

	// A mark put on the atom node directly (tr.addNodeMark → AddNodeMarkStep)
	// is stripped too (codex round 1).
	it('(c) addNodeMark on a hard break leaves the editor equal to its Y.Doc', async () => {
		const doc = new Y.Doc();
		const { editor, destroy } = mountEditor(doc);
		try {
			editor.commands.setContent('alpha beta');
			editor.chain().setTextSelection(6).setHardBreak().run();
			let breakPos = -1;
			editor.state.doc.descendants((n, pos) => {
				if (n.type.name === 'hardBreak') breakPos = pos;
			});
			expect(breakPos, 'premise: the hard break exists').toBeGreaterThan(0);
			editor.view.dispatch(editor.state.tr.addNodeMark(breakPos, editor.schema.marks.bold.create()));
			await tick();
			const { pm, y } = pmAndY(editor, doc);
			expect(pm).not.toMatch(/"hardBreak","marks"/);
			expect(pm).toBe(y);
		} finally {
			destroy();
		}
	});

	// (b) Markdown with a mark spanning a hard break: the body a tab flushes
	// after loading it equals the body every Y-derived door (another tab, a
	// reload, the materializer) produces, and a second round trip is stable.
	for (const md of ['**a\\\nb**', '*a\\\nb*', '~~a\\\nb~~', '[a\\\nb](https://example.com)', '**a  \nb**']) {
		it(`(b) ${JSON.stringify(md)}: the loading tab flushes what its Y.Doc derives, stably`, async () => {
			const doc = new Y.Doc();
			const { editor, destroy } = mountEditor(doc);
			try {
				editor.commands.setContent(md);
				await tick();
				const once = markdownOf(editor);
				const { pm, y } = pmAndY(editor, doc);
				expect(pm, 'the loading tab equals its Y.Doc').toBe(y);
				editor.commands.setContent(once);
				await tick();
				console.info('ROUNDTRIP ' + JSON.stringify({ in: md, out: once }));
				expect(markdownOf(editor), 'second round trip is stable').toBe(once);
			} finally {
				destroy();
			}
		});
	}

	// (a) Y.Docs produced on main (the committed materializer corpus: op-logs
	// main's editor wrote) load under the new schema without the editor
	// writing anything back.
	it('(a) every corpus Y.Doc loads byte-identically', async () => {
		const corpus = JSON.parse(
			fs.readFileSync(path.resolve(process.cwd(), '../internal/materialize/testdata/corpus.json'), 'utf8'),
		) as Array<{ name: string; rows: string[] }>;
		let withBreak = 0;
		for (const c of corpus) {
			const doc = new Y.Doc();
			replayFrames(doc, c.rows.map((r) => new Uint8Array(Buffer.from(r, 'base64'))));
			const before = Y.encodeStateAsUpdate(doc);
			const { editor, destroy } = mountEditor(doc);
			try {
				await tick();
				if (JSON.stringify(editor.state.doc.toJSON()).includes('"hardBreak"')) withBreak++;
				expect(Buffer.from(Y.encodeStateAsUpdate(doc)).equals(Buffer.from(before)), `${c.name}: loading wrote to the Y.Doc`).toBe(true);
			} finally {
				destroy();
			}
		}
		expect(withBreak, 'premise: the corpus contains hard breaks').toBeGreaterThan(0);
	}, 600_000);
});
