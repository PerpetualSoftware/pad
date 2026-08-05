import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * WIRING CONTRACT for the two route ESC guards (TASK-2429).
 *
 * The viewer's Escape has exactly one owner — `escapeStack` — and the only code
 * that runs it is the keydown handler in each of these two route files. Both
 * used to bail on an inline
 * `document.querySelector('dialog[open], [role="dialog"]:not(.item-pane)')`,
 * which the body-portaled viewer now MATCHES; without the shared
 * `hasForeignEscapeOwner()` (which excludes it) the guard returns early, the
 * stack never runs, and Escape closes nothing at all. That is a silent,
 * app-wide dead key.
 *
 * WHY A SOURCE ASSERTION. The unit tests around `Lightbox` drive a
 * ROUTE-SHAPED driver they define themselves, so they prove the shape works —
 * not that either route still calls it. Mounting a SvelteKit route under vitest
 * to prove the real thing costs far more than it is worth (a 2,900-line
 * component, its stores, its data loaders), while the actual regression risk is
 * mundane: someone deletes or reverts the call, or re-inlines the old selector
 * during a merge. A grep-shaped contract catches exactly that, and nothing else
 * pretends to be covered here.
 *
 * TWO THINGS KEEP IT FROM BEING A GREP THAT LIES:
 *   • COMMENTS ARE STRIPPED FIRST. Every one of these strings appears in prose
 *     in these files (this commit added several), so a whole-file search could
 *     be satisfied by a comment while the handler ran unguarded.
 *   • ASSERTIONS ARE SCOPED TO THE HANDLER that actually calls `runTopEscape`,
 *     not to the file. A guard in some unrelated helper is not this contract.
 *
 * The BEHAVIOURAL proof — a real Escape press, in a real browser, closing
 * exactly the viewer and not the pane beneath it — belongs to TASK-2436's
 * Playwright suite (DR-9), together with the inertness and stacking guarantees
 * jsdom cannot see either.
 */

const ROUTES = [
	'../../routes/[username]/[workspace]/[collection]/+page.svelte',
	'../../routes/[username]/[workspace]/[collection]/[slug]/+page.svelte',
] as const;

/** The guard call, tolerant of formatting (prettier reflow, added braces). */
const GUARD = /if\s*\(\s*hasForeignEscapeOwner\(\)\s*\)\s*\{?\s*return\s*;/;
/** Any `querySelector` whose selector string reaches for a `role="dialog"`. */
const RAW_DIALOG_QUERY = /querySelector\w*\(\s*(['"`])[^'"`]*\[\s*role\s*=\s*\\?["']?dialog/;

/**
 * Source with comments removed. Line comments are matched only when the `//`
 * is not preceded by `:`, so URLs (`https://…`) survive — the point is to drop
 * PROSE, not to be a parser.
 */
function stripComments(source: string): string {
	return source
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.replace(/<!--[\s\S]*?-->/g, '')
		.replace(/(^|[^:])\/\/[^\n]*/g, '$1');
}

/**
 * The body of the function that calls `runTopEscape()`, from its `function`
 * keyword up to that call — i.e. exactly the region the guard must sit in.
 * Returns null when there is no such call at all, which is itself a failure.
 */
function escapeHandlerPrologue(code: string): string | null {
	const call = code.search(/runTopEscape\s*\(/);
	if (call === -1) return null;
	const start = code.lastIndexOf('function ', call);
	if (start === -1) return null;
	return code.slice(start, call);
}

describe.each(ROUTES)('ESC guard wiring in %s', (relative) => {
	const source = readFileSync(fileURLToPath(new URL(relative, import.meta.url)), 'utf8');
	const code = stripComments(source);

	it('imports the shared guard from the a11y module', () => {
		expect(code).toMatch(
			/import\s*\{[^}]*\bhasForeignEscapeOwner\b[^}]*\}\s*from\s*'\$lib\/a11y\/viewerBackdrop'/
		);
	});

	it('calls it as an early return INSIDE the handler that runs the stack', () => {
		// Scoped, not file-wide: this is the assertion that would otherwise be
		// satisfiable by prose or by an unrelated helper. `return` (not
		// `preventDefault`) is the point — a foreign modal owns the key outright,
		// so the stack must not run at all.
		const prologue = escapeHandlerPrologue(code);
		expect(prologue).not.toBeNull();
		expect(prologue!).toMatch(GUARD);
	});

	it('does not reach the stack without passing the guard first', () => {
		// The ordering IS the contract: running the stack first would close the
		// viewer out from under a modal that owns the key, and guarding after the
		// fact would do nothing. Falls out of the scoping above — the prologue
		// ends AT the call — so this re-states it against the whole file, where a
		// second, unguarded `runTopEscape` would also show up.
		const occurrences = code.match(/runTopEscape\s*\(/g) ?? [];
		expect(occurrences).toHaveLength(1);
		const guardAt = code.search(GUARD);
		const stackAt = code.search(/runTopEscape\s*\(/);
		expect(guardAt).toBeGreaterThan(-1);
		expect(guardAt).toBeLessThan(stackAt);
	});

	it('no longer bails on a raw role="dialog" query', () => {
		// The pre-TASK-2429 inline check, re-introduced by hand or by a bad merge
		// resolution, is the regression this file exists to catch: it matches the
		// viewer's own `role="dialog"` root and kills its Escape. Matched by shape
		// rather than by one exact quoting, so a reformatted revert still trips it.
		expect(code).not.toMatch(RAW_DIALOG_QUERY);
	});
});
