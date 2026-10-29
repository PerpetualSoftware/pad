import { describe, it, expect } from 'vitest';
import { canonicalizeRef, extractRefs, tokenizeBody } from './refs';
import type { ChatRef } from '$lib/types';

const found = (c: string) => extractRefs(c).map((r) => (r.workspace ? `${r.workspace}::${r.ref}` : r.ref));

// Cases below are copied from internal/links/extract_test.go
// (TestExtractWikiLinks_CodeBlocksExcluded, _WorkspaceRefForms).
describe('extractRefs matches the Go extractor', () => {
	const cases: Array<[string, string, string[]]> = [
		['fenced block', 'Real: [[OUTSIDE-1]]\n```\nExample: [[INSIDE-1]] and [[INSIDE-2]]\n```\nAfter: [[OUTSIDE-2]]', ['OUTSIDE-1', 'OUTSIDE-2']],
		['fence with language tag', 'Before: [[A-1]]\n```bash\necho [[B-1]]\n```\nAfter: [[C-1]]', ['A-1', 'C-1']],
		['inline code', 'The `[[FAKE-1]]` syntax has shipped; see [[REAL-2]] for examples.', ['REAL-2']],
		['unclosed fence runs to EOF', 'Before: [[A-1]]\n```\nAfter: [[B-1]] dangling', ['A-1']],
		['inline code spans a single newline', 'Outside [[A-1]]\nstart `pre\n[[INSIDE-1]]\npost` end\nOutside [[B-1]]', ['A-1', 'B-1']],
		['inline code breaks at a blank line', 'Outside [[A-1]]\nopen `unclosed-span\n\nnew paragraph [[REAL-1]] more text\nOutside [[B-1]]', ['A-1', 'REAL-1', 'B-1']],
		['inline code breaks at a whitespace-only line', 'open `unclosed-span\n  \t  \nafter-blank [[REAL-1]]', ['REAL-1']],
		['closer matches opener length', 'Before [[A-1]] ``code with ` inside and [[INSIDE-1]]`` after [[B-1]]', ['A-1', 'B-1']],
		['single-backtick span next to a double run', 'Try `code [[INSIDE-1]] ``not-closer` then [[REAL-1]]', ['REAL-1']],
		['multi-backtick span', '``see [[INSIDE-1]]`` and [[REAL-1]]', ['REAL-1']],
		['tilde fence', 'Real: [[OUTSIDE-1]]\n~~~\nExample: [[INSIDE-1]]\n~~~\nAfter: [[OUTSIDE-2]]', ['OUTSIDE-1', 'OUTSIDE-2']],
		['mixed fence types do not pair', 'Before [[A-1]]\n```\n~~~ not a closer\n[[INSIDE-1]]\n```\nAfter [[B-1]]', ['A-1', 'B-1']],
		['fence closer needs only the fence', 'Real: [[OUTSIDE-1]]\n```\n```not-closed\n[[INSIDE-1]] still inside\n```\nAfter: [[OUTSIDE-2]]', ['OUTSIDE-1', 'OUTSIDE-2']],
		['closer trailing spaces ok', 'Before [[A-1]]\n```\n[[INSIDE-1]]\n```   \nAfter [[B-1]]', ['A-1', 'B-1']],
		['indented fence (0-3 spaces)', 'Before [[A-1]]\n   ```\n   echo [[INSIDE-1]]\n   ```\nAfter [[B-1]]', ['A-1', 'B-1']],
		['four-space indent is not a fence', 'Before\n    ```\n[[REAL-1]]\n', ['REAL-1']],
		['fence at start of content', '```\n[[INSIDE-1]]\n```\n[[OUTSIDE-1]]', ['OUTSIDE-1']],
		['ref after an inline span', '[[A-1]] `x` `[[G-1]]` [[H-1]]', ['A-1', 'H-1']],
		['workspace ref', 'Cross [[other-ws::TASK-9]] over.', ['other-ws::TASK-9']],
		['workspace ref with display', 'Cross [[other-ws::TASK-9|see this]] too.', ['other-ws::TASK-9']],
		['mixed-case bare ref is canonicalised', '[[task-5]]', ['TASK-5']],
		['invalid workspace slug is a title, not a ref', '[[INVALID::TASK-1]]', []],
		['title links are not refs', '[[Some Title]] and [[docs/Other]]', []]
	];
	for (const [name, content, want] of cases) {
		it(name, () => expect(found(content)).toEqual(want));
	}

	// TASK-4044 3c D4: the trim is Go's strings.TrimSpace (presence.ts::trimSpace), not
	// String.trim(). Go keeps U+FEFF and strips U+0085, in either engine.
	it('a BOM before the ref is not a ref (Go keeps U+FEFF)', () => {
		expect(found('[[\uFEFFTASK-1]]')).toEqual([]);
		expect(found('[[\uFEFFpad::TASK-1]]')).toEqual([]);
		expect(found('[[pad::\uFEFFTASK-1]]')).toEqual([]);
	});

	it('a NEL around the ref is trimmed (Go strips U+0085)', () => {
		expect(found('[[\u0085TASK-1]]')).toEqual(['TASK-1']);
		expect(found('[[\u0085pad::\u0085TASK-1\u0085]]')).toEqual(['pad::TASK-1']);
	});

	it('keeps the display override and raw text', () => {
		const [r] = extractRefs('x [[other-ws::TASK-9|see this]] y');
		expect(r.display).toBe('see this');
		expect(r.raw).toBe('[[other-ws::TASK-9|see this]]');
		expect(r.start).toBe(2);
	});

	it('canonicalizeRef upper-cases the prefix only', () => {
		expect(canonicalizeRef('task-5')).toBe('TASK-5');
		expect(canonicalizeRef('Ab-12')).toBe('AB-12');
	});
});

describe('tokenizeBody', () => {
	const refs: ChatRef[] = [
		{ workspace: 'pad', ref: 'TASK-1', state: 'visible', title: 'Do it', collection_slug: 'tasks' },
		{ workspace: 'other', ref: 'BUG-2', state: 'restricted' },
		{ workspace: 'pad', ref: 'TASK-3', state: 'unresolved' }
	];
	const ctx = { slug: 'pad', name: 'Pad' };
	const tok = (body: string, context_workspace?: { slug: string; name: string }) =>
		tokenizeBody({ body, context_workspace, deleted: false }, refs);

	it('links a visible bare ref through the context workspace', () => {
		expect(tok('see [[task-1]] now', ctx)).toEqual([
			{ type: 'text', text: 'see ' },
			{ type: 'link', text: 'TASK-1', href: '/-/r/pad/TASK-1', title: 'Do it', workspace: 'pad', ref: 'TASK-1' },
			{ type: 'text', text: ' now' }
		]);
	});

	it('uses the display override as the link label', () => {
		const segs = tok('[[pad::TASK-1|the task]]');
		expect(segs).toEqual([
			{ type: 'link', text: 'the task', href: '/-/r/pad/TASK-1', title: 'Do it', workspace: 'pad', ref: 'TASK-1' }
		]);
	});

	it('renders a restricted ref as a title-less placeholder', () => {
		expect(tok('[[other::BUG-2]]')).toEqual([
			{ type: 'restricted', text: 'restricted item', workspace: 'other', ref: 'BUG-2' }
		]);
	});

	it('leaves an unresolved ref as plain text', () => {
		expect(tok('x [[TASK-3]] y', ctx)).toEqual([{ type: 'text', text: 'x [[TASK-3]] y' }]);
	});

	it('leaves a ref with no entry (dropped by the server) as plain text', () => {
		expect(tok('[[pad::TASK-99]]')).toEqual([{ type: 'text', text: '[[pad::TASK-99]]' }]);
	});

	it('leaves a bare ref with no context workspace as plain text', () => {
		expect(tok('[[TASK-1]]')).toEqual([{ type: 'text', text: '[[TASK-1]]' }]);
	});

	it('an explicit workspace overrides the context workspace', () => {
		const segs = tok('[[other::BUG-2]]', ctx);
		expect(segs[0].type).toBe('restricted');
	});

	it('does not link inside code', () => {
		expect(tok('`[[pad::TASK-1]]`')).toEqual([{ type: 'text', text: '`[[pad::TASK-1]]`' }]);
	});

	it('returns nothing for a tombstone', () => {
		expect(tokenizeBody({ body: '', context_workspace: undefined, deleted: true }, refs)).toEqual([]);
	});
});
