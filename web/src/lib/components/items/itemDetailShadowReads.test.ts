// Node-project test (no DOM): the editor-markdown shadow is read only through
// its draining accessor (TASK-2232, lead ruling).
//
// The Editor delivers markdown COALESCED, so `lastEditorMarkdown` can be up to
// one coalesce window behind the document. `currentEditorMarkdown()` drains
// the pending delivery first, so a reader of it cannot see text older than
// the editor holds. A reader added later that reads the field directly could,
// and that is a lost keystroke on whatever path it feeds; this refuses one.
//
// AST, not text: an assignment, the declaration and reads inside the accessor
// are the only allowed occurrences, which a regex cannot tell from a read
// (`lastEditorMarkdown = x` vs `x = lastEditorMarkdown`, a read inside an
// arrow, a read in the markup). Comments are not nodes, so the field named in
// prose is not counted. "Inside the accessor" includes anything nested in it,
// which is why the count of accessor reads is pinned at exactly one: a second
// read added there (say, in a nested arrow) fails that count.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { parseComponent, walk } from '../../../test/identityFenceAst';

const SOURCE = readFileSync(fileURLToPath(new URL('./ItemDetail.svelte', import.meta.url)), 'utf8');
const FIELD = 'lastEditorMarkdown';
const ACCESSOR = 'currentEditorMarkdown';

type Node = { type: string; start: number; end: number; [k: string]: unknown };

/** Every occurrence of the field that is a READ outside the accessor, by line. */
function refusals(code: string): { refused: string[]; accessorReads: number; writes: number } {
	const src = parseComponent(code);
	const refused: string[] = [];
	let accessorReads = 0;
	let writes = 0;
	const visit = (n: Node, ancestors: Node[]) => {
		if (n.type !== 'Identifier' || n.name !== FIELD) return;
		const parent = ancestors[ancestors.length - 1];
		// `obj.lastEditorMarkdown` names a property, not this variable.
		if (parent?.type === 'MemberExpression' && parent.property === n && !parent.computed) return;
		if (parent?.type === 'VariableDeclarator' && parent.id === n) return;
		if (parent?.type === 'AssignmentExpression' && parent.left === n && parent.operator === '=') {
			writes++;
			return;
		}
		const inAccessor = ancestors.some((a) => a.type === 'FunctionDeclaration' && (a.id as Node & { name?: string })?.name === ACCESSOR);
		if (inAccessor) {
			accessorReads++;
			return;
		}
		refused.push(`line ${src.line(n.start)}: ${code.slice(n.start - 30, n.end + 10).replace(/\s+/g, ' ')}`);
	};
	walk(src.script as never, visit as never);
	walk(src.fragment as never, visit as never);
	return { refused, accessorReads, writes };
}

describe('ItemDetail reads the editor-markdown shadow only through its draining accessor (TASK-2232)', () => {
	it('the component as written: no other read', () => {
		const r = refusals(SOURCE);
		expect(r.refused).toEqual([]);
		// PRECONDITIONS, so the empty list above means something: the accessor
		// reads the field, and the field is written (else the walk saw nothing).
		expect(r.accessorReads).toBe(1);
		expect(r.writes).toBeGreaterThanOrEqual(2);
	});

	it('the accessor drains before it reads', () => {
		const at = SOURCE.indexOf(`function ${ACCESSOR}(`);
		expect(at).toBeGreaterThan(-1);
		const body = SOURCE.slice(at, SOURCE.indexOf('\n\t}\n', at));
		expect(body.indexOf('drainEditorUpdate?.()')).toBeGreaterThan(-1);
		expect(body.indexOf('drainEditorUpdate?.()')).toBeLessThan(body.indexOf(`return ${FIELD}`));
	});

	describe('refuses the reads a later edit would add', () => {
		const mutants: Array<[string, string, string]> = [
			['a fallback that reads the field directly', 'if (md == null) md = currentEditorMarkdown();', `if (md == null) md = ${FIELD};`],
			['a comparison that reads the field directly', `if (currentEditorMarkdown() !== markdown) return null;`, `if (${FIELD} !== markdown) return null;`],
			['a read in a new arrow', `function ${ACCESSOR}(): string | null {`, `const peek = () => ${FIELD};\n\tfunction ${ACCESSOR}(): string | null {`],
			['a read in the markup', '<ContentSkeleton variant="page" />', `<ContentSkeleton variant="page" /><span hidden>{${FIELD}}</span>`],
		];
		for (const [what, from, to] of mutants) {
			it(what, () => {
				expect(SOURCE.includes(from), `mutant anchor missing: ${from}`).toBe(true);
				expect(refusals(SOURCE.replace(from, to)).refused.length).toBeGreaterThan(0);
			});
		}
	});
});
