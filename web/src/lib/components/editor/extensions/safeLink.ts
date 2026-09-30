// The editor's link mark. Shared by the live editor (Editor.svelte) and
// (TASK-2198) the headless materializer bundle, so anything that changes the
// SCHEMA, renderHTML or markdown serialization belongs here, and nothing DOM-
// or component-dependent may be imported into this module.
import { mergeAttributes } from '@tiptap/core';
import Link from '@tiptap/extension-link';
import { FOLLOWS_TITLE_PREFIX } from '$lib/utils/followsTitle';

// Extend Link to render data-href instead of href in the editor DOM.
// This prevents mobile browsers from navigating when tapping links —
// no href attribute means nothing for the browser to follow.
// Mark attributes still store href, so markdown serialization and the
// link popover work unchanged.
//
// The follows-title marker (BUG-3315, FOLLOWS_TITLE_PREFIX in $lib/utils/markdown)
// is dropped from the DOM too: it is the title AS LOADED, which after a rename
// is stale, so it must not show as a tooltip. The mark attribute itself is kept,
// because the save reads it. A title a user wrote is rendered as before.
export const SafeLink = Link.extend({
	renderHTML({ HTMLAttributes }) {
		const merged = mergeAttributes(this.options.HTMLAttributes, HTMLAttributes);
		const { href, title, ...rest } = merged;
		const marker = typeof title === 'string' && title.startsWith(FOLLOWS_TITLE_PREFIX);
		return ['a', { ...rest, ...(marker || title == null ? {} : { title }), 'data-href': href }, 0];
	},
});

export const SAFE_LINK_OPTIONS = {
	openOnClick: false,
	autolink: true,
	linkOnPaste: true,
	HTMLAttributes: { class: 'editor-link', target: null, rel: null },
};
