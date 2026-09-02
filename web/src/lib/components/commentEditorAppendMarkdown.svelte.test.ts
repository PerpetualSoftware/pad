// The imperative append handle on `CommentEditor` (IDEA-2843, GitHub #1228).
//
// The selection toolbar's "Comment" action has to drop a blockquote of the
// reader's selection into the composer that is ALREADY MOUNTED. The obvious
// route — write the `content` prop — is a silent no-op: `content` is read once
// inside `new Editor({...})` in `onMount`, and the component has no `$effect`
// syncing it. Nothing throws, nothing warns, the text is simply gone.
//
// That failure mode is why none of these tests assert that the composer is
// visible, or that a handler ran. They assert the QUOTE TEXT is in the
// composer's document, because "the composer opened" is exactly what the
// broken version also does.
//
// Two of the assertions are shaped by a mechanism rather than by taste:
//
//  - **`blockquote`, not text.** An implementation that inserts the selection
//    as plain text, or otherwise loses the block, is RED here where an
//    assertion on the visible text alone would stay green — verified by
//    mutation. What this does NOT catch, also verified: swapping the
//    implementation's `setContent` for tiptap-markdown's inline-parsing
//    `insertContentAt` keeps the whole suite green, because a blockquote
//    survives inline normalization today. The choice between those two is
//    argued in the component and is deliberately not test-enforced.
//  - **Draft first, then quote.** An implementation that replaced the document
//    instead of appending satisfies every "the quote is present" assertion
//    ever written. The draft's survival, and its position, are the only things
//    that separate append from replace.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

vi.mock('$lib/api/client', () => ({
	api: {
		attachments: {
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}?variant=${variant ?? 'thumb-md'}`,
			upload: vi.fn(),
		},
	},
}));

const CommentEditor = (await import('./CommentEditor.svelte')).default;

type Handle = { appendMarkdown: (markdown: string) => boolean };

const QUOTE = '> The passage the reviewer highlighted';
const QUOTE_TEXT = 'The passage the reviewer highlighted';

let target: HTMLDivElement;
let app: Handle | null = null;

function mountComposer(content = ''): Handle {
	target = document.body.appendChild(document.createElement('div'));
	app = mount(CommentEditor, {
		target,
		props: { wsSlug: 'ws', itemId: 'item-A', content, onSubmit: () => {} },
	}) as unknown as Handle;
	flushSync();
	return app;
}

/** The ProseMirror surface — what the user would be looking at. */
function surface(): HTMLElement {
	const el = target.querySelector('.ce-surface');
	if (!el) throw new Error('composer surface never rendered');
	return el as HTMLElement;
}

function submitButton(): HTMLButtonElement {
	return target.querySelector('.ce-submit') as HTMLButtonElement;
}

beforeEach(() => {
	document.body.innerHTML = '';
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	vi.restoreAllMocks();
});

describe('CommentEditor.appendMarkdown', () => {
	it('puts the quote in the composer as a blockquote, on an empty composer', () => {
		const handle = mountComposer();

		// Precondition, asserted rather than assumed: the composer really is
		// empty first, so the blockquote below cannot be left over from mount.
		expect(surface().querySelector('blockquote')).toBeNull();

		expect(handle.appendMarkdown(QUOTE)).toBe(true);
		flushSync();

		const quote = surface().querySelector('blockquote');
		expect(quote).not.toBeNull();
		expect(quote!.textContent).toContain(QUOTE_TEXT);
	});

	// This one carries the `empty` flag, which no line in `appendMarkdown`
	// sets — the editor's own onUpdate does, because setContent emits by
	// default. That default is the thing under test: if a tiptap bump flips
	// it, the quote still lands and the user still cannot post it, and this
	// is what says so.
	it('enables submit, because the composer gates the button on its own empty flag', () => {
		const handle = mountComposer();
		expect(submitButton().disabled).toBe(true);

		handle.appendMarkdown(QUOTE);
		flushSync();

		// A quote that lands in a document whose `empty` flag stayed stale is a
		// comment the user cannot post — the same silent shape as losing the
		// text outright.
		expect(submitButton().disabled).toBe(false);
	});

	it('appends after an existing draft instead of replacing it, draft first', () => {
		const handle = mountComposer('Half a thought I already typed');

		expect(handle.appendMarkdown(QUOTE)).toBe(true);
		flushSync();

		const root = surface();
		expect(root.textContent).toContain('Half a thought I already typed');

		const quote = root.querySelector('blockquote');
		expect(quote).not.toBeNull();
		expect(quote!.textContent).toContain(QUOTE_TEXT);

		// Order, not just co-presence: a replace-then-restore implementation
		// could satisfy both assertions above with the draft in the wrong place.
		const draftNode = Array.from(root.querySelectorAll('p')).find((p) =>
			p.textContent?.includes('Half a thought I already typed')
		);
		expect(draftNode).toBeDefined();
		expect(
			draftNode!.compareDocumentPosition(quote!) & Node.DOCUMENT_POSITION_FOLLOWING
		).toBeTruthy();
	});

	it('quoting a second passage keeps the first', () => {
		const handle = mountComposer();

		handle.appendMarkdown('> First passage');
		flushSync();
		handle.appendMarkdown('> Second passage');
		flushSync();

		const quotes = surface().querySelectorAll('blockquote');
		expect(quotes.length).toBe(2);
		expect(quotes[0].textContent).toContain('First passage');
		expect(quotes[1].textContent).toContain('Second passage');
	});

	it('reports false and changes nothing when there is nothing to insert', () => {
		const handle = mountComposer();

		// The negative control for the honest-failure contract: the handle
		// exists to make "did nothing" distinguishable from "inserted", so a
		// blank addition must SAY so rather than return true and no-op.
		expect(handle.appendMarkdown('   \n  ')).toBe(false);
		flushSync();

		expect(surface().querySelector('blockquote')).toBeNull();
		expect(submitButton().disabled).toBe(true);
	});
});
