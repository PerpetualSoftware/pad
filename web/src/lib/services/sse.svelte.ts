import { SvelteSet } from 'svelte/reactivity';

export type SSEStatus = 'disconnected' | 'connected' | 'reconnecting';

export interface ItemEvent {
	type: string;
	workspace_id: string;
	item_id: string;
	title: string;
	collection: string;
	actor: string;
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
	'comment_deleted'
] as const;

function createSSEService() {
	let status = $state<SSEStatus>('disconnected');
	let lastEventTime = $state<number>(0);
	let eventSource: EventSource | null = null;
	const callbacks = new SvelteSet<ItemEventCallback>();

	function connect(workspaceSlug: string) {
		if (eventSource) {
			disconnect();
		}

		const url = `/api/v1/events?workspace=${encodeURIComponent(workspaceSlug)}`;
		eventSource = new EventSource(url);

		eventSource.onopen = () => {
			status = 'connected';
		};

		eventSource.onerror = () => {
			status = 'reconnecting';
		};

		eventSource.addEventListener('connected', () => {
			status = 'connected';
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
		status = 'disconnected';
	}

	function onItemEvent(callback: ItemEventCallback): () => void {
		callbacks.add(callback);
		return () => {
			callbacks.delete(callback);
		};
	}

	return {
		get status() {
			return status;
		},
		get lastEventTime() {
			return lastEventTime;
		},
		connect,
		disconnect,
		onItemEvent
	};
}

export const sseService = createSSEService();
