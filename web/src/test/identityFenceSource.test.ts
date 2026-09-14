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
import { matchBrace, matchDelimiter, readFenceSource, stripComments, withoutCatchArms } from './identityFenceSource';
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
