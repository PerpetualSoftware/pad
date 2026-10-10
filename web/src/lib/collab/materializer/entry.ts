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
import { yXmlFragmentToProseMirrorRootNode } from '@tiptap/y-tiptap';
import { createHeadlessEditor, flushPipeline, setJobWorkspace, type LinkEntry } from './headless';
export { headlessExtensions, createHeadlessEditor, flushPipeline, type LinkEntry } from './headless';
import { SCHEMA_VERSION } from '$lib/collab/schemaVersion';
import { replayFrames, updateFrame } from './replay';
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
		setJobWorkspace(typeof job.workspace_slug === 'string' ? job.workspace_slug : '');
		const pm = yXmlFragmentToProseMirrorRootNode(doc.getXmlFragment('default'), ed.schema);
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const raw: string = (ed.storage as any).markdown.serializer.serialize(pm);
		return flushPipeline(raw, index);
	} finally {
		setJobWorkspace('');
		doc.destroy();
	}
}

function bytesToBase64(b: Uint8Array): string {
	let bin = '';
	for (let i = 0; i < b.length; i++) bin += String.fromCharCode(b[i]);
	return btoa(bin);
}

/** What snapshot() returns: the markdown, and the doc's whole state as ONE op-log frame. */
export interface SnapshotResult {
	markdown: string;
	/** Base64 y-protocols Update frame carrying Y.encodeStateAsUpdate of the replayed doc. */
	frame: string;
}

/**
 * Compact one op-log into a single frame (TASK-3531). The frame replaces the
 * rows it was built from, so it must be exactly as good as them: the
 * document the rows replay to, which is the document every tab replaying them
 * holds (tabs run this same replay). It REFUSES (throws) rather than return:
 *   - a frame whose replay alone does not give the same Yjs snapshot (state
 *     vector and delete set) and the same markdown as the rows did;
 *   - a document with structs left pending (they reference something no row
 *     holds): carried in a frame but never shown, and indistinguishable
 *     afterwards, so such an op-log is left as it is.
 * A row that does not replay at all (a malformed frame) contributes nothing
 * to that document, in the materializer or in a tab, so compacting it away
 * loses nothing that ever existed.
 */
export function snapshot(jobJSON: string): string {
	return JSON.stringify(snapshotWith(getEditor(), JSON.parse(jobJSON) as MaterializeJob));
}

/** snapshot() against a given headless editor (tests pass a variant). */
export function snapshotWith(ed: Editor, job: MaterializeJob): SnapshotResult {
	if (!job || !Array.isArray(job.rows)) throw new Error('snapshot: job.rows must be an array');
	const doc = new Y.Doc();
	const check = new Y.Doc();
	try {
		replayFrames(doc, job.rows.map(base64ToBytes));
		if (doc.store.pendingStructs || doc.store.pendingDs) {
			throw new Error('snapshot: the op-log leaves pending structs; refusing to compact');
		}
		const frame = updateFrame(Y.encodeStateAsUpdate(doc));
		replayFrames(check, [frame]);
		// Unreachable with a correct Yjs encoder: anything the frame lost would
		// also change the markdown checked below. Kept as a guard against an
		// encoder regression, which no test input can produce (TASK-3531).
		if (!Y.equalSnapshots(Y.snapshot(doc), Y.snapshot(check))) {
			throw new Error('snapshot: the compacted frame does not reproduce the document; refusing to compact');
		}
		const markdown = materializeWith(ed, job);
		const again = materializeWith(ed, { ...job, rows: [bytesToBase64(frame)] });
		if (again !== markdown) {
			throw new Error('snapshot: the compacted frame renders different markdown; refusing to compact');
		}
		return { markdown, frame: bytesToBase64(frame) };
	} finally {
		doc.destroy();
		check.destroy();
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
	snapshot,
	schemaSpec: () => JSON.stringify(schemaSpec()),
	schemaVersion,
	atBlankPatched,
	atBlankPair,
};
