/**
 * Scanners for the EMITTED pdf.js bundle (TASK-4075, U8). Plain functions over
 * file text so the test can aim them at the real build output AND at synthetic
 * input that must trip them: a scanner that has never seen a hit is not known
 * to be able to find one.
 */
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

/**
 * A TRIPWIRE for direct call sites that turn a string into code: `eval(`,
 * `eval?.(`, `new Function(`, `Function(`, `Function?.(`, tagged-template
 * Function`...` and a bare `(0, eval)(`. It is NOT a proof that the bundle has no
 * dynamic code. The wall is the CSP: `script-src 'self'` with no 'unsafe-eval',
 * which makes `new Function` throw EvalError (measured in the pdf.js worker, the
 * P2 probe). Disclosed gap: `.constructor("...")` is not scanned, because a regex
 * for it would match ordinary code.
 *
 * The lookbehind keeps identifiers that merely END in these words
 * (`buildPostScriptFunction(`, `retrieval(`) out, and a leading `.` is NOT
 * excluded: `globalThis.Function("...")` is a real call site. pdf.js 6.4.299 has
 * none (CVE-2024-4367 was this class), so any hit in the emitted chunks is an
 * addition worth a human's eyes.
 */
const CODE_FROM_STRING =
	/(?<![\w$])(?:eval|Function)\s*(?:\?\.\s*)?\(|(?<![\w$])Function\s*`|\(\s*0\s*,\s*eval\s*\)\s*\(/g;

export interface Hit {
	file: string;
	index: number;
	excerpt: string;
}

export function findCodeFromString(file: string, text: string): Hit[] {
	const hits: Hit[] = [];
	for (const m of text.matchAll(CODE_FROM_STRING)) {
		const at = m.index ?? 0;
		hits.push({ file, index: at, excerpt: text.slice(Math.max(0, at - 30), at + 50) });
	}
	return hits;
}

/** Every file under `dir`, recursively, with a path relative to `dir`. */
export function walk(dir: string, rel = ''): string[] {
	const out: string[] = [];
	for (const name of readdirSync(join(dir, rel))) {
		const p = join(rel, name);
		if (statSync(join(dir, p)).isDirectory()) out.push(...walk(dir, p));
		else out.push(p);
	}
	return out;
}

export function read(dir: string, rel: string): string {
	return readFileSync(join(dir, rel), 'utf8');
}
