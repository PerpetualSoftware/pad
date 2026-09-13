// Project admission + project-count reporting for `web/vitest.config.ts` (BUG-3045).
//
// The web suite is several vitest PROJECTS (`node`, `idb`, `jsdom`). Each of the
// non-`node` ones needs devDependencies that the config used to probe with
// `require.resolve`, registering the project only when they resolved. A missing
// dependency therefore NARROWED the run instead of failing it: `npm test` exited
// 0 having executed none of the `*.svelte.test.ts` suites, and nothing in the
// output said so. That is a gate that passes when it cannot run — the same shape
// `scripts/ci-audit.mjs` was taught to refuse in BUG-2881, where an unreachable
// advisory service used to be indistinguishable from a clean audit.
//
// So the rule here is REFUSE, not narrow: an unresolvable dependency is a
// configuration error and the run stops. Every one of these packages is a
// declared devDependency, so the only way to reach the refusal is an incomplete
// install — which is exactly the state a green run must not be reported from.
//
// The refusal is UNCONDITIONAL and happens at config load, so it precedes
// vitest's own `--project` filter: `vitest --project node` is refused too, even
// though the node project would have been runnable. That is deliberate but it is
// a real cost, so state it exactly — an incomplete install is a broken state and
// the run stops on it whatever subset was asked for. The only state the refusal
// permits is a complete install, and in THAT state deliberate narrowing is never
// refused.
//
// Narrowing a complete install is what the summary line is for: it prints how
// many of the registered projects actually ran files, so a narrowed run says so
// in its own output instead of looking identical to a full one.
//
// What the line does NOT prove, because it counts FILES:
//  - not that any assertion executed — a file of `.skip`ped tests counts as run
//    (vitest's own summary reports the skips);
//  - not that a project SHOULD have had files: an idle project is printed, not
//    failed, because a file filter legitimately idles one. `projectForTestFile`
//    below is the guard for the accidental version of that;
//  - nothing at all under `--reporter=<name>`, which REPLACES configured
//    reporters, so `npm test -- --reporter=dot` prints no summary. Unavoidable
//    from a config; the default invocations (`npm run test`, `make web-test`,
//    `make check`, CI) all carry it.

/** A test project and the packages it cannot run without. */
export interface ProjectRequirement {
	/** The project's `test.name`, as registered in vitest.config.ts. */
	project: string;
	/** Bare module ids that must resolve for the project to be registered. */
	deps: string[];
}

/**
 * Every project whose registration used to be conditional on a dependency probe.
 *
 * `node` is absent deliberately: it needs nothing beyond vitest itself, so it has
 * no admission question to answer.
 */
export const PROJECT_REQUIREMENTS: readonly ProjectRequirement[] = [
	{ project: 'idb', deps: ['fake-indexeddb'] },
	{
		project: 'jsdom',
		deps: [
			'jsdom',
			'@testing-library/svelte',
			'@testing-library/jest-dom',
			'@sveltejs/vite-plugin-svelte',
		],
	},
];

/** One project that cannot be registered, and the subset of deps to blame. */
export interface UnsatisfiedProject {
	project: string;
	missing: string[];
}

/**
 * Partitions `requirements` into the projects whose deps all resolve and those
 * that are missing at least one.
 *
 * `canResolve` is injected rather than closed over so this is testable without
 * uninstalling anything.
 */
export function findUnsatisfiedProjects(
	requirements: readonly ProjectRequirement[],
	canResolve: (id: string) => boolean,
): UnsatisfiedProject[] {
	const unsatisfied: UnsatisfiedProject[] = [];
	for (const requirement of requirements) {
		const missing = requirement.deps.filter((id) => !canResolve(id));
		if (missing.length > 0) {
			unsatisfied.push({ project: requirement.project, missing });
		}
	}
	return unsatisfied;
}

/**
 * The refusal message. Names every unrunnable project and every missing package
 * — not just the first — so one `npm ci` fixes the whole set rather than
 * uncovering the next name on the next run.
 */
export function formatUnsatisfiedProjectsError(unsatisfied: readonly UnsatisfiedProject[]): string {
	const lines = [
		`vitest refused to start: ${unsatisfied.length} test ` +
			`${unsatisfied.length === 1 ? 'project' : 'projects'} cannot run.`,
		'',
	];
	for (const { project, missing } of unsatisfied) {
		lines.push(`  ${project} — cannot resolve: ${missing.join(', ')}`);
	}
	lines.push(
		'',
		'These are declared devDependencies, so this means an incomplete install.',
		'Run `npm ci` in web/ and re-run.',
		'',
		'The project is NOT omitted on a missing dependency (BUG-3045): a run that',
		'quietly drops a project exits 0 having executed none of its suites, which',
		'is a gate that passes when it cannot run.',
	);
	return lines.join('\n');
}

/**
 * The end-of-run summary: how many of the registered projects actually executed
 * a test module.
 *
 * `registered` is the post-`--project`-filter set, so a deliberately narrowed run
 * reports the narrow number and says which projects it kept. A registered project
 * that ran NO modules is called out by name but is not an error — a file-filtered
 * run (`vitest run src/lib/foo.test.ts`) legitimately leaves the other projects
 * with nothing to do.
 */
export function formatProjectRunSummary(
	registered: readonly string[],
	moduleCountsByProject: ReadonlyMap<string, number>,
): string {
	// An empty registered set means `onInit` never delivered one. Printing
	// `0/0 ran test files` next to a full run would be a summary line that cannot
	// be right — the exact failure mode this reporter exists to make visible, so
	// it says the denominator is missing instead of inventing one from the modules
	// that happened to run.
	if (registered.length === 0) {
		const observed = [...moduleCountsByProject.keys()];
		if (observed.length === 0) {
			return 'vitest projects: no test files ran';
		}
		return (
			`vitest projects: registered set unavailable (reporter onInit did not fire) — ` +
			`ran: ${observed.join(', ')}`
		);
	}

	const ran = registered.filter((name) => (moduleCountsByProject.get(name) ?? 0) > 0);
	const idle = registered.filter((name) => (moduleCountsByProject.get(name) ?? 0) === 0);

	const detail = registered
		.map((name) => {
			const count = moduleCountsByProject.get(name) ?? 0;
			return `${name} (${count} ${count === 1 ? 'file' : 'files'})`;
		})
		.join(', ');

	let summary =
		`vitest projects: ${ran.length}/${registered.length} ran test files` +
		(detail ? ` — ${detail}` : '');
	if (idle.length > 0) {
		summary += `\nvitest projects: NO test files ran for: ${idle.join(', ')}`;
	}
	return summary;
}

// ---------------------------------------------------------------------------
// Which project owns which file (BUG-3045, codex round 1 finding 3).
//
// The refusal above covers a project that cannot RUN. This covers the other way
// the suite can be green while measuring nothing: a test file that no project's
// include glob matches at all. Nothing fails — the file is simply never
// collected, the other files pass, and the run exits 0. A `*.spec.ts`, a
// `*.test.tsx`, or a `*.test.js` would land there today.
//
// The globs live here rather than in vitest.config.ts so that the config and the
// guard test read the SAME declaration; a glob changed in one place without the
// other is the drift this would otherwise miss.

/** The node project's include. Excludes the two more specific globs below. */
export const NODE_TEST_GLOB = 'src/**/*.test.ts';
/** `.svelte.test.ts` — component + rune-module tests, jsdom project. */
export const BROWSER_TEST_GLOB = 'src/**/*.svelte.test.ts';
/** `.idb.test.ts` — fake-indexeddb-backed persistence tests, idb project. */
export const IDB_TEST_GLOB = 'src/**/*.idb.test.ts';

/**
 * Files that LOOK like tests. Deliberately wider than anything the projects run
 * — the gap between this and {@link projectForTestFile} is exactly the hole the
 * guard test closes.
 */
export const TEST_SHAPED_FILE = /\.(test|spec)\.[cm]?[jt]sx?$/;

/**
 * The project that would run `relativePath`, or `null` if no project's globs
 * match it — meaning the file exists, looks like a test, and never runs.
 *
 * Order matters and mirrors the config: the two specific suffixes are checked
 * before the general one, because `.svelte.test.ts` and `.idb.test.ts` also end
 * in `.test.ts` and the node project excludes them.
 */
export function projectForTestFile(relativePath: string): string | null {
	if (relativePath.endsWith('.svelte.test.ts')) return 'jsdom';
	if (relativePath.endsWith('.idb.test.ts')) return 'idb';
	if (relativePath.endsWith('.test.ts')) return 'node';
	return null;
}

/** The subset of vitest's reporter context this reporter reads. */
interface ProjectCountReporterContext {
	projects: readonly { name: string }[];
}

/** The subset of a reported test module this reporter reads. */
interface ProjectCountReporterModule {
	project: { name: string };
}

/**
 * A vitest reporter that prints {@link formatProjectRunSummary} once, at the end
 * of the run.
 *
 * Reporting only — it never changes the run's exit status. The refusal above is
 * what fails a run; this makes a narrowed run legible.
 */
export function createProjectCountReporter(log: (message: string) => void = console.log) {
	let registered: string[] = [];
	return {
		onInit(context: ProjectCountReporterContext) {
			registered = context.projects.map((project) => project.name);
		},
		onTestRunEnd(testModules: readonly ProjectCountReporterModule[]) {
			const counts = new Map<string, number>();
			for (const testModule of testModules) {
				const name = testModule.project.name;
				counts.set(name, (counts.get(name) ?? 0) + 1);
			}
			log(formatProjectRunSummary(registered, counts));
		},
	};
}
