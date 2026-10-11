// The headless editor and the tab's flush pipeline, without the goja shims
// or the atBlank patch (TASK-2198's bundle entry, ./entry.ts, installs those
// before importing this module and re-exports what is here). Split out for
// BUG-3540 so the browser can canonicalize markdown the way an open tab's
// flush would, without patching the live editor's serializer: importing
// ./entry in a browser would install the atBlank patch on every tab.
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import TaskList from '@tiptap/extension-task-list';
import TaskItem from '@tiptap/extension-task-item';
import { Table, TableRow, TableCell, TableHeader } from '@tiptap/extension-table';
import Placeholder from '@tiptap/extension-placeholder';
import { Markdown } from 'tiptap-markdown';
import { SafeLink, SAFE_LINK_OPTIONS } from '$lib/components/editor/extensions/safeLink';
import { PadCodeBlock, PAD_CODE_BLOCK_OPTIONS } from '$lib/components/editor/extensions/padCodeBlock';
import { PAD_TABLE_OPTIONS } from '$lib/components/editor/extensions/padTable';
import { HtmlBlock } from '$lib/components/editor/extensions/htmlBlock';
import { htmlProseExtensions } from '$lib/components/editor/extensions/htmlProse';
import { BlockDragHandle } from '$lib/components/editor/block-drag-handle';
import { AttachmentImage } from '$lib/components/editor/attachment-image';
import { AttachmentChip } from '$lib/components/editor/attachment-chip';
import { AttachmentUpload } from '$lib/components/editor/attachment-upload';
import { api } from '$lib/api/client';
import { unescapeDocLinks, markdownToWikiLinks, cleanBrokenLinks } from '$lib/utils/markdown';
import type { Item } from '$lib/types';

/**
 * One entry of the workspace link index, as markdownToWikiLinks reads an Item:
 * `slug`, `item_number`, `collection_prefix` (together they form the ref) and
 * `title`. Nothing else is read — cleanBrokenLinks reads no index at all.
 * Order matters: the first entry that matches a link wins, exactly as
 * `items.find` does over the tab's localIndex.getAll(ws).
 */
export interface LinkEntry {
	slug: string;
	title: string;
	item_number?: number | null;
	collection_prefix?: string | null;
}

// The workspace of the job in progress; read by getAttachmentUrl, which the
// editor (built once) captures at construction.
let jobWorkspace = '';

/** Sets the workspace the attachment helpers build URLs for (the materializer's job). */
export function setJobWorkspace(ws: string): void {
	jobWorkspace = ws;
}

// Mirrors Editor.svelte's getAttachmentUrl, including its no-workspace arm.
const getAttachmentUrl = (uuid: string, variant?: 'thumb-sm' | 'thumb-md' | 'original') =>
	jobWorkspace ? api.attachments.downloadUrl(jobWorkspace, uuid, variant) : `pad-attachment:${uuid}`;
// Events a NodeView would stamp with this; there are no NodeViews headless.
const readHostAddress = () => ({ workspaceSlug: jobWorkspace, itemId: '', hostToken: 'materializer' });
const headless = async (): Promise<never> => {
	throw new Error('materializer is headless');
};

/**
 * The extension list, mirroring Editor.svelte's `extensions` array on its
 * collab branch (a Y.Doc supplied). Differences, all without schema or
 * serializer effect:
 *   - Collaboration / CollaborationCaret are left out: the document comes
 *     straight from the Y.Doc via yXmlFragmentToProseMirrorRootNode, and
 *     neither contributes a node or mark.
 *   - Editor.svelte extends PadCodeBlock with a NodeView and a copy plugin,
 *     and Table with a copy plugin; both are view-only.
 *   - Callbacks that touch the network, the DOM or UI (transform, upload,
 *     onError, onNotice) are inert.
 * The schema-parity test (materializerSchema.svelte.test.ts) compares this
 * against a real Editor.svelte mount.
 */
export function headlessExtensions(markdownOverrides: Record<string, unknown> = {}) {
	return [
		StarterKit.configure({ codeBlock: false, link: false, undoRedo: false, text: false }),
		...htmlProseExtensions(),
		PadCodeBlock.configure(PAD_CODE_BLOCK_OPTIONS),
		HtmlBlock,
		TaskList,
		TaskItem.configure({ nested: true }),
		Table.configure(PAD_TABLE_OPTIONS),
		TableRow,
		TableCell,
		TableHeader,
		SafeLink.configure(SAFE_LINK_OPTIONS),
		Placeholder.configure({ placeholder: 'Type / for commands...' }),
		// linkify stays off (tiptap-markdown's default) unless markdown-it is
		// >= 14.3.1: below that, linkify: true parses in quadratic time
		// (GHSA-253c-mchw-3w2r), and this bundle runs on the server.
		Markdown.configure({
			html: true,
			transformPastedText: true,
			transformCopiedText: true,
			// Empty in production; the parity test's negative control passes a
			// deliberately wrong serializer option here.
			...markdownOverrides,
		}),
		BlockDragHandle,
		AttachmentImage.configure({
			getDownloadUrl: getAttachmentUrl,
			workspaceSlug: '',
			address: readHostAddress,
			supportedFormats: () => [],
			transform: headless,
			onError: () => {},
		}),
		AttachmentChip.configure({ getDownloadUrl: getAttachmentUrl, workspaceSlug: '', address: readHostAddress }),
		AttachmentUpload.configure({ upload: headless, onError: () => {}, onNotice: () => {} }),
	];
}

/** A headless editor over headlessExtensions(markdownOverrides). */
export function createHeadlessEditor(markdownOverrides: Record<string, unknown> = {}): Editor {
	return new Editor({ element: null, extensions: headlessExtensions(markdownOverrides), content: '' });
}

/** The tab's flush pipeline over a serializer output (see the file header). */
export function flushPipeline(rawMarkdown: string, index: readonly LinkEntry[]): string {
	let md = unescapeDocLinks(rawMarkdown);
	if (index.length > 0) md = markdownToWikiLinks(md, index as unknown as Item[]);
	return cleanBrokenLinks(md);
}
