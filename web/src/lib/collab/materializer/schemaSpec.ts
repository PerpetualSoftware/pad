// A comparable description of an editor's ProseMirror schema and extension
// list (TASK-2198). The bundle exports it as schemaSpec(); the schema-parity
// test builds the same description from the LIVE Editor.svelte mount, so both
// sides go through this one function.
import type { Editor } from '@tiptap/core';

export interface SchemaSpec {
	topNode: string;
	nodeOrder: string[];
	markOrder: string[];
	nodes: Record<string, unknown>;
	marks: Record<string, unknown>;
	/** `${type}:${name}` for every registered extension, in registration order. */
	exts: string[];
}

export function schemaSpecOf(editor: Editor): SchemaSpec {
	const s = editor.schema;
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const attrDefaults = (attrs: Record<string, any> | undefined) =>
		Object.fromEntries(
			Object.entries(attrs ?? {}).map(([a, d]) => [a, d.default === undefined ? '__required__' : d.default]),
		);
	const nodes: Record<string, unknown> = {};
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	s.spec.nodes.forEach((k: string, v: any) => {
		nodes[k] = {
			content: v.content ?? null,
			group: v.group ?? null,
			marks: v.marks ?? null,
			inline: !!v.inline,
			atom: !!v.atom,
			code: !!v.code,
			defining: !!v.defining,
			isolating: !!v.isolating,
			attrs: attrDefaults(v.attrs),
		};
	});
	const marks: Record<string, unknown> = {};
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	s.spec.marks.forEach((k: string, v: any) => {
		marks[k] = {
			inclusive: v.inclusive ?? null,
			excludes: v.excludes ?? null,
			spanning: v.spanning ?? null,
			code: !!v.code,
			attrs: attrDefaults(v.attrs),
		};
	});
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const exts = editor.extensionManager.extensions.map((e: any) => `${e.type}:${e.name}`);
	return {
		topNode: s.topNodeType.name,
		nodeOrder: Object.keys(s.nodes),
		markOrder: Object.keys(s.marks),
		nodes,
		marks,
		exts,
	};
}
