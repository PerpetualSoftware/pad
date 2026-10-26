// Pure, DOM-free window logic for the web chat view (TASK-4044).
//
// A conversation is ordered by `seq`, never by `created_at`: the server hands
// out seqs, and timestamps from different writers can tie or run backwards.

import type { ChatMessage } from '$lib/types';

/**
 * Merge a fetched page into the window: ascending by seq, one row per seq, and
 * on a collision the LATER row (the page's) wins. That is what makes a
 * tombstone (`deleted: true`, empty body) replace the body it retracts in place.
 * Returns a new array; neither input is mutated.
 */
export function mergeBySeq(window: readonly ChatMessage[], page: readonly ChatMessage[]): ChatMessage[] {
	const bySeq = new Map<number, ChatMessage>();
	for (const m of window) bySeq.set(m.seq, m);
	for (const m of page) bySeq.set(m.seq, m);
	return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

/**
 * Index of the first message the reader has not seen: seq above the read marker
 * as of OPEN, not mine, not a tombstone. -1 when there is none. `messages` must
 * be ascending by seq.
 */
export function unreadDividerIndex(messages: readonly ChatMessage[], openReadSeq: number): number {
	return messages.findIndex((m) => m.seq > openReadSeq && !m.mine && !m.deleted);
}

export type PendingStatus = 'sending' | 'failed';

/** A send the server has not answered yet (or refused), shown as a bubble. */
export interface PendingSend {
	op_key: string;
	body: string;
	reply_to_seq?: number;
	status: PendingStatus;
}

export type WindowItem =
	| { kind: 'message'; message: ChatMessage }
	| { kind: 'pending'; pending: PendingSend };

/**
 * The confirmed window plus optimistic sends. Pending bubbles always sort after
 * the last confirmed seq, in the order they were added. A retry reuses the
 * pending's `op_key`, so the server's idempotency collapses a send that had in
 * fact landed.
 */
export class ChatWindow {
	messages: ChatMessage[] = [];
	pending: PendingSend[] = [];

	/** Merge a fetched page (see mergeBySeq). */
	merge(page: readonly ChatMessage[]): void {
		this.messages = mergeBySeq(this.messages, page);
	}

	addPending(op_key: string, body: string, reply_to_seq?: number): PendingSend {
		const existing = this.pending.find((p) => p.op_key === op_key);
		if (existing) return existing;
		const p: PendingSend = { op_key, body, status: 'sending' };
		if (reply_to_seq !== undefined) p.reply_to_seq = reply_to_seq;
		this.pending = [...this.pending, p];
		return p;
	}

	/**
	 * The server answered: drop the bubble and merge the returned message by seq.
	 * A `replayed: true` answer whose seq is already in the window (a realtime
	 * refetch got there first) leaves exactly one row.
	 */
	resolvePending(op_key: string, message: ChatMessage): void {
		this.pending = this.pending.filter((p) => p.op_key !== op_key);
		this.messages = mergeBySeq(this.messages, [message]);
	}

	/** The send failed: keep the bubble, marked for a retry with the same op_key. */
	failPending(op_key: string): void {
		this.pending = this.pending.map((p) => (p.op_key === op_key ? { ...p, status: 'failed' } : p));
	}

	/** Put a failed bubble back in flight. Returns it so the caller re-sends with its op_key. */
	retryPending(op_key: string): PendingSend | undefined {
		this.pending = this.pending.map((p) => (p.op_key === op_key ? { ...p, status: 'sending' } : p));
		return this.pending.find((p) => p.op_key === op_key);
	}

	/** Drop a pending bubble without a result (the user gave up on a failed send). */
	discardPending(op_key: string): void {
		this.pending = this.pending.filter((p) => p.op_key !== op_key);
	}

	/** True while a send awaits its answer; callers defer realtime refetches meanwhile. */
	hasInFlight(): boolean {
		return this.pending.some((p) => p.status === 'sending');
	}

	/** Confirmed messages by seq, then pending bubbles in the order added. */
	items(): WindowItem[] {
		return [
			...this.messages.map((message): WindowItem => ({ kind: 'message', message })),
			...this.pending.map((pending): WindowItem => ({ kind: 'pending', pending }))
		];
	}
}
