import { SvelteSet } from 'svelte/reactivity';

export type SSEStatus = 'disconnected' | 'connected' | 'reconnecting';

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
}

type ItemEventCallback = (event: ItemEvent) => void;

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
		// so the UI stays fresh even when the tab is actively visible.
		eventSource.addEventListener('sync_required', () => {
			needsSync = true;
			// Dynamic import to avoid circular dependency
			import('./sync.svelte').then(({ syncService }) => {
				syncService.triggerSync();
			});
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
		clearSyncFlag
	};
}

export const sseService = createSSEService();
