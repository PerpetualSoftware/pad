// BUG-3197: the collab flush dedupes a no-edit view against the editor's
// canonical form of the seed, computed by `canonicalEditorMarkdown` (parse +
// serialize, never touching the live document). That is only sound if it
// agrees with what the editor actually emits for the same markdown, which is
// `setContent(markdown)` then `storage.markdown.getMarkdown()`.
//
// Driven through the REAL `Editor.svelte` mount (CONVE-19), so every extension
// production registers takes part, including the ones the StarterKit-only
// harness in bug2995Roundtrip leaves out (tables, task lists, the frontmatter
// code block, attachments, links). The census on BUG-3197's trail ran the same
// comparison over every live body on a copy of the real database.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import type { Editor as TiptapEditor } from '@tiptap/core';
import { canonicalEditorMarkdown } from '$lib/collab/canonicalMarkdown';
import { Plugin, PluginKey } from '@tiptap/pm/state';
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

// Each fixture is a stored body. The first group is NOT a fixed point of the
// round trip (the reason the canonical arm exists); the second is, and pins
// that the canonical form of an already-canonical body is the body itself.
// "Fixed point" is judged in the flush's compare space, after unescapeDocLinks:
// the serializer escapes `[[`, and the flush's normalize undoes it.
const OLD_SPAN_TABLE =
	'<table class="table-wrapper" style="min-width: 50px;">\n' +
	'<colgroup><col style="min-width: 25px;"><col style="min-width: 25px;"></colgroup><tbody><tr>' +
	'<td colspan="1" rowspan="1"><p>para one</p><p>para two</p></td>' +
	'<td colspan="1" rowspan="1"><p>x</p></td></tr></tbody>\n</table>';
const notFixed: Array<[string, string]> = [
	['leading horizontal rule', '---\nText after a rule.'],
	['asterisk bullets', '* one\n* two'],
	['setext heading', 'Title\n=====\n\nbody'],
	['underscore emphasis', 'an _emphasised_ word'],
	['lazy ordered list', '1. one\n1. two\n1. three'],
	['blank-line run', 'para one\n\n\n\npara two'],
	['trailing-space break', 'a line   \nnext line'],
	['nested list indent', '- a\n    - b'],
	['body ending in a table gains a trailing newline', '| a | b |\n| --- | --- |\n| 1 | 2 |'],
	['body ending in a task list gains a trailing newline', '- [ ] todo\n- [x] done'],
	// The two shapes the population parity run found (BUG-3197 checkpoint 6):
	// both are reshaped by an appendTransaction plugin, which parsing alone skips.
	['a bare URL is autolinked', 'Tracker: https://example.com/org/repo/issues/56\n\nMore text.'],
	['an HTML table', '<table><tr><td><ul><li>a</li></ul></td></tr></table>\n\nafter'],
	// The form the editor stored before Tiptap 3.31.4 (TASK-3421): a multi-block
	// cell forces the HTML table, and 3.31.3 rendered every cell's default
	// colspan/rowspan. 3.31.4 no longer does, so a body stored then is not a
	// fixed point now; the canonical arm is what keeps opening it a no-op.
	['an HTML table stored with default spans (pre-3.31.4)', OLD_SPAN_TABLE],
];
const fixed: Array<[string, string]> = [
	['plain paragraph', 'a perfectly ordinary paragraph'],
	// BUG-3557: was reshaped (the tag parsed to an empty table the editor
	// dropped); angle-bracketed prose is now literal text and round-trips.
	['an HTML-looking tag in prose', 'Make @ui <Table> a drop-in for every list.'],
	['a generic type in prose', 'returns Promise<T> on success'],
	['frontmatter', '---\ntitle: "T"\ndate: "2026-07-01"\n---\n\nBody.'],
	['wiki link', 'see [[TASK-5]] for more'],
	['link', 'a [link](https://example.com) here'],
	['fenced code', '```go\nfunc main() {}\n```'],
	['attachment image', '![shot](pad-attachment:0b1f3c9e-1d2a-4e5f-8a9b-0c1d2e3f4a5b)'],
];

describe('canonicalEditorMarkdown agrees with the editor round trip (BUG-3197)', () => {
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
				itemId: 'item-canonical',
				hostToken: 'canonical-markdown',
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
		return (editor().storage as any).markdown.getMarkdown();
	}

	for (const [name, body] of [...notFixed, ...fixed]) {
		it(`${name}: canonical form equals what setContent + getMarkdown emits`, () => {
			const canonical = canonicalEditorMarkdown(editor(), body);
			expect(canonical).toBe(roundTrip(body));
		});
	}

	for (const [name, body] of notFixed) {
		it(`${name}: is not a fixed point, so the exact seed compare alone would miss`, () => {
			expect(unescapeDocLinks(canonicalEditorMarkdown(editor(), body))).not.toBe(body);
		});
	}

	for (const [name, body] of fixed) {
		it(`${name}: an already-canonical body is its own canonical form`, () => {
			expect(unescapeDocLinks(canonicalEditorMarkdown(editor(), body))).toBe(body);
		});
	}

	// TASK-3421: the default spans are output only. The body with them and the
	// body without them parse to the same document, so they share one canonical
	// form, and that form carries no default span.
	it('a body stored with default spans canonicalizes like the same body without them', () => {
		const withSpans = canonicalEditorMarkdown(editor(), OLD_SPAN_TABLE);
		const without = canonicalEditorMarkdown(editor(), OLD_SPAN_TABLE.replaceAll(' colspan="1" rowspan="1"', ''));
		expect(withSpans).not.toBeNull();
		expect(withSpans).toBe(without);
		expect(withSpans).not.toMatch(/colspan="1"|rowspan="1"/);
		expect(withSpans).toMatch(/<p>para one<\/p><p>para two<\/p>/);
	});

	// Codex round 4: a filterTransaction plugin that refuses the replace leaves
	// the EMPTY document. Serializing that would make "" the canonical form of a
	// non-empty body, and a user who deleted the whole body would be deduped.
	it('a refused replace answers null, never the empty document', () => {
		const key = new PluginKey('bug3197RefuseAll');
		editor().registerPlugin(new Plugin({ key, filterTransaction: () => false }));
		try {
			expect(canonicalEditorMarkdown(editor(), '* one\n* two')).toBeNull();
		} finally {
			editor().unregisterPlugin(key);
		}
	});

	it("Collaboration's filterInvalidContent is left out of the detached state", () => {
		const key = new PluginKey('filterInvalidContent');
		editor().registerPlugin(new Plugin({ key, filterTransaction: () => false }));
		try {
			expect(canonicalEditorMarkdown(editor(), '* one\n* two')).toBe('- one\n- two');
		} finally {
			editor().unregisterPlugin(key);
		}
	});

	it('does not touch the live document', () => {
		roundTrip('the live document');
		const before = editor().state.doc;
		canonicalEditorMarkdown(editor(), '* something else entirely');
		expect(editor().state.doc).toBe(before);
	});
});
