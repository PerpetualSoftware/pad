// One "the connection came back" signal (TASK-2201, audit C89).
//
// Four surfaces recovered from an outage four ways: the item pane healed in
// about half a second, the dashboard on its 30-second poll, the editor through
// the collab provider's reconnect, and the collection page not at all. Its
// "Couldn't load this collection" card stayed up after the network returned
// until the user pressed Retry, beside a pane that had already recovered,
// which taught people that error states cannot be trusted.
//
// This is the one signal: a page holding a load error subscribes and retries
// once when it fires. It fires when
//   - the live stream goes from down (`reconnecting` / `disconnected`) back to
//     up (`connected`, or `polling` where the browser has no stream slot), and
//   - the window's `online` event fires.
// The two usually arrive together, so a second trigger inside DEDUPE_MS of the
// first is the same recovery and notifies nobody again. The first connect after
// page load is not a recovery: nothing was down yet.
import { sseService, type SSEStatus } from './sse.svelte';

export const DEDUPE_MS = 2000;

type Listener = () => void;

const listeners = new Set<Listener>();
let lastFired = -Infinity;
let started = false;

function isUp(s: SSEStatus): boolean {
	return s === 'connected' || s === 'polling';
}

function isDown(s: SSEStatus): boolean {
	return s === 'reconnecting' || s === 'disconnected';
}

/** Notify the subscribers, once per recovery. Exported for tests. */
export function signalRecovered(now: number = Date.now()): void {
	if (now - lastFired < DEDUPE_MS) return;
	lastFired = now;
	for (const l of [...listeners]) {
		try {
			l();
		} catch {
			// One subscriber's failure must not stop the others retrying.
		}
	}
}

function start(): void {
	if (started || typeof window === 'undefined') return;
	started = true;
	window.addEventListener('online', () => signalRecovered());
	// The stream starts `disconnected` before its first connect, so a down
	// state counts only after the stream has been up once; otherwise every
	// page load would announce a recovery (and spend the dedupe window the
	// real one may need).
	let seenUp = false;
	let wasDown = false;
	$effect.root(() => {
		$effect(() => {
			const s = sseService.status as SSEStatus;
			if (isUp(s)) {
				if (seenUp && wasDown) signalRecovered();
				seenUp = true;
				wasDown = false;
			} else if (isDown(s) && seenUp) {
				wasDown = true;
			}
		});
	});
}

/**
 * Call `listener` whenever the connection recovers. Returns the unsubscribe.
 * Call from a component's setup or an `$effect`, and unsubscribe on teardown.
 */
export function onConnectivityRecovered(listener: Listener): () => void {
	start();
	listeners.add(listener);
	return () => {
		listeners.delete(listener);
	};
}

/** Test-only. */
export function __resetConnectivityForTests(): void {
	listeners.clear();
	lastFired = -Infinity;
}
