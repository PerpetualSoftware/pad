import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, type Plugin } from 'vite';
import { execFileSync } from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { BUILD_SOURCE_FILE, buildSourceStamp } from './src/lib/build/buildSourceStamp';

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
	plugins: [buildSource(), sveltekit()],
	server: {
		proxy: {
			'/api': {
				target: 'http://127.0.0.1:7777',
				changeOrigin: true
			}
		}
	}
});
