// BUG-3111: a code block's Copy button copied a remote peer's display name.
//
// Driven through a REAL Tiptap editor mounting the SAME NodeView factory
// Editor.svelte mounts, with a widget decoration planted inside the block.
// The decoration's DOM is the CollaborationCaret extension's own default
// render — the element that put a real name into the clipboard in the
// browser repro — rather than a hand-built look-alike.
//
// Every copy assertion is paired with a precondition that the decoration is
// really inside the block's contentDOM and that the old DOM read WOULD have
// included it. Without that leg, a decoration that never rendered would make
// "the clipboard holds only the code" pass against the bug (CONVE-12).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor, Extension } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import CodeBlock from '@tiptap/extension-code-block';
import { CollaborationCaret } from '@tiptap/extension-collaboration-caret';
import { Plugin } from '@tiptap/pm/state';
import { Decoration, DecorationSet } from '@tiptap/pm/view';

const copied: string[] = [];
vi.mock('$lib/utils/clipboard', () => ({
	copyToClipboard: async (text: string) => {
		copied.push(text);
		return true;
	},
}));

const { createPlainCodeBlockView } = await import('./codeBlockCopy');

const CODE = 'const answer = 42;';
const PEER = 'Peer Person';

// The extension's DEFAULT caret render, the one Editor.svelte uses (it
// configures CollaborationCaret with no custom `render`).
const renderCaret = (CollaborationCaret as any).options.render as (user: {
	name: string;
	color: string;
}) => HTMLElement;

// A widget decoration inside the first code block, `offset` characters into
// its text — where a remote caret would sit.
function caretInCodeBlock(offset: number) {
	return Extension.create({
		name: 'fakePeerCaret',
		addProseMirrorPlugins() {
			return [
				new Plugin({
					props: {
						decorations(state) {
							let at = -1;
							state.doc.descendants((node, pos) => {
								if (at < 0 && node.type.name === 'codeBlock') at = pos + 1 + offset;
								return at < 0;
							});
							if (at < 0) return DecorationSet.empty;
							return DecorationSet.create(state.doc, [
								Decoration.widget(at, () => renderCaret({ name: PEER, color: '#ff0000' })),
							]);
						},
					},
				}),
			];
		},
	});
}

const PlainCodeBlock = CodeBlock.extend({
	addNodeView() {
		return ((props: any) => createPlainCodeBlockView(props)) as any;
	},
});

let editor: Editor | null = null;
let host: HTMLElement;

function mount(extra: Extension[] = []) {
	host = document.createElement('div');
	document.body.appendChild(host);
	editor = new Editor({
		element: host,
		extensions: [StarterKit.configure({ codeBlock: false }), PlainCodeBlock, ...extra],
		content: `<p>before</p><pre><code class="language-js">${CODE}</code></pre><p>after</p>`,
	});
	return editor;
}

function copyButton(): HTMLButtonElement {
	const btn = host.querySelector<HTMLButtonElement>('pre.code-block .code-copy-btn');
	if (!btn) throw new Error('no copy button rendered');
	return btn;
}

async function clickCopy() {
	copyButton().click();
	// The click handler awaits the (mocked) clipboard write.
	await Promise.resolve();
	await Promise.resolve();
}

beforeEach(() => {
	copied.length = 0;
});
afterEach(() => {
	editor?.destroy();
	editor = null;
	host?.remove();
});

describe('BUG-3111: code block Copy reads the model, not the contentDOM', () => {
	it('excludes a peer caret label sitting inside the block', async () => {
		mount([caretInCodeBlock(9)]);
		const code = host.querySelector('pre.code-block code')!;

		// PRECONDITION: the caret is inside the contentDOM, and the DOM read
		// the bug used would have carried the peer's name.
		expect(code.querySelector('.collaboration-carets__label')?.textContent).toBe(PEER);
		expect(code.textContent).toBe('const ans' + PEER + 'wer = 42;');

		await clickCopy();
		expect(copied).toEqual([CODE]);
	});

	it('copies text edited after the view was built', async () => {
		// A NodeView without update() is not told about edits, so a copy that
		// captured the node at creation would return the ORIGINAL text.
		const ed = mount();
		let end = -1;
		ed.state.doc.descendants((node, pos) => {
			if (end < 0 && node.type.name === 'codeBlock') end = pos + 1 + node.content.size;
			return end < 0;
		});
		ed.commands.insertContentAt(end, ' // edited');

		await clickCopy();
		expect(copied).toEqual([CODE + ' // edited']);
	});

	it('reports failure instead of copying when the block cannot be located', async () => {
		mount();
		const btn = copyButton();
		// Simulate ProseMirror having discarded this view: getPos answers
		// undefined. Rebuild the same factory with a dead getPos and click it.
		const dead = createPlainCodeBlockView({
			node: { attrs: { language: 'js' } },
			view: editor!.view as any,
			getPos: () => undefined,
		});
		host.appendChild(dead.dom);
		const deadBtn = dead.dom.querySelector<HTMLButtonElement>('.code-copy-btn')!;
		expect(deadBtn).not.toBe(btn);
		deadBtn.click();
		await Promise.resolve();
		await Promise.resolve();
		expect(copied).toEqual([]);
		expect(deadBtn.textContent).toBe('Failed');
	});

	it('reports failure when the position resolves to something other than a code block', async () => {
		// Position 0 is the leading paragraph. Copying ITS text would put the
		// wrong content on the clipboard with a success flash.
		mount();
		const stray = createPlainCodeBlockView({
			node: { attrs: { language: 'js' } },
			view: editor!.view as any,
			getPos: () => 0,
		});
		host.appendChild(stray.dom);
		const strayBtn = stray.dom.querySelector<HTMLButtonElement>('.code-copy-btn')!;
		strayBtn.click();
		await Promise.resolve();
		await Promise.resolve();
		expect(copied).toEqual([]);
		expect(strayBtn.textContent).toBe('Failed');
	});
});
