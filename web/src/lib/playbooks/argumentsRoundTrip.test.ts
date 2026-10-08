import { describe, expect, it } from 'vitest';
import { parseArgumentsSection, updateArgumentsInBody, type PlaybookArgument } from './arguments';

/**
 * TASK-2191: the arguments form used to replace the whole `## Arguments`
 * section with canonical bullets on every keystroke, deleting prose an
 * author wrote there and rewriting tolerated bullet forms. It now edits the
 * section in place.
 */

const BODY = `# Ship

Intro text.

## Arguments

Pass the target first; everything else is optional.

- \`target\` (ref, required) — what to ship
-   \`dry-run\`   (flag) - just print the plan

Anything after the bullets stays too.

## Steps

1. Do it.
`;

const args = (body: string) => parseArgumentsSection(body);

describe('updateArgumentsInBody keeps what it did not change', () => {
	it('the same arguments leave the body byte-identical', () => {
		expect(updateArgumentsInBody(BODY, args(BODY))).toBe(BODY);
	});

	it('editing one argument re-renders only its own bullet', () => {
		const next = args(BODY).map((a) => (a.name === 'target' ? { ...a, description: 'the plan or tasks' } : a));
		const out = updateArgumentsInBody(BODY, next);
		expect(out).toContain('- `target` (ref, required) — the plan or tasks');
		// The tolerated, non-canonical bullet survives verbatim.
		expect(out).toContain('-   `dry-run`   (flag) - just print the plan');
		expect(out).toContain('Pass the target first; everything else is optional.');
		expect(out).toContain('Anything after the bullets stays too.');
		expect(out).toContain('## Steps\n\n1. Do it.');
	});

	it('a new argument goes after the last bullet, before the trailing prose', () => {
		const added: PlaybookArgument = { name: 'limit', type: 'number' };
		const out = updateArgumentsInBody(BODY, [...args(BODY), added]);
		const lines = out.split('\n');
		const dry = lines.findIndex((l) => l.includes('`dry-run`'));
		expect(lines[dry + 1]).toBe('- `limit` (number)');
		expect(out).toContain('Anything after the bullets stays too.');
	});

	it('removing an argument removes its bullet and keeps the prose', () => {
		const out = updateArgumentsInBody(BODY, args(BODY).filter((a) => a.name !== 'dry-run'));
		expect(out).not.toContain('dry-run');
		expect(out).toContain('Pass the target first');
		expect(out).toContain('Anything after the bullets stays too.');
		expect(out).not.toContain('No arguments');
	});

	it('a rename re-renders its own bullet in place', () => {
		const next = args(BODY).map((a) => (a.name === 'target' ? { ...a, name: 'ref' } : a));
		const out = updateArgumentsInBody(BODY, next);
		const lines = out.split('\n');
		expect(lines.findIndex((l) => l.includes('`ref`'))).toBeLessThan(lines.findIndex((l) => l.includes('`dry-run`')));
		expect(out).not.toContain('`target`');
	});

	it('reordering moves the bullets, not the prose', () => {
		const out = updateArgumentsInBody(BODY, [...args(BODY)].reverse());
		const lines = out.split('\n');
		expect(lines.findIndex((l) => l.includes('`dry-run`'))).toBeLessThan(lines.findIndex((l) => l.includes('`target`')));
		expect(lines.findIndex((l) => l.includes('Pass the target'))).toBeLessThan(lines.findIndex((l) => l.includes('`dry-run`')));
	});

	it('the placeholder appears only when the section has nothing else, and goes when an argument arrives', () => {
		const bare = '## Arguments\n\n- `a` (string)\n';
		const emptied = updateArgumentsInBody(bare, []);
		expect(emptied).toContain('(No arguments — this playbook takes no inputs.)');
		const refilled = updateArgumentsInBody(emptied, [{ name: 'b', type: 'string' }]);
		expect(refilled).not.toContain('No arguments');
		expect(refilled).toContain('- `b` (string)');
		// With prose present, removing every argument adds no placeholder.
		expect(updateArgumentsInBody(BODY, [])).not.toContain('No arguments');
	});

	it('a body with no section gets one appended, as before', () => {
		expect(updateArgumentsInBody('# T\n\nBody.\n', [{ name: 'x', type: 'string' }])).toBe(
			'# T\n\nBody.\n\n## Arguments\n\n- `x` (string)\n'
		);
	});
});
