import { defineConfig } from 'vitest/config';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';

// Two-project vitest setup (TASK-2081 / PLAN-1984):
//
//  - `node`  — the existing pure-TS unit suite. Plain node environment, no
//              Svelte plugin (fast; matches the pre-TASK-2081 behavior).
//  - `jsdom` — `.svelte` component + `.svelte.ts` rune-module tests. Runs in a
//              browser-like DOM with the Svelte plugin so runes/components
//              compile, and aliases `$app/environment` to a browser=true mock.
//
// Split by filename: `*.svelte.test.ts` routes to jsdom, everything else
// (`*.test.ts`) stays on node. Keeping the node suite out of jsdom avoids
// slowing/altering the pure-logic tests.
//
// The jsdom project's deps (`jsdom`, `@testing-library/svelte`,
// `@testing-library/jest-dom`) are declared in package.json but may be absent
// until `npm install` runs (worktrees share a read-only node_modules). When
// they're missing we register ONLY the node project, so `npm run test` keeps
// the existing suite green; once installed, the jsdom project activates
// automatically and `npm run test` runs BOTH.

const require = createRequire(import.meta.url);
function canResolve(id: string): boolean {
	try {
		require.resolve(id);
		return true;
	} catch {
		return false;
	}
}

const $lib = fileURLToPath(new URL('./src/lib', import.meta.url));
const appEnvironmentMock = fileURLToPath(new URL('./src/test/mocks/app-environment.ts', import.meta.url));

const browserTestDepsInstalled =
	canResolve('jsdom') &&
	canResolve('@testing-library/svelte') &&
	canResolve('@testing-library/jest-dom') &&
	canResolve('@sveltejs/vite-plugin-svelte');

const BROWSER_TEST_GLOB = 'src/**/*.svelte.test.ts';

const nodeProject = {
	resolve: { alias: { $lib } },
	test: {
		name: 'node',
		environment: 'node',
		include: ['src/**/*.test.ts'],
		// The jsdom project owns these; they'd blow up in the plain node env.
		exclude: [BROWSER_TEST_GLOB],
	},
};

export default defineConfig(async () => {
	const projects: Record<string, unknown>[] = [nodeProject];

	if (browserTestDepsInstalled) {
		// Dynamic import so a missing plugin can never crash config loading.
		const { svelte } = await import('@sveltejs/vite-plugin-svelte');
		const { svelteTesting } = await import('@testing-library/svelte/vite');
		projects.push({
			plugins: [svelte(), svelteTesting()],
			resolve: {
				alias: {
					$lib,
					// No SvelteKit plugin in this project, so provide `$app/environment`.
					'$app/environment': appEnvironmentMock,
				},
			},
			test: {
				name: 'jsdom',
				environment: 'jsdom',
				include: [BROWSER_TEST_GLOB],
				setupFiles: ['./src/test/setup-jsdom.ts'],
			},
		});
	}

	return { test: { projects } };
});
