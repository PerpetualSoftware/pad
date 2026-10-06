import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, type Plugin } from 'vite';
import { execFileSync } from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
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

export default defineConfig({
	plugins: [
		buildSource(),
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
