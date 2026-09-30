// The editor's link mark. Shared by the live editor (Editor.svelte) and
// (TASK-2198) the headless materializer bundle, so anything that changes the
// SCHEMA, renderHTML or markdown serialization belongs here, and nothing DOM-
// or component-dependent may be imported into this module.
import { mergeAttributes } from '@tiptap/core';
import Link from '@tiptap/extension-link';

// Extend Link to render data-href instead of href in the editor DOM.
// This prevents mobile browsers from navigating when tapping links —
// no href attribute means nothing for the browser to follow.
// Mark attributes still store href, so markdown serialization and the
// link popover work unchanged.
export const SafeLink = Link.extend({
	renderHTML({ HTMLAttributes }) {
		const merged = mergeAttributes(this.options.HTMLAttributes, HTMLAttributes);
		const { href, ...rest } = merged;
		return ['a', { ...rest, 'data-href': href }, 0];
	},
});

export const SAFE_LINK_OPTIONS = {
	openOnClick: false,
	autolink: true,
	linkOnPaste: true,
	HTMLAttributes: { class: 'editor-link', target: null, rel: null },
};
