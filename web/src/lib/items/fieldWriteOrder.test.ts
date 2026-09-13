// Ordering for concurrent whole-list writes to ONE field (PLAN-2857 U4, codex
// round 8, R8-1).
//
// The defect this file is the instrument for is not a display glitch: the item
// pane's 409 refetch-and-retry RE-SENDS a body the user has already replaced,
// and the server keeps it. Remove A then B from [A,B,C] and the second write
// commits first; the first write then conflicts, refetches, replays [B,C], and
// A is back on the row with no error raised anywhere.
//
// Two things are tested here, and the second is the one that makes the first
// worth reading: the guard, and a pair of NEGATIVE CONTROLS that run the same
// interleaving against each single-counter alternative. Both alternatives lose
// the update, which is why there are two counters (CONVE-34 — a green
// instrument is evidence only once it has been shown able to go red).
import { describe, it, expect, vi } from 'vitest';
import { WriteOrder, submitOrderedOCC, fieldWriteTarget, type WriteTicket } from './fieldWriteOrder';

const TARGET = fieldWriteTarget('item-1', 'colors');

/** A minimal `update_conflict`, told apart by identity rather than by shape. */
class Conflict extends Error {}
const isConflict = (e: unknown) => e instanceof Conflict;

type Row = { value: string[]; updated_at: string };

/**
 * The row, with the server's actual concurrency rule: a write carrying a stale
 * `updated_at` is refused, and a write that lands stamps a new one.
 */
class FakeRow {
	value: string[] = ['A', 'B', 'C'];
	updated_at = 't0';
	private n = 0;
	/** The last conflict thrown, so a test can assert the SAME object came back. */
	lastConflict: Conflict | null = null;
	patch(expected: string, next: string[]): Row {
		if (expected !== this.updated_at) {
			this.lastConflict = new Conflict('update_conflict');
			throw this.lastConflict;
		}
		this.value = [...next];
		this.updated_at = `t${++this.n}`;
		return this.read();
	}
	read(): Row {
		return { value: [...this.value], updated_at: this.updated_at };
	}
}

function deferred<T = void>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((r) => (resolve = r));
	return { promise, resolve };
}

describe('WriteOrder — the two counters answer two different questions', () => {
	it('a newer dispatch supersedes an older ticket, and nothing else does', () => {
		const order = new WriteOrder();
		const first = order.take(TARGET);
		expect(order.superseded(first)).toBe(false);
		const second = order.take(TARGET);
		expect(order.superseded(first)).toBe(true);
		expect(order.superseded(second)).toBe(false);
	});

	it('scopes supersession to ONE target — a write to another field is not a newer intent', () => {
		// The quantifier is the whole content of this guard: `fields_patch`
		// carries a single key and the server MERGES it, so two writes to
		// DIFFERENT fields are independent and neither supersedes the other.
		// A guard written per-item rather than per-field would silently abandon
		// a status change because a relation chip was clicked.
		const order = new WriteOrder();
		const colors = order.take(fieldWriteTarget('item-1', 'colors'));
		order.take(fieldWriteTarget('item-1', 'status'));
		order.take(fieldWriteTarget('item-2', 'colors'));
		expect(order.superseded(colors)).toBe(false);
	});

	it('claim is newest-wins among responses that resolve out of order', () => {
		const order = new WriteOrder();
		const first = order.take(TARGET);
		const second = order.take(TARGET);
		expect(order.claim(second)).toBe(true);
		// The older response is not damaged, not slow, not wrong — it is simply
		// describing a row that has already been replaced on screen.
		expect(order.claim(first)).toBe(false);
	});

	it('claim admits an older response when nothing newer has written yet', () => {
		// The CONTROL for the leg above: if claim refused on the strength of a
		// newer DISPATCH it would discard a good response whose successor may
		// still fail, and the view would keep a value the server no longer has.
		const order = new WriteOrder();
		const first = order.take(TARGET);
		order.take(TARGET);
		expect(order.claim(first)).toBe(true);
	});

	it('claim is once per ticket — a second call answers false', () => {
		const order = new WriteOrder();
		const t = order.take(TARGET);
		expect(order.claim(t)).toBe(true);
		expect(order.claim(t)).toBe(false);
	});
});

describe('submitOrderedOCC — the retry never replays a superseded body', () => {
	/**
	 * The R8-1 interleaving, driven by hand.
	 *
	 * Both writes are dispatched before either answers, so both carry `t0` —
	 * which is what the pane really does, since `item.updated_at` only moves
	 * when a response lands.
	 */
	async function runTwoRemovals(order: WriteOrder, tickets: { first: WriteTicket; second: WriteTicket }) {
		const row = new FakeRow();
		const firstSend = deferred();
		const firstRefetch = deferred();

		const first = submitOrderedOCC<Row>({
			order,
			ticket: tickets.first,
			maxRetries: 2,
			initialExpected: 't0',
			send: (expected) => firstSend.promise.then(() => row.patch(expected, ['B', 'C'])),
			refetch: () => firstRefetch.promise.then(() => row.read()),
			isConflict,
			stillCurrent: () => true
		});
		const firstSettled = first.then(
			(v) => ({ ok: true as const, v }),
			(e) => ({ ok: false as const, e })
		);

		// The SECOND removal commits first — the race this is all about.
		await submitOrderedOCC<Row>({
			order,
			ticket: tickets.second,
			maxRetries: 2,
			initialExpected: 't0',
			send: (expected) => Promise.resolve(row.patch(expected, ['C'])),
			refetch: () => Promise.resolve(row.read()),
			isConflict,
			stillCurrent: () => true
		});
		expect(row.value).toEqual(['C']);

		// Now let the first write's send answer: its `t0` is stale, so a 409.
		firstSend.resolve();
		firstRefetch.resolve();
		const outcome = await firstSettled;
		return { row, outcome };
	}

	it('abandons the older write rather than putting the removed element back', async () => {
		const order = new WriteOrder();
		const first = order.take(TARGET);
		const second = order.take(TARGET);
		const { row, outcome } = await runTwoRemovals(order, { first, second });

		expect(row.value).toEqual(['C']);
		expect(outcome.ok).toBe(false);
		// It fails with the CONFLICT it was given — the same object, not merely
		// something of the same class. The caller tells a superseded write apart
		// by asking the question this asked, and a synthesised error of the
		// right shape would satisfy an `instanceof` while losing whatever the
		// server said (round 8 enumeration: these assertions checked class and
		// message, so swapping in a different Conflict stayed green).
		expect(outcome.ok === false && outcome.e).toBe(row.lastConflict);
	});

	it('NEGATIVE CONTROL: with no supersession question at all, A comes back', async () => {
		// The in-test mutant. Everything else is identical, so a green result
		// above means the guard — not the fixture — is what keeps A off the row.
		class NoGuard extends WriteOrder {
			superseded(): boolean {
				return false;
			}
		}
		const order = new NoGuard();
		const first = order.take(TARGET);
		const second = order.take(TARGET);
		const { row, outcome } = await runTwoRemovals(order, { first, second });

		expect(row.value).toEqual(['B', 'C']);
		expect(outcome.ok).toBe(true);
	});

	it('NEGATIVE CONTROL: counting APPLIES instead of dispatches loses it too', async () => {
		// Why there are two counters rather than one. The high-water mark of what
		// WROTE is the right question for "may this response replace the screen",
		// and the wrong one here: at the moment the older write conflicts, the
		// newer write's response has not been applied by the caller yet, so an
		// applied-only guard sees nothing newer and replays the stale list.
		class AppliedOnly extends WriteOrder {
			superseded(ticket: WriteTicket): boolean {
				return ticket.n <= this.appliedFor(ticket.target);
			}
		}
		const order = new AppliedOnly();
		const first = order.take(TARGET);
		const second = order.take(TARGET);
		const { row } = await runTwoRemovals(order, { first, second });

		expect(row.value).toEqual(['B', 'C']);
	});

	it('still retries the ordinary conflict — a write nothing has superseded', async () => {
		// The other half of the control pair: the guard must not have turned the
		// OCC retry off. Somebody ELSE moves the row, and this write re-applies
		// its own value against the fresh one, which is BUG-2273 working.
		//
		// AND IT DESCRIBES A KNOWN LOSS, deliberately. The third party added D;
		// the retry re-sends the list it computed BEFORE that, so D disappears
		// although the local gesture only removed A. This leg asserts what the
		// code does, not what it should do — closing that needs the retry to
		// re-apply the GESTURE to the fresh list rather than replay its result,
		// which is a write-contract change and is filed as BUG-3038. Invert this
		// assertion when that lands; it is not an endorsement.
		const row = new FakeRow();
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		row.patch('t0', ['A', 'B', 'C', 'D']); // a third party, before we send

		const sends: string[] = [];
		const result = await submitOrderedOCC<Row>({
			order,
			ticket,
			maxRetries: 2,
			initialExpected: 't0',
			send: (expected) => {
				sends.push(expected);
				return Promise.resolve(row.patch(expected, ['B', 'C']));
			},
			refetch: () => Promise.resolve(row.read()),
			isConflict,
			stillCurrent: () => true
		});

		expect(sends).toEqual(['t0', 't1']);
		expect(result.value).toEqual(['B', 'C']);
		expect(row.value).toEqual(['B', 'C']);
	});

	it('reports the freshest row it saw while retrying', async () => {
		const row = new FakeRow();
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		row.patch('t0', ['Z']);
		const onRefetched = vi.fn();

		await submitOrderedOCC<Row>({
			order,
			ticket,
			maxRetries: 2,
			initialExpected: 't0',
			send: (expected) => Promise.resolve(row.patch(expected, ['B', 'C'])),
			refetch: () => Promise.resolve(row.read()),
			isConflict,
			stillCurrent: () => true,
			onRefetched
		});

		expect(onRefetched).toHaveBeenCalledTimes(1);
		expect(onRefetched.mock.calls[0][0].value).toEqual(['Z']);
	});

	it('gives up after maxRetries rather than spinning on a hot row', async () => {
		const row = new FakeRow();
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		let sends = 0;
		let reads = 0;

		await expect(
			submitOrderedOCC<Row>({
				order,
				ticket,
				maxRetries: 2,
				initialExpected: 'stale-0',
				send: (expected) => {
					sends++;
					return Promise.resolve(row.patch(expected, ['B', 'C']));
				},
				// A row somebody else is writing continuously: every value we
				// read is stale again by the time our re-send reaches it.
				refetch: () => Promise.resolve({ value: [], updated_at: `stale-${++reads}` }),
				isConflict,
				stillCurrent: () => true
			})
		).rejects.toBeInstanceOf(Conflict);

		expect(sends).toBe(3); // the first send plus two retries
	});

	it('does not RE-SEND once the view moves on DURING the refetch', async () => {
		// Round 8 enumeration, reproduced. The post-refetch check asked only
		// about supersession, so switching items while the GET was in flight
		// still produced another PATCH at the item the pane had just left —
		// invisible, because the caller then discards the response, and landing
		// on the row all the same.
		//
		// This is the transition the leg below cannot see: there `stillCurrent`
		// is false from the start, so the retry never begins.
		const row = new FakeRow();
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		row.patch('t0', ['A', 'B', 'C', 'D']); // a third party moved it first
		let current = true;
		let sends = 0;

		await expect(
			submitOrderedOCC<Row>({
				order,
				ticket,
				maxRetries: 2,
				initialExpected: 't0',
				send: (expected) => {
					sends++;
					return Promise.resolve(row.patch(expected, ['B', 'C']));
				},
				refetch: () => {
					current = false; // the user opens another item, right here
					return Promise.resolve(row.read());
				},
				isConflict,
				stillCurrent: () => current
			})
		).rejects.toBeInstanceOf(Conflict);

		// PRECONDITION: the first send really happened, so "one send" is a
		// refusal to RETRY rather than a refusal to start.
		expect(sends).toBe(1);
		expect(row.value).toEqual(['A', 'B', 'C', 'D']);
	});

	it('does not retry once the view has moved on', async () => {
		const row = new FakeRow();
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		const refetch = vi.fn(() => Promise.resolve(row.read()));

		await expect(
			submitOrderedOCC<Row>({
				order,
				ticket,
				maxRetries: 2,
				initialExpected: 'stale',
				send: (expected) => Promise.resolve(row.patch(expected, ['B', 'C'])),
				refetch,
				isConflict,
				stillCurrent: () => false
			})
		).rejects.toBeInstanceOf(Conflict);

		expect(refetch).not.toHaveBeenCalled();
	});

	it('lets a non-conflict failure through untouched', async () => {
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		const refetch = vi.fn(() => Promise.resolve({ value: [], updated_at: 't9' }));
		// By IDENTITY: `rejects.toThrow('network')` passes against any error
		// carrying that word, including one this function made up.
		const failure = new Error('network');

		await expect(
			submitOrderedOCC<Row>({
				order,
				ticket,
				maxRetries: 2,
				initialExpected: 't0',
				send: () => Promise.reject(failure),
				refetch,
				isConflict,
				stillCurrent: () => true
			})
		).rejects.toBe(failure);

		expect(refetch).not.toHaveBeenCalled();
	});

	it('abandons when the newer write is dispatched DURING the refetch', async () => {
		// The second of the two supersession checks. The refetch is an await
		// wide enough for a click to land inside it, and a check taken only
		// before it would have already passed.
		const row = new FakeRow();
		const order = new WriteOrder();
		const ticket = order.take(TARGET);
		row.patch('t0', ['A', 'B', 'C', 'D']);
		let sends = 0;

		await expect(
			submitOrderedOCC<Row>({
				order,
				ticket,
				maxRetries: 2,
				initialExpected: 't0',
				send: (expected) => {
					sends++;
					return Promise.resolve(row.patch(expected, ['B', 'C']));
				},
				refetch: () => {
					order.take(TARGET); // the user clicks Remove again, right here
					return Promise.resolve(row.read());
				},
				isConflict,
				stillCurrent: () => true
			})
		).rejects.toBeInstanceOf(Conflict);

		expect(sends).toBe(1);
		expect(row.value).toEqual(['A', 'B', 'C', 'D']);
	});
});
