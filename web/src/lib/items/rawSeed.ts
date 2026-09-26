// Whether the item pane may switch to the raw markdown editor, and from what
// text (BUG-3050 U1, door A4).
//
// The raw editor's saves carry no version token by design (BUG-3080 orders a
// tab's writes with client_write instead), so whatever it starts from is sent
// back on the first raw save. If that is a body behind a tab's unflushed edits,
// the save replaces them. So the switch is refused whenever the seed is not
// the live document.
//
// Judged ONCE, from the seed actually used and the editor as it stands after
// the rich->raw flush loop. Judging by how the loop exited missed a case twice
// in review (the cap after a third flush; a `deduped` exit that skips the
// re-read while another tab typed during the await).

export interface RawSeedInputs {
	/** The live editor's markdown read AFTER the flush loop; undefined when there
	 *  was no read (no provider or editor, or the flush threw). */
	liveNow: string | undefined;
	/** The last text the loop flushed, or null when it flushed nothing. */
	lastFlushed: string | null;
	/** The stored body (`item.content`). */
	stored: string;
	/** The item's `content_state` as last read. */
	contentState: string | undefined;
}

export function rawSeedDecision(inputs: RawSeedInputs): { seed: string; refuse: boolean } {
	const seed = inputs.lastFlushed ?? inputs.stored;
	if (typeof inputs.liveNow === 'string') {
		// A live read exists: the seed must BE it. This tab's own unflushed
		// typing marks the item too, which is why the marker is not consulted
		// here: an equal live read is current whatever the marker says.
		return { seed, refuse: inputs.liveNow !== seed };
	}
	// No live read: the seed is the stored body, current only if not marked.
	return { seed, refuse: inputs.contentState === 'applied_pending_flush' };
}
