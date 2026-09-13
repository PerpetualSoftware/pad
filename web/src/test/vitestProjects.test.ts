import { readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it, vi } from 'vitest';
import {
	BROWSER_TEST_GLOB,
	IDB_TEST_GLOB,
	NODE_TEST_GLOB,
	PROJECT_REQUIREMENTS,
	TEST_SHAPED_FILE,
	createProjectCountReporter,
	findUnsatisfiedProjects,
	formatProjectRunSummary,
	formatUnsatisfiedProjectsError,
	projectForTestFile,
	type ProjectRequirement,
} from './vitestProjects';

// BUG-3045. `web/vitest.config.ts` used to register the `jsdom` and `idb`
// projects only when their devDependencies resolved, so an incomplete install
// NARROWED the run: `npm test` exited 0 having executed none of the
// `*.svelte.test.ts` suites and said nothing about it.
//
// These cover the decision and the two messages. The wiring itself — that the
// config throws, and that vitest exits non-zero rather than falling back to a
// default config — is not reachable from inside a vitest run (the refusal
// happens while the config is being loaded, before any test file is collected),
// so it was verified by running `vitest run` against a copy of the config whose
// resolver reported `fake-indexeddb` and `jsdom` absent: exit 1, message
// printed, both projects and both packages named. Recorded on BUG-3045 rather
// than asserted here, per CONVE-19 — a direct-call test vouches for these
// functions, not for their binding.

const alwaysResolves = () => true;

describe('PROJECT_REQUIREMENTS', () => {
	it('covers every project the config registers conditionally, and not `node`', () => {
		expect(PROJECT_REQUIREMENTS.map((r) => r.project).sort()).toEqual(['idb', 'jsdom']);
	});

	// There is deliberately NO test that the package names in the table resolve.
	// It would be unreachable: a misspelled name makes vitest.config.ts refuse at
	// load, so the suite never starts and no assertion in this file can run. The
	// admission check IS that guard, and it is louder than a failing test — it
	// names the package and stops the run. Verified as mutant M12 on the BUG-3045
	// trail: `fake-indexeddb` -> `fake-indexedDB` exits 1 with zero tests
	// executed. An assertion here would read as coverage while covering nothing.
});

describe('findUnsatisfiedProjects', () => {
	const requirements: ProjectRequirement[] = [
		{ project: 'idb', deps: ['fake-indexeddb'] },
		{ project: 'jsdom', deps: ['jsdom', '@testing-library/svelte'] },
	];

	it('returns nothing when every dependency resolves', () => {
		expect(findUnsatisfiedProjects(requirements, alwaysResolves)).toEqual([]);
	});

	it('reports only the project whose dependency is missing', () => {
		const canResolve = (id: string) => id !== 'fake-indexeddb';
		expect(findUnsatisfiedProjects(requirements, canResolve)).toEqual([
			{ project: 'idb', missing: ['fake-indexeddb'] },
		]);
	});

	it('reports EVERY missing dependency of a project, not just the first', () => {
		// One `npm ci` should fix the whole set; naming one package at a time turns
		// a single broken install into a sequence of failed runs.
		const canResolve = () => false;
		expect(findUnsatisfiedProjects(requirements, canResolve)).toEqual([
			{ project: 'idb', missing: ['fake-indexeddb'] },
			{ project: 'jsdom', missing: ['jsdom', '@testing-library/svelte'] },
		]);
	});

	it('does not treat a partially-satisfied project as satisfied', () => {
		const canResolve = (id: string) => id === 'jsdom';
		expect(findUnsatisfiedProjects(requirements, canResolve)).toEqual([
			{ project: 'idb', missing: ['fake-indexeddb'] },
			{ project: 'jsdom', missing: ['@testing-library/svelte'] },
		]);
	});
});

describe('formatUnsatisfiedProjectsError', () => {
	it('names every unrunnable project and every missing package', () => {
		const message = formatUnsatisfiedProjectsError([
			{ project: 'idb', missing: ['fake-indexeddb'] },
			{ project: 'jsdom', missing: ['jsdom', '@testing-library/svelte'] },
		]);
		expect(message).toContain('2 test projects cannot run');
		expect(message).toContain('idb — cannot resolve: fake-indexeddb');
		expect(message).toContain('jsdom — cannot resolve: jsdom, @testing-library/svelte');
	});

	it('tells the reader how to fix it', () => {
		const message = formatUnsatisfiedProjectsError([{ project: 'idb', missing: ['x'] }]);
		expect(message).toContain('npm ci');
		expect(message).toContain('1 test project cannot run');
	});
});

describe('formatProjectRunSummary', () => {
	it('reports the full count when every project ran files', () => {
		const summary = formatProjectRunSummary(
			['node', 'idb', 'jsdom'],
			new Map([
				['node', 69],
				['idb', 7],
				['jsdom', 109],
			]),
		);
		expect(summary).toBe(
			'vitest projects: 3/3 ran test files — node (69 files), idb (7 files), jsdom (109 files)',
		);
	});

	it('reports the NARROW count when a project ran nothing, and names it', () => {
		// The point of the line: a run that measured one project must not look
		// identical to a run that measured three.
		const summary = formatProjectRunSummary(
			['node', 'idb', 'jsdom'],
			new Map([['node', 1]]),
		);
		expect(summary).toContain('1/3 ran test files');
		expect(summary).toContain('idb (0 files)');
		expect(summary).toContain('NO test files ran for: idb, jsdom');
	});

	it('reports a deliberately filtered run as complete for the projects it kept', () => {
		// `--project node` removes the others from the registered set entirely, so
		// there is nothing idle to warn about — but the denominator tells the
		// reader the run was narrowed.
		const summary = formatProjectRunSummary(['node'], new Map([['node', 69]]));
		expect(summary).toBe('vitest projects: 1/1 ran test files — node (69 files)');
		expect(summary).not.toContain('NO test files ran');
	});

	it('refuses to invent a denominator when the registered set is missing', () => {
		// M6b in the mutation matrix: a reporter that falls back to the projects
		// that ran would print a confident `2/2` for a run whose registered set
		// was never reported. `0/0` beside a full run is equally wrong, so the
		// line says the denominator is unavailable and names what it did see.
		const summary = formatProjectRunSummary(
			[],
			new Map([
				['node', 2],
				['jsdom', 1],
			]),
		);
		expect(summary).toContain('registered set unavailable');
		expect(summary).toContain('ran: node, jsdom');
		expect(summary).not.toContain('2/2');
		expect(summary).not.toContain('0/0');
	});

	it('says plainly that nothing ran when nothing ran', () => {
		expect(formatProjectRunSummary([], new Map())).toBe('vitest projects: no test files ran');
	});

	it('singularises a one-file count', () => {
		expect(formatProjectRunSummary(['node'], new Map([['node', 1]]))).toContain('node (1 file)');
	});
});

describe('createProjectCountReporter', () => {
	it('counts the modules each project ran and logs one summary at the end', () => {
		const log = vi.fn();
		const reporter = createProjectCountReporter(log);
		reporter.onInit({ projects: [{ name: 'node' }, { name: 'jsdom' }] });
		reporter.onTestRunEnd([
			{ project: { name: 'node' } },
			{ project: { name: 'jsdom' } },
			{ project: { name: 'node' } },
		]);
		expect(log).toHaveBeenCalledTimes(1);
		expect(log.mock.calls[0][0]).toBe(
			'vitest projects: 2/2 ran test files — node (2 files), jsdom (1 file)',
		);
	});

	it('reports a registered project that ran no modules', () => {
		const log = vi.fn();
		const reporter = createProjectCountReporter(log);
		reporter.onInit({ projects: [{ name: 'node' }, { name: 'jsdom' }] });
		reporter.onTestRunEnd([{ project: { name: 'node' } }]);
		expect(log.mock.calls[0][0]).toContain('NO test files ran for: jsdom');
	});

	it('does not invent a registered set when onInit never fired', () => {
		// M6b in the mutation matrix, and the leg that kills it: every other
		// reporter test calls onInit with a non-empty list, so a fallback to the
		// projects that ran is invisible to them. Without onInit the reporter has
		// no denominator, and inventing one would print a confident `2/2` for a run
		// whose project set was never reported.
		const log = vi.fn();
		const reporter = createProjectCountReporter(log);
		reporter.onTestRunEnd([{ project: { name: 'node' } }, { project: { name: 'jsdom' } }]);
		expect(log.mock.calls[0][0]).toContain('registered set unavailable');
		expect(log.mock.calls[0][0]).not.toContain('2/2');
	});

	it('takes the registered set from onInit, not from the modules that ran', () => {
		// The denominator has to come from the projects vitest resolved. Deriving
		// it from the modules would make it equal the numerator by construction and
		// the line could never report a narrowed run.
		const log = vi.fn();
		const reporter = createProjectCountReporter(log);
		reporter.onInit({ projects: [{ name: 'node' }, { name: 'idb' }, { name: 'jsdom' }] });
		reporter.onTestRunEnd([{ project: { name: 'node' } }]);
		expect(log.mock.calls[0][0]).toContain('1/3 ran test files');
	});
});

describe('every test file belongs to a project', () => {
	// BUG-3045, codex round 1 finding 3. The refusal covers a project that cannot
	// RUN; this covers the other way the suite goes green while measuring nothing
	// — a test file that no project's glob matches, which is never collected and
	// never fails. Codex checked the tree at the time and found no member; that
	// is exactly when a guard is worth adding, because the first one to appear
	// would otherwise arrive silently.

	const srcRoot = fileURLToPath(new URL('..', import.meta.url));

	function testShapedFilesUnder(dir: string, prefix = ''): string[] {
		const found: string[] = [];
		for (const entry of readdirSync(dir, { withFileTypes: true })) {
			const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
			if (entry.isDirectory()) {
				found.push(...testShapedFilesUnder(`${dir}/${entry.name}`, rel));
			} else if (TEST_SHAPED_FILE.test(entry.name)) {
				found.push(rel);
			}
		}
		return found;
	}

	it('finds the suite it is supposed to be checking', () => {
		// The precondition. Without it every assertion below passes vacuously on an
		// empty list — which is the failure mode this whole bug is about.
		const files = testShapedFilesUnder(srcRoot);
		expect(files.length).toBeGreaterThan(100);
		expect(files).toContain('test/vitestProjects.test.ts');
	});

	it('leaves no test-shaped file unowned by any project', () => {
		const orphans = testShapedFilesUnder(srcRoot).filter(
			(file) => projectForTestFile(file) === null,
		);
		// Named, not counted: the failure has to say WHICH file never runs.
		expect(orphans).toEqual([]);
	});

	it('routes each suffix to the project whose glob claims it', () => {
		expect(projectForTestFile('lib/foo.test.ts')).toBe('node');
		expect(projectForTestFile('lib/Foo.svelte.test.ts')).toBe('jsdom');
		expect(projectForTestFile('lib/foo.idb.test.ts')).toBe('idb');
	});

	it('reports the shapes that no project runs', () => {
		// These are test-shaped and unowned. If one of them ever becomes a real
		// file, the orphan check above fails and names it.
		for (const unowned of ['lib/foo.spec.ts', 'lib/foo.test.js', 'lib/Foo.test.tsx']) {
			expect(TEST_SHAPED_FILE.test(unowned)).toBe(true);
			expect(projectForTestFile(unowned)).toBeNull();
		}
	});

	it('keeps the globs and the ownership function in agreement', () => {
		// `projectForTestFile` classifies by suffix while vitest matches by glob.
		// If a glob is edited without the function, the two silently disagree and
		// the orphan check starts vouching for the wrong thing.
		expect(NODE_TEST_GLOB).toBe('src/**/*.test.ts');
		expect(BROWSER_TEST_GLOB).toBe('src/**/*.svelte.test.ts');
		expect(IDB_TEST_GLOB).toBe('src/**/*.idb.test.ts');
		for (const glob of [NODE_TEST_GLOB, BROWSER_TEST_GLOB, IDB_TEST_GLOB]) {
			const suffix = glob.replace('src/**/*', '');
			expect(projectForTestFile(`lib/x${suffix}`)).not.toBeNull();
		}
	});
});

describe('vitest.config.ts actually uses all of this', () => {
	// BUG-3045, codex round 1 finding 8, and CONVE-19: every other test in this
	// file vouches for a FUNCTION. None of them would notice if vitest.config.ts
	// stopped calling it, went back to probing dependencies inline, or dropped a
	// project. These assertions read the real config.
	//
	// Importing it re-runs its module scope, admission check included — which in a
	// complete install passes, and in an incomplete one would fail this file the
	// same way it fails the whole suite.

	async function resolveConfig() {
		const config = (await import('../../vitest.config')).default;
		expect(typeof config).toBe('function');
		return (config as (env: unknown) => Promise<Record<string, any>>)({
			mode: 'test',
			command: 'serve',
		});
	}

	it('registers every project unconditionally, none dropped', async () => {
		const resolved = await resolveConfig();
		const names = resolved.test.projects.map((project: any) => project.test.name);
		expect(names).toEqual(['node', 'idb', 'jsdom']);
	});

	it('registers exactly `node` plus the projects the requirements table covers', async () => {
		// The table and the config are two lists that have to stay the same length.
		// A project added to the config without an entry here gets no admission
		// check and can go back to failing silently; an entry with no project is a
		// refusal nobody can act on.
		const resolved = await resolveConfig();
		const registered = resolved.test.projects.map((project: any) => project.test.name).sort();
		const checked = ['node', ...PROJECT_REQUIREMENTS.map((r) => r.project)].sort();
		expect(registered).toEqual(checked);
	});

	it('keeps vitest\'s default reporter alongside the project-count one', async () => {
		// Naming any reporter REPLACES the default list. Dropping `default` here
		// would silence the ordinary pass/fail output — measured: a run so
		// configured prints the summary line and nothing else.
		const resolved = await resolveConfig();
		expect(resolved.test.reporters).toHaveLength(2);
		expect(resolved.test.reporters[0]).toBe('default');
		expect(typeof resolved.test.reporters[1].onInit).toBe('function');
		expect(typeof resolved.test.reporters[1].onTestRunEnd).toBe('function');
	});

	it('routes each project with the shared globs, not private copies', async () => {
		const resolved = await resolveConfig();
		const byName = Object.fromEntries(
			resolved.test.projects.map((project: any) => [project.test.name, project.test]),
		);
		expect(byName.node.include).toEqual([NODE_TEST_GLOB]);
		expect(byName.node.exclude).toEqual([BROWSER_TEST_GLOB, IDB_TEST_GLOB]);
		expect(byName.idb.include).toEqual([IDB_TEST_GLOB]);
		expect(byName.jsdom.include).toEqual([BROWSER_TEST_GLOB]);
	});
});
