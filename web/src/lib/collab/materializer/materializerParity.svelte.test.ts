// TASK-2198: materialize(op-log) must equal what the live tab would flush.
//
// Each case drives TWO real Editor.svelte mounts on their collab branch (the
// props ItemDetail passes there), each bound to its own Y.Doc, relaying
// updates between the docs the way the server relays between tabs. Seeded
// random edits land on both, sometimes concurrently. Every local update either
// tab emits is recorded as the y-protocols Update frame that tab would send,
// which is what item_yjs_updates stores; a SyncStep1, a SyncStep2 answer, an
// awareness frame and a malformed sync frame are mixed in to exercise replay's
// skip and catch paths.
//
// EXPECTED is tab A's own flush output, computed exactly as ItemDetail.svelte's
// collab flusher does (its `normalize` and `serialize` lambdas, restated below
// with the same $lib/utils/markdown functions against a tab-shaped Item index)
// over the live editor's getMarkdown() — with prosemirror-markdown's UPSTREAM
// atBlank, since importing the bundle entry installs the vendored patch in this
// process. ACTUAL is materialize() over the recorded frames with the 4-field
// link index projection.
//
// With PAD_GEN_MATERIALIZE_CORPUS=1 the cases are also written to
// internal/materialize/testdata/corpus.json, where TestCorpusParity replays
// them through the embedded bundle in goja. All content is synthetic.
import { describe, it, expect, vi, beforeAll, afterAll } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import fs from 'node:fs';
import path from 'node:path';
import * as Y from 'yjs';
import * as syncProtocol from 'y-protocols/sync';
import * as encoding from 'lib0/encoding';
import { Awareness } from 'y-protocols/awareness';
import type { Editor as TiptapEditor } from '@tiptap/core';
import type { Item } from '$lib/types';
import { unescapeDocLinks, markdownToWikiLinks, cleanBrokenLinks } from '$lib/utils/markdown';
import { materialize, materializeWith, createHeadlessEditor, type LinkEntry, type MaterializeJob } from './entry';
import { updateFrame, MESSAGE_SYNC } from './replay';
import { withUpstreamAtBlank } from './atBlank';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

// The real URL builder: attachment URLs reach the stored markdown through
// HTML-serialized blocks, so a stub here would hide a divergence.
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

const WS = 'ws';
const CASES = 50;

// ---------------------------------------------------------------- inputs

function mulberry32(seed: number): () => number {
	let a = seed >>> 0;
	return () => {
		a = (a + 0x6d2b79f5) | 0;
		let t = Math.imul(a ^ (a >>> 15), 1 | a);
		t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
}

// A tab's localIndex holds full Items; these carry more than the four fields
// the materializer is given, so the projection is part of what is tested.
const TAB_INDEX: Item[] = [
	{ id: 'i-1', slug: 'fix-login', title: 'Fix login', item_number: 3, collection_prefix: 'TASK', collection_slug: 'tasks' },
	{ id: 'i-2', slug: 'roadmap', title: 'Roadmap', item_number: 7, collection_prefix: 'PLAN', collection_slug: 'plans' },
	{ id: 'i-3', slug: 'legacy-doc', title: 'Legacy doc', item_number: null, collection_prefix: null, collection_slug: 'docs' },
	// Same ref-less slug shape as a later entry would have: first match wins.
	{ id: 'i-4', slug: 'dup', title: 'First dup', item_number: 12, collection_prefix: 'BUG', collection_slug: 'bugs' },
	{ id: 'i-5', slug: 'dup', title: 'Second dup', item_number: 13, collection_prefix: 'BUG', collection_slug: 'bugs' },
] as unknown as Item[];

function projectIndex(items: Item[]): LinkEntry[] {
	return items.map((i) => {
		const e: LinkEntry = { slug: i.slug, title: i.title };
		if (i.item_number) e.item_number = i.item_number;
		if (i.collection_prefix) e.collection_prefix = i.collection_prefix;
		return e;
	});
}

const UUID1 = '0b1f3c9e-1d2a-4e5f-8a9b-0c1d2e3f4a5b';
const UUID2 = '5c6d7e8f-9a0b-4c1d-8e2f-3a4b5c6d7e8f';
const UUID3 = 'a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d';

const LINKS = [
	'[Fix login](/alice/ws/tasks/TASK-3)',
	'[custom text](/alice/ws/plans/PLAN-7)',
	'[Legacy doc](/alice/ws/docs/legacy-doc)',
	'[a dup](/alice/ws/bugs/dup)',
	'[other::TASK-9](/-/r/other/TASK-9)',
	'[shown](/-/r/other/TASK-9)',
	'[Missing page](broken)',
	'[unknown](/alice/ws/tasks/TASK-999)',
	'[external](https://example.com/a?b=c)',
	'[esc \\[\\[ text](/alice/ws/tasks/TASK-3)',
];

const BLOCKS = [
	'# Heading one',
	'## Sub *heading*',
	'### H3 with `code`',
	'Plain paragraph with **bold**, *italic*, ~~strike~~ and `code`.',
	'- one\n- two\n  - nested\n- three',
	'1. first\n2. second\n3. third',
	'- [ ] open task\n- [x] done task\n  - [ ] nested task',
	'> quoted line\n> second line',
	'| a | b |\n| --- | --- |\n| 1 | 2 |\n| x | y |',
	'<table><tr><th>Head</th></tr><tr><td><ul><li>list in a cell</li></ul></td></tr></table>',
	'<table><tr><td><p>para one</p><p>para two</p></td><td>x</td></tr></table>',
	`<table><tr><td><p>with image</p><p><img src="pad-attachment:${UUID1}" alt="shot"></p></td></tr></table>`,
	'<table><tr><td colwidth="120">wide</td><td>narrow</td></tr></table>',
	'non breaking spaces here',
	'```go\nfunc main() {\n\tprintln("hi")\n}\n```',
	'```mermaid\ngraph TD\n  A-->B\n```',
	'```\nplain fence\n```',
	'---',
	`![diagram](pad-attachment:${UUID2})`,
	`[report.pdf](pad-attachment:${UUID3})`,
	'Line one\\\nline two',
	'<div class="note">raw <b>html</b> block</div>',
	'Autolink https://example.com/path here',
	'Typed literal [[not a link]] stays text',
	'See [[TASK-3]] and [[Roadmap]]',
	...LINKS.map((l) => `Paragraph linking ${l} inline.`),
];

const FRONTMATTER = '---\ntitle: "Synthetic"\ntags: [a, b]\n---\n\n';
const WORDS = ['alpha', 'beta ', ' gamma', 'δelta', 'emoji 🎉', '[', ']', '*', '_', '`', '\\', '#', '<b>', '|', 'https://x.test/p'];

// ---------------------------------------------------------------- harness

const RELAY = Symbol('relay');

interface Tab {
	doc: Y.Doc;
	editor: TiptapEditor;
	instance: Record<string, unknown>;
	target: HTMLElement;
}

function mountTab(): Tab {
	const doc = new Y.Doc();
	let editor: TiptapEditor | null = null;
	const target = document.body.appendChild(document.createElement('div'));
	const instance = mount(BodyEditor, {
		target,
		props: {
			content: '',
			itemId: 'item-parity',
			hostToken: `parity-${Math.random()}`,
			ydoc: doc,
			awareness: new Awareness(doc),
			collabUser: { name: 'Tester', color: '#123456' },
			onEditor: (e: TiptapEditor) => {
				editor = e;
			},
		},
	}) as Record<string, unknown>;
	flushSync();
	if (!editor) throw new Error('Editor.svelte did not construct a Tiptap editor');
	return { doc, editor, instance, target };
}

function unmountTab(t: Tab) {
	unmount(t.instance);
	t.target.remove();
	t.doc.destroy();
}

const tick = () => new Promise<void>((r) => setTimeout(r, 0));

function b64(u: Uint8Array): string {
	return Buffer.from(u).toString('base64');
}

/** The tab's flush output: ItemDetail's normalize, then serialize. */
function liveFlush(editor: TiptapEditor, index: Item[]): string {
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const md = withUpstreamAtBlank(() => (editor.storage as any).markdown.getMarkdown() as string);
	const normalized = unescapeDocLinks(md); // normalize
	let toSave = normalized; // serialize
	if (index.length > 0) toSave = markdownToWikiLinks(toSave, index);
	return cleanBrokenLinks(toSave);
}

interface Case {
	name: string;
	rows: string[];
	link_index: LinkEntry[];
	workspace_slug: string;
	expected: string;
}

function randomTextPos(ed: TiptapEditor, rnd: () => number): number | null {
	const size = ed.state.doc.content.size;
	for (let tries = 0; tries < 20; tries++) {
		const pos = 1 + Math.floor(rnd() * Math.max(1, size - 1));
		try {
			if (ed.state.doc.resolve(pos).parent.inlineContent) return pos;
		} catch {
			/* out of range */
		}
	}
	return null;
}

function topLevelBoundary(ed: TiptapEditor, rnd: () => number): number {
	const bounds = [0];
	ed.state.doc.forEach((node, offset) => bounds.push(offset + node.nodeSize));
	return bounds[Math.floor(rnd() * bounds.length)];
}

function randomOp(ed: TiptapEditor, rnd: () => number): void {
	const pick = <T,>(xs: T[]) => xs[Math.floor(rnd() * xs.length)];
	const size = ed.state.doc.content.size;
	const r = rnd();
	try {
		if (r < 0.22) {
			ed.commands.insertContentAt(topLevelBoundary(ed, rnd), pick(BLOCKS));
		} else if (r < 0.42) {
			const pos = randomTextPos(ed, rnd);
			if (pos !== null) ed.commands.insertContentAt(pos, { type: 'text', text: pick(WORDS) });
		} else if (r < 0.55) {
			const from = 1 + Math.floor(rnd() * Math.max(1, size - 2));
			const to = Math.min(size, from + 1 + Math.floor(rnd() * 20));
			ed.commands.deleteRange({ from, to });
		} else if (r < 0.68) {
			const from = randomTextPos(ed, rnd);
			if (from !== null) {
				const to = Math.min(ed.state.doc.resolve(from).end(), from + 1 + Math.floor(rnd() * 8));
				const chain = ed.chain().setTextSelection({ from, to });
				pick([
					() => chain.toggleBold(),
					() => chain.toggleItalic(),
					() => chain.toggleCode(),
					() => chain.toggleStrike(),
					() => chain.setLink({ href: pick(['/alice/ws/tasks/TASK-3', 'https://example.com', 'broken', '/-/r/other/TASK-9']) }),
				])().run();
			}
		} else if (r < 0.8) {
			const pos = randomTextPos(ed, rnd);
			if (pos !== null) {
				const chain = ed.chain().setTextSelection(pos);
				pick([
					() => chain.toggleBulletList(),
					() => chain.toggleOrderedList(),
					() => chain.toggleTaskList(),
					() => chain.setHeading({ level: pick([1, 2, 3] as const) }),
					() => chain.setParagraph(),
					() => chain.toggleBlockquote(),
					() => chain.setCodeBlock({ language: pick(['go', 'mermaid', 'frontmatter', '']) }),
				])().run();
			}
		} else if (r < 0.9) {
			const pos = randomTextPos(ed, rnd);
			if (pos !== null) ed.chain().setTextSelection(pos).splitBlock().run();
		} else {
			const pos = randomTextPos(ed, rnd);
			if (pos !== null) ed.chain().setTextSelection(pos).setHardBreak().run();
		}
	} catch {
		// An op that does not apply at this position is simply skipped.
	}
}

async function runCase(seed: number): Promise<Case> {
	const rnd = mulberry32(seed);
	const a = mountTab();
	const b = mountTab();
	const frames: Uint8Array[] = [];
	const queue: Array<{ to: Y.Doc; update: Uint8Array }> = [];
	a.doc.on('update', (u: Uint8Array, origin: unknown) => {
		if (origin === RELAY) return;
		frames.push(updateFrame(u));
		queue.push({ to: b.doc, update: u });
	});
	b.doc.on('update', (u: Uint8Array, origin: unknown) => {
		if (origin === RELAY) return;
		frames.push(updateFrame(u));
		queue.push({ to: a.doc, update: u });
	});
	let relayThrew = 0;
	const drain = () => {
		for (let guard = 0; queue.length > 0 && guard < 10_000; guard++) {
			const { to, update } = queue.shift()!;
			// A throw here comes from the receiving tab's y-tiptap binding
			// (observed: its selection restore resolving a stale position). In a
			// tab that throw escapes one onmessage call and the next message is
			// still handled, so the relay carries on the same way.
			try {
				Y.applyUpdate(to, update, RELAY);
			} catch {
				relayThrew++;
			}
		}
	};
	const pump = async () => {
		drain();
		await tick();
		drain();
	};
	// Collapse both selections to the document start, so a later remote change
	// does not have a stale range to restore.
	const park = () => {
		for (const t of [a, b]) {
			try {
				t.editor.commands.setTextSelection(1);
			} catch {
				/* empty doc */
			}
		}
	};
	try {
		// A joining tab's SyncStep1: a state vector, never applied.
		{
			const enc = encoding.createEncoder();
			encoding.writeVarUint(enc, MESSAGE_SYNC);
			syncProtocol.writeSyncStep1(enc, a.doc);
			frames.push(encoding.toUint8Array(enc));
		}
		// The lazy seed a tab writes into an empty document.
		const blocks = Array.from({ length: 3 + Math.floor(rnd() * 4) }, () => BLOCKS[Math.floor(rnd() * BLOCKS.length)]);
		a.editor.commands.setContent((rnd() < 0.3 ? FRONTMATTER : '') + blocks.join('\n\n'));
		await pump();

		const ops = 10 + Math.floor(rnd() * 15);
		for (let i = 0; i < ops; i++) {
			randomOp(rnd() < 0.6 ? a.editor : b.editor, rnd);
			if (rnd() < 0.3) randomOp(rnd() < 0.5 ? a.editor : b.editor, rnd); // concurrent, before relay
			park();
			await pump();
			if (i === 3) {
				// A SyncStep2 answer carrying tab B's whole state (duplicates).
				const enc = encoding.createEncoder();
				encoding.writeVarUint(enc, MESSAGE_SYNC);
				syncProtocol.writeSyncStep2(enc, b.doc);
				frames.push(encoding.toUint8Array(enc));
				frames.push(new Uint8Array([1, 3, 1, 2, 3])); // awareness frame: not a sync message
				frames.push(new Uint8Array([MESSAGE_SYNC, 7, 1, 0])); // unknown sync subtype: throws
			}
		}
		await pump();

		const index = rnd() < 0.2 ? [] : TAB_INDEX;
		const expected = liveFlush(a.editor, index);
		// The two tabs must agree, or the case does not have one right answer.
		expect(liveFlush(b.editor, index)).toBe(expected);
		relayThrows += relayThrew;
		return {
			name: `seed-${seed}`,
			rows: frames.map(b64),
			link_index: projectIndex(index),
			workspace_slug: WS,
			expected,
		};
	} finally {
		unmountTab(a);
		unmountTab(b);
	}
}

// Relay throws across all cases, reported so a generator change that makes
// them common is visible.
let relayThrows = 0;

// ---------------------------------------------------------------- tests

describe('materializer parity with the live editor flush (TASK-2198)', () => {
	const cases: Case[] = [];

	beforeAll(async () => {
		page.params.workspace = WS;
		for (let s = 1; s <= CASES; s++) cases.push(await runCase(s * 7919));
	}, 120_000);

	afterAll(() => {
		if (process.env.PAD_GEN_MATERIALIZE_CORPUS === '1') {
			const out = path.resolve(process.cwd(), '../internal/materialize/testdata/corpus.json');
			fs.mkdirSync(path.dirname(out), { recursive: true });
			fs.writeFileSync(out, JSON.stringify(cases, null, 1) + '\n');
		}
	});

	it('generates varied cases', () => {
		expect(cases).toHaveLength(CASES);
		console.info(`materializer parity: ${CASES} cases, ${cases.reduce((n, c) => n + c.rows.length, 0)} frames, ${relayThrows} relay throws`);
		const all = cases.map((c) => c.expected).join('\n');
		// The generator reaches the constructs it claims to (vantage point).
		for (const probe of ['<table', '| --- |', '- [ ]', '```mermaid', '```go', '[[TASK-3]]', 'pad-attachment:', '/api/v1/workspaces/ws/attachments/', '# ', '> ']) {
			expect(all, probe).toContain(probe);
		}
		expect(cases.some((c) => c.expected.startsWith('---\n'))).toBe(true);
		expect(cases.some((c) => c.link_index.length === 0)).toBe(true);
	});

	it('materialize() equals the live flush on every case', () => {
		const diverged: string[] = [];
		for (const c of cases) {
			const job: MaterializeJob = { rows: c.rows, link_index: c.link_index, workspace_slug: c.workspace_slug };
			if (materialize(JSON.stringify(job)) !== c.expected) diverged.push(c.name);
		}
		expect(diverged).toEqual([]);
	});

	it('NEGATIVE CONTROL: a wrong serializer option diverges', () => {
		const wrong = createHeadlessEditor({ bulletListMarker: '*' });
		try {
			const diverged = cases.filter(
				(c) => materializeWith(wrong, { rows: c.rows, link_index: c.link_index, workspace_slug: c.workspace_slug }) !== c.expected,
			);
			expect(diverged.length).toBeGreaterThan(0);
		} finally {
			wrong.destroy();
		}
	});

	it('NEGATIVE CONTROL: dropping the link index diverges where the tab converts links', () => {
		const diverged = cases.filter(
			(c) => c.link_index.length > 0 && materialize(JSON.stringify({ rows: c.rows, link_index: [], workspace_slug: WS })) !== c.expected,
		);
		expect(diverged.length).toBeGreaterThan(0);
	});
});
