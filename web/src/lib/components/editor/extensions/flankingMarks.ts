/**
 * Bold, italic and strike that always serialize to markdown that parses back
 * to the same marks (BUG-3583).
 *
 * CommonMark lets a `**` / `*` / `~~` run open only where it is left-flanking
 * and close only where it is right-flanking. A mark whose text starts with
 * punctuation right after a letter (`Hello` + bold `.World`), or ends with
 * punctuation right before a letter (bold `Hello.` + `World`), has no
 * delimiter that can do both. Measured on the headless editor with the
 * editor's own parser, every such case lost its mark on reload:
 *   - tiptap-markdown's serializer slides an unflankable delimiter inward until
 *     it flanks, so the punctuation silently loses the mark (`Hello.**World**`);
 *   - it skips that step for a mark starting at output offset 0 (a falsy
 *     `start`), so at the start of a block the run went out as
 *     `**Hello.**World`, which parses as literal asterisks: the bug report.
 *
 * Here a run is emitted with its markdown delimiter, and when it closes the
 * delimiter pair is tested against the characters actually around it, with
 * markdown-it's own `scanDelims` (what its parser and tiptap-markdown both
 * use). If either end cannot flank, the opening delimiter already written is
 * replaced with the HTML tag and the run closes with the closing tag
 * (`<strong>`, `<em>`, `<s>`), which the editor parses back to the same mark
 * (`html: true`) and which never needs to flank. A run that flanks is
 * byte-for-byte what tiptap-markdown wrote before. Code needs nothing: a code
 * span closes on its backticks whatever surrounds them (measured).
 *
 * The one character not yet written when a run closes is the one after it,
 * so that one is predicted from the document, and every uncertain neighbour
 * (a non-text node) is taken as a letter, the case that refuses the markdown
 * form. A delimiter of another mark there counts as punctuation, as it is.
 *
 * tiptap-markdown's sliding must not run for these marks: it would move the
 * delimiters this code rewrites, by offsets recorded before the rewrite. Its
 * bookkeeping for them is undone in `installFlankingGuard` below, the only
 * place this module reaches into a dependency (prosemirror-markdown's base
 * serializer state, a single shared instance: tiptap-markdown subclasses it).
 * prosemirror-markdown's own whitespace expelling still runs, so a run's edge
 * spaces still go outside its delimiters as before.
 *
 * Both the editor and the materializer get these through htmlProseExtensions(),
 * so the two serializers cannot drift.
 */
import Bold from '@tiptap/extension-bold';
import Code from '@tiptap/extension-code';
import Italic from '@tiptap/extension-italic';
import Strike from '@tiptap/extension-strike';
import Underline from '@tiptap/extension-underline';
import MarkdownIt from 'markdown-it';
import { MarkdownSerializerState } from 'prosemirror-markdown';
import type { Mark, Node as PMNode } from '@tiptap/pm/model';

const md = new MarkdownIt();

// One inline state, reused: scanDelims reads only `src` and `posMax`, and
// constructing a state per run tripled serialization time on a long document.
const scanState = new md.inline.State('', md, {}, []);

/** markdown-it's flank flags for the delimiter run starting at `src[pos]`. */
function flanks(src: string, pos: number): { can_open: boolean; can_close: boolean } {
	scanState.src = src;
	scanState.posMax = src.length;
	return scanState.scanDelims(pos, true) as { can_open: boolean; can_close: boolean };
}

/** The whole code point that ends at `s[end - 1]` (a surrogate pair stays whole). */
function cpBefore(s: string, end: number): string {
	if (end <= 0) return '';
	const lo = s.charCodeAt(end - 1);
	return end >= 2 && (lo & 0xfc00) === 0xdc00 && (s.charCodeAt(end - 2) & 0xfc00) === 0xd800 ? s.slice(end - 2, end) : s[end - 1];
}

/** The whole code point that starts at `s[at]`. */
function cpAt(s: string, at: number): string {
	if (at >= s.length) return '';
	const hi = s.charCodeAt(at);
	return (hi & 0xfc00) === 0xd800 && (s.charCodeAt(at + 1) & 0xfc00) === 0xdc00 ? s.slice(at, at + 2) : s[at];
}

const SPACE = ' ';

/** CommonMark's punctuation (Unicode P or S, so emoji too), as markdown-it decides it. */
function isPunct(cp: string): boolean {
	return !!cp && (md.utils.isMdAsciiPunct(cp.charCodeAt(0)) || md.utils.isPunctChar(cp));
}

interface OpenRun {
	name: string;
	pos: number;
}

type State = { out: string; marks: Record<string, { padFlanking?: boolean }>; inlines?: unknown[] };

const openRuns = new WeakMap<object, OpenRun[]>();

function flankingSpec(delim: string, tag: string, mixable: boolean) {
	return {
		open(state: State, mark: Mark) {
			let runs = openRuns.get(state);
			if (!runs) openRuns.set(state, (runs = []));
			runs.push({ name: mark.type.name, pos: state.out.length });
			return delim;
		},
		close(state: State, mark: Mark, parent: PMNode, index: number) {
			const runs = openRuns.get(state) ?? [];
			let i = runs.length - 1;
			while (i >= 0 && runs[i].name !== mark.type.name) i--;
			if (i < 0) return delim;
			let { pos } = runs.splice(i, 1)[0];
			// Opened at the start of a continuation line, the delimiter went out
			// in the same write as the line's prefix (`  ` in a list, `> ` in a
			// quote), so the recorded offset is the prefix's: step past it.
			while (pos < state.out.length && !state.out.startsWith(delim, pos) && /[ \t>]/.test(state.out[pos])) pos++;
			if (!state.out.startsWith(delim, pos)) return delim;
			// Edge spaces prosemirror-markdown did not expel (it skips a mark
			// already active on the node before) go outside the delimiters, as
			// tiptap-markdown's sliding put them: the long-standing invisible
			// normalization, not a reason for the HTML form.
			let lead = 0;
			while (pos + delim.length + lead < state.out.length && / |\t/.test(state.out[pos + delim.length + lead])) lead++;
			if (lead && pos + delim.length + lead < state.out.length) {
				const o = state.out;
				state.out = o.slice(0, pos) + o.slice(pos + delim.length, pos + delim.length + lead) + delim + o.slice(pos + delim.length + lead);
				pos += lead;
			}
			// Spaces right after a newline are the line's prefix (a list's
			// indentation, after a hard break), never the run's: left alone.
			let trail = '';
			const tail = / [ \t]*$|\t[ \t]*$/.exec(state.out.slice(pos + delim.length));
			if (tail && !/\n$/.test(state.out.slice(0, state.out.length - tail[0].length))) {
				trail = tail[0];
				state.out = state.out.slice(0, -trail.length);
			}
			const out = state.out;
			const ch = delim[0];
			// markdown-it reads a run of one delimiter character as ONE run, so
			// a nested mark's delimiter of the same character (`***`) is tested
			// with ours, as the parser will see it.
			let openEnd = pos + delim.length;
			while (openEnd < out.length && out[openEnd] === ch) openEnd++;
			const before = cpBefore(out, pos) || SPACE;
			const first = cpAt(out, openEnd) || SPACE;
			let closeStart = out.length;
			while (closeStart > openEnd && out[closeStart - 1] === ch) closeStart--;
			const last = closeStart > openEnd ? cpBefore(out, closeStart) : SPACE;
			// The character after the run is not written yet. Optimistic: assume
			// it lets the run close; renderInline's verification below catches
			// the textblocks where it does not, and re-renders them as HTML.
			const after = SPACE;
			// Closing can still fail on the unseen next character only after
			// punctuation, and runs merged with a nested mark's same-character
			// delimiter pair by markdown-it's rules: either marks this textblock
			// for verification. Plain prose skips it (it costs a parse).
			// A next text node starting with whitespace or punctuation lets any run
			// close, so a run ending in punctuation before one is not a risk:
			// that is most prose (`*word.* and`), and skipping it keeps the parse
			// to the textblocks that can fail.
			const next = index < parent.childCount ? parent.child(index) : null;
			const nextFirst = next?.isText ? cpAt(next.text ?? '', 0) : '';
			const afterSafe = next === null || (!!nextFirst && (/\s/.test(nextFirst) || isPunct(nextFirst)));
			if ((isPunct(last) && !afterSafe) || openEnd > pos + delim.length || closeStart < out.length) (state as { __padRisk?: boolean }).__padRisk = true;
			const ok =
				!(state as { __padForceHtml?: boolean }).__padForceHtml &&
				flanks(`${before}${out.slice(pos, openEnd)}${first}`, before.length).can_open &&
				flanks(`${last}${out.slice(closeStart)}${delim}${after}`, last.length).can_close;
			if (ok) return delim + trail;
			state.out = `${out.slice(0, pos)}<${tag}>${out.slice(pos + delim.length)}`;
			// Runs opened after this one moved right by the rewrite.
			const shift = tag.length + 2 - delim.length;
			for (const r of runs) if (r.pos > pos) r.pos += shift;
			return `</${tag}>${trail}`;
		},
		// Copies the spec this replaces (prosemirror-markdown's strong and em are
		// mixable, tiptap-markdown's strike is not): it decides how delimiters of
		// overlapping marks nest, so changing it would rewrite stored bodies that
		// already round-trip.
		mixable,
		expelEnclosingWhitespace: true,
		padFlanking: true,
	};
}

/**
 * Undo tiptap-markdown's sliding bookkeeping for the marks above. Its
 * serializer state's `markString` records each expel-whitespace mark's
 * delimiter offsets on `this.inlines` (push on open; pop and re-push with the
 * end on close) and then calls this base method; after rendering it slides any
 * delimiter that cannot flank. Popping the entry it just pushed, for a mark
 * flagged `padFlanking`, leaves nothing to slide. Idempotent.
 */
function installFlankingGuard() {
	const proto = MarkdownSerializerState.prototype as unknown as {
		markString: (this: State, mark: Mark, open: boolean, parent: PMNode, index: number) => string;
		__padFlankingGuard?: boolean;
	};
	if (proto.__padFlankingGuard) return;
	const base = proto.markString;
	proto.markString = function (mark, open, parent, index) {
		const value = base.call(this, mark, open, parent, index);
		if (this.marks[mark.type.name]?.padFlanking && Array.isArray(this.inlines)) this.inlines.pop();
		return value;
	};
	proto.__padFlankingGuard = true;
}
installFlankingGuard();

// ── Verification (renderInline) ─────────────────────────────────────────
//
// The first pass cannot see the character after a closing delimiter, nor how
// markdown-it will pair runs of one character across nested marks. So each
// textblock holding these marks is parsed back the way the editor parses it
// (markdown-it, `html: true`) and compared, character by character, with the
// marks the document gives it. Whitespace is not compared at all: moving a
// run's edge spaces outside its delimiters, and markdown-it dropping the
// spaces that start a line after a hard break, are long-standing invisible
// normalizations, and comparing them would rewrite every body that has one.
// On a mismatch the textblock is rendered again with these marks as HTML
// tags; if that does not round-trip either, the first output stands.

const OURS = new Set(['bold', 'italic', 'strike']);
const TAGS: Record<string, string> = { strong: 'bold', em: 'italic', s: 'strike', del: 'strike', b: 'bold', i: 'italic' };
const TOKENS: Record<string, string> = { strong: 'bold', em: 'italic', s: 'strike' };
const parser = new MarkdownIt({ html: true });

function docChars(parent: PMNode): string[] {
	const out: string[] = [];
	parent.forEach((child) => {
		if (!child.isText) return;
		const ms = child.marks.map((m) => m.type.name).filter((n) => OURS.has(n)).sort().join('+');
		for (const ch of child.text ?? '') if (!/\s/.test(ch)) out.push(`${ch}:${ms}`);
	});
	return out;
}

function parsedChars(markdown: string): string[] | null {
	let tokens;
	try {
		tokens = parser.parseInline(markdown, {})[0]?.children ?? [];
	} catch {
		return null;
	}
	const active = new Map<string, number>();
	const bump = (name: string, by: number) => active.set(name, (active.get(name) ?? 0) + by);
	const out: string[] = [];
	for (const t of tokens) {
		const m = /^(strong|em|s)_(open|close)$/.exec(t.type);
		if (m) {
			bump(TOKENS[m[1]], m[2] === 'open' ? 1 : -1);
			continue;
		}
		if (t.type === 'html_inline') {
			const h = /^<(\/?)([a-z]+)>$/i.exec(t.content.trim());
			if (h && TAGS[h[2].toLowerCase()]) bump(TAGS[h[2].toLowerCase()], h[1] ? -1 : 1);
			continue;
		}
		if (t.type !== 'text' && t.type !== 'code_inline') continue;
		const ms = t.type === 'code_inline' ? '' : [...active].filter(([, n]) => n > 0).map(([k]) => k).sort().join('+');
		for (const ch of t.content) if (!/\s/.test(ch)) out.push(`${ch}:${ms}`);
	}
	return out;
}

function hasOurMarks(parent: PMNode): boolean {
	let found = false;
	parent.forEach((c) => {
		if (!found && c.marks.some((m) => OURS.has(m.type.name))) found = true;
	});
	return found;
}

type InlineState = State & {
	delim?: string;
	closed?: unknown;
	atBlockStart?: boolean;
	inAutolink?: unknown;
	__padForceHtml?: boolean;
	__padRisk?: boolean;
};

/** The inline markdown a renderInline wrote, without block separators or line prefixes. */
function inlineOf(written: string, delim: string): string {
	const lines = written.replace(/^\n+/, '').split('\n');
	return lines.map((l) => (delim && l.startsWith(delim) ? l.slice(delim.length) : l)).join('\n');
}

function roundTrips(written: string, delim: string, parent: PMNode): boolean {
	const got = parsedChars(inlineOf(written, delim));
	const want = docChars(parent);
	return !!got && got.length === want.length && got.every((c, i) => c === want[i]);
}

function installVerification() {
	const proto = MarkdownSerializerState.prototype as unknown as {
		renderInline: (this: InlineState, parent: PMNode, fromBlockStart?: boolean) => void;
		__padFlankingVerify?: boolean;
	};
	if (proto.__padFlankingVerify) return;
	const base = proto.renderInline;
	proto.renderInline = function (parent, fromBlockStart) {
		if (this.__padForceHtml || !this.marks?.bold?.padFlanking || !hasOurMarks(parent)) {
			base.call(this, parent, fromBlockStart);
			return;
		}
		const saved = { out: this.out, closed: this.closed, atBlockStart: this.atBlockStart, inAutolink: this.inAutolink };
		this.__padRisk = false;
		base.call(this, parent, fromBlockStart);
		const first = this.out;
		if (!this.__padRisk || roundTrips(first.slice(saved.out.length), this.delim ?? '', parent)) return;
		Object.assign(this, saved);
		this.__padForceHtml = true;
		try {
			base.call(this, parent, fromBlockStart);
		} finally {
			this.__padForceHtml = false;
		}
		if (!roundTrips(this.out.slice(saved.out.length), this.delim ?? '', parent)) {
			Object.assign(this, saved);
			base.call(this, parent, fromBlockStart);
		}
	};
	proto.__padFlankingVerify = true;
}
installVerification();


export const PadBold = Bold.extend({
	addStorage() {
		return { markdown: { serialize: flankingSpec('**', 'strong', true), parse: {} } };
	},
});

export const PadItalic = Italic.extend({
	addStorage() {
		return { markdown: { serialize: flankingSpec('*', 'em', true), parse: {} } };
	},
});

export const PadStrike = Strike.extend({
	addStorage() {
		return { markdown: { serialize: flankingSpec('~~', 's', false), parse: {} } };
	},
});

/**
 * StarterKit's five marks in StarterKit's order, with bold, italic and strike
 * replaced. Use with StarterKit configured `bold: false, code: false,
 * italic: false, strike: false, underline: false`. The order is load-bearing:
 * a mark's rank in the schema decides how serialized delimiters of overlapping
 * marks nest, so moving one would rewrite stored bodies that combine them
 * (measured: 248 of 3000 fuzzed paragraphs).
 */
export function flankingMarkExtensions() {
	return [PadBold, Code, PadItalic, PadStrike, Underline];
}
