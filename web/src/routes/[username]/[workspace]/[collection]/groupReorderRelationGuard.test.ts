// Node-project test (no DOM): a SOURCE guard that lane order is never written
// back to the schema for a RELATION field (TASK-2998).
//
// `handleGroupReorder` persists the board's lane order as the group field's
// `options`. For a relation the lanes are ITEM IDS, so that write would put
// uuids into the schema as select options — meaningless, and hard to undo by
// hand.
//
// BoardView already withholds column dragging on a relation board, so nothing
// should reach this. The guard is here anyway because this is the DESTRUCTIVE
// end: a guard at the affordance protects only the affordances somebody
// remembered, and this file is ~3,600 lines with several of them.
//
// WHY A SOURCE GUARD: the collection page cannot be rendered in a unit test
// without standing up the whole workspace shell. WHAT IT CANNOT DO: it checks
// spellings, not behaviour — a guard comparing the wrong field, or one made
// unreachable by an earlier return, would still pass.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const SRC = readFileSync(resolve(__dirname, './+page.svelte'), 'utf8').replace(
	/^[ \t]*\/\/.*$/gm,
	'',
);

describe('handleGroupReorder', () => {
	it('refuses to persist lane order for a relation field', () => {
		const start = SRC.indexOf('async function handleGroupReorder(');
		expect(start, 'handleGroupReorder was renamed or removed').toBeGreaterThan(-1);
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));

		const guard = body.indexOf("type === 'relation'");
		const write = body.indexOf('.options = newOrder');
		expect(guard, 'the relation guard is gone').toBeGreaterThan(-1);
		expect(write, 're-point this guard: the schema write moved').toBeGreaterThan(-1);
		expect(guard, 'the write happens before the guard').toBeLessThan(write);
	});

	it('still writes lane order for an ordinary field', () => {
		// The counterfactual: a guard that refused everything would silently
		// stop persisting column order on every board on the instance, which is
		// a worse regression than the one it prevents.
		const start = SRC.indexOf('async function handleGroupReorder(');
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));
		expect(body).toContain('.options = newOrder');
		// and the refusal is conditional rather than unconditional
		expect(body).not.toMatch(/\n\t\treturn;\s*\n\t\ts\.fields\[idx\]\.options/);
	});
});
