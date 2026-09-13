import { defineConfig } from 'vitest/config';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { realpathSync } from 'node:fs';
import {
	BROWSER_TEST_GLOB,
	IDB_TEST_GLOB,
	NODE_TEST_GLOB,
	PROJECT_REQUIREMENTS,
	createProjectCountReporter,
	findUnsatisfiedProjects,
	formatUnsatisfiedProjectsError,
} from './src/test/vitestProjects.ts';

// Multi-project vitest setup (TASK-2081 / PLAN-1984, plus `idb` from
// PLAN-2636 unit 1):
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
// The non-`node` projects need devDependencies (`jsdom`,
// `@testing-library/svelte`, `@testing-library/jest-dom`,
// `@sveltejs/vite-plugin-svelte`, `fake-indexeddb`). They used to be probed with
// `require.resolve` and the project SKIPPED when they did not resolve, which made
// an incomplete install narrow the run instead of failing it — `npm test` exited
// 0 having executed none of the `*.svelte.test.ts` suites. Since BUG-3045 an
// unresolvable dependency REFUSES the run instead; see
// `src/test/vitestProjects.ts` for the reasoning and the messages.

const require = createRequire(import.meta.url);
function canResolve(id: string): boolean {
	try {
		require.resolve(id);
		return true;
	} catch {
		return false;
	}
}

const projectRoot = fileURLToPath(new URL('.', import.meta.url));
const $lib = fileURLToPath(new URL('./src/lib', import.meta.url));
const appEnvironmentMock = fileURLToPath(new URL('./src/test/mocks/app-environment.ts', import.meta.url));
const appStateMock = fileURLToPath(new URL('./src/test/mocks/app-state.ts', import.meta.url));
const appNavigationMock = fileURLToPath(new URL('./src/test/mocks/app-navigation.ts', import.meta.url));

// Agent worktrees symlink `web/node_modules` to the main checkout's
// node_modules rather than `npm ci`-ing a copy (running npm ci THROUGH the
// symlink deletes the shared tree — see CLAUDE.md's "Working in a git
// worktree" section, which also covers the svelte-kit sync prerequisite).
// Vite's dev-server fs-access guard checks a
// requested file's REALPATH against `server.fs.allow`, which defaults to the
// project root and its ancestors — a node_modules symlink that resolves
// outside that root (the worktree lives under a sibling directory tree) gets
// denied, breaking every jsdom-project test that imports a real package
// (e.g. `@testing-library/svelte/vitest`) with a confusing "does the file
// exist?" error even though it does. Explicitly allowing the resolved
// realpath fixes worktrees without changing behavior for a normal checkout,
// where the realpath is just `<project>/node_modules` — already inside the
// default allow-list. `projectRoot` MUST stay in the allow list alongside
// it — setting `server.fs.allow` REPLACES Vite's default (project root +
// ancestors), so omitting the root here would deny access to ordinary
// project source files (Codex review).
const nodeModulesRealPath = (() => {
	try {
		return realpathSync(fileURLToPath(new URL('./node_modules', import.meta.url)));
	} catch {
		return undefined;
	}
})();

// Admission check for every project that has dependencies of its own — the
// `jsdom` project (TASK-2081) and the `idb` project (PLAN-2636 unit 1). Both
// used to self-disable when their deps did not resolve; both now refuse, at
// config-load time, before a single file is collected (BUG-3045).
//
// Module scope, so the refusal lands while the config is still being LOADED —
// before any project is constructed and before a single test file is collected.
// Measured: vitest reports it as a startup error ("failed to load config") and
// exits 1; it does not fall back to a default config (BUG-3045 trail).
const unsatisfiedProjects = findUnsatisfiedProjects(PROJECT_REQUIREMENTS, canResolve);
if (unsatisfiedProjects.length > 0) {
	throw new Error(formatUnsatisfiedProjectsError(unsatisfiedProjects));
}

// The three globs are declared in src/test/vitestProjects.ts alongside
// `projectForTestFile`, so the config and the guard test that checks no test
// file falls through every project read one declaration rather than two copies
// (BUG-3045, codex round 1).
//
// IDB-backed persistence tests carry the `.idb.test.ts` suffix and must be
// excluded from the node project (which has no indexedDB — the persistence layer
// would silently no-op there and the test would pass vacuously) the same way the
// svelte glob is.

const nodeProject = {
	resolve: { alias: { $lib } },
	test: {
		name: 'node',
		environment: 'node',
		include: [NODE_TEST_GLOB],
		// The jsdom / idb projects own these; they'd blow up or no-op in the
		// plain node env.
		exclude: [BROWSER_TEST_GLOB, IDB_TEST_GLOB],
	},
};

const idbProject = {
	resolve: { alias: { $lib } },
	server: nodeModulesRealPath
		? { fs: { allow: [projectRoot, nodeModulesRealPath] } }
		: undefined,
	test: {
		name: 'idb',
		// fake-indexeddb runs in plain node — no DOM needed. Its setup installs
		// a fresh in-memory IndexedDB per test.
		environment: 'node',
		include: [IDB_TEST_GLOB],
		setupFiles: ['./src/test/setup-idb.ts'],
	},
};

export default defineConfig(async () => {
	// Dynamic import because these are heavy and only the jsdom project uses them.
	// They are guaranteed resolvable: the admission check above refused the run
	// otherwise.
	const { svelte } = await import('@sveltejs/vite-plugin-svelte');
	const { svelteTesting } = await import('@testing-library/svelte/vite');

	// Every project is registered unconditionally (BUG-3045). A project that
	// cannot run does not get dropped from this list — it stops the run.
	const projects: Record<string, unknown>[] = [
		nodeProject,
		idbProject,
		{
			plugins: [svelte(), svelteTesting()],
			resolve: {
				alias: {
					$lib,
					// No SvelteKit plugin in this project, so provide `$app/environment`
					// and `$app/state` — without a provider these don't just come back
					// undefined, they fail to RESOLVE, which is a load-time error for
					// any component that imports them (and one `vi.mock` can't rescue,
					// since resolution happens first).
					'$app/environment': appEnvironmentMock,
					'$app/state': appStateMock,
					// `$app/navigation` for the same reason (TASK-2430) — without it
					// Sidebar / TopBar / PaneHost can't even be IMPORTED under jsdom.
					'$app/navigation': appNavigationMock,
				},
			},
			server: nodeModulesRealPath
				? { fs: { allow: [projectRoot, nodeModulesRealPath] } }
				: undefined,
			test: {
				name: 'jsdom',
				environment: 'jsdom',
				include: [BROWSER_TEST_GLOB],
				setupFiles: ['./src/test/setup-jsdom.ts'],
			},
		},
	];

	return {
		test: {
			projects,
			// `default` is vitest's own reporter, and naming any reporter REPLACES
			// the default list rather than adding to it, so it has to be restated
			// here to keep the ordinary output. Measured, not assumed: with
			// `default` dropped, a run prints this line and nothing else. The added one prints how many projects actually ran files, so
			// a narrowed run (`--project`, or a file filter) says so in its own
			// output rather than looking identical to a full one (BUG-3045).
			reporters: ['default', createProjectCountReporter()],
		},
	};
});
