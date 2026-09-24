/**
 * A fake `localIndex.bootstrap` that is REACTIVE the way the real one is
 * (BUG-3192).
 *
 * The real `bootstrap` (stores/localIndex.svelte.ts) reads the workspace's
 * `bootstrapState` — a `$state` — in its synchronous prefix, writes it to
 * `'loading'` before its first await, and writes `'ready'` when the load lands.
 * Called from inside an `$effect`, the read is a dependency and both writes
 * re-run that effect. The mount suites stub it as a plain `async () => {}`,
 * which models its values and none of that, so the three loads per item page it
 * caused were invisible to every one of them. See identityEpochMock.svelte.ts
 * for the general form of that failure.
 *
 * `$state` cannot live in a `vi.hoisted` factory (the runtime is not up yet),
 * so it lives here at module scope and a suite's mock delegates to it.
 */

import { flushSync } from 'svelte';

type State = 'cold' | 'loading' | 'ready';

let state = $state<State>('cold');
let release: (() => void) | null = null;

/** Mirrors the real bootstrap's read-then-write shape; resolves on `finishBootstrap`. */
export function reactiveBootstrap(): Promise<void> {
	if (state === 'ready') return Promise.resolve();
	if (state === 'loading') return pending;
	state = 'loading';
	pending = new Promise<void>((resolve) => {
		release = () => {
			state = 'ready';
			resolve();
		};
	});
	return pending;
}
let pending: Promise<void> = Promise.resolve();

/** Lands the in-flight bootstrap: `'loading'` → `'ready'`, the real store's last write. */
export function finishBootstrap(): void {
	release?.();
	release = null;
}

export function resetBootstrap(): void {
	state = 'cold';
	release = null;
	pending = Promise.resolve();
}

/**
 * Proves the fake re-runs an effect that calls it, so a leg asserting "loaded
 * once" is measuring the page and not an inert double. Counts runs of a bare
 * effect across the same two writes the page sees.
 */
export function isBootstrapReactive(): boolean {
	resetBootstrap();
	let runs = 0;
	const stop = $effect.root(() => {
		$effect(() => {
			void reactiveBootstrap();
			runs += 1;
		});
	});
	flushSync();
	finishBootstrap();
	flushSync();
	stop();
	resetBootstrap();
	return runs >= 3;
}
