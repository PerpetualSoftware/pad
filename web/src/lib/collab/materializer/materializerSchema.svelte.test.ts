// TASK-2198: the headless materializer must build the SAME schema and
// extension list as the live editor, or its markdown is a different editor's.
//
// The live side is a real Editor.svelte mount on its collab branch, with the
// props ItemDetail passes there (ydoc, awareness, collabUser), so every
// extension production registers takes part. Both descriptions come from one
// function, schemaSpecOf.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import * as Y from 'yjs';
import { Awareness } from 'y-protocols/awareness';
import type { Editor as TiptapEditor } from '@tiptap/core';
import { schemaSpecOf } from './schemaSpec';
import { schemaSpec } from './entry';

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
const { page } = await import('$app/state');

// Registered by Editor.svelte only because a Y.Doc (and awareness) is
// supplied. Neither contributes a node or mark — the node/mark comparison
// below is over the whole schema and would show it if one did.
const LIVE_ONLY = new Set(['extension:collaboration', 'extension:collaborationCaret']);

describe('materializer schema parity (TASK-2198)', () => {
	let target: HTMLElement | null = null;
	let instance: Record<string, unknown> | null = null;

	afterEach(() => {
		if (instance) unmount(instance);
		instance = null;
		target?.remove();
	});

	function mountLive(): TiptapEditor {
		page.params.workspace = 'ws';
		const ydoc = new Y.Doc();
		let tiptap: TiptapEditor | null = null;
		target = document.body.appendChild(document.createElement('div'));
		instance = mount(BodyEditor, {
			target,
			props: {
				content: '',
				itemId: 'item-schema',
				hostToken: 'materializer-schema',
				ydoc,
				awareness: new Awareness(ydoc),
				collabUser: { name: 'Tester', color: '#123456' },
				onEditor: (e: TiptapEditor) => {
					tiptap = e;
				},
			},
		}) as Record<string, unknown>;
		flushSync();
		if (!tiptap) throw new Error('Editor.svelte did not construct a Tiptap editor');
		return tiptap;
	}

	it('the live mount registers the collab extensions this test sets aside', () => {
		const live = schemaSpecOf(mountLive());
		for (const e of LIVE_ONLY) expect(live.exts).toContain(e);
	});

	it('headless schemaSpec() equals the live editor schema', () => {
		const live = schemaSpecOf(mountLive());
		const headless = schemaSpec();
		expect(headless.topNode).toBe(live.topNode);
		expect(headless.nodeOrder).toEqual(live.nodeOrder);
		expect(headless.markOrder).toEqual(live.markOrder);
		expect(headless.nodes).toEqual(live.nodes);
		expect(headless.marks).toEqual(live.marks);
		expect(headless.exts).toEqual(live.exts.filter((e) => !LIVE_ONLY.has(e)));
	});
});
