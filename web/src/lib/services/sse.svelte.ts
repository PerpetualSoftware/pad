import { SvelteSet } from 'svelte/reactivity';

export type SSEStatus = 'disconnected' | 'connected' | 'reconnecting' | 'unauthorized';

export interface ItemEvent {
	type: string;
	id?: number;
	workspace_id: string;
	item_id: string;
	title: string;
	collection: string;
	actor: string;
	actor_name?: string;
	source: string;
	timestamp: number;
	// `seq` is the workspace-scoped monotonic mutation cursor of the
	// referenced item (PLAN-1343 / TASK-1358). Server stamps it on
	// item lifecycle events (created / updated / archived / restored)
	// so the local-first read model can apply contiguous deltas in
	// place and detect gaps that need a /items-changes backfill.
	// Optional because non-item events (workspace_updated,
	// comment_*, reaction_*) and legacy publishers omit it; clients
	// fall back to a generic deltaSync when missing.
	seq?: number;
}

type ItemEventCallback = (event: ItemEvent) => void;
type SyncRequiredCallback = () => void;

const ITEM_EVENTS = [
	'item_created',
	'item_updated',
	'item_archived',
	'item_restored',
	'workspace_updated',
	'comment_created',
	'comment_deleted',
	'reaction_added',
	'reaction_removed'
] as const;

// Cross-tab BroadcastChannel envelope. The leader tab fans events out
// via this channel so peer tabs in the same browser get live updates
// without opening their own EventSource (PLAN-1343 / TASK-1359).
type BCEnvelope =
	| { type: 'item_event'; event: ItemEvent }
	| { type: 'sync_required' }
	| { type: 'status'; status: SSEStatus };

function createSSEService() {
	let status = $state<SSEStatus>('disconnected');
	let lastEventTime = $state<number>(0);
	let needsSync = $state<boolean>(false);
	let eventSource: EventSource | null = null;
	let currentWorkspace: string = '';
	const callbacks = new SvelteSet<ItemEventCallback>();
	const syncRequiredCallbacks = new SvelteSet<SyncRequiredCallback>();

	// Cross-tab BroadcastChannel for fan-out from the leader tab to
	// peer tabs. Every connected tab (leader OR peer) keeps the
	// channel open — the leader publishes events here, peers
	// subscribe. Null on browsers without BroadcastChannel support
	// (very old / non-browser environments — every tab becomes its
	// own leader and opens its own SSE).
	let bc: BroadcastChannel | null = null;

	// Resolves the leader lock when this tab is disconnecting (or
	// switching workspaces). Returning from the lock callback
	// releases the lock; another waiting tab then acquires it and
	// becomes the new leader. Null when this tab is not the leader.
	let releaseLeaderLock: (() => void) | null = null;

	// True iff this tab currently holds the leader Web Lock.
	let isLeader = false;

	function dispatchItemEvent(data: ItemEvent) {
		lastEventTime = Date.now();
		for (const cb of callbacks) {
			cb(data);
		}
	}

	function dispatchSyncRequired() {
		needsSync = true;
		for (const cb of syncRequiredCallbacks) {
			cb();
		}
	}

	function broadcast(env: BCEnvelope) {
		if (!bc) return;
		try {
			bc.postMessage(env);
		} catch {
			/* swallow — closed channel or serialization error */
		}
	}

	function leaderElectionSupported(): boolean {
		return typeof navigator !== 'undefined' && 'locks' in navigator;
	}

	function openBroadcastChannel(workspaceSlug: string) {
		if (typeof BroadcastChannel === 'undefined') return;
		// BroadcastChannel is only safe when paired with leader
		// election. Without `navigator.locks`, every tab opens its
		// own EventSource AND would receive its peers' broadcasts —
		// firing item callbacks N times across N tabs (Codex P1 of
		// TASK-1359). Skip BC entirely in that fallback regime; the
		// per-tab EventSource still delivers the events locally.
		if (!leaderElectionSupported()) return;
		bc = new BroadcastChannel(`pad-sync-${workspaceSlug}`);
		bc.onmessage = (msg: MessageEvent<BCEnvelope>) => {
			// Peer-tab path: route messages from the leader to local
			// callbacks. The leader-side `dispatchItemEvent` /
			// `dispatchSyncRequired` already runs locally before
			// broadcasting, so the leader doesn't re-dispatch its
			// own messages. The browser's own-message-filter on
			// BroadcastChannel makes the cross-tab boundary the only
			// fan-out edge.
			const env = msg.data;
			if (env.type === 'item_event') {
				dispatchItemEvent(env.event);
			} else if (env.type === 'sync_required') {
				dispatchSyncRequired();
			} else if (env.type === 'status') {
				// Mirror the leader's connection status so peer-tab UI
				// indicators don't show "disconnected" while the
				// leader is happily streaming.
				status = env.status;
			}
		};
	}

	// `pendingSyncOnConnect` defers a `sync_required` dispatch until
	// the new EventSource is actually subscribed server-side
	// (onopen / `connected` event). Without this gate, the promoted/
	// fallback paths fire dispatchSyncRequired BEFORE the SSE
	// connection lands, so any mutation between the resulting
	// /items-changes snapshot and the new stream's first received
	// event can be missed (Codex P1 round 4 of TASK-1359).
	let pendingSyncOnConnect = false;

	function openEventSource(workspaceSlug: string) {
		const url = `/api/v1/events?workspace=${encodeURIComponent(workspaceSlug)}`;
		eventSource = new EventSource(url);

		eventSource.onopen = () => {
			status = 'connected';
			broadcast({ type: 'status', status: 'connected' });
			if (pendingSyncOnConnect) {
				pendingSyncOnConnect = false;
				dispatchSyncRequired();
				broadcast({ type: 'sync_required' });
			}
		};

		eventSource.onerror = () => {
			status = 'reconnecting';
			broadcast({ type: 'status', status: 'reconnecting' });
			// EventSource auto-reconnects and sends Last-Event-ID.
			// The server replays missed events from its buffer.
		};

		eventSource.addEventListener('connected', () => {
			status = 'connected';
			broadcast({ type: 'status', status: 'connected' });
			// Mirror onopen — some platforms fire `connected`
			// reliably before `onopen` on reconnect, others vice
			// versa. Whichever fires first claims the pending sync.
			if (pendingSyncOnConnect) {
				pendingSyncOnConnect = false;
				dispatchSyncRequired();
				broadcast({ type: 'sync_required' });
			}
		});

		// Handle sync_required: server's replay buffer couldn't cover the gap.
		eventSource.addEventListener('sync_required', () => {
			dispatchSyncRequired();
			broadcast({ type: 'sync_required' });
		});

		// Handle unauthorized: the server's periodic membership revalidation
		// detected that this session has lost access to the workspace. We
		// must close the EventSource ourselves — otherwise the browser
		// auto-reconnect would tight-loop on a 401 or a stream that
		// immediately closes again, hammering /api/v1/events.
		eventSource.addEventListener('unauthorized', () => {
			status = 'unauthorized';
			broadcast({ type: 'status', status: 'unauthorized' });
			if (eventSource) {
				eventSource.close();
				eventSource = null;
			}
			currentWorkspace = '';
		});

		for (const eventType of ITEM_EVENTS) {
			eventSource.addEventListener(eventType, (e: MessageEvent) => {
				const data: ItemEvent = JSON.parse(e.data);
				dispatchItemEvent(data);
				broadcast({ type: 'item_event', event: data });
			});
		}
	}

	/**
	 * Acquire the workspace-scoped leader lock and, if granted, open
	 * the SSE EventSource. Peer tabs queue on the same lock and only
	 * advance when the current leader's tab unloads or closes the
	 * lock — at which point another tab takes over transparently
	 * (navigator.locks releases the lock on page unload).
	 *
	 * Browsers without navigator.locks (very old / non-browser
	 * environments) skip the leader election and every tab opens
	 * its own EventSource — N× traffic but correct.
	 */
	async function requestLeader(workspaceSlug: string) {
		if (!leaderElectionSupported()) {
			// No leader election available. Fall back to per-tab SSE.
			// BC was also skipped above, so this is the per-tab N×
			// fallback path — correct, just less efficient.
			openEventSource(workspaceSlug);
			return;
		}
		// Distinguish "first leader" from "promoted follower" so we
		// only fire sync_required on promotion — the freshly-opened
		// EventSource has no Last-Event-ID and the server can't
		// replay events emitted during the gap between the old
		// leader's close and the new connection. Two signals:
		//
		//   1. `navigator.locks.query` before the request. If the
		//      slot is already held by another tab, we're definitely
		//      a future promotion. (Codex P1 round 2 of TASK-1359.)
		//   2. Grant delay. If `query` raced with two tabs starting
		//      simultaneously (both see "no holder"), one wins and
		//      the other queues. The loser would mis-classify with
		//      query alone — but the lock grant for the queued tab
		//      is delayed by however long the winner held it. Any
		//      callback that fires more than ~100ms after the
		//      request is treated as a promotion. (Codex P1 round 3
		//      of TASK-1359.) 100ms is a conservative cap on the
		//      immediate grant case (uncontested lock acquisition
		//      in a healthy browser is sub-millisecond).
		let queryPromoted = false;
		try {
			const snapshot = await navigator.locks.query();
			const held = (snapshot.held ?? []).some(
				(l) => l.name === `pad-sse-leader-${workspaceSlug}`,
			);
			queryPromoted = held;
		} catch {
			/* swallow — query unsupported on some platforms */
		}
		const requestStart =
			typeof performance !== 'undefined' ? performance.now() : Date.now();

		navigator.locks
			.request(
				`pad-sse-leader-${workspaceSlug}`,
				{ mode: 'exclusive' },
				async () => {
					// User may have already navigated away by the time
					// we acquire the lock. Bail before opening a stale
					// connection.
					if (currentWorkspace !== workspaceSlug) return;
					const grantDelay =
						(typeof performance !== 'undefined'
							? performance.now()
							: Date.now()) - requestStart;
					const promoted = queryPromoted || grantDelay > 100;
					isLeader = true;
					if (promoted) {
						// Schedule the sync to fire AFTER the SSE is
						// connected — otherwise any mutation between
						// the resulting /items-changes snapshot and
						// the new stream's first received event would
						// be missed (Codex P1 round 4).
						pendingSyncOnConnect = true;
					}
					openEventSource(workspaceSlug);
					// Hold the lock until release is signaled (by
					// disconnect() or a workspace switch). The promise
					// returned from this callback is what
					// navigator.locks awaits.
					return new Promise<void>((resolve) => {
						releaseLeaderLock = () => {
							releaseLeaderLock = null;
							isLeader = false;
							resolve();
						};
					});
				},
			)
			.catch(() => {
				// Lock request can fail in exotic cases (cross-origin
				// iframe restrictions, etc.). Close BC before falling
				// back to a per-tab EventSource — otherwise peer tabs
				// rejected the same way would each open their own SSE
				// AND keep receiving each other's broadcasts, firing
				// callbacks N times (Codex P2 round 2). Trigger
				// sync_required because we may have missed events.
				if (currentWorkspace === workspaceSlug && !eventSource) {
					if (bc) {
						bc.close();
						bc = null;
					}
					// Defer sync_required until the EventSource opens
					// (Codex P1 round 4 of TASK-1359) — same race as
					// the promotion path.
					pendingSyncOnConnect = true;
					openEventSource(workspaceSlug);
				}
			});
	}

	function connect(workspaceSlug: string) {
		// Already connected to the same workspace — no-op.
		if (currentWorkspace === workspaceSlug && (eventSource || bc)) {
			if (eventSource && eventSource.readyState !== EventSource.CLOSED) {
				return;
			}
			if (bc && !eventSource && !isLeader) {
				return; // peer tab, still listening
			}
		}

		// Different workspace or closed connection — tear down first.
		if (eventSource || bc) {
			disconnect();
		}

		currentWorkspace = workspaceSlug;
		// Always open the broadcast channel: leaders use it to fan
		// out, peers use it to receive.
		openBroadcastChannel(workspaceSlug);
		// Race for the leader slot. If we win, openEventSource runs
		// inside the lock callback. If we lose, we wait on the lock
		// while passively receiving via bc. When the leader tab
		// closes, the lock releases and our queued request fires —
		// at which point we open our own EventSource.
		// requestLeader is async (it queries the lock state first)
		// but we don't await it — the .catch() handler covers any
		// failures and connect() returns synchronously to the caller.
		void requestLeader(workspaceSlug);
	}

	function disconnect() {
		// Release the leader lock first so a peer tab can take over
		// even on the same browser session (e.g. workspace switch).
		if (releaseLeaderLock) {
			releaseLeaderLock();
		}
		if (eventSource) {
			eventSource.close();
			eventSource = null;
		}
		if (bc) {
			bc.close();
			bc = null;
		}
		currentWorkspace = '';
		isLeader = false;
		status = 'disconnected';
	}

	/** Force reconnect (e.g., after auth change). */
	function reconnect() {
		const ws = currentWorkspace;
		if (ws) {
			disconnect();
			connect(ws);
		}
	}

	function onItemEvent(callback: ItemEventCallback): () => void {
		callbacks.add(callback);
		return () => {
			callbacks.delete(callback);
		};
	}

	/**
	 * Subscribe to `sync_required` events from the server. Fires when the
	 * server's replay buffer couldn't cover a reconnect gap and the client
	 * needs to do a fresh sync. Returns an unsubscribe function.
	 *
	 * Used by syncService to drive `triggerSync()` without sse.svelte
	 * having to import sync.svelte (which would form a circular dep —
	 * sync.svelte already statically imports sseService).
	 */
	function onSyncRequired(callback: SyncRequiredCallback): () => void {
		syncRequiredCallbacks.add(callback);
		return () => {
			syncRequiredCallbacks.delete(callback);
		};
	}

	function clearSyncFlag() {
		needsSync = false;
	}

	return {
		get status() {
			return status;
		},
		get lastEventTime() {
			return lastEventTime;
		},
		get needsSync() {
			return needsSync;
		},
		get isLeader() {
			return isLeader;
		},
		connect,
		disconnect,
		reconnect,
		onItemEvent,
		onSyncRequired,
		clearSyncFlag
	};
}

export const sseService = createSSEService();
