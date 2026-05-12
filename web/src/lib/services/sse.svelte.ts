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

function createSSEService() {
	let status = $state<SSEStatus>('disconnected');
	let lastEventTime = $state<number>(0);
	let needsSync = $state<boolean>(false);
	let eventSource: EventSource | null = null;
	let currentWorkspace: string = '';
	const callbacks = new SvelteSet<ItemEventCallback>();
	const syncRequiredCallbacks = new SvelteSet<SyncRequiredCallback>();

	function connect(workspaceSlug: string) {
		// If already connected to the same workspace, don't reconnect.
		// The browser's EventSource handles reconnection automatically
		// with Last-Event-ID, so destroying it would lose that state.
		if (eventSource && currentWorkspace === workspaceSlug) {
			// EventSource is already connected (or auto-reconnecting).
			// readyState: 0=CONNECTING, 1=OPEN, 2=CLOSED
			if (eventSource.readyState !== EventSource.CLOSED) {
				return;
			}
		}

		// Different workspace or closed connection — create new EventSource
		if (eventSource) {
			disconnect();
		}

		currentWorkspace = workspaceSlug;
		const url = `/api/v1/events?workspace=${encodeURIComponent(workspaceSlug)}`;
		eventSource = new EventSource(url);

		eventSource.onopen = () => {
			status = 'connected';
		};

		eventSource.onerror = () => {
			status = 'reconnecting';
			// EventSource auto-reconnects and sends Last-Event-ID.
			// The server replays missed events from its buffer.
		};

		eventSource.addEventListener('connected', () => {
			status = 'connected';
		});

		// Handle sync_required: server's replay buffer couldn't cover the gap.
		// Trigger an immediate sync rather than waiting for a visibility change,
		// so the UI stays fresh even when the tab is actively visible. Fires
		// out via onSyncRequired() subscribers (currently syncService) — the
		// callback inversion keeps this module free of any sync.svelte import,
		// breaking the circular dep that previously required a dynamic import
		// here. (Rolldown flagged the dynamic import as ineffective because
		// sync.svelte is statically imported from 5 routes/components anyway,
		// so it's always in the main chunk — see TASK-1242.)
		eventSource.addEventListener('sync_required', () => {
			needsSync = true;
			for (const cb of syncRequiredCallbacks) {
				cb();
			}
		});

		// Handle unauthorized: the server's periodic membership revalidation
		// detected that this session has lost access to the workspace. We
		// must close the EventSource ourselves — otherwise the browser
		// auto-reconnect would tight-loop on a 401 or a stream that
		// immediately closes again, hammering /api/v1/events. Surface the
		// status so the surrounding UI can redirect to the workspace list
		// or show a "revoked" message.
		eventSource.addEventListener('unauthorized', () => {
			status = 'unauthorized';
			if (eventSource) {
				eventSource.close();
				eventSource = null;
			}
			currentWorkspace = '';
		});

		for (const eventType of ITEM_EVENTS) {
			eventSource.addEventListener(eventType, (e: MessageEvent) => {
				const data: ItemEvent = JSON.parse(e.data);
				lastEventTime = Date.now();
				for (const cb of callbacks) {
					cb(data);
				}
			});
		}
	}

	function disconnect() {
		if (eventSource) {
			eventSource.close();
			eventSource = null;
		}
		currentWorkspace = '';
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
		connect,
		disconnect,
		reconnect,
		onItemEvent,
		onSyncRequired,
		clearSyncFlag
	};
}

export const sseService = createSSEService();
