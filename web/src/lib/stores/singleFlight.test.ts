import { describe, expect, it } from 'vitest';
import { createKeyedSingleFlight } from './singleFlight';

/**
 * TASK-2947 — the keyed single-flight loader's own contract.
 *
 * The two stores that use it have their own tests for what they do with it
 * (`collectionsEnsure`, `workspaceRecovery`, `workspaceLoadAllOrdering`). These
 * legs pin the RULE, once, in the place it now has a single definition — which
 * is the whole point of extracting it: the drift these tests exist to catch is
 * a future edit to the primitive, not to a caller.
 *
 * The CONTROL leg that matters most is "run never coalesces". An over-eager
 * extraction that made `run` join an in-flight request would look like a
 * tidy-up and would quietly serve pre-change data to the eighteen
 * `loadCollections` call sites that are reacting to a change they already know
 * about.
 */

function deferred<T = void>() {
	let resolve!: (value: T) => void;
	let reject!: (reason: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

describe('createKeyedSingleFlight', () => {
	it('CONTROL — `run` ALWAYS issues; it never coalesces onto an in-flight run', async () => {
		const flight = createKeyedSingleFlight<string>();
		let calls = 0;
		const gate = deferred();

		const a = flight.run('ws', async () => {
			calls++;
			await gate.promise;
		});
		const b = flight.run('ws', async () => {
			calls++;
			await gate.promise;
		});

		expect(calls).toBe(2);
		gate.resolve();
		await Promise.all([a, b]);
	});

	it('publishes the in-flight run for its OWN key, and not for another', async () => {
		const flight = createKeyedSingleFlight<string>();
		const gate = deferred();

		const run = flight.run('alpha', async () => {
			await gate.promise;
		});

		expect(flight.inFlightFor('alpha')).toBe(run);
		expect(flight.inFlightFor('beta')).toBeNull();

		gate.resolve();
		await run;
	});

	it('releases the slot once the run settles, so a later joiner is not handed a dead promise', async () => {
		const flight = createKeyedSingleFlight<string>();
		const gate = deferred();

		const run = flight.run('alpha', async () => {
			await gate.promise;
		});
		gate.resolve();
		await run;

		expect(flight.inFlightFor('alpha')).toBeNull();
	});

	it('releases the slot when the run REJECTS, and propagates the rejection', async () => {
		const flight = createKeyedSingleFlight<string>();

		const run = flight.run('alpha', async () => {
			throw new Error('down');
		});

		await expect(run).rejects.toThrow('down');
		expect(flight.inFlightFor('alpha')).toBeNull();
	});

	it('`isLatest` goes false for a run a newer one superseded', async () => {
		const flight = createKeyedSingleFlight<string>();
		const gate = deferred();
		let firstStillLatest: boolean | null = null;

		const first = flight.run('alpha', async ({ isLatest }) => {
			await gate.promise;
			firstStillLatest = isLatest();
		});
		const second = flight.run('beta', async ({ isLatest }) => {
			expect(isLatest()).toBe(true);
		});

		gate.resolve();
		await Promise.all([first, second]);
		expect(firstStillLatest).toBe(false);
	});

	it('an OLDER run settling late does not clear the NEWER one’s slot', async () => {
		const flight = createKeyedSingleFlight<string>();
		const older = deferred();
		const newer = deferred();

		const first = flight.run('alpha', async () => {
			await older.promise;
		});
		const second = flight.run('beta', async () => {
			await newer.promise;
		});

		older.resolve();
		await first;

		// The newer run still owns the slot; a joiner asking for `beta` must get
		// its promise rather than null.
		expect(flight.inFlightFor('beta')).toBe(second);

		newer.resolve();
		await second;
		expect(flight.inFlightFor('beta')).toBeNull();
	});

	it('the loading flag is owned by the LATEST run', async () => {
		const seen: boolean[] = [];
		const flight = createKeyedSingleFlight<string>({ setLoading: (v) => seen.push(v) });
		const older = deferred();
		const newer = deferred();

		const first = flight.run('alpha', async () => {
			await older.promise;
		});
		const second = flight.run('beta', async () => {
			await newer.promise;
		});

		older.resolve();
		await first;
		// Two starts, and NO false yet — the older run settling must not flip the
		// spinner off while the newer one is still running.
		expect(seen).toEqual([true, true]);

		newer.resolve();
		await second;
		expect(seen).toEqual([true, true, false]);
	});
});
