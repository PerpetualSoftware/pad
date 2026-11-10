import { afterEach, describe, expect, it } from 'vitest';
import { createDocument, type Editor, type JSONContent } from '@tiptap/core';
import * as Y from 'yjs';
import { prosemirrorToYDoc, yXmlFragmentToProseMirrorRootNode } from '@tiptap/y-tiptap';
import { createHeadlessEditor } from '$lib/collab/materializer/headless';
import { canonicalEditorMarkdown } from '$lib/collab/canonicalMarkdown';

const editors: Editor[] = [];
function editor() {
	const ed = createHeadlessEditor();
	editors.push(ed);
	return ed;
}
function markdown(ed: Editor): string { return ed.storage.markdown.getMarkdown(); }
afterEach(() => { for (const ed of editors.splice(0)) ed.destroy(); });

const cases: [string, string][] = [
	['a\nb', 'a\nb'],
	['a\\\nb', 'a\nb'],
	['a  \nb', 'a\nb'],
	['**one**\n_two_', '**one**\n*two*'],
	['**one.**\n_two_', '**one.**\n*two*'],
	['**one\ntwo**', '**one**\n**two**'],
	['[l\nm](u)', '[l](u)\n[m](u)'],
	['Shopping\n2. eggs', 'Shopping\n2. eggs'],
	['a\n7) b', 'a\n7) b'],
	['a\n+', 'a\n+'],
	['a\n1.', 'a\n1.'],
	['a\n#tag', 'a\n#tag'],
	['a\n-5 degrees', 'a\n-5 degrees'],
	['> one\n> two', '> one\n> two'],
	['- one\n  two', '- one\n  two'],
	['1. a\n   b', '1. a\n   b'],
	['```text\na\nb\n```', '```text\na\nb\n```'],
	['---\ntitle: x\n---\nbody\nmore', '---\ntitle: x\n---\n\nbody\nmore'],
	['a\nb\n===', '# a b'],
	['a\nb\n---', '## a b'],
	['a\r\nb', 'a\nb'],
	['a\rb', 'a\nb'],
	['a\\\n\\\nb', 'a\n\\\nb'],
	['a\n \\\nb', 'a\n\\\nb'],
	['a\n- b', 'a\n\n- b'],
	['a\n1. b', 'a\n\n1. b'],
	['a\n01. b', 'a\n\n1. b'],
	['a\n> b', 'a\n\n> b'],
	['a\n# b', 'a\n\n# b'],
];

describe('body line breaks (BUG-4447)', () => {
	for (const [input, expected] of cases) {
		it(`${JSON.stringify(input)} is byte-exact and stable`, () => {
			const ed = editor();
			ed.commands.setContent(input);
			expect(markdown(ed)).toBe(expected);
			expect(canonicalEditorMarkdown(ed, input)).toBe(expected);
			ed.commands.setContent(expected);
			expect(markdown(ed)).toBe(expected);
		});
	}

	for (const input of ['**one\ntwo**', '[l\nm](u)', '**one**\n_two_', '**one.**\n_two_']) {
		it(`live, Y replay and canonical agree for ${JSON.stringify(input)}`, () => {
			const live = editor();
			const parsed = createDocument(live.storage.markdown.parser.parse(input), live.schema);
			const serialized = live.storage.markdown.serializer.serialize(parsed);
			const source = prosemirrorToYDoc(parsed);
			const replay = new Y.Doc();
			try {
				Y.applyUpdate(replay, Y.encodeStateAsUpdate(source));
				const node = yXmlFragmentToProseMirrorRootNode(replay.getXmlFragment('prosemirror'), live.schema);
				const other = editor();
				other.commands.setContent(node.toJSON());
				expect(markdown(other)).toBe(serialized);
				expect(markdown(other)).toBe(canonicalEditorMarkdown(live, input));
			} finally { source.destroy(); replay.destroy(); }
		});
	}

	function documentWith(content: JSONContent[], type = 'paragraph'): JSONContent {
		return { type: 'doc', content: [{ type, ...(type === 'heading' ? { attrs: { level: 1 } } : {}), content }] };
	}
	const text = (value: string): JSONContent => ({ type: 'text', text: value });
	const br: JSONContent = { type: 'hardBreak' };
	for (const marker of ['- x', '1. x', '---', '```', '<div>']) {
		it(`a document-origin ${JSON.stringify(marker)} stays text`, () => {
			const ed = editor();
			ed.commands.setContent(documentWith([text('a'), br, text(marker)]));
			const first = markdown(ed);
			expect(first).toBe(`a\n${marker.startsWith('<') ? '&lt;div>' : marker === '1. x' ? '1\\. x' : marker === '```' ? '\\`\\`\\`' : '\\' + marker}`);
			ed.commands.setContent(first);
			expect(markdown(ed)).toBe(first);
			expect(ed.state.doc.childCount).toBe(1);
			expect(ed.state.doc.firstChild?.type.name).toBe('paragraph');
		});
	}
	for (const [name, content, expected, type] of [
		['table delimiter', [text('| h |'), br, text('|---|')], '| h |\n\\|---|', 'paragraph'],
		['leading break', [br, text('b')], '\\\nb', 'paragraph'],
		['trailing run', [text('a'), br, br], 'a', 'paragraph'],
	] as [string, JSONContent[], string, string][]) {
		it(`${name} keeps its safe form after re-parse`, () => {
			const ed = editor();
			ed.commands.setContent(documentWith(content, type));
			expect(markdown(ed)).toBe(expected);
			ed.commands.setContent(expected);
			expect(markdown(ed)).toBe(expected);
		});
	}

	it('a heading keeps its explicit hard-break encoding', () => {
		const ed = editor();
		ed.commands.setContent(documentWith([text('a'), br, text('b')], 'heading'));
		expect(markdown(ed)).toBe('# a\\\nb');
	});

	it('a whitespace-only previous sibling keeps the explicit break', () => {
		const ed = editor();
		ed.commands.setContent(documentWith([text('a'), br, text(' '), br, text('b')]));
		expect(markdown(ed)).toBe('a\n \\\nb');
	});

	it('multiline markdown paste keeps its lines', () => {
		const ed = editor();
		ed.mount(document.createElement('div'));
		const plugin = ed.state.plugins.find((p) => p.props.clipboardTextParser);
		expect(plugin).toBeDefined();
		const slice = plugin!.props.clipboardTextParser!.call(plugin!, 'one\ntwo', ed.state.doc.resolve(1), false, ed.view);
		expect(slice).not.toBeNull();
		ed.view.dispatch(ed.state.tr.replaceSelection(slice!));
		expect(markdown(ed)).toBe('one\ntwo');
	});

	it('a table cell keeps HTML breaks', () => {
		const ed = editor();
		ed.commands.setContent('| h |\n| --- |\n| a<br>b |');
		const first = markdown(ed);
		expect(first).toContain('a<br>b');
		ed.commands.setContent(first);
		expect(markdown(ed)).toBe(first);
	});
});
