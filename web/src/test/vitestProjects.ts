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
// Deliberate narrowing is still available and is NOT refused, because it goes
// through vitest's own `--project` filter rather than through dependency
// resolution. That is what the summary line is for: it prints how many projects
// actually ran, so a narrowed run says so in its own output instead of looking
// identical to a full one.

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
