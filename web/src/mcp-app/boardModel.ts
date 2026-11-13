// Pure board state; the view owns polling, visibility and host communication.
export interface BoardItem {
	id: string;
	ref: string;
	title: string;
	status: string;
	priority: string;
	assignee: string;
	parent_ref: string;
	seq: number;
	updated_at: string;
}
export interface BoardColumn {
	status: string;
	terminal: boolean;
	abandoned: boolean;
	total: number;
	items: BoardItem[];
}
export interface Board {
	workspace: string;
	collection: string;
	columns: BoardColumn[];
	truncated: boolean;
	unplaced: number;
	cursor: string;
	access_epoch: string;
}
export interface BoardChanges {
	upserts: BoardItem[];
	removed_ids: string[];
	/** Live items now in other collections: dropped where shown, never a reason to resync. */
	departed_ids: string[];
	cursor: string;
	access_epoch: string;
	resync: boolean;
}

/**
 * Whether a delta must be answered with a fresh show instead of applyChanges.
 * An incomplete board (truncated columns, or items counted only as unplaced)
 * cannot tell which total an item it does not display was counted in. A
 * complete board displays every item, so an unknown upsert is new and an
 * unknown removal is a no-op. Departed ids never count: they only drop
 * items the board shows.
 */
export function needsResync(board: Board, changes: BoardChanges): boolean {
	if (!board.truncated && board.unplaced === 0) return false;
	const shown = new Set(board.columns.flatMap(column => column.items.map(item => item.id)));
	return changes.upserts.some(item => !shown.has(item.id)) || changes.removed_ids.some(id => !shown.has(id));
}

/** How long an incomplete board may go between full shows. */
export const INCOMPLETE_RESHOW_MS = 300_000;

/**
 * Whether an incomplete board is due a full show. Totals on such a board can
 * drift when an item it does not display leaves for another collection
 * (departed_ids cannot say where an item came from), so the drift is bounded
 * by re-reading the snapshot at least every INCOMPLETE_RESHOW_MS.
 */
export function incompleteReshowDue(board: Board, shownAt: number, now: number): boolean {
	return (board.truncated || board.unplaced > 0) && now - shownAt >= INCOMPLETE_RESHOW_MS;
}

/** Applies a delta to a board; the caller checks needsResync first. */
export function applyChanges(board: Board, changes: BoardChanges): Board {
	const columns = board.columns.map(column => ({ ...column, items: [...column.items] }));
	let unplaced = board.unplaced;
	const remove = (id: string) => {
		for (const column of columns) {
			const index = column.items.findIndex(item => item.id === id);
			if (index !== -1) { column.items.splice(index, 1); column.total--; }
		}
	};
	for (const id of [...changes.removed_ids, ...changes.departed_ids]) remove(id);
	for (const item of changes.upserts) {
		remove(item.id);
		const column = columns.find(column => column.status === item.status);
		if (column) { column.items.push(item); column.total++; }
		else unplaced++;
	}
	return { ...board, columns, unplaced, cursor: changes.cursor, access_epoch: changes.access_epoch };
}

export function nextDelay(prev: number, rateLimited: boolean): number {
	return rateLimited ? Math.min(Math.max(prev, 10_000) * 2, 120_000) : 10_000;
}

export function shouldPoll({ displayMode, visible }: { displayMode?: string; visible: boolean }): boolean {
	return displayMode === 'fullscreen' && visible;
}

const isObject = (x: unknown): x is Record<string, unknown> => x !== null && typeof x === 'object' && !Array.isArray(x);
const str = (x: unknown): string => typeof x === 'string' ? x : '';

// Tool results are untrusted input to the view: these keep only well-formed
// rows and return null for anything that is not the expected shape.
function parseItem(x: unknown): BoardItem | null {
	if (!isObject(x) || typeof x.id !== 'string' || x.id === '' || typeof x.seq !== 'number') return null;
	return {
		id: x.id, ref: str(x.ref), title: str(x.title), status: str(x.status), priority: str(x.priority),
		assignee: str(x.assignee), parent_ref: str(x.parent_ref), seq: x.seq, updated_at: str(x.updated_at)
	};
}
const parseItems = (x: unknown): BoardItem[] =>
	Array.isArray(x) ? x.map(parseItem).filter((item): item is BoardItem => item !== null) : [];

export function parseBoard(x: unknown): Board | null {
	if (!isObject(x) || !Array.isArray(x.columns) || typeof x.cursor !== 'string' || typeof x.access_epoch !== 'string') return null;
	if (typeof x.workspace !== 'string' || typeof x.collection !== 'string') return null;
	const columns: BoardColumn[] = [];
	for (const column of x.columns) {
		if (!isObject(column) || typeof column.status !== 'string') return null;
		const items = parseItems(column.items);
		const total = typeof column.total === 'number' ? column.total : items.length;
		columns.push({ status: column.status, terminal: column.terminal === true, abandoned: column.abandoned === true, total: Math.max(total, items.length), items });
	}
	return {
		workspace: x.workspace, collection: x.collection, columns, truncated: x.truncated === true,
		unplaced: typeof x.unplaced === 'number' ? x.unplaced : 0, cursor: x.cursor, access_epoch: x.access_epoch
	};
}

const parseIds = (x: unknown): string[] => Array.isArray(x) ? x.filter((id): id is string => typeof id === 'string') : [];

export function parseChanges(x: unknown): BoardChanges | null {
	if (!isObject(x) || typeof x.cursor !== 'string' || typeof x.access_epoch !== 'string') return null;
	return {
		upserts: parseItems(x.upserts), removed_ids: parseIds(x.removed_ids), departed_ids: parseIds(x.departed_ids),
		cursor: x.cursor, access_epoch: x.access_epoch, resync: x.resync === true
	};
}

/**
 * A pad_item result as the board row it replaces. Wire names: internal/models/item.go
 * (Item JSON tags), projected the way projectBoardItem does (internal/mcp/dispatch_http_board.go).
 * Fields the result omits keep the row's value.
 */
export function boardItemFrom(item: { id: string; title: string; seq: number; fields?: Record<string, unknown> }, row: BoardItem): BoardItem {
	const extra = item as Record<string, unknown>;
	const pick = (value: unknown, fallback: string) => typeof value === 'string' ? value : fallback;
	return {
		id: item.id, ref: pick(extra.ref, row.ref), title: item.title, seq: item.seq,
		status: str(item.fields?.status), priority: str(item.fields?.priority),
		// Both are omitempty on the wire, so absent means empty.
		assignee: str(extra.assigned_user_name), parent_ref: str(extra.parent_ref),
		updated_at: pick(extra.updated_at, row.updated_at)
	};
}
