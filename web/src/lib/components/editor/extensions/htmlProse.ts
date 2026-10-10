/**
 * Angle-bracketed prose survives the editor (BUG-3557).
 *
 * The editor reads markdown through markdown-it with `html: true`, so any
 * `<Name …>` in a body is passed to the browser as a tag. A tag the schema has
 * no rule for is discarded by ProseMirror's DOM parser (`the <Button>
 * component` → `the component`, `Promise<T>` → `Promise`), comments vanish,
 * and `<Table>` (HTML names are case-insensitive) becomes an empty table that
 * is then removed. The loss lands on the next flush: one keystroke, or an
 * agent's write applied by an open tab.
 *
 * Raw HTML is still needed: the editor writes some of its own nodes and marks
 * as HTML (a table with spans or multi-block cells, an HTML-only mark), and
 * those must read back. So the allowlist is the schema's own parse rules:
 *
 * - An inline tag passes through only if it is written in lower case, matches
 *   an inline node's or a mark's `parseDOM` rule (attribute conditions
 *   included), and, for a closing tag, closes one that passed. Anything else
 *   becomes literal text.
 * - An HTML block passes through only if every tag in it matches some rule;
 *   otherwise the whole block becomes an HtmlBlock, verbatim (stored as a
 *   ```html fence), so nothing in it is dropped.
 * - Comments become literal text: a hidden comment that is then dropped is
 *   the loss this exists to stop.
 *
 * On the way out, text escapes `<` only where it would open an inline tag this
 * parse lets through, or start an HTML block at the beginning of a line; elsewhere
 * `Promise<T>` stays `Promise<T>` instead of turning into `Promise&lt;T&gt;`.
 *
 * Both the live editor and the headless materializer (the server's recovery)
 * take their extensions from `htmlProseExtensions()`, so they parse the same.
 */

import { Extension, type Editor } from '@tiptap/core';
import Text from '@tiptap/extension-text';
import type MarkdownIt from 'markdown-it';
import type { Schema } from '@tiptap/pm/model';
import blockNames from 'markdown-it/lib/common/html_blocks.mjs';

interface AttrCond {
	name: string;
	op: 'has' | '=' | '^=';
	value: string;
}

interface Rule {
	tag: string;
	conds: AttrCond[];
	inline: boolean;
}

/** Wrapper tags the DOM parser walks through without losing their content. */
const TRANSPARENT = new Set(['tbody', 'thead', 'tfoot', 'colgroup', 'col']);
const VOID = new Set(['br', 'img', 'hr', 'col', 'wbr']);
const BLOCK_NAMES = new Set<string>(blockNames as string[]);

/**
 * `tag`, `tag[attr]`, `tag[attr="v"]`, `tag[attr^="v"]`; any other selector
 * form is not understood and so never lets a tag through (it becomes text,
 * which loses nothing).
 */
function parseSelector(sel: string): { tag: string; conds: AttrCond[] } | null {
	const m = /^([a-z][a-z0-9]*)((?:\[[^\]]+\])*)$/.exec(sel.trim());
	if (!m) return null;
	const conds: AttrCond[] = [];
	const attrRe = /\[([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:(\^?=)"([^"]*)")?\]/g;
	let rest = m[2];
	let a: RegExpExecArray | null;
	while ((a = attrRe.exec(m[2])) !== null) {
		conds.push({ name: a[1].toLowerCase(), op: (a[2] as AttrCond['op']) ?? 'has', value: a[3] ?? '' });
		rest = rest.replace(a[0], '');
	}
	if (rest !== '') return null;
	return { tag: m[1], conds };
}

const rulesCache = new WeakMap<Schema, Rule[]>();

/** Every tag-based parse rule in the schema, nodes and marks. */
export function schemaTagRules(schema: Schema): Rule[] {
	const cached = rulesCache.get(schema);
	if (cached) return cached;
	const rules: Rule[] = [];
	const add = (parseDOM: unknown, inline: boolean) => {
		for (const r of (parseDOM as Array<{ tag?: string }> | undefined) ?? []) {
			if (!r.tag) continue;
			for (const sel of r.tag.split(',')) {
				const p = parseSelector(sel);
				if (p) rules.push({ ...p, inline });
			}
		}
	};
	for (const name of Object.keys(schema.nodes)) {
		const type = schema.nodes[name];
		add(type.spec.parseDOM, type.isInline);
	}
	for (const name of Object.keys(schema.marks)) add(schema.marks[name].spec.parseDOM, true);
	rulesCache.set(schema, rules);
	return rules;
}

interface Tag {
	closing: boolean;
	name: string;
	attrs: Map<string, string>;
	selfClosing: boolean;
}

const TAG_RE = /^<(\/?)([A-Za-z][A-Za-z0-9-]*)((?:\s[^<>]*?)?)\s*(\/?)>$/s;

function parseTag(src: string): Tag | null {
	const m = TAG_RE.exec(src.trim());
	if (!m) return null;
	const attrs = new Map<string, string>();
	const attrRe = /([^\s=/>"']+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+)))?/g;
	let a: RegExpExecArray | null;
	while ((a = attrRe.exec(m[3] ?? '')) !== null) attrs.set(a[1].toLowerCase(), a[2] ?? a[3] ?? a[4] ?? '');
	return { closing: m[1] === '/', name: m[2], attrs, selfClosing: m[4] === '/' };
}

function condHolds(c: AttrCond, attrs: Map<string, string>): boolean {
	if (!attrs.has(c.name)) return false;
	const v = attrs.get(c.name) ?? '';
	if (c.op === '=') return v === c.value;
	if (c.op === '^=') return v.startsWith(c.value);
	return true;
}

/**
 * Whether the schema has a rule for this tag. Only a name written in lower
 * case qualifies: the editor writes lower case, and `<Table>` or `<Button>`
 * in prose is a name, not markup. A closing tag is judged by name alone.
 */
function ruleMatches(tag: Tag, rules: Rule[], inlineOnly: boolean): boolean {
	if (tag.name !== tag.name.toLowerCase()) return false;
	if (!inlineOnly && TRANSPARENT.has(tag.name)) return true;
	return rules.some(
		(r) =>
			r.tag === tag.name &&
			(!inlineOnly || r.inline) &&
			(tag.closing || r.conds.every((c) => condHolds(c, tag.attrs))),
	);
}

const STACK = Symbol('padHtmlProseStack');

function escapeText(md: MarkdownIt, s: string): string {
	return md.utils.escapeHtml(s);
}

/** The markdown-it render rules: the parse half of BUG-3557. */
export function installHtmlProseGuard(md: MarkdownIt, editor: Editor): void {
	const rules = () => schemaTagRules(editor.schema);

	md.renderer.rules.html_inline = (tokens, idx, _options, env) => {
		const src = tokens[idx].content;
		const tag = parseTag(src);
		if (!tag) return escapeText(md, src); // comments, processing instructions, CDATA
		const e = env as Record<symbol, string[] | undefined>;
		const stack = (e[STACK] ??= []);
		if (tag.closing) {
			const at = stack.lastIndexOf(tag.name);
			if (at === -1) return escapeText(md, src);
			stack.splice(at, 1);
			return src;
		}
		if (!ruleMatches(tag, rules(), true)) return escapeText(md, src);
		if (!tag.selfClosing && !VOID.has(tag.name)) stack.push(tag.name);
		return src;
	};

	md.renderer.rules.html_block = (tokens, idx) => {
		const src = tokens[idx].content;
		const all = src.match(/<!--[\s\S]*?(?:-->|$)|<\/?[A-Za-z][^<>]*>/g) ?? [];
		const keepsAll = all.every((t) => {
			if (t.startsWith('<!--')) return false;
			const tag = parseTag(t);
			return !!tag && ruleMatches(tag, rules(), false);
		});
		if (keepsAll && all.length > 0) return src;
		if (/^\s*<!--[\s\S]*-->\s*$/.test(src)) {
			// A block that is only a comment: visible text, not hidden and dropped.
			return `<p>${escapeText(md, src.trim())}</p>\n`;
		}
		const html = src.replace(/\n$/, '');
		return `<div data-pad-html-block="" data-html="${escapeText(md, html)}"></div>\n`;
	};
}

/**
 * The serialize half: escape `<` only where the parse above would read a tag
 * (or an HTML block at the start of a line), so ordinary prose keeps its
 * angle brackets verbatim.
 */
export function escapeTagOpeners(text: string, schema: Schema, atLineStart: boolean): string {
	const rules = schemaTagRules(schema);
	return text.replace(/<(\/?)([A-Za-z][A-Za-z0-9-]*)?/g, (whole, slash: string, name: string | undefined, offset: number) => {
		if (!name) return whole;
		const lower = name.toLowerCase();
		const startsLine = offset === 0 ? atLineStart : text[offset - 1] === '\n';
		if (startsLine && BLOCK_NAMES.has(lower)) return `&lt;${slash}${name}`;
		// Mid-line, markdown-it reads a tag as inline HTML, which the parse lets
		// through only for an inline node or mark.
		if (name !== lower) return whole;
		return rules.some((r) => r.tag === lower && r.inline) ? `&lt;${slash}${name}` : whole;
	});
}

interface SerializerState {
	text: (s: string, escape?: boolean) => void;
	out?: string;
}

/**
 * The Text node with the narrower escape. tiptap-markdown takes an extension's
 * own `storage.markdown` over its built-in spec for the same name.
 */
export const PadText = Text.extend({
	addStorage() {
		return {
			markdown: {
				serialize(state: SerializerState, node: { text?: string; type: { schema: Schema } }) {
					const out = state.out ?? '';
					const atLineStart = out === '' || out.endsWith('\n');
					state.text(escapeTagOpeners(node.text ?? '', node.type.schema, atLineStart));
				},
				parse: {},
			},
		};
	},
});

/** The parse guard as an extension: tiptap-markdown runs `parse.setup(md)`. */
export const HtmlProseGuard = Extension.create({
	name: 'padHtmlProseGuard',
	addStorage() {
		return {
			markdown: {
				parse: {
					setup(this: { editor: Editor }, md: MarkdownIt) {
						installHtmlProseGuard(md, this.editor);
					},
				},
			},
		};
	},
});

/**
 * Both, for the editor's and the materializer's extension lists. StarterKit
 * must be configured with `text: false` beside them.
 */
export function htmlProseExtensions() {
	return [PadText, HtmlProseGuard];
}
