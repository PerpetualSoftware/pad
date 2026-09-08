/**
 * A keyed single-flight loader (TASK-2947).
 *
 * THE MECHANISM THIS REPLACES. `collections.svelte.ts` and
 * `workspace.svelte.ts` each hand-rolled the same three parts around a store
 * load, and the two copies drifted three times in one afternoon — each drift
 * found by a reviewer rather than by a test (TASK-2200 rounds 3, 4 and 5). The
 * three parts:
 *
 *   1. A monotonic GENERATION, so only the latest call commits. A workspace
 *      switch can leave two requests in flight; without this an older response
 *      resolving last overwrites the newer one's data.
 *   2. A keyed IN-FLIGHT PROMISE, so a caller who only needs "a result exists"
 *      can JOIN the request already running instead of issuing a duplicate.
 *   3. CLEANUP GUARDED BY OWNERSHIP, so an older call settling late cannot
 *      clear a newer one's slot or flip its loading flag off.
 *
 * WHAT THIS DELIBERATELY DOES NOT DO: coalesce. `run` ALWAYS issues. Joining is
 * a separate, opt-in read via `inFlightFor`, and that asymmetry is a property to
 * preserve rather than an inconsistency to tidy away. `loadCollections` has ~19
 * call sites and eighteen of them are reacting to a KNOWN CHANGE — an SSE
 * rename, a settings save, a `collections_changed` flag, a reorder 404 — where a
 * request issued BEFORE that change cannot answer the caller. Only
 * `ensureCollections` and `recoverIfMissing` have the join intent, and they ask
 * for it by name.
 *
 * WHY NOT `$lib/attachments/viewFence.ts::createFence`, which is this codebase's
 * other generation fence. It answers a neighbouring question for a different
 * consumer: "may this continuation write to the view the user is still looking
 * at?" — for surfaces that stay MOUNTED while their props change underneath
 * them, so its identity is CAPTURED from live reactive state and re-compared on
 * every `stale()` call. A store method has no live identity to capture: the key
 * arrives as the call's own argument. And a fence has no notion of the JOIN
 * slot, which is half of what these two stores needed. Sharing the generation
 * counter is all the two have in common, and wrapping one in the other would
 * mean synthesising a `ViewIdentity` per key — more machinery than the counter
 * it replaces. Named here so the next reader does not have to re-derive it.
 *
 * The `loading` flag stays OWNED BY THE STORE and is written through the
 * optional `setLoading` callback, rather than being state on this object. Both
 * stores share their flag with other methods (`loadItems` next door in
 * collections), so a flag owned here would have had to either absorb those
 * writers or answer a different question than the one the UI reads today.
 */
export interface SingleFlightRun {
	/**
	 * True while this call is still the latest — i.e. its writes may commit.
	 * Check it AFTER every await that precedes a commit; a `false` answer means
	 * a newer call has superseded this one and its result must be dropped.
	 */
	isLatest(): boolean;
}

export interface SingleFlightOptions {
	/**
	 * Called with `true` when a run starts, and with `false` when the LATEST run
	 * settles. An older run settling late does not call it — that is the
	 * ownership rule applied to the spinner.
	 */
	setLoading?: (loading: boolean) => void;
}

export interface KeyedSingleFlight<K> {
	/**
	 * The promise of the run currently in flight FOR THIS KEY, or null.
	 *
	 * A single slot tagged with its key, not a per-key map, and that is the
	 * right structure because of the generation guard: once a run for B starts,
	 * a run for A that is still in flight has already lost the right to commit,
	 * so handing an A-joiner that promise would resolve them against a result
	 * that never lands. Issuing a fresh A request is the correct answer, and it
	 * is what a single slot produces. The key TAG guards the opposite mistake —
	 * handing an A-joiner B's promise.
	 */
	inFlightFor(key: K): Promise<void> | null;

	/**
	 * Issue a run. ALWAYS issues; never joins an existing one.
	 *
	 * `work` receives a handle whose `isLatest()` says whether this run may
	 * still commit. Guarding the commit is `work`'s job because only `work`
	 * knows which writes are the commit.
	 */
	run(key: K, work: (run: SingleFlightRun) => Promise<void>): Promise<void>;
}

export function createKeyedSingleFlight<K>(options: SingleFlightOptions = {}): KeyedSingleFlight<K> {
	const { setLoading } = options;

	// Plain counters and slots — not reactive. They fence async writes; they are
	// never rendered.
	let seq = 0;
	let inFlightKey: K | null = null;
	let inFlightPromise: Promise<void> | null = null;

	return {
		inFlightFor(key: K): Promise<void> | null {
			if (inFlightPromise && inFlightKey === key) return inFlightPromise;
			return null;
		},

		run(key: K, work: (run: SingleFlightRun) => Promise<void>): Promise<void> {
			const mySeq = ++seq;
			setLoading?.(true);
			// Published SYNCHRONOUSLY, before the work starts, so a joiner that
			// asks between this call and the first await is answered. Overwriting
			// a previous tenant is correct rather than lossy: this happens after
			// `++seq`, so the run being displaced has already lost the right to
			// commit.
			inFlightKey = key;
			const promise = (async () => {
				try {
					await work({ isLatest: () => mySeq === seq });
				} finally {
					// Only the LATEST run owns the flag and the slot. An older run
					// settling late must not flip the spinner off while a newer one
					// is running, nor clear a newer one's promise out from under a
					// joiner.
					if (mySeq === seq) {
						setLoading?.(false);
						inFlightKey = null;
						inFlightPromise = null;
					}
				}
			})();
			// Assigned after the IIFE, which is safe rather than racy: the async
			// body runs synchronously only as far as its first await, so the
			// `finally` above cannot run before this line.
			//
			// ONE TICK OF NON-EQUIVALENCE with the hand-rolled code this replaced
			// (codex round 2, TASK-2947), named because it is real. Both old
			// versions ran commit and `finally` inside ONE async function, so
			// cleanup followed the commit with no tick between. Here the commit is
			// inside `work`, and `await work(...)` costs a microtask — crossing an
			// async function boundary always does, so no callback-shaped extraction
			// can avoid it. For that tick the spinner reads true and the slot stays
			// published after the data has landed.
			//
			// Unobservable as WRONG, for a reason that is a property of the callers
			// rather than luck: each checks its own committed state BEFORE asking
			// about the slot (`ensureCollections` returns early on
			// `collectionsWorkspace === ws`, `recoverIfMissing` on a non-empty
			// `workspaces`), and anything awaiting the returned promise resumes
			// after the `finally` in both versions. A joiner that does land in the
			// window gets a promise resolving against the committed result. The leg
			// in `singleFlight.test.ts` pins that property.
			inFlightPromise = promise;
			return promise;
		},
	};
}
