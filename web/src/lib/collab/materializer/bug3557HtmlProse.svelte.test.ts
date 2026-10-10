import { describe, it, expect, vi } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import * as Y from 'yjs';
import { Awareness } from 'y-protocols/awareness';
import type { Editor as TiptapEditor } from '@tiptap/core';
import { unescapeDocLinks, cleanBrokenLinks } from '$lib/utils/markdown';
import { materializeWith, createHeadlessEditor } from './entry';
import { updateFrame } from './replay';
import { withUpstreamAtBlank } from './atBlank';

/**
 * BUG-3557: an agent writes developer prose (`the <Button> component`,
 * `Promise<T>`) to an item a person has open; the tab applies it, someone
 * types one character, and the flush used to store the body without the
 * angle-bracketed text. The editor parsed markdown with html enabled, so
 * `<Button>` was a tag the schema could not hold and was dropped, and
 * `<Table>` became an empty table that a plugin then removed.
 *
 * Each case runs the live path end to end, in the real editor on a Y.Doc:
 * the applier's setContent, one keystroke, the tab's flush; then replays the
 * SAME Y frames through the headless materializer, which the server's
 * recovery uses. Both outputs must still hold every listed fragment.
 */

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

/** [label, agent-written body, fragments that must survive verbatim] */
const CASES: Array<[string, string, string[]]> = [
	['a capitalised tag in prose', 'Make @ui <Table> a drop-in for every list.', ['<Table>', 'a drop-in']],
	['a component name', 'Use the <Button> component here.', ['<Button>', 'component here']],
	['a generic return type', 'The function returns Promise<T> on success.', ['Promise<T>']],
	['a generic collection', 'Store them in a List<T> first.', ['List<T>']],
	['a two-parameter generic', 'Index by Map<K, V> keys.', ['Map<K, V>']],
	['an HTML comment', 'Before <!-- reviewer note --> after.', ['<!-- reviewer note -->']],
	['a details block', '<details><summary>More</summary>hidden text</details>\n\nafter', ['<details>', '<summary>More</summary>', 'hidden text', '</details>']],
	['a lower-case block tag in prose', 'Wrap it in a <div> element.', ['Wrap it in a <div> element.']],
];

function flush(editor: TiptapEditor): string {
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const md = withUpstreamAtBlank(() => (editor.storage as any).markdown.getMarkdown() as string);
	return cleanBrokenLinks(unescapeDocLinks(md));
}

function run(body: string): { tab: string; materialized: string } {
	const doc = new Y.Doc();
	const frames: string[] = [];
	doc.on('update', (u: Uint8Array) => frames.push(Buffer.from(updateFrame(u)).toString('base64')));
	let editor: TiptapEditor | null = null;
	const target = document.body.appendChild(document.createElement('div'));
	const instance = mount(BodyEditor, {
		target,
		props: {
			content: '',
			itemId: 'item-3557',
			hostToken: `bug3557-${Math.random()}`,
			ydoc: doc,
			awareness: new Awareness(doc),
			collabUser: { name: 'Tester', color: '#123456' },
			onEditor: (e: TiptapEditor) => {
				editor = e;
			},
		},
	}) as Record<string, unknown>;
	flushSync();
	try {
		if (!editor) throw new Error('no editor');
		const ed = editor as TiptapEditor;
		ed.commands.setContent(body); // the applier's write (ItemDetail onApplierRequest)
		ed.commands.insertContentAt(ed.state.doc.content.size, 'Z'); // one keystroke
		const tab = flush(ed);
		const materialized = materializeWith(createHeadlessEditor(), { rows: frames, link_index: [] });
		return { tab, materialized };
	} finally {
		unmount(instance);
		target.remove();
		doc.destroy();
	}
}

describe('BUG-3557: angle-bracketed prose survives the editor round trip', () => {
	for (const [label, body, keep] of CASES) {
		it(label, () => {
			const { tab, materialized } = run(body);
			for (const fragment of keep) {
				expect(tab, `tab flush keeps ${fragment}`).toContain(fragment);
				expect(materialized, `materializer keeps ${fragment}`).toContain(fragment);
			}
		});
	}
});

/**
 * The other side of the allowlist: HTML the schema CAN hold still reads as
 * markup, so the editor's own HTML output keeps round-tripping, and text
 * that only looks like an allowlisted tag is escaped on the way out.
 */
describe('BUG-3557: HTML the schema can hold still parses as markup', () => {
	const roundTrip = (md: string) => {
		const ed = createHeadlessEditor();
		try {
			ed.commands.setContent(md);
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			const out = (ed.storage as any).markdown.getMarkdown() as string;
			return { ed, out, json: JSON.stringify(ed.getJSON()) };
		} catch (e) {
			ed.destroy();
			throw e;
		}
	};

	it('an underline tag is an underline mark, not text', () => {
		const { ed, out, json } = roundTrip('some <u>underlined</u> words');
		expect(json).toContain('"underline"');
		expect(out).toContain('<u>underlined</u>');
		ed.destroy();
	});

	it('a literal "<b>" typed as text is escaped on the way out and stays text', () => {
		const ed = createHeadlessEditor();
		ed.commands.setContent('x');
		ed.commands.insertContentAt(ed.state.doc.content.size - 1, { type: 'text', text: ' type <b> for bold' });
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const out = (ed.storage as any).markdown.getMarkdown() as string;
		expect(out).toContain('&lt;b>');
		ed.commands.setContent(out);
		expect(JSON.stringify(ed.getJSON())).not.toContain('"bold"');
		expect(ed.getText()).toContain('type <b> for bold');
		ed.destroy();
	});

	it('an unknown HTML block is kept whole as an HtmlBlock', () => {
		const { ed, json, out } = roundTrip('<details><summary>More</summary>hidden text</details>\n\nafter');
		expect(json).toContain('"htmlBlock"');
		expect(out).toContain('```html\n<details><summary>More</summary>hidden text</details>');
		ed.destroy();
	});

	it('an HTML comment is visible text', () => {
		const { ed } = roundTrip('Before <!-- reviewer note --> after.');
		expect(ed.getText()).toContain('<!-- reviewer note -->');
		ed.destroy();
	});
});
