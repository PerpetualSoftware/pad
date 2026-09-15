/**
 * The family's fake `authStore` for identity-fence suites (BUG-3084).
 *
 * WHY THIS EXISTS, and it is not convenience. Each surface's suite grew its own
 * double with `let epoch = 0` — a plain closure variable. That models the
 * store's VALUES correctly and its REACTIVITY not at all, and the real
 * `identityEpoch` is `$state`: reading it inside an `$effect` creates a
 * DEPENDENCY, so an identity change re-runs that effect.
 *
 * A non-reactive double silently converts every "did this effect re-run?"
 * question into "does this compile?". That is not a coverage gap; it is an
 * instrument answering a different question than its name. It hid a real defect
 * on the collection page through ten source assertions, ten behavioural legs, a
 * 10/10 mutation matrix, four review rounds and two lead reads — the search
 * dispatch effect re-armed its debounce with the previous user's typed text and
 * captured the NEW epoch, so the fence inside the timer passed and the old
 * query went to the server under the new identity.
 *
 * The general form, which is the part worth carrying to other families:
 *
 *   A test double that models a dependency's VALUES but not its REACTIVITY
 *   turns every question about re-running into a question about compiling.
 *   Where the subject is a framework effect, the double's reactivity IS the
 *   thing under test.
 *
 * MECHANICS. `vi.hoisted` runs before the Svelte runtime is initialised, so
 * `$state` cannot be declared inside a hoisted factory. This module declares it
 * at module scope — legal in a `.svelte.ts` — and a suite binds its hoisted
 * object to it with `bindReactiveEpoch`. Probed against a known answer before
 * being relied on, rather than assumed to work.
 */

import { flushSync } from 'svelte';

let epochSignal = $state(0);

/** The reactive epoch. Reading this inside an `$effect` creates a dependency. */
export function readEpoch(): number {
	return epochSignal;
}

export function writeEpoch(next: number): void {
	epochSignal = next;
}

export function bumpEpoch(): void {
	epochSignal += 1;
}

export function resetEpoch(): void {
	epochSignal = 0;
}

/**
 * Bind a hoisted mock object's epoch hook to the reactive signal above.
 *
 * The hoisted object holds `{ read, write }` slots and delegates to them when
 * they are set, falling back to a plain variable when they are not — so a suite
 * that forgets to bind still runs, but its "did this re-run?" assertions are
 * then measuring nothing. That is exactly the failure this module exists to
 * remove, which is why `assertReactive` below is offered alongside it.
 */
export function bindReactiveEpoch(hook: {
	read: null | (() => number);
	write: null | ((n: number) => void);
}): void {
	hook.read = readEpoch;
	hook.write = writeEpoch;
}

/**
 * Proves the SUBJECT'S OWN epoch path is reactive.
 *
 * Takes the mock's getter and its bump, because the first version of this
 * function did NOT: it read and wrote this module's signal directly, so it
 * returned true even with both hooks disconnected and every PRECONDITION leg
 * built on it passed against exactly the non-reactive double it existed to
 * detect. Codex reproduced that. A guard that cannot fail for the case it is
 * named after is the failure it is named after, wearing its own badge.
 *
 * Pass the values the PAGE will see: `() => authStore.identityEpoch` and
 * `() => authStore.bumpEpoch()` from the suite's own mock. Nothing here
 * references the module signal, so a suite that forgot `bindReactiveEpoch`
 * reads its inert fallback and this returns false.
 *
 * Returns rather than throws, so the caller owns the assertion message.
 */
export function isEpochReactive(read: () => number, bump: () => void): boolean {
	let runs = 0;
	const stop = $effect.root(() => {
		$effect(() => {
			read();
			runs += 1;
		});
	});
	flushSync();
	const baseline = runs;
	const before = read();
	bump();
	flushSync();
	const moved = read() !== before;
	const reran = runs > baseline;
	stop();
	// Both halves, because either alone is satisfiable by a broken double: a
	// bump that does not move the value proves nothing about tracking, and a
	// re-run with an unmoved value would mean the effect depends on something
	// else entirely.
	return moved && reran;
}

/**
 * The reactive USER ID, for surfaces whose recovery is keyed on
 * `authStore.userId` rather than driven by an `onIdentityChange` listener
 * (settings, the dashboard: a `(sessionUserId, wsSlug)` effect drops the data
 * and reloads). The real getter reads the `session` `$state`, so it is a
 * dependency; a suite whose double returns a plain variable turns "did the
 * keyed effect re-run?" into "does this compile?" — the same failure as the
 * epoch, one field over (BUG-3084 surface 4 found it on its first recovery
 * leg: the reload never came, and the mock was why).
 */
let userIdSignal = $state('u1');

export function readUserId(): string {
	return userIdSignal;
}

export function writeUserId(next: string): void {
	userIdSignal = next;
}

export function bindReactiveUserId(hook: {
	readUser: null | (() => string);
	writeUser: null | ((id: string) => void);
}): void {
	hook.readUser = readUserId;
	hook.writeUser = writeUserId;
}

/**
 * Proves the SUBJECT'S OWN userId path is reactive — same contract as
 * `isEpochReactive`: pass the mock's getter and setter, never this module's.
 */
export function isUserIdReactive(read: () => string, set: (id: string) => void): boolean {
	let runs = 0;
	const stop = $effect.root(() => {
		$effect(() => {
			read();
			runs += 1;
		});
	});
	flushSync();
	const baseline = runs;
	const before = read();
	set(before + '-moved');
	flushSync();
	const moved = read() !== before;
	const reran = runs > baseline;
	set(before);
	flushSync();
	stop();
	return moved && reran;
}
