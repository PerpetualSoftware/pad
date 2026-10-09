// Lane reorder planning and persistence (BUG-3259).
//
// A drag or a card-menu move hands over a whole lane in its new on-screen
// order. Every card has a `sort_order`, an INTEGER the lane sorts by (ties
// broken by created_at). Values are kept GAPPED (TASK-3525): a card is written
// SORT_GAP away from its neighbour at an end, and at the midpoint between its
// neighbours in the middle, so a move is one write until a gap closes. Every
// create path stores 0, so a lane nobody has dragged is all ties, and lanes
// ordered before TASK-3525 are dense (0, 1, 2, ...): both are spaced out
// lazily, by the first drag that needs room. A neighbour the caller may only
// VIEW (an item grant beats a collection grant) cannot be written: the server
// refuses it 403.
//
// `planLaneOrder` decides which writes to send so that the stored order sorts
// to what the user sees, leaving view-only cards where they are.
// `persistReorder` sends them in ONE all-or-nothing request (TASK-3517) and
// undoes its optimistic local writes when it is refused.
//
// The server stores whatever integers it is sent (PUT /items/sort-order), so
// nothing changes for an API writer: any values that sort in the intended
// order work, dense, gapped or negative.

/** The spacing a card is written at, away from its neighbour (TASK-3525). */
export const SORT_GAP = 1024;

/**
 * Values stay within ±SORT_LIMIT (Postgres stores sort_order as int4,
 * ±2^31). A plan that would leave it re-spaces the whole lane around 0.
 */
export const SORT_LIMIT = 2 ** 30;

export interface OrderedCard {
	id: string;
	sort_order: number;
}

export interface OrderWrite {
	id: string;
	sort_order: number;
}

export type LanePlan = { ok: true; writes: OrderWrite[] } | { ok: false };

/**
 * The fewest `sort_order` writes that make an all-editable lane, given in its
 * new on-screen order, sort strictly in that order (TASK-2230, TASK-3525).
 *
 * Every written card also writes a 'reordered' activity row (#1933), so the
 * plan writes as few cards as it can:
 *   - it keeps the longest run of cards whose stored values already increase
 *     along the lane, and writes every other card into the gap between its
 *     kept neighbours: SORT_GAP below the first or above the last at an end,
 *     spread evenly between them in the middle. Moving one card in a gapped
 *     lane is ONE write;
 *   - when a gap has no integer room left (a dense lane from before TASK-3525,
 *     a lane of ties, or a gap closed by repeated drops in one spot), it shifts
 *     the SHORTER side instead, as TASK-2230 did, but writes the shifted cards
 *     SORT_GAP apart, so the drops after it are single writes again;
 *   - a plan that would leave ±SORT_LIMIT re-spaces the whole lane around 0.
 * A lane already strictly increasing writes nothing.
 */
export function fewestWrites<T extends OrderedCard>(lane: readonly T[]): OrderWrite[] {
	const plan = gapWrites(lane) ?? shorterSideShift(lane);
	if (plan.some((w) => Math.abs(w.sort_order) > SORT_LIMIT)) return respaceAll(lane);
	return plan;
}

/** Indices of a longest strictly increasing run of values (patience sort). */
function longestIncreasing(values: readonly number[]): boolean[] {
	const tails: number[] = []; // index of the last element of the best run of each length
	const prevOf: number[] = new Array(values.length).fill(-1);
	for (let i = 0; i < values.length; i++) {
		let lo = 0;
		let hi = tails.length;
		while (lo < hi) {
			const mid = (lo + hi) >> 1;
			if (values[tails[mid]] < values[i]) lo = mid + 1;
			else hi = mid;
		}
		if (lo > 0) prevOf[i] = tails[lo - 1];
		tails[lo] = i;
	}
	const keep: boolean[] = new Array(values.length).fill(false);
	for (let i = tails.length ? tails[tails.length - 1] : -1; i >= 0; i = prevOf[i]) keep[i] = true;
	return keep;
}

/**
 * Keep the longest increasing run and write every other card into the gap
 * between its kept neighbours. Null when some gap has no integer room.
 */
function gapWrites<T extends OrderedCard>(lane: readonly T[]): OrderWrite[] | null {
	const n = lane.length;
	const keep = longestIncreasing(lane.map((c) => c.sort_order));
	const writes: OrderWrite[] = [];
	let k = 0;
	while (k < n) {
		if (keep[k]) {
			k++;
			continue;
		}
		const start = k;
		while (k < n && !keep[k]) k++;
		const m = k - start; // cards start..k-1 go between start-1 and k
		const below = start > 0 ? lane[start - 1].sort_order : -Infinity;
		const above = k < n ? lane[k].sort_order : Infinity;
		for (let j = 0; j < m; j++) {
			let v: number;
			if (below === -Infinity) v = above - SORT_GAP * (m - j);
			else if (above === Infinity) v = below + SORT_GAP * (j + 1);
			else {
				// m values strictly between below and above need above - below > m.
				if (above - below <= m) return null;
				v = below + Math.floor(((above - below) * (j + 1)) / (m + 1));
			}
			const card = lane[start + j];
			if (v !== card.sort_order) writes.push({ id: card.id, sort_order: v });
		}
	}
	return writes;
}

/**
 * TASK-2230's shorter-side shift, writing the shifted cards SORT_GAP apart:
 * the cheaper of pushing the cards before each violation down (walking right
 * to left) or the cards after it up (walking left to right). A card whose
 * value already sorts where it is keeps it.
 */
function shorterSideShift<T extends OrderedCard>(lane: readonly T[]): OrderWrite[] {
	const n = lane.length;
	const down: OrderWrite[] = [];
	let next = Infinity;
	for (let k = n - 1; k >= 0; k--) {
		const own = lane[k].sort_order;
		const v = own < next ? own : next - SORT_GAP;
		if (v !== own) down.push({ id: lane[k].id, sort_order: v });
		next = v;
	}
	const up: OrderWrite[] = [];
	let prev = -Infinity;
	for (let k = 0; k < n; k++) {
		const own = lane[k].sort_order;
		const v = own > prev ? own : prev + SORT_GAP;
		if (v !== own) up.push({ id: lane[k].id, sort_order: v });
		prev = v;
	}
	return down.length <= up.length ? down.reverse() : up;
}

/** Every card SORT_GAP apart, centred on 0, in on-screen order. */
function respaceAll<T extends OrderedCard>(lane: readonly T[]): OrderWrite[] {
	const mid = Math.floor(lane.length / 2);
	const writes: OrderWrite[] = [];
	lane.forEach((card, k) => {
		const v = (k - mid) * SORT_GAP;
		if (v !== card.sort_order) writes.push({ id: card.id, sort_order: v });
	});
	return writes;
}

/**
 * Plan the `sort_order` writes for a lane given in its new on-screen order.
 *
 * `movedId` names the card the user moved, when the caller knows it. If
 * writing that card alone is enough, the plan is that one write (see
 * `moverOnly`); otherwise the whole lane is planned as below.
 *
 * With every card editable this is `fewestWrites`: the cards that must move to
 * make the stored order match, and no others (TASK-2230; it used to be the
 * dense renumber, minus cards already there).
 *
 * With a frozen (view-only) card present, frozen cards keep their stored value
 * and each editable card gets a value strictly between the previous card's and
 * the next frozen card's, keeping its own value when that already fits. The
 * values are then strictly increasing along the lane, except that two frozen
 * cards with nothing moved between them may tie, which is the order the
 * tie-break already showed. Returns `{ ok: false }` when there is no integer
 * room, for example an editable card dropped between frozen cards at 0 and 1.
 */
export function planLaneOrder<T extends OrderedCard>(
	lane: readonly T[],
	canEdit: (card: T) => boolean,
	movedId?: string
): LanePlan {
	if (movedId !== undefined) {
		const only = moverOnly(lane, canEdit, movedId);
		if (only) return { ok: true, writes: only };
	}
	const editable = lane.map((c) => canEdit(c));
	if (editable.every(Boolean)) return { ok: true, writes: fewestWrites(lane) };

	const writes: OrderWrite[] = [];
	// The value assigned to the previous card, and whether it was frozen.
	let prev = -Infinity;
	let prevFrozen = false;
	for (let k = 0; k < lane.length; k++) {
		const card = lane[k];
		if (!editable[k]) {
			const v = card.sort_order;
			// A frozen card must not sort before what precedes it. It may tie only
			// with a frozen card directly before it (nothing moved between them).
			if (v < prev || (v === prev && !prevFrozen)) return { ok: false };
			prev = v;
			prevFrozen = true;
			continue;
		}
		// Room: strictly below the next frozen card, leaving one value for each
		// editable card between this one and it.
		let upper = Infinity;
		for (let j = k + 1; j < lane.length; j++) {
			if (!editable[j]) {
				upper = lane[j].sort_order - (j - k);
				break;
			}
		}
		const lower = prev + 1;
		if (lower > upper) return { ok: false };
		// A card that must move is written with room around it (TASK-3525):
		// SORT_GAP past its neighbour at an open end, the middle of its window
		// between two bounds.
		let v: number;
		if (card.sort_order >= lower && card.sort_order <= upper) v = card.sort_order;
		else if (lower === -Infinity) v = upper - SORT_GAP;
		else if (upper === Infinity) v = prev + SORT_GAP;
		else v = lower + Math.floor((upper - lower) / 2);
		if (v !== card.sort_order) writes.push({ id: card.id, sort_order: v });
		prev = v;
		prevFrozen = false;
	}
	return { ok: true, writes };
}

/**
 * The card the user moved, written alone into the gap it was dropped in, when
 * that is enough: every other card already sorts where it stands (ties only
 * between view-only cards, as the tie-break showed them) and the gap has
 * integer room. Null otherwise, and the caller plans the whole lane.
 *
 * It exists because a one-write plan can often write EITHER card of a swap,
 * and each written card gets the 'reordered' activity row (#1933): the row
 * belongs on the card the user moved, not on its neighbour (TASK-3525).
 */
function moverOnly<T extends OrderedCard>(
	lane: readonly T[],
	canEdit: (card: T) => boolean,
	movedId: string
): OrderWrite[] | null {
	const k = lane.findIndex((c) => c.id === movedId);
	if (k < 0 || !canEdit(lane[k])) return null;
	let last: T | undefined;
	for (let i = 0; i < lane.length; i++) {
		if (i === k) continue;
		const card = lane[i];
		if (last) {
			const tieAllowed = !canEdit(last) && !canEdit(card);
			if (card.sort_order < last.sort_order || (card.sort_order === last.sort_order && !tieAllowed)) return null;
		}
		last = card;
	}
	const below = k > 0 ? lane[k - 1].sort_order : -Infinity;
	const above = k < lane.length - 1 ? lane[k + 1].sort_order : Infinity;
	const own = lane[k].sort_order;
	if (own > below && own < above) return [];
	let v: number;
	if (below === -Infinity) v = above - SORT_GAP;
	else if (above === Infinity) v = below + SORT_GAP;
	else if (above - below >= 2) v = below + Math.floor((above - below) / 2);
	else return null;
	if (Math.abs(v) > SORT_LIMIT) return null;
	return [{ id: movedId, sort_order: v }];
}

export interface PersistDeps<T extends OrderedCard> {
	/** The card as it stands before the reorder, or undefined if unknown. */
	original: (id: string) => T | undefined;
	/** Write the optimistic row locally, before the request. */
	applyLocal: (card: T) => void;
	/** Put the original row back locally after a refusal. */
	restoreLocal: (card: T) => void;
	/**
	 * Persist every write in one request (PUT /items/sort-order, TASK-3517).
	 * Resolves to each item's seq after the write; rejects when the server
	 * refused the request, in which case it wrote nothing.
	 */
	send: (writes: OrderWrite[]) => Promise<{ id: string; seq: number }[]>;
	/** Settle the stored row locally. Returns false to stop (identity changed). */
	settle: (row: T) => boolean;
}

/**
 * Apply `writes` optimistically, then persist them in ONE request, which the
 * server applies all or nothing (TASK-3517). On a refusal every card goes back
 * to its original row, so the local order never shows a move the server did
 * not store, and the server holds no partial order either: the per-row loop
 * this replaced left the writes before a refusal in place. Returns whether
 * the reorder landed.
 */
export async function persistReorder<T extends OrderedCard>(
	writes: readonly OrderWrite[],
	deps: PersistDeps<T>
): Promise<boolean> {
	const pending: { write: OrderWrite; original: T }[] = [];
	for (const write of writes) {
		const original = deps.original(write.id);
		if (!original) continue;
		deps.applyLocal({ ...original, sort_order: write.sort_order });
		pending.push({ write, original });
	}
	if (pending.length === 0) return true;
	let stored: { id: string; seq: number }[];
	try {
		stored = await deps.send(pending.map((p) => p.write));
	} catch {
		for (const { original } of pending) deps.restoreLocal(original);
		return false;
	}
	const seqs = new Map(stored.map((r) => [r.id, r.seq]));
	for (const { write, original } of pending) {
		const seq = seqs.get(write.id);
		const row = { ...original, sort_order: write.sort_order, ...(seq === undefined ? {} : { seq }) };
		if (!deps.settle(row)) return false;
	}
	return true;
}
