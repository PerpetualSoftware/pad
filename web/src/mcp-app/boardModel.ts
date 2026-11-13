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
	// IDs learned from deltas, since the snapshot only gives the unplaced count.
	unplacedIds?: string[];
}
export interface BoardChanges {
	upserts: BoardItem[];
	removed_ids: string[];
	cursor: string;
	access_epoch: string;
	resync: boolean;
}

export function applyChanges(board: Board, changes: BoardChanges): Board {
	const columns = board.columns.map(column => ({ ...column, items: [...column.items] }));
	const unplacedIds = new Set(board.unplacedIds);
	let unplaced = board.unplaced;
	const remove = (id: string) => {
		for (const column of columns) {
			const index = column.items.findIndex(item => item.id === id);
			if (index !== -1) { column.items.splice(index, 1); column.total--; }
		}
		if (unplacedIds.delete(id)) unplaced--;
	};
	for (const id of changes.removed_ids) remove(id);
	for (const item of changes.upserts) {
		remove(item.id);
		const column = columns.find(column => column.status === item.status);
		if (column) { column.items.push(item); column.total++; }
		else { unplacedIds.add(item.id); unplaced++; }
	}
	return { ...board, columns, unplaced, unplacedIds: [...unplacedIds], cursor: changes.cursor, access_epoch: changes.access_epoch };
}

export function nextDelay(prev: number, rateLimited: boolean): number {
	return rateLimited ? Math.min(Math.max(prev, 10_000) * 2, 120_000) : 10_000;
}

export function shouldPoll({ displayMode, visible }: { displayMode?: string; visible: boolean }): boolean {
	return displayMode === 'fullscreen' && visible;
}
