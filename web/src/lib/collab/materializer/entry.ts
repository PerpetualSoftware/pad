// Headless materializer bundle entry (TASK-2198).
//
// Turns an item's collaborative op-log into the markdown a live tab would
// PATCH to items.content when it flushes, by running the editor's own code:
// Yjs -> y-tiptap -> the Tiptap schema -> tiptap-markdown's serializer -> the
// flush pipeline. Built by web/scripts/build-materializer.mjs into ONE IIFE
// (web/build-materializer/materializer.js) that the Go binary embeds and runs
// in goja (internal/materialize). It also imports cleanly under vitest, which
// is how its parity with the live editor is tested.
//
// The output is what the tab's collab flush PATCHes (ItemDetail.svelte's
// createCollabFlusher config, collabFlush.svelte.ts):
//
//     normalize: unescapeDocLinks(getMarkdown())
//     serialize: index non-empty ? markdownToWikiLinks(md, index) : md
//                then cleanBrokenLinks(md)
//
// using the SAME exported functions from $lib/utils/markdown.
//
// The first two imports must stay first: they install globals the editor
// modules read at load.
import './polyfills';
import './domShim';
import * as Y from 'yjs';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import TaskList from '@tiptap/extension-task-list';
import TaskItem from '@tiptap/extension-task-item';
import { Table, TableRow, TableCell, TableHeader } from '@tiptap/extension-table';
import Placeholder from '@tiptap/extension-placeholder';
import { Markdown } from 'tiptap-markdown';
import { yXmlFragmentToProseMirrorRootNode } from '@tiptap/y-tiptap';
import { SafeLink, SAFE_LINK_OPTIONS } from '$lib/components/editor/extensions/safeLink';
import { PadCodeBlock, PAD_CODE_BLOCK_OPTIONS } from '$lib/components/editor/extensions/padCodeBlock';
import { PAD_TABLE_OPTIONS } from '$lib/components/editor/extensions/padTable';
import { HtmlBlock } from '$lib/components/editor/extensions/htmlBlock';
import { BlockDragHandle } from '$lib/components/editor/block-drag-handle';
import { AttachmentImage } from '$lib/components/editor/attachment-image';
import { AttachmentChip } from '$lib/components/editor/attachment-chip';
import { AttachmentUpload } from '$lib/components/editor/attachment-upload';
import { api } from '$lib/api/client';
import { unescapeDocLinks, markdownToWikiLinks, cleanBrokenLinks } from '$lib/utils/markdown';
import type { Item } from '$lib/types';
import { SCHEMA_VERSION } from '$lib/collab/schemaVersion';
import { replayFrames } from './replay';
import { schemaSpecOf, type SchemaSpec } from './schemaSpec';
import {
	installAtBlankPatch,
	atBlankPatched as isAtBlankPatched,
	upstreamAtBlank,
	patchedAtBlank,
} from './atBlank';

// See atBlank.ts: a vendored, behaviour-identical replacement for
// prosemirror-markdown MarkdownSerializerState.atBlank, which is quadratic in
// the output length under goja.
installAtBlankPatch();

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

/** The job materialize() takes, as JSON. */
export interface MaterializeJob {
	/** Content-bearing op-log rows, oldest first, each one y-protocols frame, base64. */
	rows: string[];
	/** The workspace link index (see LinkEntry). */
	link_index?: LinkEntry[] | null;
	/**
	 * The item's workspace slug. A tab builds attachment URLs from its route's
	 * workspace, and those URLs reach the stored markdown when an attachment
	 * sits inside a block tiptap-markdown serializes as HTML (e.g. a table with
	 * a list in a cell). Empty gives the tab's no-workspace form,
	 * `pad-attachment:UUID`.
	 */
	workspace_slug?: string;
}

// The workspace of the job in progress; read by getAttachmentUrl, which the
// editor (built once) captures at construction.
let jobWorkspace = '';

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
		StarterKit.configure({ codeBlock: false, link: false, undoRedo: false }),
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

let editor: Editor | null = null;
function getEditor(): Editor {
	if (!editor) editor = createHeadlessEditor();
	return editor;
}

function base64ToBytes(s: string): Uint8Array {
	const bin = atob(s);
	const out = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
	return out;
}

/** The tab's flush pipeline over a serializer output (see the file header). */
export function flushPipeline(rawMarkdown: string, index: readonly LinkEntry[]): string {
	let md = unescapeDocLinks(rawMarkdown);
	if (index.length > 0) md = markdownToWikiLinks(md, index as unknown as Item[]);
	return cleanBrokenLinks(md);
}

/** Materialize one op-log. `jobJSON` is a MaterializeJob; returns the markdown. */
export function materialize(jobJSON: string): string {
	return materializeWith(getEditor(), JSON.parse(jobJSON) as MaterializeJob);
}

/** materialize() against a given headless editor (tests pass a variant). */
export function materializeWith(ed: Editor, job: MaterializeJob): string {
	if (!job || !Array.isArray(job.rows)) throw new Error('materialize: job.rows must be an array');
	const index = job.link_index ?? [];
	if (!Array.isArray(index)) throw new Error('materialize: job.link_index must be an array');
	const doc = new Y.Doc();
	try {
		replayFrames(doc, job.rows.map(base64ToBytes));
		jobWorkspace = typeof job.workspace_slug === 'string' ? job.workspace_slug : '';
		const pm = yXmlFragmentToProseMirrorRootNode(doc.getXmlFragment('default'), ed.schema);
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const raw: string = (ed.storage as any).markdown.serializer.serialize(pm);
		return flushPipeline(raw, index);
	} finally {
		jobWorkspace = '';
		doc.destroy();
	}
}

/** The headless editor's schema description (see schemaSpec.ts). */
export function schemaSpec(): SchemaSpec {
	return schemaSpecOf(getEditor());
}

/** web/src/lib/collab/schemaVersion.ts SCHEMA_VERSION, as built into this bundle. */
export function schemaVersion(): string {
	return SCHEMA_VERSION;
}

/** True iff the vendored atBlank patch is what the serializer will call. */
export function atBlankPatched(): boolean {
	return isAtBlankPatched();
}

/**
 * Test hook for the Go-side atBlank guard: both implementations' answers for a
 * serializer state whose output so far is `out`, as JSON [upstream, patched].
 * The upstream one is the reference atBlank.ts saved before patching.
 */
export function atBlankPair(out: string): string {
	return JSON.stringify([upstreamAtBlank.call({ out }), patchedAtBlank.call({ out })]);
}

// The global the Go runner reads. JSON-in / string-out keeps the boundary to
// primitive values.
(globalThis as unknown as { Materializer: unknown }).Materializer = {
	materialize,
	schemaSpec: () => JSON.stringify(schemaSpec()),
	schemaVersion,
	atBlankPatched,
	atBlankPair,
};
