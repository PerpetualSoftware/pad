import { describe, expect, it, vi } from 'vitest';
import {
	PROJECT_REQUIREMENTS,
	createProjectCountReporter,
	findUnsatisfiedProjects,
	formatProjectRunSummary,
	formatUnsatisfiedProjectsError,
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
