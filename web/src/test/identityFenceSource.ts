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

/**
 * REFUSAL IS NOT ABSENCE — the rule every surface in this family has now paid
 * for at least once, placed here so surfaces 4-7 meet it before they write
 * their first assertion (lead's ruling, BUG-3084 day 68).
 *
 * Every rule in this file asks a REFUSAL question: is a fence present, is it
 * positioned before the commit, is there one per await, does it read the right
 * epoch. Not one of them can tell a commit that was correctly REFUSED from a
 * commit that never existed — and the second is a defect, not a fix.
 *
 * What that has actually cost, three times, in three costumes:
 *   - #1372 shipped a fence that could detect the bad state and never leave it.
 *     Ten guard assertions, nine behavioural legs and a 10/10 mutation matrix,
 *     and not one asked whether the page still WORKED afterwards. Every mutant
 *     weakened a fence, so the matrix could only ever measure under-protection.
 *   - Surface 3's deferred timers: a callback that checked the identity and
 *     then cleared nothing satisfied the guard AND the whole behavioural suite.
 *     "Fix the stale write by making the write never happen" was passing.
 *   - Surface 3's `activatePlaybook`: a handler near-identical to its sibling
 *     was vouched for by source text alone, because every leg clicked the
 *     sibling's button. Near-identical is exactly when a source assertion feels
 *     sufficient and is not.
 *
 * So a surface using this core owes, alongside its refusal rules:
 *   1. one case that moves the identity and then KEEPS USING the page;
 *   2. for each fenced commit, one case under an UNCHANGED identity proving the
 *      commit still happens;
 *   3. a driven leg per handler, not per handler SHAPE — including the failure
 *      arms, whose code a source guard reads and nothing executes.
 *
 * A guard that latches into the safe state is not a safe guard; it is an outage
 * with good intentions, and it is harder to notice than the flapping it
 * replaced, precisely because nothing looks wrong.
 */

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

/**
 * The block with every `catch` ARM's body removed, so a question about the
 * SUCCESS path cannot be answered by a check that only a failure would reach.
 *
 * Needed because the obvious spellings of that question are both wrong.
 * Splitting the body at `} catch` reports CORRECT code as unguarded whenever a
 * handler commits after its try/catch rather than inside it (`handleLaneDrop`
 * on the roles board does exactly that), and balancing every arm separately
 * reports correct code too when an await lives in a NESTED try inside an outer
 * catch (`handleStatusChange` on the collection page). Both were tried; both
 * flagged working handlers, and an instrument that reports correct code gets
 * silenced while a documented blind spot does not.
 *
 * Excising the arms answers the question directly: what remains is every path
 * a request that SUCCEEDS can take, and a check found there is one such a
 * request actually reaches.
 */
export function withoutCatchArms(block: string): string {
	// FAILS CLOSED on any catch it cannot delimit (codex round 5 [P2]). The
	// first version treated `}catch` — or a catch whose brace it could not find
	// — as "there is no catch arm", so the success-path guard would go on to
	// scan FAILURE code and could pass an unfenced success path. A guard that
	// answers the wrong question on an input it does not recognise is worse than
	// one that refuses, because the refusal is visible and the wrong answer is
	// not. The refusal message asks to be taught rather than widened.
	const CATCH = /\}\s*catch\b/g;
	let out = '';
	let i = 0;
	while (true) {
		CATCH.lastIndex = i;
		const m = CATCH.exec(block);
		if (m === null) {
			out += block.slice(i);
			return out;
		}
		const at = m.index;
		const brace = block.indexOf('{', at + m[0].length);
		if (brace === -1) {
			throw new Error(
				'found a catch clause with no opening brace — this guard does not understand the ' +
					'grammar in use; re-point it rather than widening it'
			);
		}
		const end = matchBrace(block, brace);
		if (end === -1) {
			throw new Error('could not delimit a catch arm — re-point this guard rather than widening it');
		}
		out += block.slice(i, at);
		i = end + 1;
	}
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
			// FAILS CLOSED when the tab-indented shape above stops describing the
			// file (codex round 5 [P2]). The enumeration is what every population
			// assertion is built on, so a declaration it silently skips is a
			// handler no rule applies to — and the skip is invisible, because a
			// smaller map makes every "these are missing a fence" list SHORTER.
			// A loose count of the word `async function` is the cross-check: it
			// over-counts nested ones, so it can only ever be >= the strict
			// count, and a strict count BELOW it means a top-level declaration
			// was written in a shape this enumerator does not read.
			const loose = (script.match(/\basync function \w+\s*\(/g) ?? []).length;
			const nested = (script.match(/[^\n]\s+async function \w+\s*\(/g) ?? []).length;
			if (out.size < loose - nested) {
				throw new Error(
					`asyncFunctions() enumerated ${out.size} declarations but the file contains at least ` +
						`${loose - nested} written at top level. One is in a shape this guard does not ` +
						`read — teach it the shape rather than accepting the short list.`
				);
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
