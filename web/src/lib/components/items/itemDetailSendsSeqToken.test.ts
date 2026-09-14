import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// BUG-3037 — the item pane's field save must carry the SEQ token.
//
// WHY A SOURCE GUARD: ItemDetail cannot be mounted without the workspace shell
// (the same reason the sibling guards in this directory give). WHAT IT CANNOT
// DO: it checks spellings, not behaviour. The behaviour it stands in for is
// covered where it can be: occToken.test.ts pins which token is chosen for which
// row, fieldWriteOrder.test.ts pins that the retry re-reads the token through
// `tokenOf`, and internal/server/bug3037_expected_seq_test.go pins that the
// server refuses a same-second race on seq and accepts it on updated_at.
const SRC = readFileSync(resolve(__dirname, './ItemDetail.svelte'), 'utf8').replace(
	/^[ \t]*\/\/.*$/gm,
	'',
);

function updateFieldBody(): string {
	const start = SRC.indexOf('async function updateField(');
	expect(start, 'updateField was renamed or removed — re-point this guard').toBeGreaterThan(-1);
	const end = SRC.indexOf('\n\t}', SRC.indexOf('submitOrderedOCC', start));
	return SRC.slice(start, end);
}

describe('BUG-3037 — the pane sends the strong token', () => {
	it('sends expected_seq for a row that has a seq', () => {
		const body = updateFieldBody();
		expect(body, 'the field save no longer sends expected_seq').toContain('expected_seq');
		// The weak token is still REACHABLE — it is the cached-row arm — so this
		// asserts the CHOICE is made by occTokenFor rather than hardcoded.
		expect(body, 'the token is not chosen by occTokenFor').toMatch(/occTokenFor\(targetItem\)/);
		expect(body, 'the retry does not re-read the token through tokenOf').toMatch(
			/tokenOf: occTokenFor/,
		);
	});

	it('still has an updated_at arm, because a cached row has no seq', () => {
		const body = updateFieldBody();
		expect(
			body,
			'the updated_at arm is gone — a row cached before BUG-3037 would write with NO token',
		).toContain('expected_updated_at');
	});
});
