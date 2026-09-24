// A leading YAML frontmatter block survives the editor's markdown round trip
// (BUG-2692).
//
// Without this, `---\ntitle: "x"\ndate: y\n---\n\nbody` parsed as a horizontal
// rule followed by a SETEXT heading (the key lines, underlined by the closing
// `---`), and serialized back as `---\n\n## title: "x" date: y` — the metadata
// joined onto one heading line. The result is a fixed point, so the damage
// persisted, and it broke the blog-publish contract, which reads the block.
//
// The block becomes the EXISTING codeBlock node with language `frontmatter`,
// not a new node type: `codeBlock.language` is already a free string, so the
// persisted Y.Doc shape does not change and SCHEMA_VERSION does not move. It
// renders as a code block and stays editable.
//
// Recognition is deliberately strict, because a body that opens with a
// horizontal rule is real content too (MISTA-143 in the census on BUG-2692's
// trail). A block is frontmatter only when ALL of these hold:
//   1. the document's first line is `---` at byte 0 (trailing spaces or tabs
//      allowed);
//   2. a later line is `---` (trailing whitespace allowed: without that, a
//      `--- ` close fell through to the setext reading this file exists to
//      stop, codex round 1);
//   3. every line between is a `key: value` / `key:` line, an indented
//      continuation of a preceding key, or blank, and the first is a key line.
// No YAML parser is loaded: the rule is the strict line check above, chosen
// from the population (every intact block in the census is `key: value` lines
// only). Comments and top-level list items are refused, so a flattened body
// whose next `---` is a later horizontal rule, with prose above it (BLOG-1802),
// is NOT taken for frontmatter.
//
// Serialization writes the `---` form back only when it would re-parse as the
// same block: the node is the first block serialized (nothing written and no
// block closed before it, so a leading empty paragraph disqualifies it) and
// its text still passes the check above. The fences are always written as a
// bare `---`, so trailing whitespace on a fence line is not preserved. Otherwise it writes a ```frontmatter fence, which
// re-parses to the same node, so an edit never loses text or re-flattens.

import type MarkdownIt from 'markdown-it';
import type StateBlock from 'markdown-it/lib/rules_block/state_block';
import type { Node as ProseMirrorNode } from '@tiptap/pm/model';

export const FRONTMATTER_LANGUAGE = 'frontmatter';

const KEY_LINE = /^[A-Za-z_][A-Za-z0-9_-]*:(?:[ \t].*)?$/;
const CONTINUATION = /^[ \t]+\S/;

/** Whether `lines` (the text strictly between the two `---` fences) is a
 *  frontmatter body under the strict rule in this file's header. */
export function isFrontmatterBody(lines: readonly string[]): boolean {
	let sawKey = false;
	for (const line of lines) {
		if (line.trim() === '') continue;
		if (KEY_LINE.test(line)) {
			sawKey = true;
			continue;
		}
		if (sawKey && CONTINUATION.test(line)) continue;
		return false;
	}
	return sawKey;
}

function rawLine(state: StateBlock, n: number): string {
	return state.src.slice(state.bMarks[n], state.eMarks[n]);
}

function isFenceLine(line: string): boolean {
	return line.replace(/[ \t]+$/, '') === '---';
}

function frontmatterRule(state: StateBlock, startLine: number, endLine: number, silent: boolean): boolean {
	if (startLine !== 0 || state.bMarks[0] !== 0) return false;
	if (!isFenceLine(rawLine(state, 0))) return false;
	let close = -1;
	for (let n = 1; n < endLine; n++) {
		if (isFenceLine(rawLine(state, n))) {
			close = n;
			break;
		}
	}
	if (close < 0) return false;
	const inner: string[] = [];
	for (let n = 1; n < close; n++) inner.push(rawLine(state, n));
	if (!isFrontmatterBody(inner)) return false;
	if (silent) return true;
	const token = state.push('fence', 'code', 0);
	token.info = FRONTMATTER_LANGUAGE;
	token.content = inner.length > 0 ? inner.join('\n') + '\n' : '';
	token.markup = '```';
	token.map = [0, close + 1];
	state.line = close + 1;
	return true;
}

// tiptap-markdown runs every extension's `parse.setup` on each parse, against
// the same markdown-it instance, so the rule is registered once per instance
// rather than once per parse (codex round 1: the rule list grew on every
// setContent).
const registered = new WeakSet<MarkdownIt>();

/** Register the frontmatter block rule ahead of every built-in block rule. */
export function setupFrontmatter(markdownit: MarkdownIt): void {
	if (registered.has(markdownit)) return;
	registered.add(markdownit);
	markdownit.block.ruler.before('table', 'pad_frontmatter', frontmatterRule);
}

type SerializerState = {
	out: string;
	closed: unknown;
	write: (s: string) => void;
	text: (s: string, escape?: boolean) => void;
	ensureNewLine: () => void;
	closeBlock: (node: ProseMirrorNode) => void;
};

/**
 * tiptap-markdown storage for the editor's codeBlock. Every language other
 * than `frontmatter` keeps tiptap-markdown's own codeBlock spec byte-for-byte
 * (fence + language + text, the node's configured `languageClassPrefix`, the
 * same `updateDOM`), because giving the node its own storage replaces that
 * default rather than extending it.
 */
export function codeBlockMarkdownStorage(languageClassPrefix: string | null | undefined) {
	return {
		serialize(state: SerializerState, node: ProseMirrorNode) {
			const language = (node.attrs.language as string | null) || '';
			const text = node.textContent;
			if (language === FRONTMATTER_LANGUAGE && state.out === '' && !state.closed) {
				const lines = text.replace(/\n$/, '').split('\n');
				if (!lines.some(isFenceLine) && isFrontmatterBody(lines)) {
					state.write('---\n');
					state.text(text, false);
					state.ensureNewLine();
					state.write('---');
					state.closeBlock(node);
					return;
				}
			}
			state.write('```' + language + '\n');
			state.text(text, false);
			state.ensureNewLine();
			state.write('```');
			state.closeBlock(node);
		},
		parse: {
			setup(markdownit: MarkdownIt) {
				markdownit.set({ langPrefix: languageClassPrefix ?? 'language-' });
				setupFrontmatter(markdownit);
			},
			updateDOM(element: HTMLElement) {
				element.innerHTML = element.innerHTML.replace(/\n<\/code><\/pre>/g, '</code></pre>');
			},
		},
	};
}
