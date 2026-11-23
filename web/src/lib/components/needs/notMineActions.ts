import { api, isUpdateConflictError } from '$lib/api/client';
import { loadGiveTargets, type GiveTarget, type GiveTargets } from './NotMinePicker.svelte';
import type { Item, ItemUpdate, NeedsRow } from '$lib/types';

// "Not mine" and "Not mine, give to…" (TASK-4643), shared by /needs and Home
// (TASK-4686). Plain functions with no state of their own: each caller keeps
// its own busy guard and list bookkeeping. The marker goes back exactly as
// the server built it for the row; the client adds nothing, and `source` is
// the server's to set.

export const errMsg = (e: unknown, fallback = 'That did not go through') => (e instanceof Error ? e.message : fallback);

/** What a give-to changed, so Undo can put back only those columns. */
export interface Gave {
	kind: GiveTarget['kind'];
	prev: Item;
	patched: Item;
}

export type GiveResult =
	| { ok: true; gave: Gave }
	/** The reassignment was refused or never sent: nothing was written. */
	| { ok: false; stage: 'reassign'; conflict: boolean; error: unknown; message: string }
	/** The reassignment stands; only the mark failed. */
	| { ok: false; stage: 'mark'; error: unknown; message: string };

export type UndoResult =
	/** The mark is still there; nothing changed. */
	| { ok: false; message: string }
	/** The mark is gone. `notice` says the reassignment was kept. */
	| { ok: true; notice?: { message: string; type: 'info' | 'error' } };

export const markedMessage = (ref: string) => `${ref} marked not mine`;
export const gaveMessage = (ref: string, label: string) => `${ref} given to ${label} and marked not mine`;

/**
 * Roles and members per workspace, loaded once per cache. A load with a
 * failed half is not kept, so the next open tries again.
 */
export function createGiveTargetCache(): (ws: string) => Promise<GiveTargets> {
	const cache = new Map<string, Promise<GiveTargets>>();
	return (ws: string) => {
		let p = cache.get(ws);
		if (!p) {
			p = loadGiveTargets(ws);
			cache.set(ws, p);
			p.then(
				(t) => {
					if (t.rolesError || t.peopleError) cache.delete(ws);
				},
				() => cache.delete(ws)
			);
		}
		return p;
	};
}

export function markNotMine(row: NeedsRow) {
	return api.needs.notMine(row.workspace.slug, row.item.ref, { marker: row.marker });
}

/**
 * Give the item to someone else, then mark it not mine. The reassignment
 * goes first and carries the row's seq, so a change made since the list was
 * read refuses it and nothing is written. The previous assignee is read
 * just before, for Undo: the row never names it.
 */
export async function giveTo(row: NeedsRow, target: GiveTarget): Promise<GiveResult> {
	const ws = row.workspace.slug;
	const ref = row.item.ref;
	let prev: Item;
	let patched: Item;
	try {
		prev = await api.items.get(ws, ref);
		// The row's seq, not the GET's: a change since the list was read must
		// refuse the reassignment.
		const expected_seq = row.item.seq;
		const body: ItemUpdate =
			target.kind === 'role'
				? { agent_role_id: target.id, clear_assigned_user: true, expected_seq }
				: { assigned_user_id: target.id, expected_seq };
		patched = await api.items.update(ws, ref, body);
	} catch (e) {
		const conflict = isUpdateConflictError(e);
		return {
			ok: false,
			stage: 'reassign',
			conflict,
			error: e,
			message: conflict ? 'The item changed since you loaded the list. Reload and try again.' : errMsg(e)
		};
	}
	try {
		await api.needs.notMine(ws, ref, { marker: row.marker });
	} catch (e) {
		return {
			ok: false,
			stage: 'mark',
			error: e,
			message: `Gave ${ref} to ${target.label}, but marking it not mine failed: ${errMsg(e)}`
		};
	}
	return { ok: true, gave: { kind: target.kind, prev, patched } };
}

/**
 * Undo: delete the mark, then put back the columns a give-to changed,
 * guarded by the seq the give's PATCH returned. If the item changed after
 * that, the reassignment is kept and the result says so.
 */
export async function undoNotMine(row: NeedsRow, gave?: Gave): Promise<UndoResult> {
	const ws = row.workspace.slug;
	const ref = row.item.ref;
	try {
		await api.needs.undoNotMine(ws, ref);
	} catch (e) {
		return { ok: false, message: `Could not undo ${ref}: ${errMsg(e)}` };
	}
	if (!gave) return { ok: true };
	const body: ItemUpdate = { expected_seq: gave.patched.seq };
	if (gave.kind === 'role') {
		if (gave.prev.agent_role_id) body.agent_role_id = gave.prev.agent_role_id;
		else body.clear_agent_role = true;
	}
	if (gave.prev.assigned_user_id) body.assigned_user_id = gave.prev.assigned_user_id;
	else body.clear_assigned_user = true;
	try {
		await api.items.update(ws, ref, body);
	} catch (e) {
		if (isUpdateConflictError(e)) return { ok: true, notice: { message: 'The item changed since; the reassignment was kept.', type: 'info' } };
		return {
			ok: true,
			notice: { message: `${ref} is no longer marked not mine, but the reassignment was kept: ${errMsg(e)}`, type: 'error' }
		};
	}
	return { ok: true };
}
