/**
 * HtmlBlock — atomic block node for rich-content HTML islands inside an
 * otherwise markdown document. Round-trips to a ` ```html ` fenced code
 * block in storage; renders as sanitized live HTML in the WYSIWYG view
 * via a NodeView.
 *
 * Sanitization is render-time only. `attrs.html` keeps the raw user
 * input verbatim so a future source-view editor (TASK-1325) and the
 * version-diff view (TASK-1328) can show exactly what was typed —
 * including content this NodeView strips before display.
 *
 * Foundation for PLAN-1322 (TASK-1324). Authoring UI (slash menu /
 * toolbar / markdown shortcut) lives in TASK-1326; source-view editing
 * in TASK-1325; hidden-content authoring warnings in TASK-1327; diff
 * collapse in TASK-1328.
 */

import { InputRule, Node, type Editor } from '@tiptap/core';
import type MarkdownIt from 'markdown-it';
import type { Node as ProseMirrorNode } from '@tiptap/pm/model';
import { sanitizeHtmlBlock } from '$lib/utils/markdown';

/** A single htmlBlock node's identity for snapshot comparison. */
export interface HtmlBlockSnapshotEntry {
	pos: number;
	html: string;
}

/**
 * Snapshot every htmlBlock node currently in the editor, recording
 * BOTH position and `attrs.html` content. The caller pairs this with
 * `flipHtmlBlockToSource` to disambiguate a just-inserted (or
 * just-replaced) block from any pre-existing ones.
 *
 * Capturing content (not just position) is what makes the helper
 * correct in the "NodeSelection replaces an existing htmlBlock" case
 * — `insertContent` over a NodeSelection swaps the existing block
 * for the new one, leaving `after.length === before.length` but with
 * the replaced block's content changed (commonly to ''). A
 * position-only snapshot can't see that change; the content snapshot
 * does.
 */
export function captureHtmlBlockSnapshot(editor: Editor): HtmlBlockSnapshotEntry[] {
	const snapshot: HtmlBlockSnapshotEntry[] = [];
	editor.state.doc.descendants((node, pos) => {
		if (node.type.name !== 'htmlBlock') return;
		const html = typeof node.attrs.html === 'string' ? (node.attrs.html as string) : '';
		snapshot.push({ pos, html });
	});
	return snapshot;
}

/**
 * After inserting an htmlBlock node, defer one frame and synthesise a
 * click on the new block's preview pane so the user lands directly in
 * source mode (matches the spec: all three insertion paths land in
 * source).
 *
 * Identifies the new block by walking the post-dispatch htmlBlock
 * snapshot in document order and matching each entry against the
 * before-snapshot. A pre-existing block matches an after entry when:
 *
 *   - Their `html` content is identical, AND
 *   - The position is plausibly the before position shifted by
 *     ProseMirror's transaction mapping. Tolerance window:
 *       0  → block was before the insertion point (unshifted)
 *       1  → atom-block insertion shifts later positions by +1
 *       3  → mid-paragraph split adds 2 paragraph tokens + 1 atom = +3
 *      -1  → empty-paragraph collapse drops a paragraph token = -1
 *
 * The first after entry that *doesn't* match a before entry is the
 * inserted (or replaced) block. This works uniformly for:
 *   - Plain insert at empty paragraph / end of paragraph / mid-paragraph
 *   - Insert adjacent to an existing htmlBlock
 *   - Insert that replaces a NodeSelection on an existing htmlBlock
 *     (after.length === before.length but the replaced entry's html
 *     differs from its before image)
 *
 * Silent no-op if no new block is found (e.g. the insert failed).
 */
export function flipHtmlBlockToSource(
	editor: Editor,
	insertionPoint: number,
	before: HtmlBlockSnapshotEntry[],
): void {
	requestAnimationFrame(() => {
		const { state, view } = editor;
		const after = captureHtmlBlockSnapshot(editor);

		let bi = 0;
		let newPos: number | null = null;
		for (const a of after) {
			let matched = false;
			if (bi < before.length) {
				const b = before[bi];
				const shift = a.pos - b.pos;
				const isUnshifted = b.pos < insertionPoint && shift === 0;
				const isShifted =
					b.pos >= insertionPoint && (shift === -1 || shift === 1 || shift === 3);
				if ((isUnshifted || isShifted) && a.html === b.html) {
					bi++;
					matched = true;
				}
			}
			if (!matched) {
				newPos = a.pos;
				break;
			}
		}
		if (newPos === null) return;

		const dom = view.nodeDOM(newPos) as HTMLElement | null;
		if (!dom || !dom.classList.contains('html-block')) return;
		const previewPane = dom.querySelector('.html-block-preview') as HTMLElement | null;
		previewPane?.click();
	});
}

declare module '@tiptap/core' {
	interface Commands<ReturnType> {
		htmlBlock: {
			/**
			 * Insert a new HTML block at the current selection.
			 *
			 * @param options.html  Raw HTML to seed the block with. Empty by default;
			 *   the user fills it in via TASK-1325's source-view editor.
			 */
			setHtmlBlock: (options?: { html?: string }) => ReturnType;
		};
	}
}

/**
 * Returns the length of the longest backtick run in `s`. Used by the
 * markdown serializer to pick a fence length one longer than any run of
 * backticks inside the body, so the closing fence can never be eaten by
 * a literal `\`\`\`` inside the user's HTML.
 */
function longestBacktickRun(s: string): number {
	const matches = s.match(/`+/g);
	if (!matches) return 0;
	return matches.reduce((m, run) => Math.max(m, run.length), 0);
}

export const HtmlBlock = Node.create({
	name: 'htmlBlock',
	group: 'block',
	atom: true,
	selectable: true,
	draggable: true,
	defining: true,
	// Higher than the default extension priority (100) so the markdown
	// shortcut input rule below fires BEFORE CodeBlock's `^```([a-z]+)?…`
	// rule. Without this, typing ` ```html ` would create a code block
	// (language=html) instead of an htmlBlock node.
	priority: 1000,

	addAttributes() {
		return {
			html: {
				default: '',
				parseHTML: (el: HTMLElement) => el.getAttribute('data-html') ?? '',
				renderHTML: (attrs: { html?: string }) => ({ 'data-html': attrs.html ?? '' }),
			},
		};
	},

	parseHTML() {
		return [{ tag: 'div[data-pad-html-block]' }];
	},

	renderHTML({ node }) {
		// HTML round-trip form for clipboard / non-Markdown serialization.
		// The live in-editor rendering goes through addNodeView() below;
		// that path inserts SANITIZED HTML, while this path keeps the raw
		// HTML in a data-attribute so a non-aware consumer parsing our DOM
		// output can still recover the original text.
		return [
			'div',
			{
				'data-pad-html-block': '',
				'data-html': (node.attrs.html as string | undefined) ?? '',
			},
		];
	},

	addNodeView() {
		return ({ node, editor, getPos }) => {
			const wrapper = document.createElement('div');
			wrapper.className = 'html-block';
			wrapper.setAttribute('data-pad-html-block', '');
			// contenteditable=false: atom: true means the user can't edit
			// the rendered preview character-by-character. Editing flows
			// through the source-view textarea below.
			wrapper.setAttribute('contenteditable', 'false');

			// Preview pane — sanitized live HTML.
			const preview = document.createElement('div');
			preview.className = 'html-block-preview';

			// Source pane — raw HTML editor. Hidden in CSS until
			// `.html-block--editing` is set on the wrapper.
			const source = document.createElement('div');
			source.className = 'html-block-source';

			const textarea = document.createElement('textarea');
			textarea.className = 'html-block-source-input';
			textarea.spellcheck = false;
			textarea.setAttribute('aria-label', 'Edit raw HTML for this block');

			const actions = document.createElement('div');
			actions.className = 'html-block-actions';

			const doneBtn = document.createElement('button');
			doneBtn.type = 'button';
			doneBtn.className = 'html-block-done-btn';
			doneBtn.textContent = 'Done';
			doneBtn.title = 'Save and return to preview (⌘/Ctrl+Enter or Esc)';

			actions.append(doneBtn);
			source.append(textarea, actions);
			wrapper.append(preview, source);

			let lastHtml = (node.attrs.html as string | undefined) ?? '';
			let mode: 'preview' | 'source' = 'preview';

			const renderPreview = () => {
				if (!lastHtml.trim()) {
					// Empty block: show a placeholder so the user can find it
					// and click into source mode. Without this, an empty block
					// is an invisible atom and effectively unreachable.
					preview.innerHTML =
						'<span class="html-block-empty">Empty HTML block — click to edit</span>';
				} else {
					preview.innerHTML = sanitizeHtmlBlock(lastHtml);
				}
			};
			renderPreview();

			function flipToSource() {
				if (mode === 'source') return;
				mode = 'source';
				textarea.value = lastHtml;
				wrapper.classList.add('html-block--editing');
				// Defer focus to the next frame so the click that triggered
				// the flip finishes processing (otherwise some browsers swallow
				// the focus call mid-event).
				requestAnimationFrame(() => {
					textarea.focus();
					// Place caret at end of content for natural editing flow.
					const len = textarea.value.length;
					textarea.setSelectionRange(len, len);
				});
			}

			function commit() {
				const next = textarea.value;
				const pos = typeof getPos === 'function' ? getPos() : null;
				if (typeof pos !== 'number') return;
				if (next === lastHtml) return;
				const tr = editor.view.state.tr.setNodeMarkup(pos, undefined, { html: next });
				editor.view.dispatch(tr);
				// `update()` will fire when the dispatched transaction lands,
				// updating lastHtml and re-rendering the preview.
			}

			function flipToPreview() {
				if (mode === 'preview') return;
				mode = 'preview';
				wrapper.classList.remove('html-block--editing');
				// Defensive re-render in case lastHtml was the same as
				// textarea.value (commit was a no-op) — preview state needs
				// to reflect lastHtml regardless.
				renderPreview();
			}

			function commitAndFlip() {
				commit();
				flipToPreview();
			}

			preview.addEventListener('click', (e) => {
				// Read-only viewers must not enter source mode — the
				// commit() dispatch would mutate the local document and
				// trigger save attempts. Preview-only is the right UX
				// in that case.
				if (!editor.isEditable) return;
				// Don't flip if the user clicked an interactive element
				// inside the rendered preview (links, iframes, embedded
				// form controls). Those are part of the legitimate use case
				// and should respond to clicks naturally.
				const target = e.target as Element | null;
				if (target?.closest('a, button, iframe, input, textarea, select, video, audio')) {
					return;
				}
				flipToSource();
			});

			textarea.addEventListener('blur', () => {
				// Blur fires both when the user clicks outside AND when the
				// Done button click triggers commitAndFlip. The handler is
				// idempotent: a second commit with the same text is a no-op
				// (commit early-returns on next === lastHtml).
				commitAndFlip();
			});

			textarea.addEventListener('keydown', (e) => {
				if (e.key === 'Escape') {
					e.preventDefault();
					commitAndFlip();
				} else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
					e.preventDefault();
					commitAndFlip();
				}
			});

			// `mousedown.preventDefault` keeps focus on the textarea so the
			// subsequent click handler runs in the same selection context;
			// without this, the button steals focus → blur fires first →
			// commitAndFlip → click fires on a hidden element → no-op.
			doneBtn.addEventListener('mousedown', (e) => e.preventDefault());
			doneBtn.addEventListener('click', (e) => {
				e.preventDefault();
				commitAndFlip();
			});

			return {
				dom: wrapper,
				// Tell ProseMirror to ignore events that originated inside
				// our source pane — without this, typing common HTML like
				// `</div>` bubbles `/` up to the slash-menu plugin in the
				// surrounding editor, and Enter / arrow keys can be eaten
				// by ProseMirror commands instead of editing the textarea.
				// Preview-pane events still flow through normally so links /
				// iframes / embedded controls behave as users expect.
				stopEvent(event: Event) {
					// `Node` here would otherwise resolve to the TipTap `Node`
					// class (imported at the top); use `globalThis.Node` to
					// reach the DOM Node interface that source.contains expects.
					const target = event.target as globalThis.Node | null;
					return target !== null && source.contains(target);
				},
				update(updatedNode: ProseMirrorNode) {
					if (updatedNode.type.name !== 'htmlBlock') return false;
					const next = (updatedNode.attrs.html as string | undefined) ?? '';
					if (next !== lastHtml) {
						lastHtml = next;
						// Only re-render the preview pane. Don't touch the
						// textarea — the user might be mid-edit. They'll see
						// fresh content on the next flipToSource call.
						renderPreview();
					}
					return true;
				},
				// Mutations inside our sanitized innerHTML / textarea are
				// render-only — we own the wrapper. Skip ProseMirror's
				// MutationObserver to avoid re-parse loops (mirrors the
				// MermaidCodeBlock pattern in Editor.svelte).
				ignoreMutation() {
					return true;
				},
			};
		};
	},

	addCommands() {
		return {
			setHtmlBlock:
				(options) =>
				({ commands }) =>
					commands.insertContent({
						type: this.name,
						attrs: { html: options?.html ?? '' },
					}),
		};
	},

	addInputRules() {
		// Markdown shortcut: typing ` ```html ` followed by a space or newline
		// at the start of an empty paragraph creates a new htmlBlock node.
		// Mirrors CodeBlock's textblockTypeInputRule pattern but materialises
		// an atom node (the htmlBlock leaf) instead of a wrapped textblock.
		// Higher extension priority (1000, set above) ensures this fires
		// before CodeBlock's broader `^```([a-z]+)?…` rule.
		//
		// `this.editor` is captured via lexical scope — TipTap's InputRule
		// handler config does NOT pass `editor` directly. By the time the
		// rule fires, the extension instance has been bound to the editor.
		const extension = this;
		return [
			new InputRule({
				find: /^```html[\s\n]$/,
				handler: ({ state, range }) => {
					// Snapshot pre-dispatch htmlBlock entries (pos + html)
					// so the flip helper can disambiguate the new block
					// from any existing ones, including the replace-existing
					// case where after.length === before.length. state.doc
					// here is the pre-dispatch document.
					const before: HtmlBlockSnapshotEntry[] = [];
					state.doc.descendants((node, pos) => {
						if (node.type.name !== 'htmlBlock') return;
						const html = typeof node.attrs.html === 'string' ? (node.attrs.html as string) : '';
						before.push({ pos, html });
					});
					state.tr.replaceRangeWith(
						range.from,
						range.to,
						extension.type.create({ html: '' }),
					);
					if (extension.editor) {
						flipHtmlBlockToSource(extension.editor, range.from, before);
					}
				},
			}),
		];
	},

	addStorage() {
		return {
			markdown: {
				/**
				 * Serialize an htmlBlock node to a ` ```html ` fenced block.
				 * Uses a fence one backtick longer than the longest run inside
				 * the body so a literal `\`\`\`` in the user's HTML can't close
				 * the fence early. Always emits a trailing newline before the
				 * closing fence so the fence is a standalone line.
				 */
				serialize(
					state: { write: (s: string) => void; closeBlock: (node: ProseMirrorNode) => void },
					node: ProseMirrorNode,
				) {
					const raw = typeof node.attrs.html === 'string' ? (node.attrs.html as string) : '';
					const fenceLen = Math.max(3, longestBacktickRun(raw) + 1);
					const fence = '`'.repeat(fenceLen);
					const body = raw.endsWith('\n') ? raw : `${raw}\n`;
					state.write(`${fence}html\n`);
					state.write(body);
					state.write(fence);
					state.closeBlock(node);
				},
				parse: {
					/**
					 * Override markdown-it's fence renderer so ` ```html `
					 * blocks become an htmlBlock NODE rather than the default
					 * codeBlock. Other fences still pass through to the
					 * default renderer (preserving syntax-highlighted
					 * code-block behavior for `js`, `go`, etc.).
					 *
					 * markdown-it's `escapeHtml` escapes `&`, `<`, `>`, `"`
					 * — sufficient for a double-quoted attribute value.
					 */
					setup(markdownit: MarkdownIt) {
						const defaultFence = markdownit.renderer.rules.fence;
						markdownit.renderer.rules.fence = (tokens, idx, options, env, self) => {
							const token = tokens[idx];
							const info = (token.info ?? '').trim().toLowerCase();
							if (info === 'html') {
								const escaped = markdownit.utils.escapeHtml(token.content);
								return `<div data-pad-html-block="" data-html="${escaped}"></div>\n`;
							}
							return defaultFence
								? defaultFence(tokens, idx, options, env, self)
								: self.renderToken(tokens, idx, options);
						};
					},
				},
			},
		};
	},
});
