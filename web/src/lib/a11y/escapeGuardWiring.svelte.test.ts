import { describe, it, expect, vi, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
	acquire,
	isBlockedByModal,
	hasForeignEscapeOwner,
	__resetViewerBackdropForTests,
} from './viewerBackdrop';
import {
	pushEscapeHandler,
	topEscapePriority,
	ESCAPE_PRIORITY,
} from '$lib/stores/escapeStack';

/**
 * WIRING CONTRACT for the two route ESC guards (TASK-2429).
 *
 * The viewer's Escape has exactly one owner — `escapeStack` — and the only code
 * that runs it is the keydown handler in each of these two route files. Both
 * used to bail on an inline
 * `document.querySelector('dialog[open], [role="dialog"]:not(.item-pane)')`,
 * which the body-portaled viewer now MATCHES; without the shared
 * `hasForeignEscapeOwner()` (which excludes it) the guard returns early, the
 * stack never runs, and Escape closes nothing at all. That is a silent,
 * app-wide dead key.
 *
 * WHY A SOURCE ASSERTION. The unit tests around `Lightbox` drive a
 * ROUTE-SHAPED driver they define themselves, so they prove the shape works —
 * not that either route still calls it. Mounting a SvelteKit route under vitest
 * to prove the real thing costs far more than it is worth (a 2,900-line
 * component, its stores, its data loaders), while the actual regression risk is
 * mundane: someone deletes or reverts the call, or re-inlines the old selector
 * during a merge. A grep-shaped contract catches exactly that, and nothing else
 * pretends to be covered here.
 *
 * TWO THINGS KEEP IT FROM BEING A GREP THAT LIES:
 *   • COMMENTS ARE STRIPPED FIRST. Every one of these strings appears in prose
 *     in these files (this commit added several), so a whole-file search could
 *     be satisfied by a comment while the handler ran unguarded.
 *   • ASSERTIONS ARE SCOPED TO THE HANDLER that actually calls `runTopEscape`,
 *     not to the file. A guard in some unrelated helper is not this contract.
 *
 * The BEHAVIOURAL proof — a real Escape press, in a real browser, closing
 * exactly the viewer and not the pane beneath it — belongs to TASK-2436's
 * Playwright suite (DR-9), together with the inertness and stacking guarantees
 * jsdom cannot see either.
 *
 * TASK-2430 EXTENDS this file rather than starting a new one, because the
 * subject is the same two handlers. It adds (a) the same scoped contract for
 * the ARBITRATION guard plus its ordering against the legacy guard and the nav
 * keys, and (b) a behavioural route-shaped-driver suite at the bottom that
 * exercises the empty-stack and viewer-frontmost legs of the precedence rule
 * for real. The native top-layer leg stays with TASK-2436 — jsdom has no top
 * layer and no `:modal`.
 */

const ROUTES = [
	'../../routes/[username]/[workspace]/[collection]/+page.svelte',
	'../../routes/[username]/[workspace]/[collection]/[slug]/+page.svelte',
] as const;

/** The guard call, tolerant of formatting (prettier reflow, added braces). */
const GUARD = /if\s*\(\s*hasForeignEscapeOwner\(\)\s*\)\s*\{?\s*return\s*;/;
/** Any `querySelector` whose selector string reaches for a `role="dialog"`. */
const RAW_DIALOG_QUERY = /querySelector\w*\(\s*(['"`])[^'"`]*\[\s*role\s*=\s*\\?["']?dialog/;

/**
 * Source with comments removed. Line comments are matched only when the `//`
 * is not preceded by `:`, so URLs (`https://…`) survive — the point is to drop
 * PROSE, not to be a parser.
 */
function stripComments(source: string): string {
	return source
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.replace(/<!--[\s\S]*?-->/g, '')
		.replace(/(^|[^:])\/\/[^\n]*/g, '$1');
}

/**
 * The body of the function that calls `runTopEscape()`, from its `function`
 * keyword up to that call — i.e. exactly the region the guard must sit in.
 * Returns null when there is no such call at all, which is itself a failure.
 */
function escapeHandlerPrologue(code: string): string | null {
	const call = code.search(/runTopEscape\s*\(/);
	if (call === -1) return null;
	const start = code.lastIndexOf('function ', call);
	if (start === -1) return null;
	return code.slice(start, call);
}

/** The TASK-2430 arbitration bail, tolerant of formatting. */
const ARBITRATION_GUARD = /if\s*\(\s*isBlockedByModal\(\s*null\s*\)\s*\)\s*\{?\s*return\s*;/;

/**
 * The body of the route's WINDOW keydown handler — the one wired to
 * `<svelte:window onkeydown={…}>`. Found by name from that binding rather than
 * hard-coded, and sliced to the next top-level `function` so a guard sitting in
 * some neighbouring helper cannot satisfy these assertions.
 */
function pageKeydownHandler(code: string): string | null {
	const wiring = code.match(/<svelte:window[^>]*onkeydown=\{(\w+)\}/);
	if (!wiring) return null;
	const start = code.search(new RegExp(`function\\s+${wiring[1]}\\s*\\(`));
	if (start === -1) return null;
	const next = code.slice(start + 1).search(/\n\tfunction\s/);
	return next === -1 ? code.slice(start) : code.slice(start, start + 1 + next);
}

describe.each(ROUTES)('ESC guard wiring in %s', (relative) => {
	const source = readFileSync(fileURLToPath(new URL(relative, import.meta.url)), 'utf8');
	const code = stripComments(source);

	it('imports the shared guard from the a11y module', () => {
		expect(code).toMatch(
			/import\s*\{[^}]*\bhasForeignEscapeOwner\b[^}]*\}\s*from\s*'\$lib\/a11y\/viewerBackdrop'/
		);
	});

	it('calls it as an early return INSIDE the handler that runs the stack', () => {
		// Scoped, not file-wide: this is the assertion that would otherwise be
		// satisfiable by prose or by an unrelated helper. `return` (not
		// `preventDefault`) is the point — a foreign modal owns the key outright,
		// so the stack must not run at all.
		const prologue = escapeHandlerPrologue(code);
		expect(prologue).not.toBeNull();
		expect(prologue!).toMatch(GUARD);
	});

	it('does not reach the stack without passing the guard first', () => {
		// The ordering IS the contract: running the stack first would close the
		// viewer out from under a modal that owns the key, and guarding after the
		// fact would do nothing. Falls out of the scoping above — the prologue
		// ends AT the call — so this re-states it against the whole file, where a
		// second, unguarded `runTopEscape` would also show up.
		const occurrences = code.match(/runTopEscape\s*\(/g) ?? [];
		expect(occurrences).toHaveLength(1);
		const guardAt = code.search(GUARD);
		const stackAt = code.search(/runTopEscape\s*\(/);
		expect(guardAt).toBeGreaterThan(-1);
		expect(guardAt).toBeLessThan(stackAt);
	});

	// ── TASK-2430: Escape must still REACH the stack ───────────────────────
	//
	// The single most dangerous way to get TASK-2430 wrong is to hoist an
	// `isBlockedByModal` bail to the top of these handlers. It reads plausibly —
	// "a viewer is in front, so stand down" — and it silently kills the viewer's
	// OWN Escape: the viewer registers on `escapeStack`, and these handlers are
	// the only code that runs the stack. That is exactly the dead key TASK-2429
	// existed to fix, reintroduced.
	//
	// Escape's three-way precedence is already complete WITHOUT an arbitration
	// bail: `hasForeignEscapeOwner()` stands down for a native `dialog:modal`
	// and for foreign sheets, and deliberately EXCLUDES the viewer so the stack
	// dispatches to it. So the contract here is a NEGATIVE one.

	it('does not bail on arbitration before running the escape stack', () => {
		// Scoped to the prologue — the region between the handler's `function`
		// keyword and `runTopEscape()`. A bail anywhere in there returns before
		// the viewer's Escape can be dispatched.
		const prologue = escapeHandlerPrologue(code);
		expect(prologue).not.toBeNull();
		expect(prologue!).not.toMatch(ARBITRATION_GUARD);
	});

	it('no longer bails on a raw role="dialog" query', () => {
		// The pre-TASK-2429 inline check, re-introduced by hand or by a bad merge
		// resolution, is the regression this file exists to catch: it matches the
		// viewer's own `role="dialog"` root and kills its Escape. Matched by shape
		// rather than by one exact quoting, so a reformatted revert still trips it.
		expect(code).not.toMatch(RAW_DIALOG_QUERY);
	});
});

/**
 * The collection route is the one whose keydown handler owns MORE than Escape:
 * `j`/`k`/arrows/`Enter`/`Tab` drive list + board navigation. Escape was never
 * the only key at stake, so that half DOES need the arbitration guard — placed
 * BELOW the Escape dispatch (so it cannot swallow the viewer's Escape) and
 * ABOVE the nav switch (so it actually guards it). Both bounds are asserted;
 * either one alone is satisfiable by a guard in the wrong place.
 */
describe('collection route — nav keys are behind the arbitration guard', () => {
	const source = readFileSync(fileURLToPath(new URL(ROUTES[0], import.meta.url)), 'utf8');
	const code = stripComments(source);

	it('imports the arbitration helper from the a11y module', () => {
		expect(code).toMatch(
			/import\s*\{[^}]*\bisBlockedByModal\b[^}]*\}\s*from\s*'\$lib\/a11y\/viewerBackdrop'/
		);
	});

	it('guards AFTER the escape dispatch and BEFORE the nav switch', () => {
		const body = pageKeydownHandler(code);
		expect(body).not.toBeNull();
		const arbitrationAt = body!.search(ARBITRATION_GUARD);
		const escapeDispatchAt = body!.search(/runTopEscape\s*\(/);
		// The `j` case is the first arm of the nav switch; `moveBoardFocus` is the
		// board half. Both must be downstream of the guard.
		const navAt = body!.search(/case\s+'j'\s*:/);
		const boardAt = body!.search(/moveBoardFocus\s*\(/);
		for (const at of [arbitrationAt, escapeDispatchAt, navAt, boardAt]) {
			expect(at).toBeGreaterThan(-1);
		}
		expect(escapeDispatchAt).toBeLessThan(arbitrationAt);
		expect(arbitrationAt).toBeLessThan(navAt);
		expect(arbitrationAt).toBeLessThan(boardAt);
	});
});

/**
 * The full-page item host's keydown owns ONLY Escape, so it must carry NO
 * arbitration bail at all — there is no navigation half for one to guard, and
 * any bail would sit upstream of the stack dispatch.
 */
describe('item route — no arbitration bail in its Escape-only handler', () => {
	const code = stripComments(
		readFileSync(fileURLToPath(new URL(ROUTES[1], import.meta.url)), 'utf8'),
	);

	it('does not call isBlockedByModal anywhere in the file', () => {
		expect(code).not.toMatch(/isBlockedByModal\s*\(/);
	});
});

/**
 * BEHAVIOURAL proof of the precedence rules, in the shape the collection route
 * actually has: an ESCAPE path that must reach the escape stack, and a
 * NAVIGATION path that must not act while anything is in front.
 *
 * This is the counterpart to the source contracts above — they prove the route
 * calls things in the right ORDER; this proves the order decides correctly. The
 * asymmetry is the whole point and is where TASK-2430 was first implemented
 * WRONG: a single arbitration bail at the top satisfies every "nav key did not
 * act" assertion while silently killing the viewer's Escape.
 *
 * The NATIVE top-layer leg is deliberately absent — jsdom implements neither
 * the top layer nor `:modal` (the selector throws, which is the unsupported
 * path `isBlockedByModal` answers "no native modal" for), so a jsdom assertion
 * there would test the fallback rather than the contract. It belongs to
 * TASK-2436's Playwright suite.
 */
describe('route-shaped driver — Escape reaches the stack, nav keys do not act', () => {
	/**
	 * The collection route's shape, INCLUDING the two priority-gated pane
	 * branches that sit between the foreign-modal guard and the stack dispatch.
	 * They are modelled because they are the branches that could still close a
	 * layer UNDER the viewer, and they are driven by the REAL `escapeStack`
	 * (`topEscapePriority()`), not by a hand-rolled stand-in — so a change to the
	 * viewer's registered priority would surface here.
	 */
	function drive(
		e: KeyboardEvent,
		runStack: () => boolean,
		navigate: () => void,
		panePop?: () => void,
	): 'foreign' | 'pane-pop' | 'stack' | 'blocked' | 'navigated' {
		if (e.key === 'Escape') {
			if (hasForeignEscapeOwner()) return 'foreign';
			if (topEscapePriority() === ESCAPE_PRIORITY.pane) {
				panePop?.();
				return 'pane-pop';
			}
			runStack();
			return 'stack';
		}
		if (isBlockedByModal(null)) return 'blocked';
		navigate();
		return 'navigated';
	}

	const unregister: Array<() => void> = [];

	afterEach(() => {
		while (unregister.length) unregister.pop()!();
		__resetViewerBackdropForTests();
		document.body.innerHTML = '';
	});

	function press(key: string): KeyboardEvent {
		return new KeyboardEvent('keydown', { key, cancelable: true });
	}

	/** Register a handler on the REAL escape stack for the duration of a test. */
	function register(fn: () => boolean, priority: number) {
		unregister.push(pushEscapeHandler(fn, priority));
	}

	function mountViewer(): HTMLElement {
		const viewer = document.createElement('div');
		// The marker class is precisely what makes the LEGACY guard ignore it, so
		// that Escape falls through to the stack instead of standing down.
		viewer.className = 'attachment-viewer';
		viewer.setAttribute('role', 'dialog');
		document.body.appendChild(viewer);
		return viewer;
	}

	it('EMPTY STACK: Escape runs the stack and nav keys navigate', () => {
		const runStack = vi.fn(() => true);
		const navigate = vi.fn();
		expect(drive(press('Escape'), runStack, navigate)).toBe('stack');
		expect(drive(press('j'), runStack, navigate)).toBe('navigated');
		expect(runStack).toHaveBeenCalledTimes(1);
		expect(navigate).toHaveBeenCalledTimes(1);
	});

	it('EMPTY STACK with a foreign sheet: the legacy guard owns Escape', () => {
		// A shipped `role="dialog"` sheet with no lease and no stack registration
		// — the case TASK-2429's guard exists for, and which must be UNCHANGED.
		const sheet = document.createElement('div');
		sheet.setAttribute('role', 'dialog');
		document.body.appendChild(sheet);

		const runStack = vi.fn(() => true);
		expect(drive(press('Escape'), runStack, vi.fn())).toBe('foreign');
		expect(runStack).not.toHaveBeenCalled();
	});

	it('VIEWER FRONTMOST: Escape STILL reaches the stack — it is the viewer\u2019s own key', () => {
		// The regression guard for TASK-2430's first implementation. A blanket
		// `isBlockedByModal` bail here would return before the stack ran, and the
		// viewer — whose Escape lives ONLY on that stack — would become
		// undismissable by keyboard.
		const lease = acquire(mountViewer());
		const runStack = vi.fn(() => true);
		expect(drive(press('Escape'), runStack, vi.fn())).toBe('stack');
		expect(runStack).toHaveBeenCalledTimes(1);
		lease.release();
	});

	it('VIEWER FRONTMOST: the priority-gated pane branch stands down', () => {
		// With a pane registered and NO viewer, Escape takes the pane branch —
		// today's behaviour. Once a viewer registers ABOVE it, that branch must
		// stop firing, or one press would pop a pane layer underneath the viewer.
		// Driven through the real stack, so this tracks the actual priorities.
		register(() => true, ESCAPE_PRIORITY.pane);
		const panePop = vi.fn();
		const runStack = vi.fn(() => true);
		expect(drive(press('Escape'), runStack, vi.fn(), panePop)).toBe('pane-pop');
		expect(panePop).toHaveBeenCalledTimes(1);

		const lease = acquire(mountViewer());
		register(() => true, ESCAPE_PRIORITY.viewer);
		expect(drive(press('Escape'), runStack, vi.fn(), panePop)).toBe('stack');
		expect(panePop).toHaveBeenCalledTimes(1);
		expect(runStack).toHaveBeenCalledTimes(1);
		lease.release();
	});

	it('VIEWER FRONTMOST OVER AN OPEN SHEET: Escape is still owned by the stack', () => {
		// The dead-key cross-product (found in review round 5). A `role="dialog"`
		// sheet open BENEATH the viewer used to make `hasForeignEscapeOwner()`
		// true, so the driver returned 'foreign' — while the sheet itself,
		// correctly guarded by TASK-2430, declined. Nobody owned the key.
		const sheet = document.createElement('div');
		sheet.setAttribute('role', 'dialog');
		document.body.appendChild(sheet);
		const runStack = vi.fn(() => true);
		// Sheet alone: it owns Escape, exactly as before.
		expect(drive(press('Escape'), runStack, vi.fn())).toBe('foreign');

		const lease = acquire(mountViewer());
		expect(drive(press('Escape'), runStack, vi.fn())).toBe('stack');
		expect(runStack).toHaveBeenCalledTimes(1);

		// Viewer gone → the sheet is frontmost again and reclaims the key.
		lease.release();
		expect(drive(press('Escape'), runStack, vi.fn())).toBe('foreign');
	});

	it('VIEWER FRONTMOST: nav keys are blocked, and resume on release', () => {
		const lease = acquire(mountViewer());
		const navigate = vi.fn();
		expect(drive(press('j'), vi.fn(() => true), navigate)).toBe('blocked');
		expect(drive(press('Enter'), vi.fn(() => true), navigate)).toBe('blocked');
		expect(drive(press('Tab'), vi.fn(() => true), navigate)).toBe('blocked');
		expect(navigate).not.toHaveBeenCalled();

		lease.release();
		expect(drive(press('j'), vi.fn(() => true), navigate)).toBe('navigated');
		expect(navigate).toHaveBeenCalledTimes(1);
	});
});

/**
 * TWO FURTHER GLOBAL ESCAPE OWNERS, found by TASK-2430's round-2 sweep and
 * NOT in the task's original owner list. Neither route can host an attachment
 * viewer (neither mounts `ItemDetail`, so neither mounts a viewer host), but
 * `+layout.svelte` mounts `CreateWorkspaceModal` / `OpenChildrenDialog` on BOTH
 * — so a native top-layer dialog CAN sit in front of them, and one press would
 * otherwise cancel the dialog and mutate the layer underneath it.
 *
 * The console shell has a real behavioural suite
 * (`routes/console/consoleShellEscape.svelte.test.ts`). The workspace GRAPH
 * route does not: it is a 3d-force-graph host with a WebGL renderer, which
 * jsdom cannot mount, so this source contract — deletion-proof, comment-
 * stripped, scoped to the handler — is the coverage it gets here. Its
 * behavioural proof belongs with TASK-2436.
 */
describe('other global Escape owners carry the arbitration guard', () => {
	const OTHERS = [
		'../../routes/[username]/[workspace]/graph/+page.svelte',
		'../../routes/console/+layout.svelte',
	] as const;

	it.each(OTHERS)('%s guards its window keydown handler', (relative) => {
		const code = stripComments(
			readFileSync(fileURLToPath(new URL(relative, import.meta.url)), 'utf8'),
		);
		expect(code).toMatch(
			/import\s*\{[^}]*\bisBlockedByModal\b[^}]*\}\s*from\s*'\$lib\/a11y\/viewerBackdrop'/
		);
		const body = pageKeydownHandler(code);
		expect(body).not.toBeNull();
		expect(body!).toMatch(ARBITRATION_GUARD);
	});
});
