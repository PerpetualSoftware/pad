// The instrument for the instruments (BUG-3084).
//
// `identityFenceSource.ts` is code with an adversary: every guard in this
// family rests on its enumerations being COMPLETE and its delimiting being
// CORRECT, and both failures are silent — a matcher that runs past a block's
// end satisfies assertions from unrelated code, and one that stops early
// reports a handler as unfenced when it is fenced. Neither shows up as an
// error. So the core is driven here against inputs whose answers are known,
// including the two shapes that have broken brace matchers in this repo
// before: a brace inside a string literal, and a template interpolation.
import { describe, it, expect } from 'vitest';
import { matchBrace, matchDelimiter, readFenceSource, stripComments, withoutCatchArms, untrackedSpans, trackedEpochReads, stateDeclarations } from './identityFenceSource';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

describe('matchBrace', () => {
	it('matches a plain nested block', () => {
		const src = '{ a { b } c }X';
		expect(matchBrace(src, 0)).toBe(src.indexOf('X') - 1);
	});

	it('does not open on a brace inside a string literal', () => {
		// Without string awareness the `{` in `"{"` opens a level that is never
		// closed, and the matcher runs to the end of the file.
		const src = '{ const s = "{"; }X';
		expect(matchBrace(src, 0)).toBe(src.indexOf('X') - 1);
	});

	it('does not close on a brace inside a string literal', () => {
		const src = '{ const s = "}"; }X';
		expect(matchBrace(src, 0)).toBe(src.indexOf('X') - 1);
	});

	it('survives an escaped quote inside a string', () => {
		const src = '{ const s = "a\\"} b"; }X';
		expect(matchBrace(src, 0)).toBe(src.indexOf('X') - 1);
	});

	it('handles a template literal containing braces and an interpolation', () => {
		const src = '{ const s = `a {${ f({ k: 1 }) } b`; }X';
		expect(matchBrace(src, 0)).toBe(src.indexOf('X') - 1);
	});

	it('returns -1 rather than a wrong answer when the block never closes', () => {
		expect(matchBrace('{ a { b }', 0)).toBe(-1);
	});

	it('returns -1 when pointed at something that is not the open delimiter', () => {
		expect(matchBrace('  {}', 0)).toBe(-1);
	});
});

describe('matchDelimiter on parens', () => {
	it('matches a nested call', () => {
		const src = '(a, f(b), c)X';
		expect(matchDelimiter(src, 0, '(', ')')).toBe(src.indexOf('X') - 1);
	});

	it('is not closed early by a paren inside a template interpolation', () => {
		// The interpolation's parens are BALANCED, so this leg has to carry an
		// unbalanced one or it measures nothing: `(`${ g(1) }`, 0)` passes
		// against a matcher with no string awareness at all, which is the false
		// green a first version of this suite shipped.
		const src = '(`${ g(")") }`, 0)X';
		expect(matchDelimiter(src, 0, '(', ')')).toBe(src.indexOf('X') - 1);
	});

	it('keeps interpolation braces out of the pair being matched', () => {
		// The specific fault of a SHARED counter (the first version of
		// `matchDelimiter`): `${` incremented the paren depth, and the
		// interpolation's `}` did not decrement it, so the matcher stayed
		// inside the template for the rest of the file and returned -1.
		const src = '(`${ ({ a: 1 }) }`, 0)X';
		expect(matchDelimiter(src, 0, '(', ')')).toBe(src.indexOf('X') - 1);
	});

	it('is not closed early by a paren inside a string', () => {
		const src = '(") ", 0)X';
		expect(matchDelimiter(src, 0, '(', ')')).toBe(src.indexOf('X') - 1);
	});
});

describe('stripComments', () => {
	it('removes block and line comments', () => {
		expect(stripComments('a /* x */ b // y\nc')).toBe('a  b \nc');
	});

	it('is why the guards read `code` — a deleted fence leaves its doc comment behind', () => {
		const withDoc = '// calls identityHeld(captured)\nconst a = 1;';
		expect(stripComments(withDoc)).not.toContain('identityHeld');
	});
});

describe('asyncFunctions delimiting', () => {
	function fixture(script: string): ReturnType<typeof readFenceSource> {
		const dir = mkdtempSync(join(tmpdir(), 'fence-'));
		const file = join(dir, 'fixture.svelte');
		writeFileSync(file, `<script lang="ts">\n${script}\n</script>\n<div />\n`, 'utf8');
		return readFenceSource(pathToFileURL(file));
	}

	it('does not stop at a brace inside the PARAMETER TYPE', () => {
		// The defect this function was written to fix, found by a guard
		// assertion going red on the collection page rather than by reading:
		// taking the first `{` after the declaration lands on the parameter's
		// inline type, so the "body" is a few characters of annotation — no
		// awaits, no commits, and every assertion about that handler passes
		// vacuously.
		const src = fixture(
			'\tasync function runIt(\n' +
			'\t\topts?: { undo?: (ids: string[]) => void }\n' +
			'\t): Promise<string[]> {\n' +
			'\t\tconst r = await go();\n' +
			'\t\tMARKER;\n' +
			'\t\treturn r;\n' +
			'\t}'
		);
		const body = src.asyncFunctions().get('runIt');
		expect(body, 'runIt was not enumerated at all').toBeDefined();
		expect(body, 'the body was cut short at the parameter type').toContain('MARKER');
		expect(body!.split('await ').length - 1).toBe(1);
	});

	it('handles a braced RETURN type too', () => {
		const src = fixture(
			'\tasync function go(): Promise<{ a: string }> {\n' +
			'\t\tawait x();\n' +
			'\t\tMARKER;\n' +
			'\t}'
		);
		expect(src.asyncFunctions().get('go')).toContain('MARKER');
	});

	it('fails closed when the body brace is not where it expects', () => {
		// Widening the search is the move that would cost the property above:
		// a guard that hunts for a plausible brace will always find one.
		const src = fixture(
			'\tasync function odd()\n' +
			'\t{\n' +
			'\t\tawait x();\n' +
			'\t}'
		);
		expect(() => src.asyncFunctions()).toThrow(/re-point this guard/);
	});

	it('delimits a nested async arrow with a DESTRUCTURED parameter from its arrow, not from the parameter brace', () => {
		// ItemDetail's collab `save: async ({ ws, itemId, toSave, keepalive }) => {…}`.
		// Delimiting from `async` returned `async ({ ws, itemId, toSave, keepalive }`
		// as the body — no awaits, no commits — and a guard row keyed on a token
		// inside the real body matched nothing (BUG-3084, ItemDetail).
		const src = fixture(
			'\tconst saver = {\n' +
			'\t\tsave: async ({ ws, itemId }) => {\n' +
			'\t\t\tawait go(ws, itemId);\n' +
			'\t\t\tMARKER;\n' +
			'\t\t},\n' +
			'\t};'
		);
		const nested = src.nestedAsyncCallbacks();
		expect(nested).toHaveLength(1);
		expect(nested[0].body, 'the body was cut short at the destructured parameter').toContain('MARKER');
		expect(nested[0].body).toContain('await go');
	});

	it('reads an anonymous async function whose parameter type is braced', () => {
		const src = fixture(
			'\tconst f = async function (opts: { a: string }) {\n' +
			'\t\tawait x(opts);\n' +
			'\t\tMARKER;\n' +
			'\t};'
		);
		const nested = src.nestedAsyncCallbacks();
		expect(nested).toHaveLength(1);
		expect(nested[0].body).toContain('MARKER');
	});

	it('REFUSES an expression-bodied async arrow rather than taking the next brace in the file', () => {
		const src = fixture(
			'\tconst f = async (x) => go(x);\n' +
			'\tfunction unrelated() {\n' +
			'\t\tMARKER;\n' +
			'\t}'
		);
		expect(() => src.nestedAsyncCallbacks()).toThrow(/not brace-bodied/);
	});

	it('does not attribute an unrelated block to a timer with an identifier callback', () => {
		// `setTimeout(fn, 0)` has no inline body; a naive `indexOf('{')` grabs
		// the next block in the file and asserts against code that has nothing
		// to do with the timer.
		const src = fixture(
			'\tconst t = setTimeout(fn, 0);\n' +
			'\tfunction unrelated() {\n' +
			'\t\tMARKER;\n' +
			'\t}'
		);
		const timers = src.deferredTimers();
		expect(timers).toHaveLength(1);
		expect(timers[0].body).not.toContain('MARKER');
	});
});

describe('withoutCatchArms', () => {
	it('removes a catch arm and keeps everything else', () => {
		const body = 'A try { B } catch (e) { HIDDEN } C';
		const out = withoutCatchArms(body);
		expect(out).not.toContain('HIDDEN');
		expect(out).toContain('A');
		expect(out).toContain('B');
		expect(out).toContain('C');
	});

	it('keeps a commit that follows the try/catch', () => {
		// The false positive the narrower `slice(firstAwait, indexOf("} catch"))`
		// form produces: `handleLaneDrop` on the roles board awaits inside its
		// try, commits AFTER the whole try/catch, and is correct — but the
		// narrow window ends at `} catch` and never sees the check.
		const body = 'try { await go(); } catch (e) { log(e); } if (!identityHeld(x)) return; commit();';
		expect(withoutCatchArms(body)).toContain('identityHeld(x)');
	});

	it('keeps an await that lives in a NESTED try inside an outer catch out of the success path', () => {
		// The other false positive, from balancing arms separately:
		// `handleStatusChange` on the collection page has its retry-await inside
		// a nested try within the outer catch. Excising arms removes the whole
		// outer catch, nested block included, so the success path does not
		// inherit an await only a failure reaches.
		const body = 'try { await a(); if (!identityHeld(x)) return; } catch (e) { try { await retry(); } catch (f) { log(f); } }';
		const out = withoutCatchArms(body);
		expect(out).not.toContain('retry()');
		expect(out).toContain('await a()');
	});

	it('handles several catch arms', () => {
		const body = 'try { A } catch { X1 } try { B } catch { X2 } C';
		const out = withoutCatchArms(body);
		expect(out).not.toContain('X1');
		expect(out).not.toContain('X2');
		expect(out).toContain('C');
	});

	it('is a no-op on a body with no catch', () => {
		expect(withoutCatchArms('A B C')).toBe('A B C');
	});

	it('throws rather than guessing when an arm cannot be delimited', () => {
		expect(() => withoutCatchArms('try { A } catch (e) { unterminated')).toThrow(/re-point this guard/);
	});
});

describe('untrackedSpans', () => {
	it('covers only the untrack call, so a read outside one is still tracked', () => {
		const block = "const e = captureIdentity(); untrack(() => unrelated());";
		const spans = untrackedSpans(block);
		expect(spans).toHaveLength(1);
		const readAt = block.indexOf('captureIdentity()');
		expect(
			spans.some(([a, b]) => readAt >= a && readAt <= b),
			'the capture sits outside the untrack call and must not be exempted by it — the first ' +
				'version of the effect rule tested for `untrack(` anywhere in the block and exempted ' +
				'exactly this shape'
		).toBe(false);
	});

	it('throws rather than guessing when an untrack call cannot be delimited', () => {
		expect(() => untrackedSpans('untrack(() => {')).toThrow(/re-point this guard/);
	});
});

describe('trackedEpochReads', () => {
	it('reports a read outside untrack and ignores one inside it', () => {
		const reads = trackedEpochReads(
			'const a = captureIdentity(); untrack(() => pageIdentityHeld());'
		);
		expect(reads).toHaveLength(1);
		expect(reads[0]).toContain('captureIdentity(');
	});

	it('reports a read inside a NESTED FUNCTION, because nesting is not deferral', () => {
		// The premise that killed the previous helper: an IIFE, a forEach
		// callback, a Promise executor and everything before an async function's
		// first await all run SYNCHRONOUSLY, and Svelte tracks reads through
		// ordinary calls. A rule that strips nested bodies hides real
		// dependencies. What is genuinely deferred is named in a page's
		// disposition table instead.
		const reads = trackedEpochReads('(() => { captureIdentity(); })();');
		expect(
			reads,
			'a read inside an immediately-invoked function is tracked, and stripping nested bodies ' +
				'would have hidden it'
		).toHaveLength(1);
	});
});

describe('effectBlocks fails closed on forms it cannot read', () => {
	function sourceWith(effect: string): URL {
		const dir = mkdtempSync(join(tmpdir(), 'fence-'));
		const file = join(dir, 'x.svelte');
		writeFileSync(file, `<script lang="ts">\n\t${effect}\n</script>\n<div></div>`);
		return pathToFileURL(file);
	}

	it('reads a brace-bodied arrow', () => {
		const src = readFenceSource(sourceWith('$effect(() => { captureIdentity(); });'));
		const blocks = src.effectBlocks();
		expect(blocks).toHaveLength(1);
		expect(trackedEpochReads(blocks[0]!.body)).toHaveLength(1);
	});

	it('REFUSES an expression-bodied arrow instead of mis-delimiting it', () => {
		// Taking "the first `{` after the call" returns the TIMER's empty body
		// here, so a rule reading it sees no epoch access and passes while the
		// capture is tracked. A wrong span is a silent pass; a refusal is red.
		const src = readFenceSource(
			sourceWith('$effect(() => (captureIdentity(), setTimeout(() => {}, 200), undefined));')
		);
		expect(() => src.effectBlocks()).toThrow(/not a brace-bodied arrow/);
	});
});

describe('untrackedSpans exempts only what actually runs untracked', () => {
	it('does NOT exempt a read evaluated while building the argument', () => {
		// `untrack((captureIdentity(), () => {}))` runs the capture BEFORE
		// untrack is called, so the read is tracked. Exempting the call's whole
		// span hid it; a compiled probe confirmed the effect re-runs on an epoch
		// bump (codex round 2 [P2]). This form is refused rather than guessed at.
		expect(() => trackedEpochReads('untrack((captureIdentity(), () => {}));')).toThrow(
			/does not read that form/
		);
	});

	it('exempts an expression-bodied callback, which is the form the codebase uses', () => {
		expect(trackedEpochReads('untrack(() => captureIdentity());')).toHaveLength(0);
	});

	it('exempts a brace-bodied callback too', () => {
		expect(trackedEpochReads('untrack(() => { captureIdentity(); });')).toHaveLength(0);
	});
});

describe('epoch reads are found regardless of ordinary whitespace', () => {
	it('matches spaced call and member forms', () => {
		// Unchanged runtime reads with different formatting were producing zero
		// matches, so the guard accepted them silently (codex round 2 [P3]).
		expect(trackedEpochReads('captureIdentity ();')).toHaveLength(1);
		expect(trackedEpochReads('const x = authStore . identityEpoch;')).toHaveLength(1);
	});
});

describe('matchBrace understands regex literals', () => {
	it('does not close a block at a `}` inside a regex', () => {
		// `const p = /}/;` otherwise ends the body early and every read after it
		// vanishes — silently, with the effect still enumerated (codex round 3).
		const code = '{ const p = /}/; captureIdentity(); }';
		const end = matchBrace(code, 0);
		expect(end, 'the block was truncated at the regex literal').toBe(code.length - 1);
	});

	it('still treats division as division', () => {
		const code = '{ const r = a / b; const s = c / d; }';
		expect(matchBrace(code, 0)).toBe(code.length - 1);
	});
});

describe('stateDeclarations enumerates the $state population by STATEMENT', () => {
	// The regex every page guard used to carry, kept here as the CONTROL: each
	// fixture below states what the regex answered, so the case that motivated
	// the hoist stays red on the old instrument rather than being described.
	const LEGACY = /^\s*let\s+(\w+)\s*(?::[^=]*)?=\s*\$state/gm;
	const legacy = (code: string) => [...code.matchAll(LEGACY)].map((m) => m[1]!);

	it('reads plain, typed and generic declarations, in order', () => {
		const code = [
			'\tlet loading = $state(true);',
			'\tlet dashboard = $state<DashboardResponse | null>(null);',
			'\tlet tags: string[] = $state([]);',
			'\tlet plain = 3;',
			'\tlet derived = $derived(x);',
		].join('\n');
		expect(stateDeclarations(code)).toEqual(['loading', 'dashboard', 'tags']);
		expect(legacy(code)).toEqual(['loading', 'dashboard', 'tags']);
	});

	it('an uninitialised typed let ABOVE a $state line does not swallow it (BUG-3084 M12)', () => {
		// The dashboard page's own shape. The legacy regex let the annotation
		// run across the newline into the next statement: ONE match, named
		// after the wrong variable, and the real $state never enumerated.
		const code = [
			'\tlet pollTimer: ReturnType<typeof setInterval> | undefined;',
			'\tlet onboardingDismissed = $state(false);',
		].join('\n');
		expect(stateDeclarations(code)).toEqual(['onboardingDismissed']);
		expect(legacy(code), 'the control: the legacy regex is wrong here, and must stay wrong').toEqual(['pollTimer']);
	});

	it('a genuine annotation spanning lines is still found (the [^=\\n] fix would miss it)', () => {
		const code = ['\tlet m: Map<', '\t\tstring,', '\t\tnumber', '\t> = $state(new Map());'].join('\n');
		expect(stateDeclarations(code)).toEqual(['m']);
		const narrowFix = [...code.matchAll(/^\s*let\s+(\w+)\s*(?::[^=\n]*)?=\s*\$state/gm)].map((m) => m[1]!);
		expect(narrowFix, "the surface-4 guard's own fix: closes the adjacency case, opens this one").toEqual([]);
	});

	it('an object type with semicolons inside its braces does not end the statement early', () => {
		const code = '\tlet track: { slug: string; onboarding: boolean } | null = $state(null);\n\tlet other = 1;';
		expect(stateDeclarations(code)).toEqual(['track']);
	});

	it('a semicolon inside a string in the initialiser does not end the statement early', () => {
		const code = "\tlet s = $state('a;b');\n\tlet n = 1;";
		expect(stateDeclarations(code)).toEqual(['s']);
	});

	it('does not match `let` inside an identifier or a $state that is not the initialiser', () => {
		const code = ['\tlet outlet = 1;', '\tlet x = foo($state(1));', '\tconst y = $state(2);'].join('\n');
		expect(stateDeclarations(code)).toEqual([]);
	});

	it('end of input terminates the last statement; an unclosed bracket throws', () => {
		expect(stateDeclarations('\tlet a = $state(1)')).toEqual(['a']);
		expect(() => stateDeclarations('\tlet a = $state((1')).toThrow(/could not delimit/);
	});

	// The four shapes codex built against the first scanner (round 1 on the
	// hoist), each accepted by the compiler and each read wrong by a depth
	// counter that did not know the grammar.
	it('a regex literal containing a brace does not swallow the declarations after it', () => {
		const code = ['\tlet open = /{/;', '\tlet hidden = $state(1);', '\tlet close = /}/;'].join('\n');
		expect(stateDeclarations(code)).toEqual(['hidden']);
	});

	it('every binding of a multi-binding let is inspected', () => {
		expect(stateDeclarations('\tlet visible = $state(0), hidden = $state(1);')).toEqual(['visible', 'hidden']);
		expect(stateDeclarations('\tlet total = 0, done = 0;')).toEqual([]);
		expect(stateDeclarations('\tlet a = 0, b = $state(1), c: number = 2;')).toEqual(['b']);
	});

	it('a generic type default is not mistaken for the initialiser', () => {
		const code = '\tlet callback: <T = string>(value: T) => T = $state((value) => value);';
		expect(stateDeclarations(code)).toEqual(['callback']);
	});

	it('automatic semicolon insertion still ends the statement (the wrong-name bug, one costume over)', () => {
		const code = ['\tlet timer: number | undefined', '\tlet hidden = $state(1);'].join('\n');
		expect(stateDeclarations(code)).toEqual(['hidden']);
		const asiInit = ['\tlet a = 1', '\tlet b = $state(2)', '\tconst c = 3'].join('\n');
		expect(stateDeclarations(asiInit)).toEqual(['b']);
		// But a newline INSIDE an initialiser that has not started yet is not a boundary.
		expect(stateDeclarations('\tlet d =\n\t\t$state(4);')).toEqual(['d']);
	});

	// Round 2's three shapes.
	it('a type-argument list on the initialiser does not split the bindings', () => {
		expect(stateDeclarations('\tlet data = $state<Record<string, number>>({}), hidden = $state(1);')).toEqual(['data', 'hidden']);
		expect(stateDeclarations('\tlet a = 1 < 2, b = $state(1);')).toEqual(['b']);
	});

	it('a parenthesised initialiser is still that initialiser', () => {
		expect(stateDeclarations('\tlet data = ($state(1));\n\tlet raw = ($state.raw([]));')).toEqual(['data', 'raw']);
	});

	it('a `let` inside a template literal is not a declaration', () => {
		const code = ['\tconst tpl = `x', '\tlet fake = $state(1);', '\t`;', '\tlet real = $state(2);'].join('\n');
		expect(stateDeclarations(code)).toEqual(['real']);
		const withInterp = ['\tconst tpl = `${ `inner` }', '\tlet fake = $state(1);`;', '\tlet real = $state(2);'].join('\n');
		expect(stateDeclarations(withInterp)).toEqual(['real']);
		expect(stateDeclarations("\tconst s = 'no\\nlet fake = $state(1);';\n\tlet real = $state(2);")).toEqual(['real']);
	});

	// Round 3's two shapes.
	it('an unspaced comparison is not a type-argument list', () => {
		expect(stateDeclarations('\tlet a = 1<2;\n\tlet hidden = $state(1);')).toEqual(['hidden']);
		expect(stateDeclarations('\tlet a = x<y, b = $state(1);')).toEqual(['b']);
		expect(stateDeclarations('\tlet a = $state<Map<string, number>>(new Map()), b = $state(1);')).toEqual(['a', 'b']);
	});

	it("a let nested in an initialiser's function body is still enumerated, as the regex did", () => {
		const code = ['\tlet make = () => {', '\t\tlet hidden = $state(1);', '\t\treturn hidden;', '\t};', '\tlet top = $state(2);'].join('\n');
		expect(stateDeclarations(code)).toEqual(['hidden', 'top']);
		expect(stateDeclarations('\tlet x = foo(() => { let inner = $state(0); return inner; });')).toEqual(['inner']);
	});

	// Round 4's three shapes.
	it('a template interpolation body is code and is walked', () => {
		const code = ['\tconst tpl = `${(() => {', '\t\tlet hidden = $state(1);', '\t\treturn hidden;', '\t})()}`;', '\tlet top = $state(2);'].join('\n');
		expect(stateDeclarations(code)).toEqual(['hidden', 'top']);
	});

	it("an enclosing block's closer ends an unsemicolonised binding", () => {
		expect(stateDeclarations('\tfunction f() { let ordinary = 0 } let hidden = $state(1);')).toEqual(['hidden']);
		expect(stateDeclarations('\tfoo(() => { let inner = $state(0) }); let after = $state(1);')).toEqual(['inner', 'after']);
	});

	it('nested type arguments keep their depth', () => {
		expect(stateDeclarations('\tlet data = $state<Map<Map<string, number>, 1 | 2>>(new Map());')).toEqual(['data']);
		expect(stateDeclarations('\tlet data = $state<Map<Map<string, number>, 1 | 2>>(new Map()), b = $state(0);')).toEqual(['data', 'b']);
	});

	// Round 5's three shapes.
	it('whitespace and newlines inside and after a type-argument list are fine', () => {
		expect(stateDeclarations('\tlet data = $state<Map<string, 1 | 2>> (new Map());')).toEqual(['data']);
		expect(stateDeclarations('\tlet data = $state<\n\t\tMap<string, number>\n\t>(new Map()), b = $state(0);')).toEqual(['data', 'b']);
	});

	it('an arrow returning a regex literal is a regex, not code', () => {
		expect(stateDeclarations("\tconst re = () => /{let fake = $state(0);}/;\n\tlet real = $state(1);")).toEqual(['real']);
		expect(stateDeclarations("\tconst re = () => /'/;\n\tlet real = $state(1);")).toEqual(['real']);
	});

	it('a type member named `let` inside an annotation is not a declaration', () => {
		expect(stateDeclarations('\tlet ordinary: { let (): number }; let hidden = $state(0);')).toEqual(['hidden']);
	});

	it('walks a destructuring let and refuses one backed by $state', () => {
		expect(stateDeclarations('\tlet { a, b } = props;\n\tlet c = $state(1);')).toEqual(['c']);
		expect(() => stateDeclarations('\tlet [x] = $state([1]);')).toThrow(/destructuring/);
	});

	it('is exposed on FenceSource and reads the script block', () => {
		const dir = mkdtempSync(join(tmpdir(), 'fence-'));
		const file = join(dir, 'page.svelte');
		writeFileSync(file, '<script lang="ts">\n\tlet a: T | undefined;\n\tlet b = $state(0);\n</script>\n<p>{b}</p>\n');
		expect(readFenceSource(pathToFileURL(file)).stateDeclarations()).toEqual(['b']);
	});
});
