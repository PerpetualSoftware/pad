// TASK-4195: the Go renderer (internal/server/render/attachments.go) and this
// one must emit byte-identical file chips. Both assert the same fixture, so a
// change on either side fails its own suite.
import { describe, expect, it } from 'vitest';
import fixture from './testdata/file-chip-parity.json';
import { renderAttachmentChip } from './attachments';

describe('file chip: byte parity with the Go renderer', () => {
	it.each(fixture.cases)('$name', (c) => {
		const html = renderAttachmentChip(
			{ ...c.meta, width: null, height: null },
			c.display_text || undefined,
			c.workspace
		);
		expect(html).toBe(c.html);
	});
});
