// Lane reorder planning and persistence (BUG-3259).
//
// A drag or a card-menu move hands over a whole lane in its new on-screen
// order. Every card in it gets a `sort_order` (a dense INTEGER per lane, ties
// broken by created_at), so moving one card renumbers its neighbours. A
// neighbour the caller may only VIEW (an item grant beats a collection grant)
// cannot be written: the server refuses it 403.
//
// `planLaneOrder` decides which writes to send so that the stored order sorts
// to what the user sees, leaving view-only cards where they are.
// `persistReorder` sends them and undoes its optimistic local writes when one
// is refused.

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
 * Plan the `sort_order` writes for a lane given in its new on-screen order.
 *
 * With every card editable this is the dense renumber the views have always
 * sent (index = sort_order), minus cards already there, so nothing changes for
 * an account without a view-only neighbour.
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
	canEdit: (card: T) => boolean
): LanePlan {
	const editable = lane.map((c) => canEdit(c));
	if (editable.every(Boolean)) {
		const writes = lane.flatMap((c, i) => (c.sort_order === i ? [] : [{ id: c.id, sort_order: i }]));
		return { ok: true, writes };
	}

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
		let v: number;
		if (card.sort_order >= lower && card.sort_order <= upper) v = card.sort_order;
		else if (lower === -Infinity) v = Math.min(card.sort_order, upper);
		else v = lower;
		if (v !== card.sort_order) writes.push({ id: card.id, sort_order: v });
		prev = v;
		prevFrozen = false;
	}
	return { ok: true, writes };
}

export interface PersistDeps<T extends OrderedCard> {
	/** The card as it stands before the reorder, or undefined if unknown. */
	original: (id: string) => T | undefined;
	/** Write the optimistic row locally, before the request. */
	applyLocal: (card: T) => void;
	/** Put the original row back locally after a refusal. */
	restoreLocal: (card: T) => void;
	/** Persist one write; resolves to the server's row. */
	send: (write: OrderWrite) => Promise<T>;
	/** Settle the server's row locally. Returns false to stop (identity changed). */
	settle: (row: T) => boolean;
}

/**
 * Apply `writes` optimistically, then persist them one at a time (SQLite takes
 * one writer). A refusal stops the loop and restores every card not yet
 * confirmed, the refused one included, to its original row, so the local order
 * never shows a move the server did not store. Returns whether all landed.
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
	for (let i = 0; i < pending.length; i++) {
		let row: T;
		try {
			row = await deps.send(pending[i].write);
		} catch {
			for (const { original } of pending.slice(i)) deps.restoreLocal(original);
			return false;
		}
		if (!deps.settle(row)) return false;
	}
	return true;
}
