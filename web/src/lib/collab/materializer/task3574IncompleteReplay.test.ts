// TASK-3574 claim (1): recovery (materializeWith) serialized an op-log whose
// replay left structs PENDING (they reference a row the log does not hold),
// while compaction (snapshotWith) refused the same input. A truncated but
// nonblank body then passed the store's only guard (blank output) and the
// recovery flush advanced the watermark over content it never saw.
import { describe, it, expect } from 'vitest';
import * as Y from 'yjs';
import { createHeadlessEditor, materializeWith, snapshotWith } from './entry';
import { updateFrame } from './replay';

const b64 = (u: Uint8Array) => Buffer.from(u).toString('base64');

/** Three sequential updates; the third's insertion is anchored on the second's text. */
function threeUpdates(): Uint8Array[] {
	const doc = new Y.Doc();
	const ups: Uint8Array[] = [];
	doc.on('update', (u: Uint8Array) => ups.push(u));
	const frag = doc.getXmlFragment('default');
	const p = new Y.XmlElement('paragraph');
	const t = new Y.XmlText();
	p.insert(0, [t]);
	doc.transact(() => {
		frag.insert(0, [p]);
		t.insert(0, 'alpha');
	});
	doc.transact(() => t.insert(5, ' beta'));
	doc.transact(() => t.insert(10, ' gamma'));
	expect(ups).toHaveLength(3);
	return ups;
}

describe('TASK-3574 (1): an incomplete replay is refused, not serialized', () => {
	const ed = createHeadlessEditor();

	it('control: the complete op-log materializes all three updates', () => {
		const rows = threeUpdates().map((u) => b64(updateFrame(u)));
		expect(materializeWith(ed, { rows, link_index: [], workspace_slug: 'ws' }).trim()).toBe('alpha beta gamma');
	});

	it('premise: compaction refuses the op-log with the middle row missing', () => {
		const [u1, , u3] = threeUpdates();
		const rows = [u1, u3].map((u) => b64(updateFrame(u)));
		expect(() => snapshotWith(ed, { rows, link_index: [], workspace_slug: 'ws' })).toThrow(/pending structs/);
	});

	it('recovery refuses the same op-log instead of returning the truncated body', () => {
		const [u1, , u3] = threeUpdates();
		const rows = [u1, u3].map((u) => b64(updateFrame(u)));
		let out: string | undefined;
		let threw = false;
		try {
			out = materializeWith(ed, { rows, link_index: [], workspace_slug: 'ws' });
		} catch {
			threw = true;
		}
		expect(threw, `recovery returned ${JSON.stringify(out)} from an op-log missing a row`).toBe(true);
	});
});
