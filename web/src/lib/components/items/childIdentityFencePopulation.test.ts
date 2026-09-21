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
 *   (c) DEPTH. A send reached through a LOCAL helper is modelled to exactly ONE
 *       hop (`loadTimeline()`, `loadChildren()` — see `requestingHelperNames`),
 *       and a fence reached through a local helper likewise (`stale()`). A
 *       second hop in either direction is not modelled. For sends that is
 *       fail-OPEN, which is why it is stated here rather than implied.
 *       A send inside a callback passed to a helper that is not itself awaited
 *       here also reads as having no await before it.
 *   (d) COMMITS. This guard covers REQUESTS (`api.*`, `fetch`, and one-hop
 *       local helpers around them). A post-await assignment to component state
 *       is equally capable of showing one identity's data to the next, and is
 *       NOT modelled. The two commit rows fixed in this unit
 *       (`TimelineVersionCard.ensureResolved`, `ChildItems.loadChildren`) are
 *       covered by their own fences and by the driven legs, not by this guard.
 *       `ItemPicker.invokeCreate` and `ItemAttachmentStrip`'s post-confirmation
 *       delete are known members of this gap, found by hand.
 *   (e) NON-AWAIT SUSPENSION. `ItemAttachmentStrip.confirmDelete` re-checks on
 *       the far side of a NON-BLOCKING in-app confirmation menu — structurally
 *       the right move, on the wrong quantity (`paint.isCurrent()` compares
 *       `{ws, item}`). There is no `await` there at all, so nothing in this
 *       guard's model applies: a logout and different login while that menu is
 *       open passes the check and the DELETE goes out as the new user. Found by
 *       hand, not by this guard, and it is the clearest evidence that this
 *       guard is a floor and not a ceiling.
 *
 * FAIL-CLOSED, which is the property that makes the gaps survivable: anything
 * this guard cannot classify is a FAILURE, never a skip. A file it cannot parse
 * fails. A send whose enclosing function it cannot resolve fails. It never
 * concludes "safe" from not having understood something — so the way this guard
 * goes wrong is by asking to be taught, which is visible, rather than by going
 * quiet, which is not.
 *
 * KNOWN GAPS ARE ENUMERATED, NOT TOLERATED IN BULK (lead ruling). The paths
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
	// ---- BUG-3105 PR B: the burn-down list. Same item, next unit. ----
	// Each of these is an await-then-send path — a REQUEST, a COMMIT, or both —
	// with no identity fence. PR A fixed ShareDialog (6) and ItemPicker's
	// upward hand-off; these are what is left, and PR B empties this array.

	// ItemTimeline: loadMore and the six comment/reaction handlers were on
	// BUG-3095's list for their REQUESTS; loadTimeline and refreshFromSSE join
	// them under the commit model (both paint entries after their await).

	// CopyItemDialog: handleConfirm and runPreflight were REQUEST rows; the
	// dispatch pair and the two loaders are commit rows. dispatchCopy is the
	// one to treat carefully in PR B — the copy has no idempotency key, so its
	// fence must REFUSE, never re-send.

	// EditCollectionModal: handleArchive is the REQUEST row (delete -> list ->
	// delete). The other three commit after their await.
	'lib/components/collections/EditCollectionModal.svelte::handleArchive',
	'lib/components/collections/EditCollectionModal.svelte::handleSave',
	'lib/components/collections/EditCollectionModal.svelte::loadCollectionOptions',
	'lib/components/collections/EditCollectionModal.svelte::loadPreviewContext',

	// ItemAttachmentStrip: confirmDelete was fixed in BUG-3095 (row 9). These
	// three are its await-side siblings, including the delete's own
	// continuation and the global deletion broadcast it fires.
	'lib/components/items/ItemAttachmentStrip.svelte::performDelete',
	'lib/components/items/ItemAttachmentStrip.svelte::revalidateAfterRestore',
	'lib/components/items/ItemAttachmentStrip.svelte::<nested in $effect>',

	// The two backlink panels: identical shape, commit-only, no post-await
	// request. Fenced the same way in PR B.
	'lib/components/BacklinksPanel.svelte::loadFirstPage',
	'lib/components/BacklinksPanel.svelte::loadMore',
	'lib/components/RelationBacklinksPanel.svelte::loadFirstPage',
	'lib/components/RelationBacklinksPanel.svelte::loadMore',

	// QuickActionsMenu: handleAction / handleSaveNewAction were REQUEST rows;
	// readPresence commits presence state after its await.
	'lib/components/common/QuickActionsMenu.svelte::handleAction',
	'lib/components/common/QuickActionsMenu.svelte::handleSaveNewAction',
	'lib/components/common/QuickActionsMenu.svelte::readPresence',

	// PushToAgentDialog: handleSend was the REQUEST row; refreshPresence is its
	// commit counterpart.
	'lib/components/items/PushToAgentDialog.svelte::handleSend',
	'lib/components/items/PushToAgentDialog.svelte::refreshPresence',

	// FieldEditor: createRelationTarget commits through the parent's onchange,
	// which reaches ItemDetail.updateField and issues a PATCH. It is also
	// ItemPicker's `oncreate` caller, so PR B should consume the predicate
	// ItemPicker now passes up rather than inventing a second mechanism.
	'lib/components/fields/FieldEditor.svelte::createRelationTarget',
	'lib/components/fields/FieldEditor.svelte::<nested in writeRelationList>',

	// EditorBubbleMenu.handleCreate — worth its own note. BUG-3095's body
	// called this file THE REFERENCE PATTERN. It is not: its `sinceEpoch` is
	// localIndex.scopeEpochFor(ws), the projection-scope epoch, and the file
	// has no identity fence at all. The commit model now puts it on the list
	// mechanically, where before that was an argument on a trail.
	'lib/components/editor/EditorBubbleMenu.svelte::handleCreate',

	// Editor's onMount callback: `api.server.capabilities()` fire-and-forget.
	// Probably NOT a defect — the endpoint is public and carries no user-scoped
	// data — but "this endpoint is public" is a judgement the guard cannot
	// make, so PR B should record the decision rather than re-derive it.
	'lib/components/editor/Editor.svelte::<callback of onMount>',

];


function calleeText(src: AstSource, n: Node): string {
	return src.text(n.callee).replace(/\s+/g, '').replace(/\?\./g, '.');
}

/** A DIRECT REQUEST: an `api.*` call or a bare `fetch(`. */
function isDirectRequest(src: AstSource, n: Node): boolean {
	if (n.type !== 'CallExpression') return false;
	const c = calleeText(src, n);
	return c === 'fetch' || c.startsWith('api.') || c.startsWith('apiClient.');
}

/**
 * Script-level function names whose body issues a direct request — so a CALL to
 * one of them is itself a send.
 *
 * WHY THIS EXISTS, and it was a genuine hole rather than a refinement. The
 * commonest post-await send in this codebase is not an `api.*` call; it is a
 * call to the component's own loader: `await api.comments.create(…)` then
 * `await loadTimeline()`, `await api.items.create(…)` then `loadChildren()`.
 * Matching only `api.*` syntactically inside the handler misses every one of
 * them, and the miss is a FALSE NEGATIVE — the guard reports such a path as
 * having no send at all and stays silent.
 *
 * Found by three independent readers auditing the same population by hand while
 * this guard was already green; `ItemTimeline` alone has six such handlers, none
 * of which this guard saw. Recorded here rather than quietly fixed because the
 * lesson is the guard's, not the codebase's: a green source scanner had not been
 * shown able to go red for the shape that dominates the population.
 *
 * ONE hop, matching `fenceHelperNames`: a helper calling a helper that requests
 * is not modelled and reads as no-send, which is the fail-open direction and is
 * therefore declared as gap (c) in the header rather than left implicit.
 */
function requestingHelperNames(src: AstSource): Set<string> {
	const names = new Set<string>();
	walk(src.script, (n) => {
		let fn: Node | null = null;
		let name = '';
		if (n.type === 'FunctionDeclaration' && n.id?.type === 'Identifier') {
			fn = n;
			name = n.id.name;
		} else if (n.type === 'VariableDeclarator' && isFnNode(n.init) && n.id?.type === 'Identifier') {
			fn = n.init;
			name = n.id.name;
		}
		if (!fn || !name) return;
		let requests = false;
		walk(fn, (m) => {
			if (isDirectRequest(src, m)) requests = true;
		});
		if (requests) names.add(name);
	});
	return names;
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
	// A call whose CALLEE IS A FUNCTION is an IIFE — `(async () => {…})()` — and
	// its "callee text" is the entire function body. Using that as a label
	// produced a multi-kilobyte key containing the whole source of the function,
	// which is unreadable, and unstable under any edit inside it. A contract key
	// has to be short and has to survive edits to the code it names, so an IIFE
	// is named for the nearest NAMED thing enclosing it instead.
	const calleeLabel = (c: Node): string | null => {
		if (isFnNode(c.callee)) return null;
		const t = calleeText(src, c);
		return t.length <= 40 ? t : null;
	};
	if (parent?.type === 'CallExpression' && parent.callee) {
		const l = calleeLabel(parent);
		if (l) return `<callback of ${l}>`;
	}
	for (let i = ancestors.length - 1; i >= 0; i--) {
		const a = ancestors[i];
		if (a.type === 'CallExpression' && a.callee) {
			const l = calleeLabel(a);
			if (l) return `<nested in ${l}>`;
		}
		if (isFnNode(a) && a.id?.name) return `<nested in ${a.id.name}>`;
		if (a.type === 'VariableDeclarator' && a.id?.type === 'Identifier') {
			return `<nested in ${a.id.name}>`;
		}
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

/**
 * Script-scope bindings a handler can COMMIT to — component state.
 *
 * BUG-3105 step 1. BUG-3095's guard modelled only REQUESTS, and said so as a
 * declared gap. A post-await assignment to component state leaks the same way a
 * request does, one direction reversed: instead of the previous identity's
 * intent going out on the next identity's cookie, the previous identity's DATA
 * lands in the next identity's pane. `ShareDialog` is the proof — six unguarded
 * await-then-commit paths, no fence of any kind anywhere in the file, and it
 * appeared NOWHERE in BUG-3095's emitted list.
 *
 * Only top-level `let`/`var` count. A `const` cannot be reassigned, and a
 * binding declared INSIDE the handler is that handler's own local — writing it
 * after an await commits nothing anyone else can read.
 */
function componentStateNames(src: AstSource): Set<string> {
	const names = new Set<string>();
	for (const stmt of src.script.body ?? []) {
		if (stmt.type !== 'VariableDeclaration' || stmt.kind === 'const') continue;
		for (const d of stmt.declarations ?? []) {
			if (d.id) patternNamesInto(d.id, names);
		}
	}
	return names;
}

/** Every identifier bound by a declaration pattern (destructuring included). */
function patternNamesInto(p: Node, out: Set<string>) {
	if (!p) return;
	switch (p.type) {
		case 'Identifier':
			out.add(p.name);
			return;
		case 'ObjectPattern':
			for (const pr of p.properties ?? []) patternNamesInto(pr.value ?? pr.argument, out);
			return;
		case 'ArrayPattern':
			for (const el of p.elements ?? []) if (el) patternNamesInto(el, out);
			return;
		case 'AssignmentPattern':
			patternNamesInto(p.left, out);
			return;
		case 'RestElement':
			patternNamesInto(p.argument, out);
			return;
	}
}

/**
 * Names destructured out of `$props()` — the PARENT's callbacks.
 *
 * Calling one after an await is a commit too, and the sharpest kind: it hands
 * the previous identity's data upward into a parent that has already moved on.
 * `ItemPicker.invokeCreate` is the member that made this worth modelling — it
 * awaits the parent's `oncreate` and cannot be fenced by the caller either,
 * because it passes neither workspace nor epoch up.
 */
function propCallbackNames(src: AstSource): Set<string> {
	const names = new Set<string>();
	walk(src.script, (n) => {
		if (n.type !== 'VariableDeclarator' || !n.init) return;
		if (n.init.type !== 'CallExpression') return;
		if (calleeText(src, n.init) !== '$props') return;
		const all = new Set<string>();
		patternNamesInto(n.id, all);
		// A callback by convention: `onFoo` / `onfoo`. Narrow deliberately — a
		// plain data prop is not something a handler can commit THROUGH.
		for (const nm of all) if (/^on[A-Za-z]/.test(nm) || /^on[a-z]/.test(nm)) names.add(nm);
	});
	return names;
}

/** The root identifier an assignment target writes through. */
function assignmentRoot(n: Node): string | null {
	let t = n;
	while (t && (t.type === 'MemberExpression' || t.type === 'ChainExpression')) {
		t = t.object ?? t.expression;
	}
	return t && t.type === 'Identifier' ? t.name : null;
}

interface Finding {
	path: string;
	fn: string;
	line: number;
	callee: string;
	reason: string;
}

function analyseFile(path: string): { findings: Finding[]; fenced: string[] } {
	let code: string;
	try {
		code = readFileSync(ROOT + path, 'utf8');
	} catch (e) {
		// FAIL CLOSED, and cleanly: a POPULATION entry naming a file that is not
		// there is a finding, not a crash. Raised while mutating this guard — an
		// ENOENT escaping here reads as a broken test rather than as the guard
		// reporting a stale population, which is the same confusion the
		// population check exists to prevent.
		return {
			findings: [{ path, fn: '<file>', line: 0, callee: '<read>', reason: `missing file: ${String(e)}` }],
			fenced: [],
		};
	}
	return analyseSource(path, code);
}

/**
 * True when `n` sits inside the FINALIZER of a try statement, at any depth
 * within the function under analysis (the caller already excludes nested
 * functions). `ancestors` runs outermost-first, as `walk` supplies it.
 */
function inFinalizer(ancestors: Node[], n: Node): boolean {
	const chain = [...ancestors, n];
	for (let i = 0; i < chain.length - 1; i++) {
		const a = chain[i];
		if (a.type === 'TryStatement' && a.finalizer && a.finalizer === chain[i + 1]) return true;
	}
	return false;
}

/**
 * A plain `=` whose right-hand side is a LITERAL (`false`, `null`, `0`, `''`).
 * Template literals are not `Literal` nodes and do not qualify: `${x}` can carry
 * data. Neither does `undefined`, which is an Identifier and could be shadowed.
 */
function isLiteralAssignment(n: Node): boolean {
	return n.type === 'AssignmentExpression' && n.operator === '=' && n.right?.type === 'Literal';
}

/**
 * The start of the innermost `catch`/`finally` clause holding `n` whose try
 * contains one of the `preceding` awaits in a part that ENTERS that clause —
 * the block for a catch; the block or the handler for a finally. -Infinity when
 * there is none, so it never lowers a bound.
 */
function clauseBound(ancestors: Node[], n: Node, preceding: Node[]): number {
	const chain = [...ancestors, n];
	const within = (outer: Node | null | undefined, x: Node) =>
		!!outer && outer.start <= x.start && outer.end >= x.end;
	let bound = -Infinity;
	for (let i = 0; i < chain.length - 1; i++) {
		const t = chain[i];
		if (t.type !== 'TryStatement') continue;
		const next = chain[i + 1];
		let entering: Node[] = [];
		if (t.handler && next === t.handler) entering = [t.block];
		else if (t.finalizer && next === t.finalizer) entering = [t.block, t.handler];
		else continue;
		if (preceding.some((a) => entering.some((part) => within(part, a)))) {
			bound = Math.max(bound, next.start);
		}
	}
	return bound;
}

function analyseSource(path: string, code: string): { findings: Finding[]; fenced: string[] } {
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
	const helperSends = requestingHelperNames(src);
	const stateNames = componentStateNames(src);
	const propCallbacks = propCallbackNames(src);

	/** What a send or commit is CALLED, for the finding text. */
	const describe = (n: Node): string => {
		if (n.type === 'AssignmentExpression' || n.type === 'UpdateExpression') {
			const root = assignmentRoot(n.type === 'UpdateExpression' ? n.argument : n.left);
			return `${root} = …`;
		}
		return calleeText(src, n);
	};

	const isRequestNode = (n: Node) =>
		isDirectRequest(src, n) ||
		(n.type === 'CallExpression' && helperSends.has(calleeText(src, n)));

	const isCommitNode = (n: Node) => {
		if (n.type === 'AssignmentExpression') {
			const root = assignmentRoot(n.left);
			return !!root && stateNames.has(root);
		}
		if (n.type === 'UpdateExpression') {
			const root = assignmentRoot(n.argument);
			return !!root && stateNames.has(root);
		}
		if (n.type === 'CallExpression') return propCallbacks.has(calleeText(src, n));
		return false;
	};

	const isSend = (n: Node) => isRequestNode(n) || isCommitNode(n);

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
			if (!isSend(n)) return;
			// A nested function's sends belong to that function, not this one.
			for (const a of ancestors) {
				if (a !== fn && isFnNode(a)) return;
			}
			// A LITERAL assignment inside a `finally` is not a commit (BUG-3105,
			// lead ruling 1 for PR B): a commit carries data across the await, and
			// a literal carries none. This is what lets a busy flag clear
			// UNCONDITIONALLY — `finally { creating = false }` must run under any
			// identity, or a failed request leaves the component disabled. Narrow
			// by construction: a non-literal assignment in a finally still fires,
			// and so does a literal assignment anywhere else. Both are pinned by
			// the fixture test at the bottom of this file.
			if (isLiteralAssignment(n) && inFinalizer(ancestors, n)) return;

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
			let lowerBound = loop ? loop.start : Math.max(...preceding.map((a) => a.start));
			// A CATCH or FINALLY is not "after" the fence calls in its try block
			// in the sense position implies (BUG-3105, found by the non-literal
			// control below). A catch is entered from the await THROWING, so a
			// fence textually after that await never ran; a finally is entered on
			// every exit, INCLUDING the fence's own early return — which is the
			// exact path on which the identity has changed. So when an await
			// preceding the send lies inside that try, the fence must be called
			// inside the clause itself. A fence called before the whole try still
			// counts: returning there never enters it.
			lowerBound = Math.max(lowerBound, clauseBound(ancestors, n, preceding));
			const ok = fenceOffsets.some((o) => o > lowerBound && o < n.start);

			const label = `${path}::${name}`;
			if (ok) {
				fenced.push(label);
			} else {
				findings.push({
					path,
					fn: name,
					line: src.line(n.start),
					callee: describe(n),
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

	it('every await-then-send path in the population holds an identity fence', () => {
		const findings: Finding[] = [];
		for (const path of POPULATION) findings.push(...analyseFile(path).findings);
		const unlisted = findings
			.map((f) => ({ ...f, label: `${f.path}::${f.fn}` }))
			.filter((f) => !KNOWN_UNFENCED.includes(f.label));

		// ONE ROW PER PATH, not per offending statement (lead ruling, BUG-3105).
		// The commit model made the difference load-bearing: a single handler can
		// commit a dozen times, so `BacklinksPanel.loadFirstPage` alone emitted
		// eight rows and the real shape of the list — how many HANDLERS need
		// fencing — was buried. The fix is per handler, so the unit of the
		// contract is the handler; the first offending line is enough to find it
		// and the count says how much is behind it.
		const byPath = new Map<string, typeof unlisted>();
		for (const f of unlisted) {
			const list = byPath.get(f.label) ?? [];
			list.push(f);
			byPath.set(f.label, list);
		}
		const rows = [...byPath.entries()]
			.map(([label, fs]) => {
				const first = fs.reduce((a, b) => (a.line <= b.line ? a : b));
				const more = fs.length - 1;
				return `${label} @${first.line} → ${first.callee}${more > 0 ? ` (+${more} more)` : ''} (${first.reason})`;
			})
			.sort();

		expect(
			rows,
			'An await-then-send path with no identity fence — a REQUEST or a COMMIT ' +
				'reachable after an await. Fence the handler with authStore.identityFence(), ' +
				'or — if it belongs to the follow-up unit — add its exact file::function ' +
				'path to KNOWN_UNFENCED.'
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

describe('BUG-3105 — the literal-in-finally refinement, with its controls', () => {
	// One handler shape, three variants that differ ONLY in the statement under
	// test, so each leg discriminates exactly one property of the refinement.
	const fixture = (body: string) => `<script lang="ts">
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	let busy = $state(false);
	let data = $state<unknown>(null);
	async function go() {
		const isSameIdentity = authStore.identityFence();
		busy = true;
		try {
			const r = await api.items.get('ws', 'slug');
			if (!isSameIdentity()) return;
			data = r;
		} finally {
			${body}
		}
	}
</script>`;
	const rows = (code: string) =>
		analyseSource('fixture.svelte', code).findings.map((f) => `${f.fn}:${f.callee}`);

	it('a LITERAL assignment inside a finally is not a commit', () => {
		expect(rows(fixture('busy = false;'))).toEqual([]);
	});

	it('CONTROL: a NON-literal assignment inside a finally still fires', () => {
		// Without this leg the refinement is indistinguishable from "ignore
		// finally blocks", which would be a real hole: `data` carries the
		// response across the await.
		expect(rows(fixture('data = String(busy);'))).toEqual(['go:data = …']);
	});

	it('CONTROL: a literal assignment OUTSIDE a finally still fires', () => {
		const code = fixture('busy = false;').replace(
			'data = r;',
			'data = r;\n\t\t\tawait api.items.get(\'ws\', \'other\');\n\t\t\tbusy = false;'
		);
		expect(rows(code)).toEqual(['go:busy = …']);
	});

	it('CONTROL: a commit in a CATCH is not protected by a fence in the try', () => {
		// The catch is entered from the await throwing, so the fence call that
		// sits textually between them never ran.
		const code = fixture('busy = false;').replace(
			'\t\t} finally {',
			'\t\t} catch (e) {\n\t\t\tdata = e;\n\t\t} finally {'
		);
		expect(rows(code)).toEqual(['go:data = …']);
	});

	it('a fence called BEFORE the try protects its finally — returning there never enters it', () => {
		// Guards the clause rule against over-reach: it binds only when the
		// preceding await is INSIDE the try.
		const code = `<script lang="ts">
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	let data = $state<unknown>(null);
	async function go() {
		const isSameIdentity = authStore.identityFence();
		const r = await api.items.get('ws', 'slug');
		if (!isSameIdentity()) return;
		try {
			data = r;
		} finally {
			data = String(r);
		}
	}
</script>`;
		expect(rows(code)).toEqual([]);
	});
});

/** Exported for the guard's own tests. */
export { analyseFile, analyseSource, POPULATION, KNOWN_UNFENCED };
