// One open chat thread (TASK-4044 PR 3a, rulings 4cf4d4b9 and 2de240d9): a
// per-mount store, not a singleton. It holds the window of messages the page
// shows and never decides what the server's unread count is.
//
// Open: GET the conversation, then one page. With nothing unread that is the
// newest page; with unread it is the page AFTER read_seq (up to the server
// max of 200), so the "N new" divider is always inside the window. Seqs are
// contiguous per conversation (a tombstone keeps its row), so "older rows
// exist" is oldest_seq > 1 and "newer rows exist" is newest < last_seq.
//
// Realtime: the thread reloads from chatStore.version, which every hint,
// resync and focus shares through one coalesced unread fetch, so it never
// subscribes to hints and never reads a notice field (carry-forward 2.7).
//
// WINDOW OPS (TASK-4044 3b S1, rulings 3293b1c6 and 5c95ef40). Every op that
// fetches and then writes the window (a load, an own row's placement, a delete's
// tombstone) runs through ONE promise chain, fetch and apply in the same step,
// so none interleave. The published window is always contiguous: an own row past
// unfetched peer rows is reached by paging from the window's newest (#fill); if
// that fails or hits REFRESH_MAX_PAGES the row is DETACHED: `detached` holds it,
// `hasNewer` is true, the page shows it after "Show newer messages", and
// newestSeq, mark-read, the divider and loadNewer's cursor ignore it. Send and
// delete POSTs stay concurrent; only their apply is queued.
//
// REFRESH (R1, ruling 4cf4d4b9). Rows newer than the window's newest are
// fetched ANCHORED AT THE NEWEST end: after_seq = newest, ASC, at most
// REFRESH_MAX_PAGES pages (1,000 rows), then hasNewer is set. Then tombstones:
// one DESC newest-200 page merged only inside the window's [oldest, newest].
// With `hasNewer` already true step 1 is skipped and step 2 is an ASC page from
// the window's oldest row. At most one refresh is queued. A refresh never merges
// while a send is in flight (the committed row would show beside its bubble;
// op_key is not echoed): it sets #refreshDeferred and each send settle runs one.
// RESIDUAL, disclosed: a retraction of a row older than the newest 200 shows on
// reopen, not live. Known (N4): a send that times out AFTER the server committed
// it shows the row beside the failed bubble after a refresh, until Retry.
//
// Identity: every answer is fenced by a seq and authStore.identityFence().
import { untrack } from 'svelte';
import { api, isNotFoundError, isReplyTargetDeletedError, isReplyTargetNotFoundError } from '$lib/api/client';
import { isRequestTooLargeError, validateDraft, type SendBlock } from '$lib/chat/compose';
import { ChatWindow, mergeBySeq, unreadDividerIndex, type WindowItem } from '$lib/chat/model';
import { authStore } from '$lib/stores/auth.svelte';
import { chatStore } from '$lib/stores/chat.svelte';
import type { ChatConversationSummary, ChatMessage } from '$lib/types';

/** Rows in the first page when nothing is unread, and in a load-earlier page. */
export const THREAD_PAGE = 50;
/** The server's page maximum (store/conversations.go maxConversationPage). */
export const THREAD_MAX_PAGE = 200;
/** Pages one refresh will follow from the newest end before it sets hasNewer. */
export const REFRESH_MAX_PAGES = 5;

export type ThreadStatus = 'loading' | 'ready' | 'notFound' | 'failed';

/**
 * How a send ended. `sent`: confirmed. `failed`: the bubble stays for Retry.
 * `stale`: the thread moved on (reopened, disposed, another identity); nothing
 * is left to show. The rest are definite refusals with no bubble: the caller
 * gets its text back.
 */
export type SendOutcome = 'sent' | 'failed' | 'stale' | SendBlock | 'reply_deleted' | 'reply_missing';

export class ChatThread {
	conversation = $state<ChatConversationSummary | null>(null);
	/** The confirmed window, ascending by seq; REPLACED on change. */
	messages = $state.raw<ChatMessage[]>([]);
	/** The window as rendered: confirmed rows, then this reader's pending sends. */
	items = $state.raw<WindowItem[]>([]);
	/** Bumped when an own send is shown or confirmed; the page scrolls to the end. */
	ownSendTick = $state(0);
	/** read_seq as of open, frozen for the mount: the divider never moves. */
	openReadSeq = $state(0);
	status = $state<ThreadStatus>('loading');
	refreshFailed = $state(false);
	olderFailed = $state(false);
	newerFailed = $state(false);
	loadingOlder = $state(false);
	loadingNewer = $state(false);
	hasOlder = $state(false);
	hasNewer = $state(false);
	/** Own rows confirmed past the window's contiguous end, shown after the "Show newer" marker. */
	detached = $state.raw<ChatMessage[]>([]);
	/** Set by the page from its bottom sentinel. */
	atBottom = $state(false);

	#win = new ChatWindow();
	#n: number | null = null;
	#seq = 0;
	#lastSeq = 0;
	#readSeq = 0;
	/** The window-op chain (see WINDOW OPS above). */
	#ops: Promise<void> = Promise.resolve();
	#refreshQueued: Promise<void> | null = null;
	#refreshDeferred = false;
	#marking = false;
	#stopRoot: () => void;
	#stopIdentity: () => void;

	constructor() {
		let first = true;
		this.#stopRoot = $effect.root(() => {
			$effect(() => {
				void chatStore.version;
				if (first) {
					first = false;
					return;
				}
				untrack(() => void this.refresh());
			});
		});
		this.#stopIdentity = authStore.onIdentityChange(() => {
			this.#seq++;
			this.#reset();
		});
	}

	/** The newest confirmed seq in the window, 0 when empty. */
	get newestSeq(): number {
		return this.messages.length ? this.messages[this.messages.length - 1].seq : 0;
	}

	/** Rows the "N new" divider counts: the unreadDividerIndex predicate. */
	get unreadCount(): number {
		return this.messages.filter((m) => m.seq > this.openReadSeq && !m.mine && !m.deleted).length;
	}

	get dividerIndex(): number {
		return unreadDividerIndex(this.messages, this.openReadSeq);
	}

	// Also closes the thread: with no #n and status 'loading', a version bump after
	// an identity change fetches nothing until the page opens it again (TASK-4044
	// r1 H1, ruling on triage ba47338c). open() sets both straight after.
	#reset() {
		this.#n = null;
		this.status = 'loading';
		this.conversation = null;
		this.messages = [];
		this.items = [];
		this.#win = new ChatWindow();
		this.openReadSeq = 0;
		this.refreshFailed = this.olderFailed = this.newerFailed = false;
		this.loadingOlder = this.loadingNewer = false;
		this.hasOlder = this.hasNewer = false;
		this.atBottom = false;
		this.#lastSeq = 0;
		this.#readSeq = 0;
		this.detached = [];
		this.#ops = Promise.resolve();
		this.#refreshQueued = null;
		this.#refreshDeferred = false;
		this.#marking = false;
	}

	// `beyond` is the server's last_seq when the answer left rows beyond the
	// window, or null when it reached the end: the window's own newest then stands
	// for last_seq. last_seq is read apart from the page rows, so a message landing
	// between the two would otherwise read as a gap and make the next refresh skip
	// its append.
	#publish(beyond: number | null) {
		this.messages = this.#win.messages;
		this.items = this.#win.items();
		if (this.detached.length) this.detached = this.detached.filter((m) => m.seq > this.newestSeq);
		this.#lastSeq = Math.max(beyond ?? this.newestSeq, this.detached.at(-1)?.seq ?? 0);
		const oldest = this.messages.length ? this.messages[0].seq : 0;
		this.hasOlder = oldest > 1;
		this.hasNewer = this.newestSeq < this.#lastSeq;
	}

	/** A guard for one async step: false once the thread was reopened, disposed or re-fenced. */
	#guard(): () => boolean {
		const mine = this.#seq;
		const same = authStore.identityFence();
		return () => mine === this.#seq && same();
	}

	async open(n: number | null): Promise<void> {
		this.#seq++;
		this.#reset();
		this.#n = n;
		if (n === null) {
			this.status = 'notFound';
			return;
		}
		this.status = 'loading';
		const current = this.#guard();
		await this.#enqueue(current, async () => {
			try {
				const conv = await api.chat.getConversation(n);
				if (!current()) return;
				this.conversation = conv;
				const page =
					conv.unread > 0
						? await api.chat.listMessages(n, { after_seq: conv.read_seq, limit: THREAD_MAX_PAGE })
						: await api.chat.listMessages(n, { limit: THREAD_PAGE });
				if (!current()) return;
				this.openReadSeq = page.read_seq;
				this.#readSeq = page.read_seq;
				this.#win.merge(page.messages);
				this.#publish(conv.unread > 0 && page.has_more ? page.last_seq : null);
				this.status = 'ready';
			} catch (err) {
				if (current()) this.status = isNotFoundError(err) ? 'notFound' : 'failed';
			}
		});
	}

	#enqueue(current: () => boolean, run: () => Promise<void>): Promise<void> {
		this.#ops = this.#ops.then(() => (current() ? run() : undefined)).catch(() => {});
		return this.#ops;
	}

	/** Fetch the page before the window's oldest row. A failure keeps the window. */
	async loadOlder(): Promise<void> {
		const n = this.#n;
		if (n === null || this.status !== 'ready' || this.loadingOlder || !this.hasOlder) return;
		const current = this.#guard();
		this.loadingOlder = true;
		this.olderFailed = false;
		await this.#enqueue(current, async () => {
			try {
				const page = await api.chat.listMessages(n, { before_seq: this.messages[0].seq, limit: THREAD_PAGE });
				if (!current()) return;
				this.#win.merge(page.messages);
				this.#publish(this.#lastSeq);
			} catch {
				if (current()) this.olderFailed = true;
			} finally {
				if (current()) this.loadingOlder = false;
			}
		});
	}

	/** Fetch one page after the window's newest row. */
	async loadNewer(): Promise<void> {
		const n = this.#n;
		if (n === null || this.status !== 'ready' || this.loadingNewer || !this.hasNewer) return;
		const current = this.#guard();
		this.loadingNewer = true;
		this.newerFailed = false;
		await this.#enqueue(current, async () => {
			try {
				if (!this.hasNewer || this.#win.hasInFlight()) return;
				const page = await api.chat.listMessages(n, { after_seq: this.newestSeq, limit: THREAD_MAX_PAGE });
				// A send in flight: its committed row may be in this page beside its bubble.
				if (!current() || this.#win.hasInFlight()) return;
				this.#win.merge(page.messages);
				this.#publish(page.has_more ? page.last_seq : null);
			} catch {
				if (current()) this.newerFailed = true;
			} finally {
				if (current()) this.loadingNewer = false;
			}
		});
	}

	/** Bring new rows and tombstones into the window (see the REFRESH note on top). */
	async refresh(): Promise<void> {
		const n = this.#n;
		if (n === null || this.#held()) return;
		if (this.#refreshQueued) return this.#refreshQueued;
		const current = this.#guard();
		return (this.#refreshQueued = this.#enqueue(current, () => this.#refresh(n, current)));
	}

	#held(): boolean {
		if (!this.#win.hasInFlight()) return false;
		this.#refreshDeferred = true;
		return true;
	}

	async #refresh(n: number, current: () => boolean): Promise<void> {
		this.#refreshQueued = null;
		if (this.status !== 'ready' || this.#held()) return;
		this.refreshFailed = false;
		try {
			const gap = this.hasNewer;
			if (!gap) {
				let cursor = this.newestSeq;
				for (let pages = 0; pages < REFRESH_MAX_PAGES; pages++) {
					const page = await api.chat.listMessages(n, { after_seq: cursor, limit: THREAD_MAX_PAGE });
					if (!current() || this.#held()) return;
					this.#win.merge(page.messages);
					this.#publish(page.has_more ? page.last_seq : null);
					if (!page.has_more || page.messages.length === 0) break;
					cursor = page.newest_seq;
				}
			}
			if (this.messages.length > 0) {
				const lo = this.messages[0].seq;
				const hi = this.newestSeq;
				const page = gap
					? await api.chat.listMessages(n, { after_seq: lo - 1, limit: THREAD_MAX_PAGE })
					: await api.chat.listMessages(n, { limit: THREAD_MAX_PAGE });
				if (!current() || this.#held()) return;
				this.#win.merge(page.messages.filter((m) => m.seq >= lo && m.seq <= hi));
				this.#publish(this.#lastSeq);
			}
			this.tryMarkRead();
		} catch {
			if (current()) this.refreshFailed = true;
		}
	}

	/** After a send settles: run the refresh that was deferred while it was in flight. */
	#settled() {
		if (this.#refreshDeferred && !this.#win.hasInFlight()) {
			this.#refreshDeferred = false;
			void this.refresh();
		}
	}

	/** Publish after a pending change; the window beyond stays beyond. */
	#publishAfterWrite() {
		this.#publish(this.hasNewer ? this.#lastSeq : null);
	}

	/**
	 * Send `body`. The bubble is shown at once; the outcome says what the caller
	 * should do (see SendOutcome). Nothing is sent for a draft the server would
	 * refuse (validateDraft), and a 400 for an over-size request or a 409/422 for
	 * a reply target maps to our own outcome, with no bubble left behind.
	 */
	async send(body: string, replyToSeq?: number): Promise<SendOutcome> {
		const n = this.#n;
		if (n === null || this.status !== 'ready') return 'stale';
		const opKey = crypto.randomUUID();
		const block = validateDraft(body, replyToSeq, opKey);
		if (block) return block;
		this.#win.addPending(opKey, body, replyToSeq);
		this.#publishAfterWrite();
		this.ownSendTick++;
		return this.#post(n, opKey, body, replyToSeq);
	}

	/** Send a failed bubble again with the SAME op_key. */
	async retry(opKey: string): Promise<SendOutcome> {
		const n = this.#n;
		if (n === null || this.status !== 'ready') return 'stale';
		const p = this.#win.retryPending(opKey);
		if (!p) return 'stale';
		this.#publishAfterWrite();
		return this.#post(n, opKey, p.body, p.reply_to_seq);
	}

	/** Give up on a failed send. */
	discard(opKey: string): void {
		this.#win.discardPending(opKey);
		this.#publishAfterWrite();
		this.#settled();
	}

	/**
	 * Place a confirmed own row (queued). Contiguous with the end: resolve. Past it,
	 * with another send in flight: detach, fetching nothing (a page could carry its
	 * committed row beside its bubble). With hasNewer true: move the window to the
	 * newest page (ruling b3dd1e54; a row older than it is history). Else #fill.
	 */
	async #applyOwn(n: number, opKey: string, answer: ChatMessage, current: () => boolean): Promise<void> {
		const win = this.#win;
		const sent = [...win.messages, ...this.detached].find((m) => m.seq === answer.seq) ?? answer; // a held row (maybe a tombstone) beats a replayed answer (I5)
		const others = () => win.pending.some((p) => p.status === 'sending' && p.op_key !== opKey);
		let at: 'resolve' | 'moved' | 'detach' = 'resolve';
		if (this.hasNewer || sent.seq > this.newestSeq + 1) {
			at = 'detach';
			if (!others() && this.hasNewer) {
				let rows: ChatMessage[] | null = null;
				try {
					rows = (await api.chat.listMessages(n, { limit: THREAD_PAGE })).messages;
				} catch {} // a failed fetch keeps the window; the row detaches (ruling ec6bbe31)
				if (!current()) return;
				if (rows && !others()) {
					win.messages = mergeBySeq([], rows.length && sent.seq < rows[0].seq ? rows : [sent, ...rows]);
					at = 'moved';
				}
			} else if (!others() && !this.hasNewer && (await this.#fill(n, sent.seq, current, others))) {
				at = 'resolve';
			}
		}
		if (!current()) return;
		if (at === 'resolve' && !(win.messages.length && sent.seq < win.messages[0].seq)) win.resolvePending(opKey, sent); // below the window is history (R1)
		else win.discardPending(opKey);
		if (at === 'detach') this.detached = [...this.detached.filter((m) => m.seq !== sent.seq), sent].sort((a, b) => a.seq - b.seq);
		this.#publish(null);
	}

	/** Page after_seq from the newest up to the row before `upTo` (bounded), publishing each page. */
	async #fill(n: number, upTo: number, current: () => boolean, others: () => boolean): Promise<boolean> {
		try {
			for (let pages = 0; pages < REFRESH_MAX_PAGES && this.newestSeq < upTo - 1; pages++) {
				const page = await api.chat.listMessages(n, { after_seq: this.newestSeq, limit: THREAD_MAX_PAGE });
				if (!current() || page.messages.length === 0 || others()) return false; // a send began: its row could ride this page
				this.#win.merge(page.messages.filter((m) => m.seq < upTo));
				this.#publish(null);
			}
		} catch {
			return false;
		}
		return this.newestSeq >= upTo - 1;
	}

	async #post(n: number, opKey: string, body: string, replyToSeq?: number): Promise<SendOutcome> {
		const current = this.#guard();
		const win = this.#win;
		const refused = (outcome: SendOutcome): SendOutcome => {
			win.discardPending(opKey);
			this.#publishAfterWrite();
			this.#settled();
			return outcome;
		};
		try {
			const res = await api.chat.sendMessage(n, {
				body,
				op_key: opKey,
				...(replyToSeq !== undefined ? { reply_to_seq: replyToSeq } : {})
			});
			if (!current()) return 'stale';
			await this.#enqueue(current, () => this.#applyOwn(n, opKey, res.message, current));
			if (!current()) return 'stale';
			this.ownSendTick++;
			this.#settled();
			return 'sent';
		} catch (err) {
			if (!current()) return 'stale';
			if (isReplyTargetDeletedError(err)) return refused('reply_deleted');
			if (isReplyTargetNotFoundError(err)) return refused('reply_missing');
			if (isRequestTooLargeError(err)) return refused('too_large');
			win.failPending(opKey);
			this.#publishAfterWrite();
			this.#settled();
			return 'failed';
		}
	}

	/**
	 * Delete a message (V6, V7: idempotent, a repeat answers 200 with the same
	 * tombstone) and merge the tombstone in place. True when it is gone.
	 */
	async remove(seq: number): Promise<boolean> {
		const n = this.#n;
		if (n === null || this.status !== 'ready') return false;
		const current = this.#guard();
		try {
			const res = await api.chat.deleteMessage(n, seq);
			if (!current()) return false;
			await this.#enqueue(current, async () => {
				// Only a row the window or the detached list holds: a re-anchor meanwhile may have moved it (r2 H3).
				if (this.#win.messages.some((m) => m.seq === seq)) this.#win.merge([res.message]);
				else this.detached = this.detached.map((m) => (m.seq === seq ? res.message : m));
				this.#publish(this.#lastSeq);
			});
			return current();
		} catch (err) {
			if (current() && isNotFoundError(err)) this.status = 'notFound';
			return false;
		}
	}

	/**
	 * Tell the server what was DISPLAYED: the newest confirmed seq in the window,
	 * and only while the bottom sentinel is in view on a visible tab. Never marks
	 * locally on a failure; the next refresh or visibility change retries.
	 */
	tryMarkRead(): void {
		const n = this.#n;
		if (n === null || this.status !== 'ready' || !this.atBottom || this.#marking) return;
		if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
		const seq = this.newestSeq;
		if (seq <= this.#readSeq) return;
		const current = this.#guard();
		this.#marking = true;
		let marked = false;
		api.chat
			.markRead(n, seq)
			.then(() => {
				if (!current()) return;
				marked = true;
				this.#readSeq = Math.max(this.#readSeq, seq);
				// POST /read publishes no notice, so the badge is stale until this.
				chatStore.resync();
			})
			.catch(() => {})
			.finally(() => {
				if (!current()) return;
				this.#marking = false;
				// Rows that landed while this was in flight. Only after a success, so a
				// failing mark cannot loop, and only when the window grew past `seq`
				// (TASK-4044 r2, ruling on the r2 triage).
				if (marked && this.newestSeq > seq) this.tryMarkRead();
			});
	}

	dispose(): void {
		this.#seq++;
		this.#stopRoot();
		this.#stopIdentity();
	}
}
