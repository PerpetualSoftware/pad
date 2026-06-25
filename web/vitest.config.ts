import { defineConfig } from 'vitest/config';
import { fileURLToPath } from 'node:url';

// Standalone vitest config (kept separate from vite.config.ts so the
// SvelteKit plugin doesn't run during unit tests — the WebMCP unit suite is
// plain TS, no Svelte components). `$lib` is aliased so any future test that
// imports a `$lib/...` module resolves; the current WebMCP modules import
// `$lib/...` only in type positions (erased at runtime).
export default defineConfig({
	resolve: {
		alias: {
			$lib: fileURLToPath(new URL('./src/lib', import.meta.url)),
		},
	},
	test: {
		include: ['src/**/*.test.ts'],
		environment: 'node',
	},
});
