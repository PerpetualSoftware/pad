// BUG-2692: a leading YAML frontmatter block was flattened by the editor's
// markdown round trip into `---\n\n## key: value key: value`, a fixed point, so
// the damage persisted (four blog posts were stored that way).
//
// Driven through the REAL `Editor.svelte` mount (CONVE-19): the codeBlock the
// fix extends is configured there, so a test of frontmatter.ts alone would
// vouch for the module and not for the editor that uses it. The two halves are
// the ones production performs: `setContent(markdown)` (the applier and seed
// path) and `storage.markdown.getMarkdown()` (what the flush reads back).
//
// Fixtures are synthetic but have the SHAPES the census on BUG-2692's trail
// found: blog frontmatter (key: value lines only), a body that opens with a
// horizontal rule and has no closing line (MISTA-143's shape), and a flattened
// body whose next `---` is a later horizontal rule with prose above it
// (BLOG-1802's shape).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';
import { unescapeDocLinks } from '$lib/utils/markdown';

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

vi.mock('$lib/api/client', () => ({
	api: {
		server: { capabilities: () => new Promise(() => {}) },
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
			transform: vi.fn(),
			upload: vi.fn(),
		},
		items: { list: vi.fn(async () => []) },
	},
}));

const { default: BodyEditor } = await import('./Editor.svelte');
const { page } = await import('$app/state');

describe('Editor markdown round trip — leading frontmatter (BUG-2692)', () => {
	let target: HTMLElement;
	let instance: Record<string, unknown> | null = null;
	let tiptap: TiptapEditor | null = null;

	beforeEach(() => {
		page.params.workspace = 'ws';
		target = document.body.appendChild(document.createElement('div'));
		tiptap = null;
		instance = mount(BodyEditor, {
			target,
			props: {
				content: '',
				itemId: 'item-fm',
				hostToken: 'frontmatter-roundtrip',
				onEditor: (e: TiptapEditor) => {
					tiptap = e;
				},
			},
		}) as Record<string, unknown>;
		flushSync();
	});

	afterEach(() => {
		if (instance) unmount(instance);
		target.remove();
	});

	function editor(): TiptapEditor {
		if (!tiptap) throw new Error('Editor.svelte did not construct a Tiptap editor');
		return tiptap;
	}
	function roundTrip(markdown: string): string {
		editor().commands.setContent(markdown);
		return unescapeDocLinks((editor().storage as any).markdown.getMarkdown());
	}
	function firstNode() {
		const first = editor().state.doc.firstChild;
		return { type: first?.type.name, language: first?.attrs.language as string | undefined };
	}

	const blog =
		'---\ntitle: "Pad v0.9: closing the visibility perimeter"\ndescription: "A security-led release."\ndate: "2026-07-01"\nauthor: The Pad Team\n---\n\nThe body starts here.\n\n## A section\n\nMore text.';

	it('a blog frontmatter block round-trips byte-for-byte, as a frontmatter code block', () => {
		expect(roundTrip(blog)).toBe(blog);
		expect(firstNode()).toEqual({ type: 'codeBlock', language: 'frontmatter' });
	});

	it('the round trip is stable: a second pass changes nothing', () => {
		expect(roundTrip(roundTrip(blog))).toBe(blog);
	});

	it('an empty-value key and an indented continuation are frontmatter too', () => {
		const md = '---\ntitle: x\ntags:\n  - one\n  - two\n---\n\nbody';
		expect(roundTrip(md)).toBe(md);
		expect(firstNode().language).toBe('frontmatter');
	});

	it('a body that OPENS WITH A HORIZONTAL RULE and never closes it stays a rule (MISTA-143 shape)', () => {
		const md = '---\n\nSurfaced a stale premise as a fresh one, twice in one session.\n\nThe correction came the day before.';
		const out = roundTrip(md);
		expect(firstNode().type).toBe('horizontalRule');
		expect(out).toBe(md);
	});

	it('a later horizontal rule with PROSE above it is not frontmatter (BLOG-1802 flattened shape)', () => {
		const md =
			'---\n\n## title: "A post" description: "d"\n\nA paragraph of prose.\n\n---\n\nMore after the rule.';
		roundTrip(md);
		expect(firstNode().type).toBe('horizontalRule');
		expect(editor().state.doc.toJSON().content.some((n: { attrs?: { language?: string } }) => n.attrs?.language === 'frontmatter')).toBe(false);
	});

	it('a --- that is not at byte 0 is not frontmatter', () => {
		roundTrip('Intro line.\n\n---\ntitle: x\n---\n\nbody');
		expect(editor().state.doc.toJSON().content.some((n: { attrs?: { language?: string } }) => n.attrs?.language === 'frontmatter')).toBe(false);
	});

	it('an edit that makes the block invalid falls back to a fence, losing no text and not re-flattening', () => {
		roundTrip(blog);
		const first = editor().state.doc.firstChild!;
		editor()
			.chain()
			.insertContentAt({ from: 1, to: 1 + first.content.size }, 'just prose now')
			.run();
		const out = unescapeDocLinks((editor().storage as any).markdown.getMarkdown());
		expect(out.startsWith('```frontmatter\njust prose now\n```')).toBe(true);
		expect(roundTrip(out)).toBe(out);
	});

	it('CRLF input is recognised as frontmatter (markdown-it normalises line endings first)', () => {
		roundTrip('---\r\ntitle: x\r\ndate: y\r\n---\r\n\r\nbody');
		expect(firstNode()).toEqual({ type: 'codeBlock', language: 'frontmatter' });
	});

	it('a closing fence with trailing spaces is recognised, not read as a setext underline', () => {
		const out = roundTrip('---\ntitle: x\ndate: y\n---  \n\nbody');
		expect(firstNode()).toEqual({ type: 'codeBlock', language: 'frontmatter' });
		expect(out).toBe('---\ntitle: x\ndate: y\n---\n\nbody');
	});

	it('a frontmatter block that is not the FIRST block is written as a fence, not as ---', () => {
		roundTrip(blog);
		editor().chain().insertContentAt(0, { type: 'paragraph' }).run();
		const out = unescapeDocLinks((editor().storage as any).markdown.getMarkdown());
		expect(out.startsWith('---')).toBe(false);
		expect(out).toContain('```frontmatter\ntitle:');
	});

	it('repeated parses do not register the rule again', () => {
		roundTrip(blog);
		const md = (editor().storage as any).markdown.parser.md;
		const count = () =>
			md.block.ruler.__rules__.filter((r: { name: string }) => r.name === 'pad_frontmatter').length;
		const before = count();
		roundTrip(blog);
		roundTrip(blog);
		expect(before).toBe(1);
		expect(count()).toBe(1);
	});

	// A NON-REGRESSION leg, deliberately: it passes on the pre-fix code too.
	// It pins that replacing the codeBlock's markdown storage kept the default
	// fence form for every other language.
	it('other code blocks keep the default fence serialization', () => {
		const md = '```go\nfunc main() {}\n```\n\n```\nplain\n```';
		expect(roundTrip(md)).toBe(md);
	});
});
