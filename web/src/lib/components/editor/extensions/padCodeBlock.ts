// The editor's codeBlock node, minus its DOM-only parts (the NodeView and the
// copy-button plugin, which Editor.svelte adds on top). Shared by the live
// editor and (TASK-2198) the headless materializer bundle, so anything that
// changes the SCHEMA, renderHTML or markdown serialization belongs here, and
// nothing DOM- or component-dependent may be imported into this module.
import CodeBlock from '@tiptap/extension-code-block';
import { codeBlockMarkdownStorage } from './frontmatter';

export const PadCodeBlock = CodeBlock.extend({
	// A leading frontmatter block round-trips as a codeBlock with language
	// `frontmatter` (BUG-2692); every other language keeps tiptap-markdown's
	// default fence spec, reproduced in codeBlockMarkdownStorage.
	addStorage() {
		return {
			...this.parent?.(),
			markdown: codeBlockMarkdownStorage(this.options.languageClassPrefix),
		};
	},
});

export const PAD_CODE_BLOCK_OPTIONS = { HTMLAttributes: { class: 'code-block' } };
