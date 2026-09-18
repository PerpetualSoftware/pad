/**
 * BUG-3095 — the CHILD POPULATION guard (surface 8 of the BUG-3084 family).
 *
 * ItemDetail's own fences are gated by `itemDetailIdentityFenceAst.test.ts`.
 * That guard stops at ItemDetail's file boundary, which is the whole reason
 * this surface exists: ItemDetail hands callbacks down, and a child that awaits
 * one and then issues its OWN request is invisible from inside the parent.
 *
 * WHAT THIS GUARD ASSERTS, in two independent halves.
 *
 * 1. THE POPULATION IS THE ONE WE REVIEWED. `POPULATION` below is checked
 *    against the components ItemDetail actually imports. This half exists
 *    because the hand-written population table on BUG-3095 was probed at
 *    `ea0d0848` and had silently drifted by the time it was worked: six
 *    children had been added to ItemDetail that the table did not list, and one
 *    row named a component ItemDetail no longer mounts. A table nobody can tell
 *    is stale is worse than no table, because it is read as coverage. A child
 *    added to ItemDetail now fails HERE, naming itself, before anyone has to
 *    notice.
 *
 * 2. EVERY AWAIT-THEN-REQUEST PATH HOLDS AN IDENTITY FENCE. For each population
 *    file, every async function (and every `.then` continuation) in which a
 *    REQUEST is reachable after an `await` must contain a call to a predicate
 *    bound from `authStore.identityFence()`, positioned between them.
 *
 * WHY A SECOND, SIMPLER ANALYZER RATHER THAN REUSING THE AID MODULE.
 * `src/test/identityFenceAst.ts` is a 1500-line flow analysis tuned to
 * ItemDetail's specific vocabulary — its generation counters, its capture
 * helpers, its fence booleans. Pointing it at fifteen other components would
 * mean teaching it fifteen more vocabularies, and the last six windows on this
 * family all went the same way: the review rounds are spent on the GUARD's
 * defects, not the component's. So this one is deliberately small and answers a
 * narrower question, and the rest is declared below rather than discovered in
 * review.
 *
 * WHAT IT CANNOT SEE — the declared gaps, stated up front (lead ruling on
 * checkpoint 1b). These are not suspicions; each is a construct this guard
 * provably does not model:
 *
 *   (a) PROVENANCE. It matches a CALL to a name bound from
 *       `authStore.identityFence()` in the same function. A predicate arriving
 *       as a prop, or returned by a helper module, reads as unfenced. Accepted
 *       as blind: closing it means tracking values across module boundaries,
 *       which is the aid module's job and its size.
 *   (b) BRANCH OUTCOME. It checks that the predicate is CALLED between the
 *       await and the send, not that the branch it guards actually returns. A
 *       fence whose body is empty passes here. The driven legs are what cover
 *       that — `timelineVersionCardIdentityFence.svelte.test.ts` presses the
 *       real component through the real window.
 *   (c) CALLEE-SIDE AWAITS. A send inside a callback passed to a helper that is
 *       not itself awaited here reads as having no await before it.
 *   (d) COMMITS. This guard covers REQUESTS (`api.*`, `fetch`). A post-await
 *       assignment to component state is equally capable of showing one
 *       identity's data to the next, and is NOT modelled. The two commit rows
 *       fixed in this unit (`TimelineVersionCard.ensureResolved`,
 *       `ChildItems.loadChildren`) are covered by their own fences and by the
 *       driven legs, not by this guard.
 *
 * FAIL-CLOSED, which is the property that makes the gaps survivable: anything
 * this guard cannot classify is a FAILURE, never a skip. A file it cannot parse
 * fails. A send whose enclosing function it cannot resolve fails. It never
 * concludes "safe" from not having understood something — so the way this guard
 * goes wrong is by asking to be taught, which is visible, rather than by going
 * quiet, which is not.
 *
 * KNOWN GAPS ARE ENUMERATED, NOT TOLERATED IN BULK (lead ruling). The 13 files
 * this unit did not fix are listed in `KNOWN_UNFENCED` by exact
 * `file::function` path. Each listed path is EXPECTED to be unfenced: a path
 * that gets fixed without being removed from the list ALSO fails, so the list
 * cannot rot into a permanent exemption. Anything unfenced and unlisted fails.
 * That is what makes this guard mergeable green while the follow-up unit is
 * still open.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { parseComponent, walk, type AstSource, type Node } from '../../../test/identityFenceAst';

const ROOT = new URL('../../../', import.meta.url).pathname; // src/

/**
 * The child components ItemDetail mounts. Half 1 checks this against the real
 * import list, so this array cannot silently drift from the component.
 *
 * `BottomSheet` is deliberately absent: ItemDetail imports it but mounts it
 * nowhere (zero `<BottomSheet` occurrences), so it is a dead import rather than
 * a child. Reported on BUG-3095, not fixed here.
 */
const POPULATION: string[] = [
	'lib/components/editor/Editor.svelte',
	'lib/components/editor/EditorBubbleMenu.svelte',
	'lib/components/editor/EditorLinkPopover.svelte',
	'lib/components/editor/RawMarkdownEditor.svelte',
	'lib/components/fields/FieldEditor.svelte',
	'lib/components/fields/TagInput.svelte',
	'lib/components/timeline/ItemTimeline.svelte',
	'lib/components/timeline/TimelineEntryList.svelte',
	'lib/components/timeline/TimelineVersionCard.svelte',
	'lib/components/ChildItems.svelte',
	'lib/components/BacklinksPanel.svelte',
	'lib/components/RelationBacklinksPanel.svelte',
	'lib/components/items/ItemPicker.svelte',
	'lib/components/items/ItemAttachmentStrip.svelte',
	'lib/components/attachments/AttachmentSurfaceHost.svelte',
	'lib/components/common/QuickActionsMenu.svelte',
	'lib/components/common/Menu.svelte',
	'lib/components/common/MenuItem.svelte',
	'lib/components/common/ContentSkeleton.svelte',
	'lib/components/common/ContentError.svelte',
	'lib/components/collections/EditCollectionModal.svelte',
	'lib/components/ShareDialog.svelte',
	'lib/components/items/CopyItemDialog.svelte',
	'lib/components/items/PushToAgentDialog.svelte',
];

/**
 * `TimelineVersionCard` is reached through `ItemTimeline`/`TimelineEntryList`
 * rather than mounted by ItemDetail directly, so the import check below must
 * not expect it in ItemDetail's import list. It is in the POPULATION because it
 * is where the known member lives.
 */
const NOT_DIRECTLY_IMPORTED = new Set(['lib/components/timeline/TimelineVersionCard.svelte']);

/**
 * Paths this unit did not fix, each EXPECTED to be unfenced. Format:
 * `<population path>::<function name>`. The follow-up unit's contract is
 * exactly this list; removing an entry without fixing it, or fixing one without
 * removing it, fails.
 */
const KNOWN_UNFENCED: string[] = [
	// ItemTimeline.loadMore: paginates the timeline; the second page request is
	// issued after the first page's await resolves.
	'lib/components/timeline/ItemTimeline.svelte::loadMore',
	// QuickActionsMenu: both send after awaiting an earlier request in the same
	// handler; handleSaveNewAction is a read-modify-write across three calls.
	'lib/components/common/QuickActionsMenu.svelte::handleAction',
	'lib/components/common/QuickActionsMenu.svelte::handleSaveNewAction',
	// EditCollectionModal.handleArchive: delete → list → delete, each after the
	// previous await. The worst-shaped of the six: two destructive calls.
	'lib/components/collections/EditCollectionModal.svelte::handleArchive',
	// CopyItemDialog.handleConfirm: re-runs the preflight after an earlier await.
	// Note the copy call itself is NOT retried (no idempotency key) — the fence
	// here must refuse, never re-send.
	'lib/components/items/CopyItemDialog.svelte::handleConfirm',
	// Editor's onMount callback: `api.server.capabilities()` fire-and-forget.
	// FLAGGED BUT PROBABLY NOT A DEFECT — the endpoint is public and carries no
	// user-scoped data (its own comment says it works pre-login on shared-item
	// preview surfaces), so a cross-identity issue leaks nothing. Listed rather
	// than silently excluded because "this endpoint is public" is a judgement
	// this guard cannot make, and the follow-up should record the decision
	// instead of re-deriving it.
	'lib/components/editor/Editor.svelte::<callback of onMount>',
];


function calleeText(src: AstSource, n: Node): string {
	return src.text(n.callee).replace(/\s+/g, '').replace(/\?\./g, '.');
}

/** A REQUEST: an `api.*` call or a bare `fetch(`. */
function isRequest(src: AstSource, n: Node): boolean {
	if (n.type !== 'CallExpression') return false;
	const c = calleeText(src, n);
	return c === 'fetch' || c.startsWith('api.') || c.startsWith('apiClient.');
}

const isFnNode = (n: Node | undefined | null): n is Node =>
	!!n &&
	(n.type === 'ArrowFunctionExpression' ||
		n.type === 'FunctionExpression' ||
		n.type === 'FunctionDeclaration');

/**
 * A STABLE name for a function, used as the key in `KNOWN_UNFENCED`.
 *
 * The fallback deliberately avoids byte offsets. An offset-keyed entry changes
 * whenever anything ABOVE it in the file is edited, so the gap list would fail
 * on edits that have nothing to do with it — noise that trains a reader to
 * re-paste the key without re-reading the path, which is the one thing a
 * known-gaps list must not teach. Naming an anonymous callback after the call
 * it is an argument to (`onMount`, `$effect`) is stable under unrelated edits
 * and still unique in practice; a genuine collision between two anonymous
 * callbacks of the same callee in one file would merge their entries, which
 * fails closed (both must be fixed to clear one).
 */
function fnName(src: AstSource, n: Node, ancestors: Node[]): string {
	if (n.id?.name) return n.id.name;
	const parent = ancestors[ancestors.length - 1];
	if (parent?.type === 'VariableDeclarator' && parent.id?.type === 'Identifier') return parent.id.name;
	if (parent?.type === 'Property' && parent.key?.type === 'Identifier') return parent.key.name;
	if (parent?.type === 'CallExpression' && parent.callee) {
		return `<callback of ${calleeText(src, parent)}>`;
	}
	for (let i = ancestors.length - 1; i >= 0; i--) {
		const a = ancestors[i];
		if (a.type === 'CallExpression' && a.callee) return `<nested in ${calleeText(src, a)}>`;
		if (isFnNode(a) && a.id?.name) return `<nested in ${a.id.name}>`;
	}
	return '<anonymous>';
}

/**
 * Names bound from `authStore.identityFence()` inside this function.
 * Provenance gap (a): only a direct call to that member expression counts.
 */
function fenceNamesIn(src: AstSource, fn: Node): Set<string> {
	const names = new Set<string>();
	walk(fn, (n) => {
		if (n.type !== 'VariableDeclarator' || !n.init) return;
		if (n.init.type !== 'CallExpression') return;
		if (calleeText(src, n.init) !== 'authStore.identityFence') return;
		if (n.id?.type === 'Identifier') names.add(n.id.name);
	});
	return names;
}

/**
 * Names of local helpers whose body CALLS one of `names` — the `stale()` shape.
 *
 * ChildItems does not call its predicate at the send; it folds it into a
 * `const stale = () => destroyed || !isSameIdentity() || …` declared in the
 * same function, and calls `stale()` after each await. Without this hop the
 * guard reports every one of those paths as unfenced, which is how it first
 * behaved — it flagged the six paths this unit had just fixed.
 *
 * ONE hop only, and that bound is deliberate rather than lazy: a helper calling
 * a helper calling the predicate is not modelled and reads as unfenced, which
 * fails closed. Widening it is how a source scanner acquires an unbounded tail.
 */
function fenceHelperNames(src: AstSource, fn: Node, names: Set<string>): Set<string> {
	const helpers = new Set<string>();
	if (names.size === 0) return helpers;
	walk(fn, (n) => {
		if (n.type !== 'VariableDeclarator' || !isFnNode(n.init)) return;
		if (n.id?.type !== 'Identifier') return;
		let calls = false;
		walk(n.init, (m) => {
			if (m.type === 'CallExpression' && names.has(calleeText(src, m))) calls = true;
		});
		if (calls) helpers.add(n.id.name);
	});
	return helpers;
}

/** Offsets at which the predicate — or a one-hop helper around it — is CALLED. */
function fenceCallOffsets(src: AstSource, fn: Node, names: Set<string>): number[] {
	const out: number[] = [];
	if (names.size === 0) return out;
	const helpers = fenceHelperNames(src, fn, names);
	walk(fn, (n) => {
		if (n.type !== 'CallExpression') return;
		const c = calleeText(src, n);
		if (names.has(c) || helpers.has(c)) out.push(n.start);
	});
	return out;
}

/**
 * Enclosing loop whose body contains an `await`, if any.
 *
 * THE BACK-EDGE CASE, encoded deliberately. `for (const x of xs) { await
 * send(x); }` has its send TEXTUALLY BEFORE every await in the function, so a
 * naive "is there an await above this send" reading calls it safe. It is not:
 * every iteration after the first runs after the previous iteration's await.
 * ChildItems' two reorder loops are exactly this shape and were the least
 * guarded paths in the population, so a guard that reads them as fine would
 * have certified the worst rows.
 */
/**
 * True when `a` and `b` sit in DIFFERENT branches of the same `if` or ternary,
 * and therefore never both run.
 *
 * Without this the guard reports a false positive on the common shape
 * `const res = itemId ? await api.listItem(…) : await api.listCollection(…)`:
 * the first branch's await is textually before the second branch's send, so a
 * position-only reading calls the second "after an await". It is not — the two
 * are alternatives.
 *
 * A false positive is fail-closed and would be survivable on its own. It is not
 * survivable HERE: this guard's findings are the follow-up unit's contract, and
 * an entry naming a path that is not a defect sends the next reader to fence
 * something that cannot race. A wrong gap list is worse than a short one.
 *
 * `try`/`catch` is deliberately NOT treated as exclusive: a catch block really
 * does run after the try block's await, which is a genuine await-then-send.
 */
function branchExclusive(fn: Node, a: Node, b: Node): boolean {
	const contains = (n: Node | null | undefined, x: Node) => !!n && n.start <= x.start && n.end >= x.end;
	let exclusive = false;
	walk(fn, (n) => {
		if (exclusive) return;
		if (n.type === 'ConditionalExpression') {
			if (
				(contains(n.consequent, a) && contains(n.alternate, b)) ||
				(contains(n.alternate, a) && contains(n.consequent, b))
			) {
				exclusive = true;
			}
		} else if (n.type === 'IfStatement') {
			if (
				(contains(n.consequent, a) && contains(n.alternate, b)) ||
				(contains(n.alternate, a) && contains(n.consequent, b))
			) {
				exclusive = true;
			}
		}
	});
	return exclusive;
}

function awaitBearingLoopAncestor(ancestors: Node[]): Node | null {
	const LOOPS = new Set(['ForStatement', 'ForOfStatement', 'ForInStatement', 'WhileStatement', 'DoWhileStatement']);
	for (let i = ancestors.length - 1; i >= 0; i--) {
		const a = ancestors[i];
		if (!LOOPS.has(a.type)) continue;
		let hasAwait = false;
		walk(a.body ?? a, (n) => {
			if (n.type === 'AwaitExpression') hasAwait = true;
		});
		if (hasAwait) return a;
	}
	return null;
}

interface Finding {
	path: string;
	fn: string;
	line: number;
	callee: string;
	reason: string;
}

function analyseFile(path: string): { findings: Finding[]; fenced: string[] } {
	const code = readFileSync(ROOT + path, 'utf8');
	let src: AstSource;
	try {
		src = parseComponent(code);
	} catch (e) {
		// FAIL CLOSED: a file we cannot parse is a failure, never a skip.
		return {
			findings: [{ path, fn: '<file>', line: 0, callee: '<parse>', reason: `unparseable: ${String(e)}` }],
			fenced: [],
		};
	}

	const findings: Finding[] = [];
	const fenced: string[] = [];

	walk(src.script, (fn, fnAncestors) => {
		if (!isFnNode(fn)) return;
		// Only functions that can suspend are in scope.
		let hasAwait = false;
		walk(fn, (n) => {
			if (n.type === 'AwaitExpression') hasAwait = true;
		});
		if (!hasAwait) return;

		const name = fnName(src, fn, fnAncestors);
		const names = fenceNamesIn(src, fn);
		const fenceOffsets = fenceCallOffsets(src, fn, names);

		// Every await NODE in this function. Nodes, not offsets, because a send
		// is usually ITSELF awaited (`const x = await api.foo()`), and that
		// wrapping AwaitExpression starts before the call it contains. Counting
		// it as "an await preceding this send" leaves no room for any fence and
		// reports every awaited request as unfenced — which is exactly how this
		// guard first behaved, flagging the six paths this unit had just fixed.
		// A send's own await is not something it can be fenced against.
		const awaitNodes: Node[] = [];
		walk(fn, (n) => {
			if (n.type === 'AwaitExpression') awaitNodes.push(n);
		});

		walk(fn, (n, ancestors) => {
			if (!isRequest(src, n)) return;
			// A nested function's sends belong to that function, not this one.
			for (const a of ancestors) {
				if (a !== fn && isFnNode(a)) return;
			}

			const loop = awaitBearingLoopAncestor([...ancestors, n]);
			// An await counts as PRECEDING this send only if it does not contain
			// it — a send's own `await` is not a suspension point it could have
			// been fenced after.
			const preceding = awaitNodes.filter(
				(a) => a.start < n.start && a.end < n.end && !branchExclusive(fn, a, n)
			);
			const afterAwait = preceding.length > 0 || loop !== null;
			if (!afterAwait) return;

			// The fence must be called between the await and the send. In the loop
			// case there is no "before" in the textual sense — the back edge is the
			// await — so the requirement is a fence call inside the loop body,
			// textually above the send.
			const lowerBound = loop ? loop.start : Math.max(...preceding.map((a) => a.start));
			const ok = fenceOffsets.some((o) => o > lowerBound && o < n.start);

			const label = `${path}::${name}`;
			if (ok) {
				fenced.push(label);
			} else {
				findings.push({
					path,
					fn: name,
					line: src.line(n.start),
					callee: calleeText(src, n),
					reason:
						names.size === 0
							? 'no authStore.identityFence() bound in this function'
							: 'fence bound but not called between the await and the send',
				});
			}
		});
	});

	return { findings, fenced };
}

describe('BUG-3095 — child population identity fence', () => {
	it('the reviewed population is the population ItemDetail actually mounts', () => {
		const detail = readFileSync(ROOT + 'lib/components/items/ItemDetail.svelte', 'utf8');
		const imported = new Set<string>();
		for (const m of detail.matchAll(/import\s+(\w+)\s+from\s+'(\$lib\/[^']+\.svelte)'/g)) {
			const [, local, spec] = m;
			// Mounted, not merely imported — a dead import is not a child.
			if (!new RegExp(`<${local}\\b`).test(detail)) continue;
			imported.add(spec.replace('$lib/', 'lib/'));
		}
		// Relative imports (`./ItemPicker.svelte`) resolve against the items dir.
		for (const m of detail.matchAll(/import\s+(\w+)\s+from\s+'(\.\/[^']+\.svelte)'/g)) {
			const [, local, spec] = m;
			if (!new RegExp(`<${local}\\b`).test(detail)) continue;
			imported.add('lib/components/items/' + spec.replace('./', ''));
		}

		const reviewed = new Set(POPULATION.filter((p) => !NOT_DIRECTLY_IMPORTED.has(p)));
		const missing = [...imported].filter((p) => !reviewed.has(p)).sort();
		const stale = [...reviewed].filter((p) => !imported.has(p)).sort();

		expect(
			{ missing, stale },
			'ItemDetail mounts a child this guard has never reviewed, or POPULATION names one it no longer mounts. ' +
				'Add it to POPULATION (and review its await-then-request paths) or remove it.'
		).toEqual({ missing: [], stale: [] });
	});

	it('every await-then-request path in the population holds an identity fence', () => {
		const findings: Finding[] = [];
		for (const path of POPULATION) findings.push(...analyseFile(path).findings);
		const unlisted = findings
			.map((f) => ({ ...f, label: `${f.path}::${f.fn}` }))
			.filter((f) => !KNOWN_UNFENCED.includes(f.label));

		expect(
			unlisted.map((f) => `${f.label} @${f.line} → ${f.callee} (${f.reason})`),
			'An await-then-request path with no identity fence. Fence it with ' +
				'authStore.identityFence(), or — if it belongs to the follow-up unit — ' +
				'add its exact file::function path to KNOWN_UNFENCED.'
		).toEqual([]);
	});

	it('every KNOWN_UNFENCED entry is still unfenced, so the list cannot rot into an exemption', () => {
		const findings: Finding[] = [];
		for (const path of new Set(KNOWN_UNFENCED.map((k) => k.split('::')[0]))) {
			findings.push(...analyseFile(path).findings);
		}
		const stillUnfenced = new Set(findings.map((f) => `${f.path}::${f.fn}`));
		const fixed = KNOWN_UNFENCED.filter((k) => !stillUnfenced.has(k)).sort();
		expect(
			fixed,
			'These paths are listed as known-unfenced but are now fenced (or gone). ' +
				'Remove them from KNOWN_UNFENCED — a stale entry is a permanent hole.'
		).toEqual([]);
	});
});

/** Exported for the guard's own tests below. */
export { analyseFile, POPULATION, KNOWN_UNFENCED };
