// The selection toolbar's Comment action and its gate (IDEA-2843, GitHub #1228).
//
// The Comment action renders when the host supplies `onComment` — a composer
// to quote into IS the capability, so there is no separate flag. Both actions
// then sit behind the host's editor-mutation gate.
//
// It was briefly gated peek-independently, on the theory that a peeking master
// can comment (BUG-2263) but could not act on a selection. That state does not
// exist: a drag-selection in a peeking master re-activates it
// (focus-follows-editing), which was MEASURED in
// `e2e/selection-comment-peek.spec.ts` rather than argued. The tests below are
// what is left once that unreachable case is dropped.
//
// Driven through a REAL Tiptap editor: the toolbar's visibility is a function
// of `selectionUpdate`, and a hand-built stub would pin the assertions to my
// idea of when that fires rather than to when it does.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';

vi.mock('$lib/api/client', () => ({
	api: { items: { create: vi.fn() } },
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: vi.fn() } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { scopeEpochFor: () => 0, upsert: vi.fn() },
}));

const EditorBubbleMenu = (await import('./EditorBubbleMenu.svelte')).default;

const DOC = '<p>First paragraph here.</p><p>Second paragraph here.</p>';

let target: HTMLDivElement;
let editorHost: HTMLDivElement;
let editor: Editor;
let app: Record<string, unknown> | null = null;

function mountMenu(opts: { onComment?: (markdown: string) => boolean }) {
	target = document.body.appendChild(document.createElement('div'));
	app = mount(EditorBubbleMenu, {
		target,
		props: {
			editor,
			wsSlug: 'ws',
			collections: [],
			onComment: opts.onComment,
		},
	}) as Record<string, unknown>;
	flushSync();
}

/** Select "First paragraph here." — a real transaction, so the menu reacts. */
function selectFirstParagraph() {
	editor.commands.setTextSelection({ from: 1, to: 22 });
	flushSync();
}

function button(name: string): HTMLButtonElement | null {
	return Array.from(target.querySelectorAll('button')).find((b) =>
		b.textContent?.trim().startsWith(name)
	) as HTMLButtonElement | undefined ?? null;
}

/** The toolbar's Comment button carries a distinguishing accessible name. */
function commentButtonLabel(): string | null {
	return target.querySelector('.comment-btn')?.getAttribute('aria-label') ?? null;
}

beforeEach(() => {
	document.body.innerHTML = '';
	editorHost = document.body.appendChild(document.createElement('div'));
	editor = new Editor({ element: editorHost, content: DOC, extensions: [StarterKit] });
	// jsdom has no layout: `coordsAtPos` walks to a text node and calls
	// `getClientRects`, which does not exist there. Only the menu's POSITION
	// depends on it, and position is not what these tests are about — the
	// gate and the quote are. Stubbing it keeps the real selection plumbing
	// (`selectionUpdate`, `state.doc.textBetween`) intact, which is the part
	// the assertions actually rest on.
	vi.spyOn(editor.view, 'coordsAtPos').mockReturnValue({
		top: 10,
		bottom: 20,
		left: 10,
		right: 20,
	});
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	editor.destroy();
	vi.restoreAllMocks();
});

describe('selection toolbar — Comment action', () => {
	it('offers no Comment action when the host has no composer to quote into', () => {
		// The menu is a generic editor component; only a host that wires
		// `onComment` has somewhere for a quote to go. Rendering the button
		// without one would be a control that does nothing.
		mountMenu({});
		selectFirstParagraph();

		expect(button('Comment')).toBeNull();
		expect(button('Extract')).not.toBeNull();
	});

	it('names the Comment button for what it acts on, not just "Comment"', () => {
		// The comment composer's submit button is also named "Comment". Two
		// buttons with one name and different effects is an ambiguity for
		// anyone navigating by name — and it is what made the first end-to-end
		// run of this feature fail on a locator rather than on behaviour.
		mountMenu({ onComment: () => true });
		selectFirstParagraph();

		expect(commentButtonLabel()).toBe('Comment on selection');
	});

	it('shows both actions when a composer is wired', () => {
		mountMenu({ onComment: () => true });
		selectFirstParagraph();

		expect(button('Comment')).not.toBeNull();
		expect(button('Extract')).not.toBeNull();
	});

	it('shows nothing at all without a selection', () => {
		// The gate that actually hides both actions is the host's — this menu
		// only ever renders while something is selected.
		mountMenu({ onComment: () => true });

		expect(button('Comment')).toBeNull();
		expect(button('Extract')).toBeNull();
	});

	it('hands the host a blockquote of the selected text', () => {
		const quoted: string[] = [];
		mountMenu({
			onComment: (md) => {
				quoted.push(md);
				return true;
			},
		});
		selectFirstParagraph();
		button('Comment')!.click();
		flushSync();

		// The quote, not the raw text: asserting only "contains the passage"
		// would pass on an implementation that dropped the blockquote entirely.
		expect(quoted).toEqual(['> First paragraph here.']);
	});

	it('leaves the document untouched — quoting is not Extract', () => {
		const before = editor.getHTML();
		mountMenu({ onComment: () => true });
		selectFirstParagraph();
		button('Comment')!.click();
		flushSync();

		// Extract REPLACES the selection with a wiki-link. Comment must not:
		// the reader is reading, and a passage they quote has to still be there
		// for them to quote again or keep reading from.
		expect(editor.getHTML()).toBe(before);
	});
});
