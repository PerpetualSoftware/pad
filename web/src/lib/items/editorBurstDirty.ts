// The dirty marks for one editor burst (TASK-2232).
//
// The Editor reports a change synchronously (`onDirty`) and its markdown later,
// coalesced. A burst can end with the markdown it started from (a change
// markdown does not carry, or one undone inside the window), and then no save
// follows to clear the marks the burst raised. Main never raised them for such
// a change, because it compared markdown on every transaction. So the marks a
// burst RAISED (they were clear when it began) are put back when it settles
// unchanged; marks an earlier real change set are owed a save and stay.

export interface BurstDirtyIO {
	/** This pane's own dirty shadow. */
	localDirty(): boolean;
	setLocalDirty(v: boolean): void;
	/** Whether this pane may touch the singleton (an active, not peeking, side). */
	ownsStore(): boolean;
	storeDirty(): boolean;
	setStoreDirty(v: boolean): void;
}

export function createBurstDirty(io: BurstDirtyIO) {
	let raisedLocal = false;
	let raisedStore = false;
	return {
		/** A change: mark dirty now, remembering which marks this burst raised. */
		changed() {
			if (!io.localDirty()) raisedLocal = true;
			if (io.ownsStore()) {
				if (!io.storeDirty()) raisedStore = true;
				io.setStoreDirty(true);
			}
			io.setLocalDirty(true);
		},
		/** The burst produced new markdown: its marks are owed a save. */
		delivered() {
			raisedLocal = false;
			raisedStore = false;
		},
		/** The burst settled on the markdown it started from: drop what it raised. */
		settledUnchanged() {
			if (raisedLocal) io.setLocalDirty(false);
			if (raisedStore && io.ownsStore()) io.setStoreDirty(false);
			raisedLocal = false;
			raisedStore = false;
		},
	};
}
