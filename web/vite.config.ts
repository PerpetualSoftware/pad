import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, type Plugin } from 'vite';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { PDFJS_FALLBACK_DIR, PDFJS_FALLBACK_FILES } from './src/lib/attachments/pdfjsAssets.ts';
import { BUILD_SOURCE_FILE, buildSourceStamp } from './src/lib/build/buildSourceStamp';
import { dynamicCompileOptions } from './svelteCompileOptions.ts';

const WEB_DIR = fileURLToPath(new URL('.', import.meta.url));

// Stamp the bundle with where it was built from (TASK-3233); see
// buildSourceStamp.ts. Build only: the dev server never needs it.
function buildSource(): Plugin {
	return {
		name: 'pad-build-source',
		apply: 'build',
		buildStart() {
			const stamp = buildSourceStamp((args) =>
				execFileSync('git', args, { cwd: WEB_DIR, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] })
			);
			writeFileSync(new URL(`./static/${BUILD_SOURCE_FILE}`, import.meta.url), JSON.stringify(stamp) + '\n');
		}
	};
}

// The PDF viewer's JPX/JBIG2 decoders without wasm (TASK-4075). pdf.js builds
// their URL by string concatenation at run time, so a bundler cannot find them:
// copy exactly the two no-wasm fallback files out of node_modules into the build
// output at the fixed path src/lib/attachments/pdfjsAssets.ts names. A copy
// step, not a vendored copy, so the files always match the pinned pdfjs-dist.
// The .wasm files, qcms and quickjs are NOT shipped. The dev server serves the
// same two paths so a PDF with such an image renders under `npm run dev` too.
function pdfjsFallbacks(): Plugin {
	const source = (name: string) =>
		new URL(`./node_modules/pdfjs-dist/wasm/${name}`, import.meta.url);
	return {
		name: 'pad-pdfjs-fallbacks',
		generateBundle() {
			for (const name of PDFJS_FALLBACK_FILES) {
				this.emitFile({
					type: 'asset',
					// fileName (not name): an emitted `name` would be content-hashed.
					fileName: `${PDFJS_FALLBACK_DIR.replace(/^\//, '')}${name}`,
					source: readFileSync(source(name))
				});
			}
		},
		configureServer(server) {
			server.middlewares.use((req, res, next) => {
				const url = (req.url ?? '').split('?')[0];
				const name = PDFJS_FALLBACK_FILES.find((f) => url === `${PDFJS_FALLBACK_DIR}${f}`);
				if (!name) return next();
				res.setHeader('Content-Type', 'text/javascript');
				res.end(readFileSync(source(name)));
			});
		}
	};
}

export default defineConfig({
	plugins: [
		buildSource(),
		pdfjsFallbacks(),
		// The SvelteKit and Svelte config, here rather than in svelte.config.js
		// (TASK-3423; SvelteKit 3 refuses that file). Kit's own keys (adapter)
		// stay with kit; the rest, dynamicCompileOptions, is forwarded to
		// vite-plugin-svelte.
		sveltekit({
			adapter: adapter({
				pages: 'build',
				assets: 'build',
				fallback: 'index.html'
			}),
			// SvelteKit 3 removed the built-in `$lib`; this keeps it (TASK-3423).
			alias: { $lib: 'src/lib' },
			dynamicCompileOptions
		})
	],
	server: {
		proxy: {
			'/api': {
				target: 'http://127.0.0.1:7777',
				changeOrigin: true
			}
		}
	}
});
