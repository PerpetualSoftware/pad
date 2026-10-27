import { describe, it, expect } from 'vitest';
import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { findCodeFromString, walk } from './pdfjsBuildOutput';
import { PDFJS_FALLBACK_DIR, PDFJS_FALLBACK_FILES } from './pdfjsAssets';

// TASK-4075 U8 — the invariants on the EMITTED bundle, which no source-level test
// can see: the chunks that actually ship the pdf.js main thread and worker.
//
// This replaces the ruling's "pin isEvalSupported:false": pdfjs-dist 6.4.299 has
// no such option (grep finds nothing in build/*.mjs), so pinning it could not go
// red. The CVE-2024-4367 class is closed structurally instead: PostScript
// functions compile to generated wasm (only under `useWasm`, which the viewer
// turns off) or run in an IR interpreter, never through `new Function`. This test
// is what keeps that true across a dependency bump: any `eval(` / `Function(`
// call site appearing in what ships fails it.
//
// IT READS THE BUILD, so it needs one: the web job runs `npm run build` before
// `npm run test`. A missing build FAILS rather than skips — a test that skips
// when its subject is absent cannot go red.

const BUILD = resolve(fileURLToPath(new URL('../../..', import.meta.url)), 'build');

// The build is walked once and each file read once, shared by every test below.
// The @napi-rs scan used to re-walk the tree and re-decode every .js/.mjs as UTF-8
// per test; that took 4060 ms against the 5000 ms default and timed out once in a
// full run (TASK-4075 r2, S1).
let walked: string[] | undefined;
const bytes = new Map<string, Buffer>();
const texts = new Map<string, string>();

function allFiles(): string[] {
	return (walked ??= walk(BUILD));
}

function buf(rel: string): Buffer {
	let b = bytes.get(rel);
	if (!b) bytes.set(rel, (b = readFileSync(resolve(BUILD, rel))));
	return b;
}

function text(rel: string): string {
	let t = texts.get(rel);
	if (t === undefined) texts.set(rel, (t = buf(rel).toString('utf8')));
	return t;
}

function emitted() {
	if (!existsSync(BUILD)) {
		throw new Error(`web/build is missing. Run \`npm run build\` in web/ first: this test reads the emitted bundle (${BUILD}).`);
	}
	const files = allFiles().filter((f) => /\.(m?js)$/.test(f));
	const main = files.filter((f) => f.startsWith('_app/immutable/') && buf(f).includes('workerSrc') && buf(f).includes('getDocument'));
	const worker = files.filter((f) => /(^|\/)pdf\.worker\.min[^/]*\.mjs$/.test(f));
	const fallbacks = PDFJS_FALLBACK_FILES.map((n) => `${PDFJS_FALLBACK_DIR.replace(/^\//, '')}${n}`);
	return { files, main, worker, fallbacks };
}

describe('the emitted pdf.js bundle (U8)', () => {
	it('contains the pdf.js main chunk, the worker and exactly the two fallbacks (so the scans below are not vacuous)', () => {
		const { main, worker, fallbacks } = emitted();
		expect(main.length, `main pdf.js chunk(s): ${main}`).toBeGreaterThanOrEqual(1);
		expect(worker, `worker asset(s): ${worker}`).toHaveLength(1);
		for (const f of fallbacks) expect(existsSync(resolve(BUILD, f)), f).toBe(true);
		expect(fallbacks).toEqual(['_app/pdfjs/openjpeg_nowasm_fallback.js', '_app/pdfjs/jbig2_nowasm_fallback.js']);
	});

	it('has no eval( / Function( call site in the main chunk, the worker or either fallback', () => {
		const { main, worker, fallbacks } = emitted();
		const hits = [...main, ...worker, ...fallbacks].flatMap((f) => findCodeFromString(f, text(f)));
		expect(hits.map((h) => `${h.file}@${h.index}: ${h.excerpt}`)).toEqual([]);
	});

	it('ships no wasm, qcms or quickjs under the pdf.js directory, and no wasm anywhere', () => {
		const { files } = emitted();
		const all = allFiles();
		expect(all.filter((f) => f.endsWith('.wasm')), 'wasm files in the build').toEqual([]);
		expect(all.filter((f) => f.startsWith('_app/pdfjs/')).sort()).toEqual([...PDFJS_FALLBACK_FILES].map((n) => `_app/pdfjs/${n}`).sort());
		expect(files.filter((f) => /quickjs|qcms|sandbox/i.test(f))).toEqual([]);
	});

	// Measured at this head: 105 ms alone (was 4060 ms before the rewrite, in a full
	// run). The explicit timeout is headroom for a cold disk cache or a loaded box, not
	// a licence to grow: if this test nears it, the scan has got slower.
	it('bundles no @napi-rs module: no native binary, no package path, and the lone reference is the inert Node-only require', { timeout: 20_000 }, () => {
		// Paths first: they need no file read at all.
		const all = allFiles();
		expect(all.filter((f) => f.endsWith('.node') || f.includes('@napi-rs'))).toEqual([]);
		// Then the contents of each .js/.mjs, read once (and shared with the tests above); only a
		// file whose bytes contain the needle is decoded and matched.
		const needle = Buffer.from('@napi-rs');
		const refs = all
			.filter((f) => /\.(m?js)$/.test(f) && buf(f).includes(needle))
			.flatMap((f) => [...text(f).matchAll(/.{0,60}@napi-rs.{0,40}/g)].map((m) => ({ f, text: m[0] })));
		// pdf.js reaches it only as `process.getBuiltinModule("module").createRequire(import.meta.url)("@napi-rs/canvas")`
		// inside its Node canvas factory: a run-time require, not an import the bundler follows.
		for (const r of refs) expect(r.text, r.f).toMatch(/createRequire\(import\.meta\.url\)\(`?["']?@napi-rs\/canvas/);
		expect(refs.length).toBeLessThanOrEqual(1);
	});
});

describe('the code-from-string scanner (can it fail?)', () => {
	it.each([
		['eval(', 'x=eval("1")'],
		['new Function(', 'return new Function("a","return a")'],
		['bare Function(', 'const f=Function("return 1")'],
		['member Function(', 'globalThis.Function("return 1")()'],
		['indirect eval', '(0, eval)("1")'],
		['spaced', 'eval  ("1")']
	])('flags %s', (_n, src) => {
		expect(findCodeFromString('x.js', src).length).toBeGreaterThan(0);
	});

	it.each([
		['an identifier ending in Function', 'buildPostScriptFunction(a)'],
		['retrieval', 'retrieval(x)'],
		['a property named eval', 'o.evalCount=1'],
		['plain text', 'const a=1;function f(){}']
	])('does not flag %s', (_n, src) => {
		expect(findCodeFromString('x.js', src)).toEqual([]);
	});
});
