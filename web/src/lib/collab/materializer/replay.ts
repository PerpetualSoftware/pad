// Op-log replay for the headless materializer (TASK-2198).
//
// Each item_yjs_updates row is one y-protocols WebSocket message, and this
// replays them the way a tab's CollabProvider consumes them
// (wsProvider.svelte.ts, the MESSAGE_SYNC arm of its onmessage):
//   - a frame whose message type is not MESSAGE_SYNC (0) is not a document
//     change and is skipped;
//   - MESSAGE_SYNC goes through syncProtocol.readSyncMessage, so SyncStep1
//     (a state vector) is answered into a throwaway encoder and never applied,
//     while SyncStep2 and Update are applied;
//   - a frame that throws is skipped and replay continues, because a throw in
//     one onmessage call does not stop the next message from being handled.
// The server hands over CONTENT-BEARING rows only
// (item_yjs_updates.content_bearing); the spike measured content-bearing-only
// replay equal to full replay on 423 of 423 real items.
import * as Y from 'yjs';
import * as syncProtocol from 'y-protocols/sync';
import * as decoding from 'lib0/decoding';
import * as encoding from 'lib0/encoding';

export const MESSAGE_SYNC = 0;

export interface ReplayCounts {
	step1: number;
	step2: number;
	update: number;
	skipped: number;
	threw: number;
}

export function replayFrames(doc: Y.Doc, frames: Uint8Array[]): ReplayCounts {
	const counts: ReplayCounts = { step1: 0, step2: 0, update: 0, skipped: 0, threw: 0 };
	for (const bytes of frames) {
		if (bytes.length === 0) {
			counts.skipped++;
			continue;
		}
		try {
			const dec = decoding.createDecoder(bytes);
			if (decoding.readVarUint(dec) !== MESSAGE_SYNC) {
				counts.skipped++;
				continue;
			}
			const enc = encoding.createEncoder();
			encoding.writeVarUint(enc, MESSAGE_SYNC);
			const subtype = syncProtocol.readSyncMessage(dec, enc, doc, null);
			if (subtype === syncProtocol.messageYjsSyncStep1) counts.step1++;
			else if (subtype === syncProtocol.messageYjsSyncStep2) counts.step2++;
			else counts.update++;
		} catch {
			counts.threw++;
		}
	}
	return counts;
}

/** Encode one Yjs update as the y-protocols Update frame a tab sends. */
export function updateFrame(update: Uint8Array): Uint8Array {
	const enc = encoding.createEncoder();
	encoding.writeVarUint(enc, MESSAGE_SYNC);
	syncProtocol.writeUpdate(enc, update);
	return encoding.toUint8Array(enc);
}
