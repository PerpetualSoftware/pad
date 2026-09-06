// Node-project test (no DOM): a SOURCE-level guard on the blocked-field notice
// in `CopyItemDialog.svelte` (IDEA-2899), the same shape as
// `fieldEditorRelationCallers.test.ts` and for the same reason — the dialog is
// a large component with no harness, and these are facts about what it RENDERS
// that no unit test of `copyNeedsValue` can see.
//
// WHAT THIS PROVES: that the CLI command is gated on a field the CLI can
// actually fill, and that the unavailable-target branch exists.
//
// WHAT IT DOES NOT PROVE: that either branch is reached, or that the rendered
// text is right. It is a tripwire, not a test of behaviour. Its measured limit
// is the same one every source pin has: a mutation that leaves the matched TEXT
// in place while making it unreachable — wrapping the block in `{#if false}`,
// say — survives it. Deleting the gate does not. Delete this file the day the
// dialog grows a component harness.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const source = readFileSync(
	fileURLToPath(new URL('./CopyItemDialog.svelte', import.meta.url)),
	'utf8'
	// Comments stripped, so prose about the rule can never satisfy a pin ON the
	// rule — the mistake that would make this file self-congratulatory.
).replace(/<!--[\s\S]*?-->/g, '');

describe('IDEA-2899 — the blocked-field notice advises only where advice works', () => {
	it('offers the CLI only for a field the CLI can fill', () => {
		// `blockedFields[0]` was correct while every blocked row was
		// type-shaped. With an unavailable relation target sorted first it names
		// the ONE field `--field` cannot set either, sending the user to run a
		// command that is refused for exactly the reason they are stuck.
		expect(source).toMatch(/\{#if cliFillableField\}/);
		expect(source).toMatch(/--field \{cliFillableField\.key\}=value/);
		expect(source).not.toMatch(/--field \{blockedFields\[0\]\.key\}=value/);
	});

	it('branches the message on why the row is uncollectable', () => {
		expect(source).toMatch(/uncollectableReason\(f\) === 'unavailable_target'/);
	});

	it('names the target collection in the unavailable branch', () => {
		// Without the slug the message says a field is unusable and not which
		// collection is missing — which is the same "told there is a problem,
		// not told what" failure this change exists to fix, one level up.
		expect(source).toMatch(/<code>\{f\.collection\}<\/code>/);
	});
});
