/**
 * Where the PDF viewer's two no-wasm decoder fallbacks are served from
 * (TASK-4075).
 *
 * pdf.js decodes JPEG2000 (JPX) and JBIG2 images with wasm by default. The
 * viewer runs it with `useWasm: false`, because the CSP carries no
 * `'wasm-unsafe-eval'` and none is to be added: with `useWasm: false` the worker
 * `import()`s `${wasmUrl}openjpeg_nowasm_fallback.js` / `jbig2_nowasm_fallback.js`
 * instead. Those two files are copied from `node_modules/pdfjs-dist/wasm/` into
 * the build output by the `pdfjs-fallbacks` plugin in vite.config.ts (a copy
 * step, not a vendored copy), at this FIXED, unhashed path: the worker builds
 * the URL by string concatenation, so a bundler cannot rewrite it.
 *
 * ONLY these two files. The `.wasm` files, `qcms_bg.wasm` (ICC colour
 * management; no JS fallback exists, so ICC-profiled images render without
 * colour management) and `quickjs-eval` (PDF scripting) are deliberately not
 * shipped.
 *
 * ABSOLUTE ON PURPOSE. A dynamic `import()` inside a worker resolves a relative
 * specifier against the WORKER script's URL, so a relative directory would point
 * into `_app/immutable/assets/`. The directory is therefore joined to the page's
 * origin at the call site.
 */
export const PDFJS_FALLBACK_DIR = '/_app/pdfjs/';

export const PDFJS_FALLBACK_FILES: readonly string[] = [
	'openjpeg_nowasm_fallback.js',
	'jbig2_nowasm_fallback.js'
];
