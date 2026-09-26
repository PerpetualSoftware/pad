/**
 * The acceptance suite every route surface's identity gate runs (TASK-3097).
 * The route guards used to be regex scanners (`identityFenceSource.ts`), and
 * BUG-3084's round 4 named five classes of edit a scanner cannot see. The
 * gate (`identityFenceGate.ts`) is a change detector, so it refuses those
 * edits by construction. This suite is what shows it, per surface, on that
 * surface's own code:
 *
 *   1. an unlisted committer — a write or call slipped in after an await;
 *   2. an unlisted async shape — a new async function the table has no row
 *      for (an object method, a nested arrow);
 *   3. a fence borrowed from a neighbour — a unit's own check removed while
 *      the checks around it stay;
 *   4. a fence helper trusted by name — the helper's body changed and its
 *      name kept;
 *   5. conditional-return masking — a fence's return made conditional on
 *      something that never holds.
 *
 * Each mutant must be refused, and the refusal must NAME the unit or helper
 * it edited, so a mutant refused for an unrelated reason does not count. The
 * CONTROL is the other half: an edit outside every hashed function is
 * accepted, which is what makes the gate a detector of THESE edits rather
 * than of any edit.
 *
 * The aid (`analysisReport`) is not required quiet on a route surface (lead
 * ruling (a) on TASK-3097): its trusted vocabulary names ItemDetail's fences.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { refusals, type GateTable } from './identityFenceGate';

export type RoundFourClass = 1 | 2 | 3 | 4 | 5;

export interface GateMutant {
	/** Which round-4 class this edit is a member of. */
	cls: RoundFourClass;
	what: string;
	/** Exact source text to replace. It must occur exactly once. */
	old: string;
	new: string;
	/** Text a refusal must contain, usually the unit's label (`load()`) or `helper x()`. */
	names: string;
}

export interface SurfaceGate {
	surface: string;
	source: URL;
	table: GateTable;
	mutants: GateMutant[];
	/** An edit outside every hashed function: the gate must accept it. */
	control: { old: string; new: string };
}

const REVIEWED = /^[0-9a-f]{12}$/;

function occurrences(hay: string, needle: string): number {
	return hay.split(needle).length - 1;
}

export function identityGateSuite(g: SurfaceGate): void {
	const code = readFileSync(g.source, 'utf8');
	const gate = (c: string) => refusals(g.table, c);

	describe(`${g.surface}: the identity gate (TASK-3097)`, () => {
		it('the component as written passes the gate', () => {
			expect(gate(code)).toEqual([]);
		});

		it('every row and every helper carries the hash it was reviewed on', () => {
			const rows = [...Object.values(g.table.asyncFunctions), ...g.table.nested, ...g.table.markup, ...g.table.continuations];
			expect(rows.length, 'the table is empty').toBeGreaterThan(0);
			for (const r of rows) expect(r.reviewed, `row (${r.why}) has no reviewed hash`).toMatch(REVIEWED);
			for (const [h, v] of Object.entries(g.table.helpers)) expect(v, `helper ${h}`).toMatch(REVIEWED);
			for (const r of g.table.identifierCallbacks) expect(r.reviewed, r.text).toMatch(REVIEWED);
		});

		it('covers every round-4 class with at least one mutant', () => {
			const classes = new Set(g.mutants.map((m) => m.cls));
			for (const c of [1, 2, 3, 4, 5] as const) expect(classes.has(c), `no class-${c} mutant`).toBe(true);
		});

		for (const m of g.mutants) {
			it(`refuses a class-${m.cls} edit: ${m.what}`, () => {
				expect(occurrences(code, m.old), `the mutant's anchor must occur exactly once: ${m.old}`).toBe(1);
				const mutated = code.replace(m.old, m.new);
				expect(mutated).not.toBe(code);
				const out = gate(mutated);
				expect(out.length, 'the gate accepted it').toBeGreaterThan(0);
				expect(
					out.some((line) => line.includes(m.names)),
					`no refusal names ${m.names}:\n${out.join('\n')}`
				).toBe(true);
			});
		}

		it('CONTROL: an edit outside every hashed function is accepted', () => {
			expect(occurrences(code, g.control.old), 'the control anchor must occur exactly once').toBe(1);
			const edited = code.replace(g.control.old, g.control.new);
			expect(edited).not.toBe(code);
			expect(gate(edited)).toEqual([]);
		});
	});
}
