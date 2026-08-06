// `isEditorOwnedImage` — the boundary between markup a surface rendered and
// DOM a live editor owns (PLAN-2392 DR-12 / TASK-2432).
//
// Both produce `img[data-attachment-id]`, so a delegated handler cannot tell
// them apart by selector. Getting this predicate wrong in either direction is a
// real failure: too broad and ItemTimeline stops opening its OWN thumbnails;
// too narrow and it keeps stripping semantics off a live CommentEditor's
// images and opening viewers on keys that editor needs.
import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { isEditorOwnedImage } from './editorOwnedImage';

describe('isEditorOwnedImage', () => {
	let root: HTMLElement;

	beforeEach(() => {
		root = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => root.remove());

	function build(html: string): HTMLElement {
		root.innerHTML = html;
		const img = root.querySelector('img[data-attachment-id]');
		if (!img) throw new Error('fixture has no attachment image');
		return img as HTMLElement;
	}

	it('claims an image inside a live editor', () => {
		// The shape CommentEditor mounts inside a timeline comment card.
		const img = build(`
			<div class="entry-list">
				<div class="comment-card">
					<div class="ce-surface">
						<div class="ProseMirror" contenteditable="true">
							<p><span class="attachment-image-wrapper"
								><img data-attachment-id="u1" alt="draft"
							/></span></p>
						</div>
					</div>
				</div>
			</div>`);
		expect(isEditorOwnedImage(img)).toBe(true);
	});

	it('does NOT claim a rendered comment body image', () => {
		// The `{@html}` output ItemTimeline legitimately owns — nested just as
		// deeply, so depth is not what the predicate is keying on.
		const img = build(`
			<div class="entry-list">
				<div class="comment-card">
					<div class="comment-body">
						<p><img data-attachment-id="u1" alt="posted" /></p>
					</div>
				</div>
			</div>`);
		expect(isEditorOwnedImage(img)).toBe(false);
	});

	it('does not claim a rendered body that merely SITS NEXT TO an editor', () => {
		// A comment card showing a posted body while a reply editor is open
		// below it. A predicate that keyed on the entry list, the card, or "this
		// subtree contains an editor" would get this one wrong.
		root.innerHTML = `
			<div class="comment-card">
				<div class="comment-body"><img data-attachment-id="posted" /></div>
				<div class="ProseMirror" contenteditable="true">
					<img data-attachment-id="draft" />
				</div>
			</div>`;
		const posted = root.querySelector<HTMLElement>('img[data-attachment-id="posted"]')!;
		const draft = root.querySelector<HTMLElement>('img[data-attachment-id="draft"]')!;
		expect(isEditorOwnedImage(posted)).toBe(false);
		expect(isEditorOwnedImage(draft)).toBe(true);
	});

	it('is not fooled by a comment body that CLAIMS to be an editor', () => {
		// Rendered markdown may carry raw HTML, and the sanitizer allows both
		// `class` and `data-attachment-id` — so this is ordinary user content,
		// not an editor. A class-only test would hand it to nobody: no
		// delegation, no accessibility pass, no way to open it at all.
		const img = build(`
			<div class="comment-body">
				<div class="ProseMirror"><img data-attachment-id="u1" /></div>
			</div>`);
		expect(isEditorOwnedImage(img)).toBe(false);
	});

	it('claims a READ-ONLY editor too', () => {
		// A frozen editor still owns its DOM; ProseMirror marks it
		// contenteditable="false", so presence — not truthiness — is the test.
		const img = build(`
			<div class="ProseMirror" contenteditable="false">
				<img data-attachment-id="u1" />
			</div>`);
		expect(isEditorOwnedImage(img)).toBe(true);
	});

	it('does not claim a detached image', () => {
		const img = document.createElement('img');
		img.setAttribute('data-attachment-id', 'u1');
		expect(isEditorOwnedImage(img)).toBe(false);
	});
});
