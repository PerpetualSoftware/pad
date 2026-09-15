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
 *
 * AN EXEMPTION IS THE MOST DANGEROUS LINE IN A GUARD — it is the one place
 * the instrument agrees not to look (BUG-3084 checkpoint 21, the rule the
 * collection-page fold paid for four times in three review rounds).
 *
 * Thirteen findings across #1379's rounds; two were defects in the page.
 * Eleven were these guards failing OPEN, and the reviewer defeated the guard
 * four separate times AFTER it had been fixed — every time by constructing a
 * shape and running it, never by reading:
 *   - a disposition that covered a whole EFFECT exempted the effect's own
 *     defect: removing the `untrack` produced zero offenders;
 *   - a disposition scoped by TOKEN but not by POSITION let a synchronous
 *     `identityHeld()` beside the untracked capture restore the dependency;
 *   - `untrack((captureIdentity(), () => {}))` evaluates its argument BEFORE
 *     tracking is disabled, so exempting the call's whole span hid the read;
 *   - `const p = /}/;` inside an effect ended the body at the regex's brace,
 *     every effect still enumerated, zero offenders, page compiled.
 *
 * Three of the four were exemptions too WIDE, not rules too narrow. So an
 * exemption written against this core:
 *   1. names WHICH READ it covers (the token), never merely which block;
 *   2. is bounded POSITIONALLY wherever its reason is positional — "inside
 *      the timer" means AFTER the timer, and `afterMarker` says so;
 *   3. owes a CONTROL that puts the original defect back THROUGH it — not
 *      "the guard catches the defect" but "the guard still catches the
 *      defect once this exemption exists beside it".
 *
 * The third is the one that kept being skipped, and it is the one every
 * defeat above walked through. A guard tested only without its exemptions has
 * been tested for a file that does not exist.
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
	/**
	 * Every `$effect(...)` body on the page.
	 *
	 * The population no other enumerator reaches, and the one BUG-3084 found
	 * last and hardest. A reactive effect that READS the identity epoch takes a
	 * DEPENDENCY on it, so an identity change re-runs the effect — which on the
	 * collection page re-armed a debounce with the previous user's typed text
	 * and captured the NEW epoch, so the fence inside the timer passed and the
	 * old query went to the server under the new identity. The guard was not
	 * missing and did not fail; it was RE-CREATED by the framework at the one
	 * moment it needed to stay put.
	 */
	effectBlocks(): EnumeratedBlock[];
	/**
	 * The names of every `let … = $state(…)` declaration in the script, in
	 * order. The population the transient-state disposition tables are held
	 * against: a guard that enumerates these itself with a regex has already
	 * been wrong once (`stateDeclarations` below says how), so the four page
	 * guards read this instead.
	 */
	stateDeclarations(): string[];
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
		// REGEX LITERALS (codex round 3 [P2]). A `}` inside one — `const p = /}/;`
		// — otherwise closes the block early and every read after it vanishes,
		// silently, with the block still counted. Comments are stripped before
		// this runs, so a `/` here is either division or a regex start; the
		// distinction is the standard one, made on the previous significant
		// character. Handled rather than refused, because the codebase uses
		// regex literals and a guard must cover the grammar actually in use.
		if (ch === '/') {
			const prev = code.slice(0, i).replace(/\s+$/, '');
			const last = prev.slice(-1);
			const isRegexStart =
				prev === '' ||
				'(,=:[!&|?{};+-*%~^'.includes(last) ||
				/\b(return|typeof|case|in|of|new|delete|void|instanceof)$/.test(prev);
			if (isRegexStart) {
				let k = i + 1;
				let inClass = false;
				for (; k < code.length; k++) {
					const c = code[k];
					if (c === '\\') { k++; continue; }
					if (c === '[') { inClass = true; continue; }
					if (c === ']') { inClass = false; continue; }
					if (c === '\n') break; // not a regex after all — leave it alone
					if (c === '/' && !inClass) { i = k; break; }
				}
				if (k < code.length && code[k] === '/') continue;
			}
		}
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

/**
 * The character ranges covered by `untrack(...)` calls in `block`.
 *
 * An exemption must apply to a particular READ, not to any block that merely
 * contains an `untrack` somewhere. The first version of the effect-dependency
 * rule tested for `untrack(` anywhere in the effect, so
 * `const e = captureIdentity(); untrack(() => unrelatedWork());` passed with the
 * capture still tracked (codex round 1 [P2]).
 *
 * This REPLACES a `syncBodyOnly()` helper that stripped every nested function
 * body on the premise that a read inside one is not tracked. That premise is
 * FALSE: an IIFE, a `forEach` callback, a Promise executor, and everything
 * before an async function's first await all run synchronously, and Svelte
 * tracks reads through ordinary calls. The helper therefore HID real
 * dependencies — a fail-open inside the instrument built to catch fail-opens.
 * What is genuinely deferred (a subscription callback, a timer) is now named in
 * each page's disposition table, where the claim is visible and arguable rather
 * than buried in a regex.
 *
 * FAILS CLOSED: an `untrack(` whose parentheses cannot be matched throws rather
 * than being skipped, since skipping narrows the exempt region silently.
 */
export function untrackedSpans(block: string): Array<[number, number]> {
	const out: Array<[number, number]> = [];
	const re = /\buntrack\s*\(/g;
	let m: RegExpExecArray | null;
	while ((m = re.exec(block)) !== null) {
		const open = block.indexOf('(', m.index);
		const close = matchDelimiter(block, open, '(', ')');
		if (close === -1) {
			throw new Error(
				'could not delimit an untrack(...) call — re-point this guard rather than widening it'
			);
		}
		// THE CALLBACK BODY, not the whole call (codex round 2 [P2]).
		// `untrack((captureIdentity(), () => {}))` evaluates that capture while
		// building the argument — BEFORE untrack disables tracking — so the read
		// is tracked and exempting the call's whole span hides it. A compiled
		// probe confirmed the effect re-runs on an epoch bump.
		//
		// Only a call whose argument is IMMEDIATELY a brace-bodied arrow is
		// understood; anything else throws rather than being guessed at.
		const inner = block.slice(open + 1, close);
		const arrow = inner.indexOf('=>');
		// The argument must be a BARE `()` arrow — expression- or brace-bodied,
		// both of which the codebase uses. Anything before the arrow is argument
		// construction and is NOT exempt.
		if (arrow === -1 || inner.slice(0, arrow).trim() !== '()') {
			throw new Error(
				'untrack(...) is not called with a plain `() => …` callback — this guard does not read ' +
					'that form, and treating the whole call as exempt would hide a read evaluated while ' +
					'building the argument. Teach it the shape rather than widening it.'
			);
		}
		out.push([open + 1 + arrow, close]);
		re.lastIndex = close;
	}
	return out;
}

/** Reads of the identity epoch, by the four spellings the family uses. */
export const EPOCH_READ =
	/captureIdentity\s*\(|authStore\s*\.\s*identityEpoch|pageIdentityHeld\s*\(|identityHeld\s*\(/g;

/**
 * Every epoch read in `block` that is NOT lexically inside an `untrack(...)`.
 *
 * Returns the matched text with a little surrounding context, so a guard's
 * failure message can name WHICH read rather than only the block.
 *
 * Deliberately makes no attempt to decide whether a read is deferred. That
 * judgement killed the previous helper: "inside a nested function" is not the
 * same as "runs later", and guessing produced silent passes. A read that really
 * is deferred — a subscription callback, a timer — is named in the calling
 * page's disposition table, where the claim can be read and argued with.
 */
export function trackedEpochReads(block: string): string[] {
	const spans = untrackedSpans(block);
	const out: string[] = [];
	const re = new RegExp(EPOCH_READ.source, 'g');
	let m: RegExpExecArray | null;
	while ((m = re.exec(block)) !== null) {
		const at = m.index;
		if (spans.some(([a, b]) => at >= a && at <= b)) continue;
		out.push(block.slice(Math.max(0, at - 60), at + m[0].length).replace(/\s+/g, ' ').trim());
	}
	return out;
}

/**
 * The same reads, as `{ token, context }`.
 *
 * A disposition must be able to say WHICH reads it covers, not merely which
 * effect. Exempting a whole effect let the `searchTimeout = setTimeout` entry
 * skip the synchronous capture that WAS this unit's defect — removing its
 * `untrack` produced zero offenders (codex round 2 [P2]).
 */
export function trackedEpochReadDetails(
	block: string
): Array<{ token: string; context: string; index: number }> {
	const spans = untrackedSpans(block);
	const out: Array<{ token: string; context: string; index: number }> = [];
	const re = new RegExp(EPOCH_READ.source, 'g');
	let m: RegExpExecArray | null;
	while ((m = re.exec(block)) !== null) {
		const at = m.index;
		if (spans.some(([a, b]) => at >= a && at <= b)) continue;
		out.push({
			token: m[0].replace(/\s+/g, ''),
			context: block.slice(Math.max(0, at - 60), at + m[0].length).replace(/\s+/g, ' ').trim(),
			index: at,
		});
	}
	return out;
}

/**
 * Every `let NAME[: Type] = $state(...)` declaration in `code`, by name.
 *
 * STATEMENT-AWARE, not a line regex — and the difference is the whole reason
 * this lives in the core (BUG-3084 surface 4, mutation M12). Four page guards
 * carried `let\s+(\w+)\s*(?::[^=]*)?=\s*\$state` with the `m` flag. The
 * negated class in the optional type annotation admits `\n`, so on
 *
 *     let pollTimer: ReturnType<typeof setInterval> | undefined;
 *     let onboardingDismissed = $state(false);
 *
 * it matched ONE declaration named `pollTimer` — the annotation ran across the
 * line break into the next statement's `= $state` — and `onboardingDismissed`
 * was never enumerated. The guard went red for a name that is not `$state`
 * while the real one was silently absent: a red instrument, for the wrong
 * reason, hiding the hole it had. Three sibling guards had the same regex and
 * no adjacent pair to trigger it, which is a hole that has not fired yet, not
 * an absence of one.
 *
 * A fix of `[^=\n]` closes that case and opens the opposite one: a genuine
 * annotation that spans lines (`let m: Map<\n string,\n number\n> = $state(…)`)
 * would then be skipped. So this walks each `let` STATEMENT instead: from the
 * name to the first `;` at bracket depth zero, and asks whether that statement
 * initialises with `$state`. An object type with `;` inside its braces, a
 * generic across lines, and an uninitialised typed `let` beside a `$state`
 * line all come out right, and the core's own suite drives each of them —
 * with the legacy regex run on the same fixture as the control that goes red.
 *
 * FAILS CLOSED: a `let` whose statement never terminates throws rather than
 * being skipped, since skipping narrows the population silently.
 */
export function stateDeclarations(code: string): string[] {
	const out: string[] = [];
	const re = /(^|[;{}\n])\s*let\s+([A-Za-z_$][\w$]*)/g;
	let m: RegExpExecArray | null;
	while ((m = re.exec(code)) !== null) {
		const name = m[2]!;
		let i = m.index + m[0].length;
		let depth = 0;
		let quote: string | null = null;
		let initAt = -1;
		let endAt = -1;
		for (; i < code.length; i++) {
			const c = code[i]!;
			if (quote) {
				if (c === '\\') { i++; continue; }
				if (c === quote) quote = null;
				continue;
			}
			if (c === '"' || c === "'" || c === '`') { quote = c; continue; }
			if (c === '{' || c === '(' || c === '[') { depth++; continue; }
			if (c === '}' || c === ')' || c === ']') { depth--; continue; }
			if (depth === 0 && c === ';') { endAt = i; break; }
			if (depth === 0 && initAt === -1 && c === '=' && code[i + 1] !== '=' && code[i + 1] !== '>') {
				initAt = i + 1;
			}
		}
		if (endAt === -1) {
			throw new Error(
				`could not find the end of the statement declaring \`${name}\` — re-point this guard rather than widening it`
			);
		}
		if (initAt !== -1 && /^\s*\$state\b/.test(code.slice(initAt, endAt))) out.push(name);
		re.lastIndex = endAt;
	}
	return out;
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
		effectBlocks(): EnumeratedBlock[] {
			const out: EnumeratedBlock[] = [];
			// `$effect(` and `$effect.pre(` — NOT `$effect.root(`, which creates a
			// scope rather than a tracked reaction. Enumerated by construction so
			// a new effect cannot arrive unseen.
			const re = /\$effect(?:\.pre)?\s*\(/g;
			let m: RegExpExecArray | null;
			while ((m = re.exec(script)) !== null) {
				const label = `$effect at offset ${m.index}`;
				const openParen = script.indexOf('(', m.index);
				const closeParen = matchDelimiter(script, openParen, '(', ')');
				if (closeParen === -1) {
					throw new Error(`could not delimit ${label} — re-point this guard rather than widening it`);
				}
				// The INNER body of a BRACE-BODIED ARROW, and nothing else.
				//
				// FAILS CLOSED on every other form (codex round 1 [P2]). Taking
				// "the first `{` after the call" mis-delimits an expression-bodied
				// arrow — `$effect(() => (captureIdentity(), setTimeout(() => {}, 200)))`
				// returns the TIMER's empty body, so a rule reading it sees no
				// epoch access and passes. Destructured parameter defaults do the
				// same. An enumerator that returns the wrong span is worse than
				// one that refuses: a refusal is visible in red, a wrong span is
				// a silent pass.
				const call = script.slice(m.index, closeParen + 1);
				const argStart = call.indexOf('(') + 1;
				const arrow = call.indexOf('=>');
				const brace = call.indexOf('{');
				// The argument must be a BARE `() => { … }`. A parameter default
				// that is itself an arrow — `$effect((unused = () => {}) => …)` —
				// otherwise lands the scanner on the default's body and returns
				// an empty one, reporting no reads at all (codex round 2 [P2]).
				if (
					arrow === -1 ||
					brace === -1 ||
					brace < arrow ||
					call.slice(argStart, arrow).trim() !== '()' ||
					call.slice(arrow + 2, brace).trim() !== ''
				) {
					throw new Error(
						`${label} is not a brace-bodied arrow — this guard does not read that form. ` +
							'Teach it the shape rather than accepting a body it may have mis-delimited.'
					);
				}
				const end = matchBrace(call, brace);
				if (end === -1) {
					throw new Error(`could not delimit ${label}'s body — re-point this guard rather than widening it`);
				}
				out.push({ label, body: call.slice(brace + 1, end), index: m.index });
			}
			return out;
		},
		stateDeclarations(): string[] {
			return stateDeclarations(script);
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
