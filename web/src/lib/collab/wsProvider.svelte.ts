/**
 * Collab WebSocket provider — y-websocket-style binary protocol bound
 * to Pad's `/api/v1/collab/{itemID}` endpoint (TASK-1259, PLAN-1248).
 *
 * Wire format (mirrors the server-side y-protocol decoder in
 * `internal/collab/room.go`):
 *
 *   ┌────┬─────────────────────────┐
 *   │ 0  │ y-protocols/sync bytes  │   sync step1 / step2 / update
 *   ├────┼─────────────────────────┤
 *   │ 1  │ awareness update bytes  │   y-protocols/awareness
 *   └────┴─────────────────────────┘
 *
 * The first byte is a varUint message-type discriminator. The server
 * persists every sync frame (type 0) into the op-log and rebroadcasts
 * to other peers; awareness frames (type 1) are broadcast only —
 * presence is ephemeral.
 *
 * This module is `.svelte.ts` so the connection state can be exposed
 * as a Svelte 5 rune (`$state`) for UX consumers (TASK-1264 pending-
 * sync indicator). Pure-TS callers can still read `connected` /
 * `synced` as plain boolean fields.
 */

import * as Y from 'yjs';
import * as syncProtocol from 'y-protocols/sync';
import * as awarenessProtocol from 'y-protocols/awareness';
import * as encoding from 'lib0/encoding';
import * as decoding from 'lib0/decoding';
import { SCHEMA_VERSION } from './schemaVersion';

/** First-byte discriminators on the wire. Must match the constants in
 *  `internal/collab/room.go` (yMessageSync / yMessageAwareness). */
const MESSAGE_SYNC = 0;
const MESSAGE_AWARENESS = 1;

/** Reconnect backoff: 1s, 2s, 4s, … capped at 30s. Reset on a clean
 *  open. Sophisticated mobile reconnect (visibility, network state)
 *  is TASK-1265's concern; this is the floor behavior. */
const RECONNECT_BASE_MS = 1_000;
const RECONNECT_MAX_MS = 30_000;

/** After this many consecutive failed reconnect attempts, the public
 *  `state` signal flips to `'offline'` so UX (TASK-1264 indicator) can
 *  surface a hard failure rather than a perpetual yellow "reconnecting"
 *  spinner. The provider keeps trying — backoff continues — but the
 *  user-visible state is honest about the situation.
 *
 *  Three attempts ≈ 1s + 2s + 4s of failure before declaring offline. */
const OFFLINE_THRESHOLD = 3;

/** Fallback grace before declaring `synced` true on connections that
 *  never receive an explicit syncStep2. The dumb-relay server replays
 *  the op-log as a sequence of BinaryMessage frames but doesn't
 *  generate its own step2; an empty/pruned op-log + first-peer
 *  connect therefore never arrives at the explicit-sync signal. The
 *  grace lets any actual replay land first; after it, downstream
 *  consumers (lazy seed in TASK-1261) can safely treat
 *  `synced` as "the server has shown us everything it has." */
const SYNC_GRACE_MS = 1_000;

/**
 * Handler invoked when the server delivers an `applier_request`
 * (designated-applier protocol from TASK-1257). Receives the markdown
 * the CLI / MCP / API caller is trying to apply; should call
 * `editor.commands.setContent(markdown)` (which routes through the
 * y-tiptap binding and propagates as Y.Doc ops) and return `true`
 * once the content is applied. Returning `false` (or throwing) means
 * "I can't apply right now" — the provider will NOT ack, so the
 * server falls back to a direct write after its applier timeout.
 *
 * IMPORTANT: handlers that mutate state MUST honour `expiresAtMillis`
 * BEFORE the mutation. The provider checks expiry before invoking
 * the handler and re-checks before sending the ack, but the actual
 * mutation is owned by the caller — only the handler can refuse to
 * apply once the deadline has passed. Returning `false` after a
 * stale check keeps the late-apply hazard closed.
 */
export type ApplierRequestHandler = (
	markdown: string,
	requestID: string,
	expiresAtMillis: number,
) => boolean | Promise<boolean>;

/**
 * Callback fired when the server sends a `force_refresh` control
 * frame (TASK-1319). The reconnecting client's `?since=<id>` was
 * below the server's MIN(item_yjs_updates.id) — rows it expected
 * to replay have been pruned (op-log GC, schema rebuild, or
 * PruneAndApply), so reconnecting from local Y.Doc state would
 * produce a corrupt patch. The handler should:
 *
 *   1. Call `provider.destroy()`.
 *   2. Discard the local `Y.Doc` and create a fresh one.
 *   3. Build a new provider against the fresh doc — its lazy-seed
 *      path (TASK-1261) will re-encode `items.content` into ops at
 *      the current schema version.
 *   4. Show the user a toast explaining what happened.
 *
 * Receiving this callback means the provider has already cleared
 * its sessionStorage cursor; do NOT try to keep using the existing
 * provider/doc after it fires.
 */
export type ForceRefreshHandler = () => void;

/**
 * Per-tab op-log cursor protocol (TASK-1319).
 *
 * `sessionStorage` is per-tab and survives page reload (which
 * matches the "the editor reload should pick up where the user
 * left off" UX) but dies with the tab — no cross-tab leakage of
 * a stale cursor that might force-refresh a different tab. Keys
 * are namespaced + scoped to the item id so multi-item editing
 * sessions in the same tab don't collide.
 *
 * `localStorage` was deliberately rejected: shared between tabs of
 * the same item, Tab A advancing the cursor would leave Tab B with
 * a value that doesn't match Tab B's Y.Doc state, and B's next
 * reconnect would land on a stale-cursor force_refresh even though
 * B has been a stable session the whole time.
 */
const CURSOR_STORAGE_PREFIX = 'pad:collab:cursor:';

function cursorStorageKey(itemID: string): string {
	return `${CURSOR_STORAGE_PREFIX}${itemID}`;
}

function readStoredCursor(itemID: string): number {
	if (typeof sessionStorage === 'undefined') return 0;
	try {
		const raw = sessionStorage.getItem(cursorStorageKey(itemID));
		if (!raw) return 0;
		const n = Number.parseInt(raw, 10);
		return Number.isFinite(n) && n >= 0 ? n : 0;
	} catch {
		// sessionStorage can throw in privacy-mode iframes / quota cases.
		return 0;
	}
}

function writeStoredCursor(itemID: string, id: number): void {
	if (typeof sessionStorage === 'undefined') return;
	try {
		sessionStorage.setItem(cursorStorageKey(itemID), String(id));
	} catch {
		// Quota / privacy mode — best effort.
	}
}

function clearStoredCursor(itemID: string): void {
	if (typeof sessionStorage === 'undefined') return;
	try {
		sessionStorage.removeItem(cursorStorageKey(itemID));
	} catch {
		// Best effort.
	}
}

/**
 * Public connection state surfaced to UX (TASK-1264 pending-sync
 * indicator). Strictly more informative than `connected` + `synced`
 * because it distinguishes the initial handshake from a mid-session
 * reconnect from a hard "we've given up trying" failure:
 *
 *   - `connecting`   — socket attempting initial open OR open but
 *                      handshake not yet done. Show a neutral spinner.
 *   - `synced`       — socket open AND server has answered our
 *                      syncStep1 (or grace expired). Show green dot.
 *   - `reconnecting` — socket dropped after a successful session;
 *                      backoff retry in flight. Show yellow.
 *   - `offline`     — multiple consecutive reconnect failures past
 *                     `OFFLINE_THRESHOLD`. Provider keeps trying but
 *                     the UI surfaces a hard-failure colour (red).
 */
export type CollabConnectionState = 'connecting' | 'synced' | 'reconnecting' | 'offline';

export interface CollabProviderOptions {
	/**
	 * Override the WebSocket URL. Defaults to a same-origin URL based
	 * on `window.location` — that matches the auth-cookie path the
	 * server expects. Tests / SSR pass an explicit URL.
	 */
	url?: string;
	/**
	 * Override `WebSocket` — used by tests to stub the network layer.
	 */
	WebSocketImpl?: typeof WebSocket;
	/**
	 * Designated-applier handler. See `ApplierRequestHandler`.
	 * If unset, applier_request frames are dropped (server falls back
	 * after timeout). Production callers always set this.
	 */
	onApplierRequest?: ApplierRequestHandler;
	/**
	 * Force-refresh handler (TASK-1319). See `ForceRefreshHandler`.
	 * Required for production: without it, a force_refresh frame
	 * silently destroys the socket without telling the page,
	 * leaving the editor on stale state.
	 */
	onForceRefresh?: ForceRefreshHandler;
}

export class CollabProvider {
	readonly itemID: string;
	readonly ydoc: Y.Doc;
	readonly awareness: awarenessProtocol.Awareness;
	readonly url: string;

	/** True while the underlying socket is OPEN. Reactive in Svelte 5. */
	connected = $state(false);

	/**
	 * True after the server has answered our initial syncStep1 with a
	 * syncStep2 (i.e. we have whatever state the server had at connect
	 * time). Persisted across reconnects — once a session has synced
	 * once, dropping back to `false` would force the lazy-seed path
	 * (TASK-1261) to re-run. Kept reactive for the pending-sync UI
	 * indicator (TASK-1264).
	 */
	synced = $state(false);

	/**
	 * Public connection state for UX consumers (TASK-1264). Always
	 * derive UI from this instead of `connected` + `synced`
	 * separately — the four-state machine encodes "we never made it"
	 * vs "we made it then dropped" vs "we've given up", which the
	 * two-bool combination loses.
	 */
	state = $state<CollabConnectionState>('connecting');

	/**
	 * Highest op-log id this provider has been notified of by the
	 * server (TASK-1319). Initialised from sessionStorage so a page
	 * reload picks up where the previous incarnation left off, and
	 * persisted on every op_log_cursor control frame so a subsequent
	 * reconnect can announce `?since=<id>` and either get a delta
	 * replay (efficient) or a force_refresh (the rows it expected
	 * have been pruned).
	 *
	 * Reactive so a future UX consumer can show "X ops applied" or
	 * gate a flush button on `lastOpLogID === serverMaxOpLogID`. Read
	 * by the items-PATCH `?source=collab-snapshot` flush so the
	 * server can advance the GC watermark when this cursor matches
	 * MAX(op-log.id).
	 */
	lastOpLogID = $state(0);

	/**
	 * True after the server has sent ANY op_log_cursor control frame
	 * in this provider's lifetime — including the initial post-
	 * replay cursor of 0 against an empty op-log. The boolean
	 * distinguishes "I've heard from the server about its op-log
	 * state" (safe to send local edits) from "I've heard nothing"
	 * (network drop happened between replay binaries and the
	 * post-replay cursor frame; my Y.Doc may be populated by
	 * server rows but I have no anchor — propagating local edits
	 * derived from this state can corrupt items.content via the
	 * next 5s flush). Gating `handleDocUpdate` on this flag closes
	 * the residual "stale Y.Doc + cursor 0" send path. Per Codex
	 * round 14 [P1] of TASK-1319.
	 *
	 * Reset on `force_refresh` (the provider is destroyed there
	 * anyway) and on construction.
	 */
	private cursorAnchored = false;

	/**
	 * Buffer of local Yjs updates that fired before
	 * `cursorAnchored` flipped true. Each entry is the raw
	 * `update` Uint8Array from `ydoc.on('update', ...)`. On
	 * anchor we flush them in order so the server gets every
	 * causally-required struct — Yjs ops are incremental and a
	 * silently-dropped keystroke can leave later ops referencing
	 * structs no peer has. Per Codex round 15 [P1] of TASK-1319.
	 *
	 * Capped because in pathological "anchor never arrives"
	 * scenarios the buffer could grow unbounded; once we cross
	 * the cap we trigger `force_refresh`-equivalent recovery via
	 * the onForceRefresh handler so the user gets a clean
	 * rebuild rather than a stale-then-divergent session.
	 */
	private preAnchorUpdates: Uint8Array[] = [];
	private static readonly MAX_PRE_ANCHOR_UPDATES = 1000;

	/**
	 * Tracks whether the server has applied a sync update to our
	 * Y.Doc (replay binary or live peer op). Distinguishes
	 * "Y.Doc has remote-derived state" (suspect on cursor=0
	 * because the server's op-log is now empty — points to a
	 * mid-session prune) from "Y.Doc has only local pre-anchor
	 * edits" (safe — those were typed locally and the buffer
	 * holds them for replay on anchor). Per Codex round 19 [P1]
	 * of TASK-1319.
	 */
	private remoteSyncApplied = false;

	private ws: WebSocket | null = null;
	private readonly WebSocketImpl: typeof WebSocket;
	private readonly onApplierRequest?: ApplierRequestHandler;
	private readonly onForceRefresh?: ForceRefreshHandler;
	private destroyed = false;
	private reconnectAttempts = 0;
	private reconnectTimer: ReturnType<typeof setTimeout> | undefined;
	private syncGraceTimer: ReturnType<typeof setTimeout> | undefined;

	private readonly handleDocUpdate: (update: Uint8Array, origin: unknown) => void;
	private readonly handleAwarenessUpdate: (changes: { added: number[]; updated: number[]; removed: number[] }, origin: unknown) => void;
	private readonly handleBeforeUnload: () => void;
	private readonly handleVisibilityChange: () => void;
	private readonly handleOnline: () => void;
	private readonly handleOffline: () => void;

	constructor(itemID: string, ydoc: Y.Doc, options: CollabProviderOptions = {}) {
		this.itemID = itemID;
		this.ydoc = ydoc;
		this.awareness = new awarenessProtocol.Awareness(ydoc);

		this.WebSocketImpl = options.WebSocketImpl ?? globalThis.WebSocket;
		this.onApplierRequest = options.onApplierRequest;
		this.onForceRefresh = options.onForceRefresh;
		// Restore the per-tab cursor BEFORE the first connect. The
		// `since=<id>` query string is appended in connect() each
		// time so a reconnect after some live edits announces a
		// fresher cursor than the page-load value.
		//
		// **Cursor invariant**: lastOpLogID > 0 IFF Y.Doc has applied
		// server-acked ops. The Y.Doc isn't persisted across page
		// reload — every `new CollabProvider(itemID, new Y.Doc(), ...)`
		// mints an empty Y.Doc. A non-zero stored cursor against a
		// fresh empty Y.Doc would announce `?since=N`, the server
		// would replay only id > N, and the client would be missing
		// rows 1..N from its Y.Doc. Reset the stored cursor to 0 in
		// that case so the server replays from scratch. Per Codex
		// round 13 [P1] of TASK-1319.
		const storedCursor = readStoredCursor(itemID);
		const ydocStateVector = Y.encodeStateVector(ydoc);
		// An empty Y.Doc's state vector is the single VARINT 0 byte
		// (0x00); anything longer means at least one client id has
		// state.
		const ydocIsEmpty = ydocStateVector.length <= 1;
		if (storedCursor > 0 && ydocIsEmpty) {
			clearStoredCursor(itemID);
			this.lastOpLogID = 0;
		} else {
			this.lastOpLogID = storedCursor;
		}
		// `url` is the BASE URL (no `since=` appended). connect()
		// re-derives the connect URL on every attempt so the
		// `since=<id>` parameter reflects the current cursor. Test
		// callers passing `options.url` get to override with their
		// own URL — the test stub doesn't care about the cursor
		// query string, and re-deriving from a custom URL would be
		// wrong (we'd append `since=` to whatever they provided).
		this.url = options.url ?? defaultCollabBaseUrl(itemID);

		this.handleDocUpdate = (update, origin) => {
			// Skip ops that came from us applying a server message —
			// otherwise we'd echo every remote keystroke back to the
			// server (which would persist + rebroadcast it again).
			if (origin === this) return;
			// Cursor-anchor gate (TASK-1319 round 14 [P1]). If the
			// server has not yet confirmed our position in its op-log
			// (no op_log_cursor frame received in this session), our
			// Y.Doc may be populated by replay binaries whose
			// trailing cursor frame got lost to a network blip.
			// Sending local edits in that state lets the server
			// append + originator-cursor back, after which the next
			// flush would carry an "anchored" cursor and pass the
			// server's MIN check — overwriting items.content with
			// markdown derived from the stale-Y.Doc base. Buffer
			// edits locally instead; when a cursor arrives (or the
			// provider rebuilds via force_refresh), the next user
			// input fires handleDocUpdate again and we send normally.
			//
			// The trade-off: if a network blip persists indefinitely,
			// local edits sit in the editor but never reach the
			// server. That's acceptable — the user is offline either
			// way; the provider's reconnect loop will eventually
			// either (a) resync and propagate, or (b) trigger
			// force_refresh and the user gets a clean rebuild.
			if (!this.cursorAnchored) {
				// Buffer for replay on anchor (round 15 [P1]).
				// Yjs updates are causal — silently dropping one
				// keystroke makes later ops reference structs no
				// peer can resolve. Saving them ensures the full
				// chain reaches the server once it confirms our
				// position.
				if (this.preAnchorUpdates.length >= CollabProvider.MAX_PRE_ANCHOR_UPDATES) {
					// Pathological case: anchor never arrives. The
					// buffer would grow unbounded. Destroy this
					// provider SYNCHRONOUSLY before invoking the
					// recovery callback so a late op_log_cursor
					// can't anchor and flush the partial buffer
					// (the dropped prefix would reintroduce the
					// causal-missing-update bug). Per Codex round
					// 16 [P2] of TASK-1319.
					console.warn(
						'collab: pre-anchor buffer overflow; triggering recovery',
					);
					clearStoredCursor(this.itemID);
					this.lastOpLogID = 0;
					this.preAnchorUpdates = [];
					this.destroy();
					try {
						this.onForceRefresh?.();
					} catch (err) {
						console.warn('collab: onForceRefresh threw on overflow', err);
					}
					return;
				}
				this.preAnchorUpdates.push(update);
				return;
			}
			const enc = encoding.createEncoder();
			encoding.writeVarUint(enc, MESSAGE_SYNC);
			syncProtocol.writeUpdate(enc, update);
			this.send(encoding.toUint8Array(enc));
		};

		this.handleAwarenessUpdate = (changes, _origin) => {
			const ids = [...changes.added, ...changes.updated, ...changes.removed];
			if (ids.length === 0) return;
			const enc = encoding.createEncoder();
			encoding.writeVarUint(enc, MESSAGE_AWARENESS);
			encoding.writeVarUint8Array(
				enc,
				awarenessProtocol.encodeAwarenessUpdate(this.awareness, ids),
			);
			this.send(encoding.toUint8Array(enc));
		};

		this.handleBeforeUnload = () => {
			// Tell peers we're gone so cursor ghosts disappear promptly
			// instead of waiting for the awareness-state-timeout to
			// reap us.
			awarenessProtocol.removeAwarenessStates(
				this.awareness,
				[this.ydoc.clientID],
				'window unload',
			);
		};

		// Mobile-survival listeners (TASK-1265 / PLAN-1248). Mobile is
		// the known-fragile WS environment: iOS Safari can suspend the
		// JS runtime in the background, the OS may silently drop the
		// connection, and the user expects the editor to "just work"
		// when they swipe back to the tab.
		this.handleVisibilityChange = () => {
			if (typeof document === 'undefined') return;
			if (document.visibilityState !== 'visible') return;
			// Tab returned to foreground. Force a reconnect even if
			// the socket reports OPEN: iOS Safari (and other mobile
			// suspends) can silently kill the WS transport, leaving
			// `readyState === OPEN` until the next send fails.
			// `forceReconnect()` always closes + reconnects from a
			// clean slate. The 30s backoff ceiling would otherwise
			// dominate UX after a long iOS Safari background period.
			this.forceReconnect();
		};
		this.handleOnline = () => {
			// Network came back. Cancel any pending backoff timer
			// and try connecting now instead of waiting for the next
			// scheduled tick. The current `reconnectAttempts` counter
			// is preserved — a failing online-recovery attempt should
			// honour prior failures' backoff, not reset to zero.
			this.forceReconnect();
		};
		this.handleOffline = () => {
			// We lost network connectivity. Surface the offline state
			// immediately so the badge doesn't claim 'synced' while
			// no traffic is moving, and tear down the live socket so
			// no in-flight sync frame can flip `synced/state` back
			// after the offline event lands. closeSocket() removes
			// the message listener (so onMessage can't process a
			// stale syncStep2) AND removes the close listener (so
			// scheduleReconnect won't fire from the natural close);
			// we therefore have to inline our own scheduleReconnect
			// call. Per Codex rounds 3-4 [P2].
			this.state = 'offline';
			if (this.ws) {
				this.closeSocket(1000, 'network-offline');
				this.runDisconnectCleanup({ demoteState: false });
				// Keep the backoff alive in case the 'online' event
				// never fires (some browsers / edge cases) — the
				// connect() inside scheduleReconnect will keep
				// failing while truly offline and will succeed once
				// the network is back. De-dup any existing timer to
				// avoid leaking parallel backoffs.
				if (this.reconnectTimer !== undefined) {
					clearTimeout(this.reconnectTimer);
					this.reconnectTimer = undefined;
				}
				this.scheduleReconnect();
			} else {
				// No live socket — still kill any stray sync-grace
				// timer so it can't fire and contradict 'offline'.
				clearTimeout(this.syncGraceTimer);
				this.syncGraceTimer = undefined;
			}
		};

		this.ydoc.on('update', this.handleDocUpdate);
		this.awareness.on('update', this.handleAwarenessUpdate);

		if (typeof window !== 'undefined') {
			window.addEventListener('beforeunload', this.handleBeforeUnload);
			window.addEventListener('online', this.handleOnline);
			window.addEventListener('offline', this.handleOffline);
		}
		if (typeof document !== 'undefined') {
			document.addEventListener('visibilitychange', this.handleVisibilityChange);
		}

		this.connect();
	}

	/**
	 * Cancel any pending backoff timer, tear down any existing
	 * socket (including an apparently-OPEN one), and try connecting
	 * fresh. Idempotent. Safe to call when destroyed (no-op).
	 *
	 * The motivating case for "even when OPEN" is iOS Safari (and
	 * similar mobile suspends): the OS can silently kill the WS
	 * transport while leaving JS state intact, so `readyState` may
	 * read OPEN long after the connection is dead. The browser only
	 * notices on the next send → eventual close, by which time the
	 * user has been staring at a stale "Synced" badge for seconds.
	 * On visibility-resume / online events we therefore close
	 * unconditionally and reconnect from a clean slate. Per Codex
	 * review round 1 [P2].
	 *
	 * `closeSocket()` removes the close listener BEFORE calling
	 * `ws.close()`, which means `onClose`'s bookkeeping (clearing
	 * `connected` / `syncGraceTimer`, dropping stale peer awareness)
	 * never runs. We have to do that bookkeeping inline — but
	 * deliberately skip the `scheduleReconnect()` step, because the
	 * explicit `connect()` call below replaces it.
	 */
	private forceReconnect(): void {
		if (this.destroyed) return;

		if (this.reconnectTimer !== undefined) {
			clearTimeout(this.reconnectTimer);
			this.reconnectTimer = undefined;
		}

		if (this.ws) {
			this.closeSocket(1000, 'force-reconnect');
			this.runDisconnectCleanup({ demoteState: true });
		}

		this.connect();
	}

	/**
	 * Cleanup shared by `onClose` and the paths that bypass it via
	 * `closeSocket()` (which removes the close listener before calling
	 * `ws.close()`): `forceReconnect`, `handleOffline`, plus any
	 * future teardown that needs the same end state.
	 *
	 * - Always: clear `connected`, kill the per-open syncGraceTimer,
	 *   drop stale non-self awareness so peer cursors don't linger.
	 * - With `demoteState: true`: also mirror `onClose`'s state demote
	 *   (keep 'offline' sticky; otherwise pre-sync→'connecting',
	 *   post-sync→'reconnecting'). `handleOffline` skips this because
	 *   it has already set state='offline' explicitly.
	 *
	 * Per Codex round 5 NIT — extracting this helper so a future
	 * teardown path doesn't drift out of sync.
	 */
	private runDisconnectCleanup(opts: { demoteState: boolean }): void {
		this.connected = false;
		clearTimeout(this.syncGraceTimer);
		this.syncGraceTimer = undefined;
		if (opts.demoteState && this.state !== 'offline') {
			this.state = this.synced ? 'reconnecting' : 'connecting';
		}
		const peerIds: number[] = [];
		this.awareness.getStates().forEach((_state, clientID) => {
			if (clientID !== this.ydoc.clientID) peerIds.push(clientID);
		});
		if (peerIds.length > 0) {
			awarenessProtocol.removeAwarenessStates(this.awareness, peerIds, this);
		}
	}

	/** Cleanly close the WS, unbind doc/awareness handlers, and
	 *  prevent further reconnect attempts. Safe to call more than
	 *  once; idempotent. */
	destroy(): void {
		if (this.destroyed) return;
		this.destroyed = true;

		if (this.reconnectTimer !== undefined) {
			clearTimeout(this.reconnectTimer);
			this.reconnectTimer = undefined;
		}
		clearTimeout(this.syncGraceTimer);
		this.syncGraceTimer = undefined;

		// Best-effort presence cleanup before tearing the socket down.
		// If we're already disconnected the awareness send is a no-op.
		awarenessProtocol.removeAwarenessStates(
			this.awareness,
			[this.ydoc.clientID],
			'destroy',
		);

		this.ydoc.off('update', this.handleDocUpdate);
		this.awareness.off('update', this.handleAwarenessUpdate);
		this.awareness.destroy();

		if (typeof window !== 'undefined') {
			window.removeEventListener('beforeunload', this.handleBeforeUnload);
			window.removeEventListener('online', this.handleOnline);
			window.removeEventListener('offline', this.handleOffline);
		}
		if (typeof document !== 'undefined') {
			document.removeEventListener('visibilitychange', this.handleVisibilityChange);
		}

		this.closeSocket(1000, 'destroyed');
		this.connected = false;
	}

	private connect(): void {
		if (this.destroyed) return;

		let ws: WebSocket;
		try {
			ws = new this.WebSocketImpl(this.connectUrl());
		} catch (err) {
			// Construction can throw on a malformed URL; reschedule
			// instead of swallowing — same surface the runtime would
			// hit on a failed handshake.
			this.scheduleReconnect();
			console.warn('collab: WebSocket construction failed', err);
			return;
		}

		ws.binaryType = 'arraybuffer';

		ws.addEventListener('open', this.onOpen);
		ws.addEventListener('message', this.onMessage);
		ws.addEventListener('close', this.onClose);
		ws.addEventListener('error', this.onError);

		this.ws = ws;
	}

	private readonly onOpen = (): void => {
		this.connected = true;
		// Public state ONLY moves on real progress: an OPEN socket
		// alone is not progress (a flaky proxy can OPEN→CLOSE-before-
		// sync repeatedly). State stays at whatever the most recent
		// close handler set it to (or 'connecting' on first attempt)
		// until the sync handshake or grace timer below flips it to
		// 'synced'. Specifically:
		//   - 'offline' stays 'offline' (don't flicker to 'connecting'
		//     until the proxy actually delivers a sync). Per Codex
		//     round 2 [P2].
		//   - 'connecting'/'reconnecting' stay as-is, awaiting sync.
		// 'synced' is unreachable here in practice because onClose
		// always demotes it before scheduling a reconnect, so we
		// don't need an explicit guard.
		// NB: reconnectAttempts is also NOT reset here — same reason.
		// Reset is owned by the actual-sync paths (syncStep2 branch +
		// syncGraceTimer below). Per Codex round 1 [P2].

		// Initial syncStep1: send our current state vector. Server
		// replays the op-log (which contains all prior peer ops) so
		// we end up with a converged Y.Doc. The server itself doesn't
		// reply with a step2 — the dumb-relay design lets the
		// replayed ops + any concurrent peer's responses do the work.
		const enc = encoding.createEncoder();
		encoding.writeVarUint(enc, MESSAGE_SYNC);
		syncProtocol.writeSyncStep1(enc, this.ydoc);
		this.send(encoding.toUint8Array(enc));

		// Push our current Y.Doc state as a single update. This is
		// how local-only edits made while disconnected reach the
		// server: handleDocUpdate's `send()` is a no-op when the
		// socket is closed (no buffering), so without this catch-up
		// frame those ops would never persist or broadcast on
		// reconnect. Yjs CRDT updates are idempotent — when the
		// server already has these ops via op-log replay this is a
		// harmless no-op for peers. For very large docs this is
		// wasteful and TASK-1265's mobile-reconnect work can replace
		// it with a buffered-queue approach; for v1 the simplicity
		// wins.
		//
		// **Skip when the session is unanchored** (cursorAnchored
		// false). A non-empty Y.Doc with no server-acked cursor is
		// a known-stale combination (network blip during the
		// post-replay cursor write left us holding replay-applied
		// state that the server may have since pruned). Sending
		// Y.encodeStateAsUpdate in that case can resurrect ops the
		// server has pruned and corrupt items.content on the next
		// flush. The server's replay (and the lazy-seed path's
		// items.content fallback) repopulates Y.Doc canonically;
		// we don't need to push our state to recover.
		//
		// Anchored at cursor=0 IS valid — that's the legitimate
		// 'server has nothing in op-log yet' case. Local edits
		// made during a brief offline window before reconnect
		// MUST go out via this catch-up path; gating on
		// `lastOpLogID > 0` instead would leave them stranded.
		// Per Codex round 17 [P2] of TASK-1319.
		if (this.cursorAnchored) {
			const enc3 = encoding.createEncoder();
			encoding.writeVarUint(enc3, MESSAGE_SYNC);
			syncProtocol.writeUpdate(enc3, Y.encodeStateAsUpdate(this.ydoc));
			this.send(encoding.toUint8Array(enc3));
		}

		// Broadcast our local awareness state (if any) so peers see
		// us right away. With no local state set this is a no-op.
		const localState = this.awareness.getLocalState();
		if (localState !== null) {
			const enc2 = encoding.createEncoder();
			encoding.writeVarUint(enc2, MESSAGE_AWARENESS);
			encoding.writeVarUint8Array(
				enc2,
				awarenessProtocol.encodeAwarenessUpdate(this.awareness, [this.ydoc.clientID]),
			);
			this.send(encoding.toUint8Array(enc2));
		}

		// Grace fallback: if we don't see an explicit syncStep2 within
		// SYNC_GRACE_MS, flip `synced` true anyway. The dumb-relay
		// server replays the op-log as BinaryMessage frames but never
		// sends its own step2, so an empty/pruned op-log + first-peer
		// connect would otherwise leave `synced` stuck at false —
		// blocking the lazy seed in TASK-1261. The grace gives any
		// real replay rows time to land first. Per Codex review
		// round 1.
		clearTimeout(this.syncGraceTimer);
		this.syncGraceTimer = setTimeout(() => {
			if (!this.synced) this.synced = true;
			if (this.connected) {
				this.state = 'synced';
				// Treat the grace expiry as a successful sync —
				// the dumb-relay design means an empty/pruned op-log
				// + first peer is the canonical "everything is fine"
				// case. Reset backoff so a subsequent disconnect
				// starts fresh. Per Codex review round 1 [P2].
				this.reconnectAttempts = 0;
			}
		}, SYNC_GRACE_MS);
	};

	private readonly onMessage = (e: MessageEvent): void => {
		const data = e.data;

		// TextMessage frames carry JSON control envelopes (today:
		// `applier_request` from the designated-applier protocol).
		// Mirrors the server's read-loop branching in
		// `internal/collab/room.go`.
		if (typeof data === 'string') {
			// Pin the source socket so a force-reconnect (TASK-1265)
			// during the awaited applier handler doesn't ack on a
			// different connection — the server keys pending acks by
			// the conn that delivered the request. `e.currentTarget`
			// is the socket the listener was bound to, which is more
			// strictly correct than reading `this.ws` (the latter is
			// also our current socket, but only by current invariant).
			this.handleControlMessage(data, e.currentTarget as WebSocket | null);
			return;
		}

		if (!(data instanceof ArrayBuffer) || data.byteLength === 0) return;

		const decoder = decoding.createDecoder(new Uint8Array(data));
		const messageType = decoding.readVarUint(decoder);

		switch (messageType) {
			case MESSAGE_SYNC: {
				// Mark that the server applied SOMETHING to our
				// Y.Doc. Distinguishes "remote replay landed" from
				// "only local edits in Y.Doc" so the cursor=0 +
				// non-empty-Y.Doc safety check (round 18) doesn't
				// false-positive on legitimate local pre-anchor
				// edits. Per Codex round 19 [P1].
				this.remoteSyncApplied = true;
				const enc = encoding.createEncoder();
				encoding.writeVarUint(enc, MESSAGE_SYNC);
				const subtype = syncProtocol.readSyncMessage(decoder, enc, this.ydoc, this);
				// readSyncMessage writes a reply only when the inbound
				// frame was a syncStep1 (encoder gets a syncStep2
				// payload appended). For step2 / update there's no
				// reply — encoder still has the leading message-type
				// byte (length == 1) and we skip the send.
				if (encoding.length(enc) > 1) {
					this.send(encoding.toUint8Array(enc));
				}
				if (subtype === syncProtocol.messageYjsSyncStep2) {
					this.synced = true;
					if (this.connected) {
						this.state = 'synced';
						// Successful sync — reset the backoff counter
						// so a subsequent disconnect starts fresh.
						// Per Codex review round 1 [P2].
						this.reconnectAttempts = 0;
					}
				}
				break;
			}
			case MESSAGE_AWARENESS: {
				awarenessProtocol.applyAwarenessUpdate(
					this.awareness,
					decoding.readVarUint8Array(decoder),
					this,
				);
				break;
			}
			default:
				// Unknown / future message type — silently ignore so
				// older clients survive a server that grows new
				// envelope types.
				break;
		}
	};

	private async handleControlMessage(raw: string, sourceWs: WebSocket | null): Promise<void> {
		let msg: {
			type?: string;
			request_id?: string;
			markdown?: string;
			expires_at_millis?: number;
			op_log_id?: number;
		};
		try {
			msg = JSON.parse(raw);
		} catch {
			console.warn('collab: dropping malformed control message');
			return;
		}

		switch (msg.type) {
			case 'op_log_cursor': {
				// TASK-1319: server announces the highest persisted
				// op-log id we should now consider applied. Persist
				// per-tab so a reconnect (or page reload) can
				// announce `?since=<id>` and either get a delta
				// replay or a force_refresh.
				if (typeof msg.op_log_id !== 'number' || !Number.isFinite(msg.op_log_id)) {
					console.warn('collab: op_log_cursor missing op_log_id');
					return;
				}
				if (msg.op_log_id < 0) return;
				// Pre-anchor sanity check: cursor=0 means the
				// server's op-log is currently empty. If this Y.Doc
				// already has state at this moment, those ops came
				// from somewhere — almost certainly replay binaries
				// from a previous connection within this provider's
				// life that didn't reach the post-replay cursor
				// frame, followed by a server-side prune
				// (PruneAndApply, schema rebuild, dormant GC) during
				// our disconnect. Letting this anchor would mark a
				// stale Y.Doc as authoritative; the next send-on-open
				// or flush would resurrect pre-prune state and
				// overwrite canonical items.content.
				//
				// Trigger force-refresh recovery instead. Per Codex
				// round 18 [P1] of TASK-1319.
				if (!this.cursorAnchored && msg.op_log_id === 0 && this.remoteSyncApplied) {
					// Remote replay binaries reached us before this
					// cursor=0 frame, but the server now reports an
					// empty op-log — a prune happened mid-session.
					// Y.Doc holds pre-prune state. Recover.
					//
					// We GATE on remoteSyncApplied so legitimate
					// local pre-anchor edits (user typed before the
					// initial cursor=0 of an empty op-log arrived)
					// don't trip this branch — those updates live in
					// preAnchorUpdates and will flush on anchor.
					// Per Codex round 19 [P1] of TASK-1319.
					console.warn(
						'collab: cursor=0 received against remote-applied Y.Doc; treating as force_refresh',
					);
					clearStoredCursor(this.itemID);
					this.lastOpLogID = 0;
					this.preAnchorUpdates = [];
					this.destroy();
					try {
						this.onForceRefresh?.();
					} catch (err) {
						console.warn(
							'collab: onForceRefresh threw on stale cursor=0',
							err,
						);
					}
					return;
				}
				// Anchor the session: any cursor frame — including
				// the initial post-replay cursor=0 against an empty
				// op-log — proves the server has finished its
				// replay-time view of the op-log. handleDocUpdate
				// gates on this so local edits can't propagate
				// before the server has weighed in. Per Codex
				// round 14 [P1].
				const wasAnchored = this.cursorAnchored;
				this.cursorAnchored = true;
				// Flush buffered pre-anchor updates in order so
				// causal Yjs ops aren't silently dropped — sending
				// only later updates would let the server (and
				// peers) miss intervening structs. Per Codex round
				// 15 [P1].
				if (!wasAnchored && this.preAnchorUpdates.length > 0) {
					const buffered = this.preAnchorUpdates;
					this.preAnchorUpdates = [];
					for (const upd of buffered) {
						const enc = encoding.createEncoder();
						encoding.writeVarUint(enc, MESSAGE_SYNC);
						syncProtocol.writeUpdate(enc, upd);
						this.send(encoding.toUint8Array(enc));
					}
				}
				// Never regress the cursor: the server's INITIAL
				// post-replay cursor frame can in theory follow a
				// MORE-RECENT live op the previous incarnation
				// already wrote out (race window between the
				// async post-replay cursor write and a concurrent
				// AppendYjsUpdate that the bus already broadcast).
				// Taking the max guards against that.
				if (msg.op_log_id <= this.lastOpLogID) return;
				this.lastOpLogID = msg.op_log_id;
				writeStoredCursor(this.itemID, msg.op_log_id);
				return;
			}
			case 'force_refresh': {
				// TASK-1319: server has decided we can't safely
				// continue (our `?since=<id>` was below MIN). Clear
				// the persisted cursor so a downstream rebuild
				// starts fresh, then notify the page so it can
				// recreate the editor on a clean Y.Doc.
				clearStoredCursor(this.itemID);
				this.lastOpLogID = 0;
				// Destroy this provider SYNCHRONOUSLY before
				// invoking the page-level handler. Without this,
				// the consumer's recovery path is async (e.g.
				// awaits an items.get refetch before swapping
				// providers); meanwhile our own onClose handler
				// schedules a reconnect that races the recovery,
				// re-opens with `?since=0`, and pushes
				// `Y.encodeStateAsUpdate` of the (still-stale)
				// local Y.Doc — recreating exactly the
				// stale-content corruption force_refresh was
				// meant to prevent. destroy() sets
				// `this.destroyed = true` so scheduleReconnect
				// short-circuits, removes window/document
				// listeners, and closes the socket. Per Codex
				// round 6 [P1].
				this.destroy();
				if (this.onForceRefresh) {
					try {
						this.onForceRefresh();
					} catch (err) {
						console.warn('collab: onForceRefresh threw', err);
					}
				} else {
					console.warn(
						'collab: received force_refresh but no onForceRefresh handler is installed; editor will be on stale state',
					);
				}
				return;
			}
			case 'applier_request': {
				if (!msg.request_id || typeof msg.markdown !== 'string') {
					console.warn('collab: applier_request missing required fields');
					return;
				}
				if (!this.onApplierRequest) {
					// No handler installed — server falls back after timeout.
					return;
				}

				// Late-apply guard. The server stamps each applier_request
				// with an expires_at_millis specifically so backgrounded
				// tabs that wake up after the timeout don't overwrite
				// peers' newer edits with stale markdown. Check before
				// invoking the handler AND again before acking — the
				// handler is awaited and could span the deadline.
				const expiresAt = msg.expires_at_millis ?? 0;
				if (expiresAt > 0 && Date.now() > expiresAt) {
					console.warn('collab: dropping expired applier_request', msg.request_id);
					return;
				}

				let applied = false;
				try {
					applied = await this.onApplierRequest(
						msg.markdown,
						msg.request_id,
						expiresAt,
					);
				} catch (err) {
					console.warn('collab: applier handler threw, treating as not-applied', err);
					applied = false;
				}

				if (!applied) return;
				if (expiresAt > 0 && Date.now() > expiresAt) {
					// Awaited handler crossed the deadline. The server
					// has likely retried or fallen back; don't ack a
					// late apply.
					console.warn('collab: applier_request acked too late, suppressing', msg.request_id);
					return;
				}

				const ack = JSON.stringify({
					type: 'applier_ack',
					request_id: msg.request_id,
				});
				// MUST send the ack on the same socket that delivered
				// the request — the server keys pending acks by conn,
				// so an ack sent on a post-force-reconnect new socket
				// is silently ignored and the external write waits
				// for the applier-timeout fallback (TASK-1257). Per
				// Codex review round 2 [P2] of TASK-1265.
				if (
					sourceWs &&
					sourceWs === this.ws &&
					sourceWs.readyState === this.WebSocketImpl.OPEN
				) {
					sourceWs.send(ack);
				} else {
					console.warn(
						'collab: applier source socket gone, suppressing late ack',
						msg.request_id,
					);
				}
				return;
			}
			default:
				// Unknown / future control type — silently ignore so
				// older clients survive a server that grows new ones.
				return;
		}
	}

	private readonly onClose = (): void => {
		// Shared cleanup with forceReconnect / handleOffline:
		// connected=false, syncGraceTimer cleared, peer awareness
		// dropped, and (with demoteState) the state demote that
		// preserves 'offline' across backoff retries while splitting
		// pre-sync→'connecting' from post-sync→'reconnecting'.
		// Per Codex rounds 1-5 of TASK-1264/-1265.
		this.runDisconnectCleanup({ demoteState: true });

		if (this.destroyed) return;

		// NB: do NOT reset reconnectAttempts based on `wasConnected`
		// here. Reaching OPEN is not proof of a real working session
		// (a flaky proxy can ESTABLISH then immediately CLOSE before
		// any sync frame lands); only `synced` is. The reset is owned
		// by the syncStep2 branch and the grace timer in onOpen — the
		// actual successful-sync edges of the state machine. Per Codex
		// review round 1 [P2] of TASK-1264.
		this.scheduleReconnect();
	};

	private readonly onError = (): void => {
		// Browsers fire 'close' immediately after 'error' on a failed
		// WS handshake; cleanup happens there. We just suppress the
		// default uncaught-error noise.
	};

	private scheduleReconnect(): void {
		if (this.destroyed) return;
		const delay = Math.min(
			RECONNECT_MAX_MS,
			RECONNECT_BASE_MS * 2 ** this.reconnectAttempts,
		);
		this.reconnectAttempts++;
		// Public state stays at the close-time variant
		// ('connecting' before any sync, 'reconnecting' after) until
		// we've burned through OFFLINE_THRESHOLD attempts; after that
		// the UX flips to 'offline' so the user knows it's not coming
		// back on its own. Provider keeps trying in the background.
		// Per Codex rounds 1-2 [P2/NIT].
		if (this.reconnectAttempts > OFFLINE_THRESHOLD) {
			this.state = 'offline';
		} else if (this.state !== 'offline') {
			this.state = this.synced ? 'reconnecting' : 'connecting';
		}
		this.reconnectTimer = setTimeout(() => {
			this.reconnectTimer = undefined;
			this.connect();
		}, delay);
	}

	private send(data: Uint8Array): void {
		if (this.ws && this.ws.readyState === this.WebSocketImpl.OPEN) {
			// TS 6's lib.dom narrows WebSocket.send to require a buffer
			// view backed by ArrayBuffer (not SharedArrayBuffer). lib0's
			// `toUint8Array` returns the looser `Uint8Array<ArrayBufferLike>`,
			// which is functionally fine at runtime but trips the
			// type checker. Cast through BufferSource — the bytes are
			// always page-local.
			this.ws.send(data as unknown as BufferSource);
		}
	}

	/**
	 * Compute the URL to connect to on this attempt. The base URL
	 * is the (immutable) collab endpoint with `schema_version`
	 * already encoded; we additionally append `since=<id>` whenever
	 * `lastOpLogID > 0` so the server can:
	 *   - replay only the rows past our cursor (efficient resume), OR
	 *   - send `force_refresh` if our cursor is below MIN (rows
	 *     pruned out from under us) and close the conn.
	 *
	 * Skipping the param when `lastOpLogID === 0` matches the
	 * server's "treat as fresh client" branch and keeps test fixtures
	 * that hard-code the base URL working as-is.
	 */
	private connectUrl(): string {
		if (this.lastOpLogID <= 0) return this.url;
		const sep = this.url.includes('?') ? '&' : '?';
		return `${this.url}${sep}since=${this.lastOpLogID}`;
	}

	private closeSocket(code: number, reason: string): void {
		const ws = this.ws;
		if (!ws) return;
		ws.removeEventListener('open', this.onOpen);
		ws.removeEventListener('message', this.onMessage);
		ws.removeEventListener('close', this.onClose);
		ws.removeEventListener('error', this.onError);
		try {
			if (ws.readyState === this.WebSocketImpl.OPEN || ws.readyState === this.WebSocketImpl.CONNECTING) {
				ws.close(code, reason);
			}
		} catch {
			// Already closed / never opened — nothing to do.
		}
		this.ws = null;
	}
}

function defaultCollabBaseUrl(itemID: string): string {
	if (typeof window === 'undefined' || typeof location === 'undefined') {
		throw new Error('CollabProvider: cannot derive URL outside the browser');
	}
	const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
	// Always announce the client's SCHEMA_VERSION (TASK-1268). The
	// server validates this BEFORE upgrading the WebSocket; a
	// mismatch returns an HTTP 400 so the user sees a real error
	// rather than a silently-degraded session. The server also uses
	// the per-item rebuild path internally if any persisted op-log
	// rows are stamped with an older version.
	//
	// `since=<id>` is NOT in the base URL; it's appended per-attempt
	// by `connectUrl()` so a reconnect after live edits announces the
	// freshest cursor available. Per TASK-1319.
	const params = new URLSearchParams({ schema_version: SCHEMA_VERSION });
	return `${proto}//${location.host}/api/v1/collab/${encodeURIComponent(itemID)}?${params.toString()}`;
}
