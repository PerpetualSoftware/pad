// TASK-3574 (4): what a set-aside replays to. The rows the store's tests set
// aside (internal/store/task3574_set_aside_whole_test.go, the same pinned
// frames): the WHOLE log replays to the document; the old unflushed-suffix
// selection replayed to nothing, every edit pending.
import { describe, it, expect } from 'vitest';
import * as Y from 'yjs';
import { replayFrames } from './replay';

const frames: Record<string, string> = {
	u1: 'AAIsAQOSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYQA=',
	u2: 'AAIQAQGSIQeEkiEGBSBiZXRhAA==',
	u3: 'AAIRAQGSIQyEkiELBiBnYW1tYQA=',
	step2: 'AAE2AQSSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYYSSIQYFIGJldGEA',
};
const bytes = (n: string) => new Uint8Array(Buffer.from(frames[n], 'base64'));

function replay(names: string[]) {
	const doc = new Y.Doc();
	replayFrames(doc, names.map(bytes));
	return { pending: doc.store.pendingStructs !== null, text: doc.getXmlFragment('default').toString() };
}

describe('TASK-3574 (4): set-aside rows replay on their own', () => {
	it('the whole log (what is now set aside) replays to the document', () => {
		const r = replay(['u1', 'u2', 'step2', 'u3']);
		expect(r.pending).toBe(false);
		expect(r.text).toContain('alpha beta gamma');
	});
	it('the old unflushed suffix alone replays to nothing', () => {
		expect(replay(['u2', 'u3'])).toEqual({ pending: true, text: '' });
	});
});
