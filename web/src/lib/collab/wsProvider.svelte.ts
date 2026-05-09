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

/** First-byte discriminators on the wire. Must match the constants in
 *  `internal/collab/room.go` (yMessageSync / yMessageAwareness). */
const MESSAGE_SYNC = 0;
const MESSAGE_AWARENESS = 1;

/** Reconnect backoff: 1s, 2s, 4s, … capped at 30s. Reset on a clean
 *  open. Sophisticated mobile reconnect (visibility, network state)
 *  is TASK-1265's concern; this is the floor behavior. */
const RECONNECT_BASE_MS = 1_000;
const RECONNECT_MAX_MS = 30_000;

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

	private ws: WebSocket | null = null;
	private readonly WebSocketImpl: typeof WebSocket;
	private readonly onApplierRequest?: ApplierRequestHandler;
	private destroyed = false;
	private reconnectAttempts = 0;
	private reconnectTimer: ReturnType<typeof setTimeout> | undefined;
	private syncGraceTimer: ReturnType<typeof setTimeout> | undefined;

	private readonly handleDocUpdate: (update: Uint8Array, origin: unknown) => void;
	private readonly handleAwarenessUpdate: (changes: { added: number[]; updated: number[]; removed: number[] }, origin: unknown) => void;
	private readonly handleBeforeUnload: () => void;

	constructor(itemID: string, ydoc: Y.Doc, options: CollabProviderOptions = {}) {
		this.itemID = itemID;
		this.ydoc = ydoc;
		this.awareness = new awarenessProtocol.Awareness(ydoc);

		this.WebSocketImpl = options.WebSocketImpl ?? globalThis.WebSocket;
		this.url = options.url ?? defaultCollabUrl(itemID);
		this.onApplierRequest = options.onApplierRequest;

		this.handleDocUpdate = (update, origin) => {
			// Skip ops that came from us applying a server message —
			// otherwise we'd echo every remote keystroke back to the
			// server (which would persist + rebroadcast it again).
			if (origin === this) return;
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

		this.ydoc.on('update', this.handleDocUpdate);
		this.awareness.on('update', this.handleAwarenessUpdate);

		if (typeof window !== 'undefined') {
			window.addEventListener('beforeunload', this.handleBeforeUnload);
		}

		this.connect();
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
		}

		this.closeSocket(1000, 'destroyed');
		this.connected = false;
	}

	private connect(): void {
		if (this.destroyed) return;

		let ws: WebSocket;
		try {
			ws = new this.WebSocketImpl(this.url);
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
		this.reconnectAttempts = 0;
		this.connected = true;

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
		const enc3 = encoding.createEncoder();
		encoding.writeVarUint(enc3, MESSAGE_SYNC);
		syncProtocol.writeUpdate(enc3, Y.encodeStateAsUpdate(this.ydoc));
		this.send(encoding.toUint8Array(enc3));

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
		}, SYNC_GRACE_MS);
	};

	private readonly onMessage = (e: MessageEvent): void => {
		const data = e.data;

		// TextMessage frames carry JSON control envelopes (today:
		// `applier_request` from the designated-applier protocol).
		// Mirrors the server's read-loop branching in
		// `internal/collab/room.go`.
		if (typeof data === 'string') {
			this.handleControlMessage(data);
			return;
		}

		if (!(data instanceof ArrayBuffer) || data.byteLength === 0) return;

		const decoder = decoding.createDecoder(new Uint8Array(data));
		const messageType = decoding.readVarUint(decoder);

		switch (messageType) {
			case MESSAGE_SYNC: {
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

	private async handleControlMessage(raw: string): Promise<void> {
		let msg: { type?: string; request_id?: string; markdown?: string; expires_at_millis?: number };
		try {
			msg = JSON.parse(raw);
		} catch {
			console.warn('collab: dropping malformed control message');
			return;
		}

		switch (msg.type) {
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
				if (this.ws && this.ws.readyState === this.WebSocketImpl.OPEN) {
					this.ws.send(ack);
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
		const wasConnected = this.connected;
		this.connected = false;
		// The sync-grace timer is per-open; if it hasn't fired yet
		// it would set synced=true even after we lost the socket.
		// Cancel so reconnect's onOpen can install a fresh one.
		clearTimeout(this.syncGraceTimer);
		this.syncGraceTimer = undefined;

		// Clear ALL non-self awareness states. Without this, peer
		// cursors would linger after the socket drops and reappear
		// in stale positions on reconnect.
		const ids: number[] = [];
		this.awareness.getStates().forEach((_state, clientID) => {
			if (clientID !== this.ydoc.clientID) ids.push(clientID);
		});
		if (ids.length > 0) {
			awarenessProtocol.removeAwarenessStates(this.awareness, ids, this);
		}

		if (this.destroyed) return;

		// If we never even completed an open, this counts as a failed
		// attempt and bumps the backoff. wasConnected==true means we
		// had a real session that dropped — start backoff at the floor.
		if (wasConnected) {
			this.reconnectAttempts = 0;
		}
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

function defaultCollabUrl(itemID: string): string {
	if (typeof window === 'undefined' || typeof location === 'undefined') {
		throw new Error('CollabProvider: cannot derive URL outside the browser');
	}
	const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
	return `${proto}//${location.host}/api/v1/collab/${encodeURIComponent(itemID)}`;
}
