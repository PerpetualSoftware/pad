// Shared SOURCE-guard primitives for the identity-fence family (BUG-3084).
//
// WHY THIS EXISTS. BUG-3006 (#1370) established the two-instrument shape for
// one route: a behavioural suite that owns the SEMANTICS of individual
// handlers, and a source guard that owns the POPULATION — because a
// behavioural suite only ever covers the members someone thought to write a
// case for, and cannot tell you a new unfenced handler was added. BUG-3084 is
// the same fix across seven more surfaces, so the population half is extracted
// here with the file path as its only parameter. The SEMANTICS half is
// deliberately NOT extracted: it drives one case per timing class for the
// route it covers and does not generalise.
//
// WHAT A SOURCE GUARD CANNOT DO, so nothing built on this is mistaken for
// proof: it checks SPELLINGS. It cannot see a fence comparing the wrong two
// values, one made unreachable by an earlier return, or one placed after the
// commit it is supposed to guard. Those are the behavioural suite's job.
//
// EVERY ENUMERATOR HERE FAILS CLOSED. A body it cannot delimit throws rather
// than being skipped or sliced to end-of-file — `itemDetailIdentityDiscards\
// TeardownWrites.test.ts` documents what an unbounded slice costs (assertions
// satisfied by unrelated occurrences hundreds of lines away), and BUG-3030
// documents the other direction (an `indexOf` returning -1 made a `slice(fn,
// -1)` scan most of a 7,900-line file). A guard that silently covers less is
// worse than one that is red.
import { readFileSync } from 'node:fs';

export interface EnumeratedBlock {
	/** How this block is named in a failure message. */
	label: string;
	/** The block's source, from its opening token to its closing brace. */
	body: string;
	/** 0-based index of the opening token within the stripped source. */
	index: number;
}

export interface FenceSource {
	/** The file as written, comments included. */
	readonly raw: string;
	/** The file with comments stripped. Assertions run against THIS. */
	readonly code: string;
	/** Everything before `</script>`. */
	readonly script: string;
	/** Everything from `</script>` on. */
	readonly markup: string;
	/**
	 * Top-level `async function` declarations in the script block, by name.
	 * This is instrument A, and on its own it is NOT the population — see
	 * `nestedAsyncCallbacks` and `markupAsyncArrows`.
	 */
	asyncFunctions(): Map<string, string>;
	/** Lines in the MARKUP containing an inline `async (` arrow. Instrument B. */
	markupAsyncArrows(): string[];
	/**
	 * `async` callbacks in the script that are NOT top-level declarations:
	 * IIFEs, subscription callbacks, async timer bodies. Instrument C, and the
	 * one BUG-3006's pair had no member of — the collection page has four, so
	 * a population built from A and B alone is short by four commit points
	 * there (BUG-3084 checkpoint 1).
	 */
	nestedAsyncCallbacks(): EnumeratedBlock[];
	/** `setTimeout(` / `setInterval(` callback bodies, async or not. */
	deferredTimers(): EnumeratedBlock[];
}

/**
 * Strip comments. Load-bearing rather than tidy: a fence's own doc comment
 * quotes the identifiers these guards assert on, so a guard that did not strip
 * would pass on the documentation after the code was deleted (#1370's own
 * note). This is also why every assertion built on this reads `code`.
 */
export function stripComments(src: string): string {
	return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|\s)\/\/[^\n]*/g, '$1');
}

/**
 * Index of the brace that closes the one at `openIndex`, or -1.
 *
 * STRING-AWARE, and that is not decoration: a matcher that counts braces
 * blindly opens on a `{` inside a string or template literal and then
 * mis-delimits every block after it. This file's own test suite drives that
 * case directly rather than asserting in a comment that it cannot happen here
 * — the previous time that assertion was made in a comment, the matcher it
 * defended was broken (identity doc, day 1).
 *
 * Comments must already be stripped; `stripComments` is the only supported
 * input. A `/` is therefore never a comment start here, and division vs regex
 * is not disambiguated — a regex literal containing an unbalanced brace or a
 * quote would break this, so callers get -1 (and a throw) rather than a wrong
 * answer if that ever arrives.
 */
export function matchBrace(code: string, openIndex: number): number {
	return matchDelimiter(code, openIndex, '{', '}');
}

/**
 * The `{}`-aware matcher above, generalised to any paired delimiter.
 *
 * Template interpolations are tracked with their OWN brace counter, kept
 * separate from the pair being matched. Folding the two together is wrong in a
 * way that only shows on `()`: a `${...}` would then decrement the paren depth
 * and the matcher would close the call early, silently. The core's own suite
 * drives a template literal containing both braces and parens for exactly this.
 */
export function matchDelimiter(code: string, openIndex: number, open: string, close: string): number {
	if (code[openIndex] !== open) return -1;
	let depth = 0;
	let quote: string | null = null;
	/** Running `{}` nesting, independent of the pair being matched. */
	let braces = 0;
	/** `braces` as it stood when each open `${` interpolation started. */
	const interpolations: number[] = [];
	for (let i = openIndex; i < code.length; i++) {
		const ch = code[i];
		if (quote) {
			if (ch === '\\') { i++; continue; }
			if (ch === quote) { quote = null; continue; }
			if (quote === '`' && ch === '$' && code[i + 1] === '{') {
				interpolations.push(braces);
				braces++;
				quote = null;
				i++;
			}
			continue;
		}
		if (ch === "'" || ch === '"' || ch === '`') { quote = ch; continue; }
		if (ch === '{') {
			braces++;
			if (open === '{') depth++;
			continue;
		}
		if (ch === '}') {
			// Closing an interpolation returns to template-literal mode; it is
			// not a `}` of the code being matched.
			if (interpolations.length > 0 && braces === interpolations[interpolations.length - 1] + 1) {
				braces--;
				interpolations.pop();
				quote = '`';
				continue;
			}
			braces--;
			if (close === '}') {
				depth--;
				if (depth === 0) return i;
			}
			continue;
		}
		if (ch === open) { depth++; continue; }
		if (ch === close) {
			depth--;
			if (depth === 0) return i;
		}
	}
	return -1;
}

/**
 * Delimit a block whose body is the FIRST `{` at or after `openIndex`. Correct
 * for callbacks and timer bodies, and WRONG for a function declaration — see
 * `delimitDeclaration`.
 */
function delimit(code: string, openIndex: number, label: string): string {
	const brace = code.indexOf('{', openIndex);
	if (brace === -1) {
		throw new Error(`could not find the body of ${label} — re-point this guard rather than widening it`);
	}
	const end = matchBrace(code, brace);
	if (end === -1) {
		throw new Error(`could not delimit ${label} — re-point this guard rather than widening it`);
	}
	return code.slice(openIndex, end + 1);
}

/**
 * Delimit a FUNCTION DECLARATION's body.
 *
 * Not `delimit` above, and the difference is not cosmetic: a signature can
 * contain braces of its own, in a parameter's inline type (`opts?: { undo?:
 * (ids: string[]) => void }`) or in a return type (`: Promise<{ a: string }>`).
 * Taking the FIRST `{` after the declaration keyword lands on that type, and
 * the block then ends at the type's `}` — so the "body" is a few characters of
 * type annotation with no awaits and no commits in it.
 *
 * That is not hypothetical: it is what this function was written to fix. The
 * collection page's `runBulkOn` has a braced parameter type, and every
 * assertion about it passed vacuously — the per-await count skipped it as a
 * body with zero awaits, and only an assertion about a string that HAPPENS to
 * live past the type (`onAction`) went red. A guard that measures nothing
 * usually does not announce it.
 *
 * The rule: the body brace is the LAST `{` on the line carrying the parameter
 * list's closing paren. Every form in this codebase puts it there, including
 * multi-line signatures. A signature that does not FAILS rather than guessing.
 */
function delimitDeclaration(code: string, openIndex: number, label: string): string {
	const openParen = code.indexOf('(', openIndex);
	if (openParen === -1) {
		throw new Error(`could not find ${label}'s parameter list — re-point this guard`);
	}
	const closeParen = matchDelimiter(code, openParen, '(', ')');
	if (closeParen === -1) {
		throw new Error(`could not delimit ${label}'s parameter list — re-point this guard`);
	}
	const lineEnd = code.indexOf('\n', closeParen);
	const line = code.slice(closeParen, lineEnd === -1 ? code.length : lineEnd);
	const braceOnLine = line.lastIndexOf('{');
	if (braceOnLine === -1) {
		throw new Error(
			`${label}'s body brace is not on the line that closes its parameter list — re-point this guard rather than widening it`
		);
	}
	const brace = closeParen + braceOnLine;
	const end = matchBrace(code, brace);
	if (end === -1) {
		throw new Error(`could not delimit ${label} — re-point this guard rather than widening it`);
	}
	return code.slice(openIndex, end + 1);
}

export function readFenceSource(url: URL): FenceSource {
	const raw = readFileSync(url, 'utf8');
	const code = stripComments(raw);
	const scriptEnd = code.indexOf('</script>');
	if (scriptEnd === -1) {
		throw new Error(`no </script> in ${url.pathname} — re-point this guard`);
	}
	const script = code.slice(0, scriptEnd);
	const markup = code.slice(scriptEnd);

	return {
		raw,
		code,
		script,
		markup,
		asyncFunctions(): Map<string, string> {
			const out = new Map<string, string>();
			const re = /\n\tasync function (\w+)\s*\(/g;
			let m: RegExpExecArray | null;
			while ((m = re.exec(script)) !== null) {
				out.set(m[1], delimitDeclaration(script, m.index, `${m[1]}()`));
			}
			return out;
		},
		markupAsyncArrows(): string[] {
			return markup.split('\n').filter((line) => /async\s*\(/.test(line));
		},
		nestedAsyncCallbacks(): EnumeratedBlock[] {
			const out: EnumeratedBlock[] = [];
			// `async (args) =>`, `async arg =>`, and anonymous `async function (`.
			// A NAMED `async function foo(` cannot match either alternative, so
			// instrument A's rows are excluded by construction rather than by a
			// filter that could drift out of step with A's own regex.
			const re = /async\s*(?:\([^)]*\)|\w+)\s*=>|async\s+function\s*\(/g;
			let m: RegExpExecArray | null;
			while ((m = re.exec(script)) !== null) {
				const label = `nested async callback at offset ${m.index}`;
				out.push({ label, body: delimit(script, m.index, label), index: m.index });
			}
			return out;
		},
		deferredTimers(): EnumeratedBlock[] {
			const out: EnumeratedBlock[] = [];
			const re = /\b(setTimeout|setInterval)\s*\(/g;
			let m: RegExpExecArray | null;
			while ((m = re.exec(script)) !== null) {
				const label = `${m[1]} at offset ${m.index}`;
				const openParen = script.indexOf('(', m.index);
				const closeParen = matchDelimiter(script, openParen, '(', ')');
				if (closeParen === -1) {
					throw new Error(`could not delimit ${label} — re-point this guard rather than widening it`);
				}
				// The body is the callback's own braces INSIDE the call. A timer
				// whose callback is a bare identifier (`setTimeout(fn, 0)`) has no
				// inline body, and the call text is returned for it rather than the
				// next `{` in the file — which is what a naive `indexOf('{')` would
				// grab, silently attributing an unrelated block to this timer.
				const call = script.slice(m.index, closeParen + 1);
				const brace = call.indexOf('{');
				if (brace === -1) {
					out.push({ label, body: call, index: m.index });
					continue;
				}
				const end = matchBrace(call, brace);
				if (end === -1) {
					throw new Error(`could not delimit ${label}'s callback — re-point this guard rather than widening it`);
				}
				out.push({ label, body: call.slice(brace, end + 1), index: m.index });
			}
			return out;
		},
	};
}
