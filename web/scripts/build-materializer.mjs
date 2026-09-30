// Builds the headless materializer bundle (TASK-2198):
//   src/lib/collab/materializer/entry.ts -> build-materializer/materializer.js
//
// ONE self-contained IIFE that assigns globalThis.Materializer. The Go binary
// embeds it (embed.go, `//go:embed web/build-materializer/materializer.js`)
// and runs it in goja (internal/materialize), so a fresh checkout needs this
// file before `go build` works, exactly like web/build. `npm run build` runs
// it after `vite build`.
//
// Usage: node scripts/build-materializer.mjs [--outfile <path>] [--define KEY=VALUE]...
//   --outfile  write somewhere else (tests build deliberately broken variants
//              this way, never over the real output)
//   --define   extra esbuild defines, same syntax as esbuild's
import { build } from 'esbuild';
import path from 'node:path';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

let outfile = path.join(web, 'build-materializer', 'materializer.js');
const extraDefines = {};
const argv = process.argv.slice(2);
for (let i = 0; i < argv.length; i++) {
	const a = argv[i];
	if (a === '--outfile') outfile = path.resolve(argv[++i]);
	else if (a === '--define') {
		const kv = argv[++i];
		const eq = kv.indexOf('=');
		extraDefines[kv.slice(0, eq)] = kv.slice(eq + 1);
	} else {
		console.error(`build-materializer: unknown argument ${a}`);
		process.exit(2);
	}
}

await build({
	entryPoints: [path.join(web, 'src/lib/collab/materializer/entry.ts')],
	bundle: true,
	format: 'iife',
	// Neutral, not browser: nothing here may assume a window. The DOM the
	// editor needs comes from domShim.ts (linkedom).
	platform: 'neutral',
	mainFields: ['module', 'main'],
	conditions: ['import', 'default'],
	// goja implements ES2022 minus a few corners; esbuild lowers anything newer.
	target: 'es2022',
	minify: true,
	legalComments: 'none',
	outfile,
	alias: { $lib: path.join(web, 'src/lib') },
	loader: { '.json': 'json', '.svg': 'text' },
	define: {
		'process.env.NODE_ENV': '"production"',
		'import.meta.env.DEV': 'false',
		'import.meta.env.PROD': 'true',
		'import.meta.env.SSR': 'false',
		...extraDefines,
	},
	logLevel: 'warning',
});

const bytes = fs.statSync(outfile).size;
console.log(`build-materializer: wrote ${path.relative(process.cwd(), outfile)} (${bytes} bytes)`);
