/**
 * BUG-3084, ItemDetail — the identity GATE and the analysis AID. See
 * `src/test/identityFenceAst.ts` for what each claims and what each cannot
 * see. `itemDetailIdentityFence.test.ts` beside this keeps the site pins;
 * `itemDetailIdentityLoad.svelte.test.ts` owns the semantics on a mount.
 *
 * The GATE (`refusals`, lead ruling on checkpoint 69):
 *   - every unit the AST yields matches exactly one row below, and every row
 *     exactly one unit;
 *   - every row, and every helper in HELPERS, carries `reviewed`: the hash of
 *     the code it was last reviewed on (`coverage`). A mismatch refuses,
 *     naming the row and the new hash.
 * To accept an edit: read the row and `analysisReport` for the edited code,
 * then paste the printed hash.
 *
 * The AID (`analysisReport`): the flow analysis, which reads each row's
 * reasons.
 *   - `may` lists the commits a unit may make while no fence holds. A key is
 *     an assignment target's root name, or a call's callee text.
 *   - A row with `may` also pins the unit's exact `code`.
 *   - A row with `may` or `startSafe` pins its enclosing context (`in`).
 *
 * Round 4's mutants and the later defect fixtures still run: the gate
 * refuses each of them, and the aid still reports each of them.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import {
	parseComponent,
	declarations,
	enumerateUnits,
	analyseUnit,
	walk,
	refusedConstructs,
	asyncUnitStartsSafe,
	type AstSource,
	type Node,
	type Unit,
} from '../../../test/identityFenceAst';
import round4 from './itemDetailIdentityFence.round4.json';
import round6 from './itemDetailIdentityFence.round6.json';
import round7 from './itemDetailIdentityFence.round7.json';
import round8 from './itemDetailIdentityFence.round8.json';

interface GuardMutant {
	id: string;
	finding: string;
	what: string;
	old: string;
	new: string;
}
const ROUND4_MUTANTS: GuardMutant[] = round4.mutants;

const SOURCE = readFileSync(new URL('./ItemDetail.svelte', import.meta.url), 'utf8');

interface Row {
	why: string;
	may?: string[];
	/** Exact statement texts read as a bare await (their argument is not walked). */
	bareAwaits?: string[];
	/** Start the unit safe. Only with a `pin` proving why. */
	startSafe?: boolean;
	pin?: (src: AstSource, unit: Unit) => string | null;
	/**
	 * Callbacks this unit creates that may commit something, by callback key
	 * (see `AnalyseOptions.callbacks`). Every other callback it creates starts
	 * unsafe and may commit nothing.
	 */
	callbacks?: Record<string, { may: string[]; why: string }>;
	/**
	 * For a top-level function with `may`: the hash of its code (`codeOf`) when
	 * the allowance was last reviewed. `may` covers the whole function, so any
	 * edit to it refuses until the allowance is re-read and this is updated
	 * (round 6 class 8).
	 */
	reviewed?: string;
}

/** Top-level `async function` declarations, by name. */
const ASYNC_FUNCTIONS: Record<string, Row> = {
	adoptOrConvergeToLiveCollection: { reviewed: 'bf80efe8fab6', why: 'myGen against loadGeneration before adopting' },
	reconcileCollectionSegment: { reviewed: '716296138c0e', why: 'identityHeld after the list fetch; retags and navigates only under it' },
	jumpToSection: { reviewed: '643129c7a73b', why: 'switches this instance\'s tab and scrolls to an anchor', may: ['document.getElementById', 'document.getElementById(anchorId).scrollIntoView'] },
	ensureGraphComp: { reviewed: 'c41db6a9cb03', why: 'lazy-loads a component module into this instance', may: ['ItemGraphComp', 'graphLoadError'] },
	handleCopyRef: { reviewed: 'bf402ccdf03f', why: 'switchedAway before the copied flag' },
	loadData: { reviewed: '41cab0301a6d', why: 'IS the load: myGen against loadGeneration after every await' },
	startEditTitle: { reviewed: 'c9ad3a7e30a3', why: 'focuses and sizes the input it opened synchronously', may: ['el', 'titleInputEl.focus', 'titleInputEl.setSelectionRange'] },
	saveTitle: { reviewed: '95404f8d9759', why: 'gen against loadGeneration on both arms' },
	updateField: {
		reviewed: '78c200f8a749',
		why: 'stillCurrent() on every arm, the OCC refetch and the open-children confirm',
		callbacks: {
			'submitOrderedOCC({send})': {
				may: ['api.items.update'],
				why: 'submitOrderedOCC calls send first with no await before it, and re-sends only after stillCurrent() with no await between (fieldWriteOrder.test.ts: "does not RE-SEND once the view moves on DURING the refetch")',
			},
			'submitOrderedOCC({refetch})': {
				may: ['api.items.get'],
				why: 'submitOrderedOCC refetches only after stillCurrent() with no await between (fieldWriteOrder.test.ts: "does not retry once the view has moved on"); a read',
			},
		},
	},
	flushTagSaver: {
		reviewed: '3610729f7b4b',
		why: 'identityHeld(saver.epoch) before every commit and send; the unfenced writes are to this burst\'s own identity-stamped record, and the finally deletes that record only if the registry still holds it (the get)',
		may: ['saver', 'tagSavers.get', 'tagSavers.delete'],
	},
	refreshCollectionIfMoved: { reviewed: 'c17ede44c83b', why: 'gen against loadGeneration after the fetch' },
	loadTagSuggestions: { reviewed: 'a2a65a168907', why: 'identityHeld after the fetch; the identity listener re-runs it' },
	stampSourceUrl: { reviewed: 'cddbde8eae48', why: 'switchedAway on both arms' },
	refreshFromSource: { reviewed: '3dab29f242b3', why: 'switchedAway on every arm' },
	updateAssignedUser: { reviewed: '0983ca9ddfee', why: 'gen against loadGeneration on both arms' },
	updateAgentRole: { reviewed: '036ec822b47e', why: 'gen against loadGeneration on both arms' },
	flushRawIfPending: {
		reviewed: '127df117c1c9',
		why: 'genAtFlush against loadGeneration after each PATCH; the re-entrancy waiter returns state; the finally clears this drain\'s own in-flight flag',
		may: ['rawFlushInFlight'],
		bareAwaits: ['await new Promise((r) => setTimeout(r, 50));'],
	},
	refreshLinksPreservingOnFailure: { reviewed: '07106cafda42', why: 'returns a value; its callers fence' },
	flushCollabBeforeRestore: { reviewed: 'b92b41b4befa', why: 'identityHeld before its failure toast' },
	closeCopyDialog: { reviewed: 'de6f9da6db49', why: 'restores focus after closing synchronously', may: ['paneMenuTrigger.focus'] },
	closePushDialog: { reviewed: '3514a78253cc', why: 'restores focus after closing synchronously', may: ['paneMenuTrigger.focus'] },
	flushContentBeforeCopy: { reviewed: 'b0588d22f4a7', why: 'returns a boolean to the dialog' },
	handleCopied: { reviewed: '7e8a0cbad376', why: 'switchedAway before adopting the refreshed item' },
	handleDelete: { reviewed: '21973bed0774', why: 'switchedAway on both arms' },
	handleRestore: { reviewed: 'c1c8eda7c183', why: 'switchedAway on every arm' },
	handleDeleteLink: { reviewed: 'cb560b98a8d8', why: 'switchedAway after each await' },
	handleCreateLink: { reviewed: '11f0f12d870b', why: 'switchedAway after each await' },
	handleMove: { reviewed: '998d3f75df53', why: 'stillOnSource() on every arm, including inside navIfStillCurrent' },
};

interface SignedRow extends Row {
	/** Tested against the unit's function text (whitespace collapsed). */
	body: RegExp;
	/** For a continuation, tested against the deferring call's text up to the callback. */
	call?: RegExp;
	/**
	 * The unit's enclosing context, which must equal `contextOf`: the innermost
	 * enclosing function's name, `callee(…)` for an anonymous call argument, or
	 * `{key}` for an object property. Required on a row that carries `may` or
	 * `startSafe`, so the row cannot re-point by a MOVE (round 5 P2-8).
	 */
	in?: string;
	/**
	 * The unit's exact code, comments dropped (`codeOf`). Required on a row
	 * that carries `may`, so the allowance covers that code and nothing else
	 * (round 5 P2-7).
	 */
	code?: string;
}

/** Async functions that are not top-level declarations, in the script. */
const NESTED: SignedRow[] = [
	{ body: /event\.type === 'collection_updated'/, why: 'SSE: callbackGen after the collection fetch, itemGen on item branches', reviewed: 'ff9561c55657' },
	{ body: /result\.type === 'caught_up'/, why: 'sync: callbackGen after the reconciliation, itemGen on item branches', reviewed: 'd278038b7d00' },
	{ body: /flushCollabContent\(/, why: 'collab save: isForegroundCurrent (genAtFlush) before UI feedback', reviewed: 'aa3702e7a8ec' },
];

/** Async functions in the markup. */
const MARKUP: SignedRow[] = [
	{ body: /startGen/, why: 'Rich toggle: startGen against loadGeneration after each await', reviewed: '8163f09db53e' },
	{ body: /genAtToggle/, why: 'Markdown toggle: genAtToggle against loadGeneration after each await', reviewed: 'ab80fb08e5d6' },
];

const collapse = (s: string) => s.replace(/\s+/g, ' ');

/** A function's params and statements, whitespace collapsed; comments are not statements, so they drop out. */
function codeOf(src: AstSource, fn: Node): string {
	const params = `(${fn.params.map((p: Node) => src.text(p)).join(', ')}) => `;
	const b = fn.body;
	const body = b.type === 'BlockStatement' ? `{ ${b.body.map((st: Node) => src.text(st)).join(' ')} }` : src.text(b);
	return collapse(params + body);
}

const parents = new WeakMap<AstSource, Map<Node, Node>>();

function parentsOf(src: AstSource): Map<Node, Node> {
	let m = parents.get(src);
	if (!m) {
		const map = new Map<Node, Node>();
		const visit = (n: Node, anc: Node[]) => {
			if (anc.length) map.set(n, anc.at(-1)!);
		};
		walk(src.script, visit);
		walk(src.fragment, visit);
		parents.set(src, map);
		m = map;
	}
	return m;
}

function contextOf(src: AstSource, unit: Unit): string {
	const f = unit.enclosing.at(-1);
	if (!f) return '(top level)';
	const p = parentsOf(src).get(f);
	if (f.type === 'FunctionDeclaration') return f.id.name;
	if (p?.type === 'VariableDeclarator' && p.init === f && p.id.type === 'Identifier') return p.id.name;
	if (p?.type === 'CallExpression') return `${collapse(src.text(p.callee))}(…)`;
	if (p?.type === 'Property' && !p.computed && p.key.type === 'Identifier') return `{${p.key.name}}`;
	return `(anonymous ${p?.type ?? 'function'})`;
}

function clearsBeforeFirstAwait(src: AstSource, fnName: string, timer: string): string | null {
	const fn = src.script.body.find((s: Node) => s.type === 'FunctionDeclaration' && s.id?.name === fnName);
	if (!fn) return `${fnName} is gone`;
	// The clear must DOMINATE the first await: a statement of the function body
	// itself, reached on every path — no await, return or throw in any
	// statement before it (round 6 class 9: a clear made conditional, or moved
	// into an arrow nobody calls, used to pass by position).
	const ownExit = (st: Node) => {
		let found = false;
		walk(st, (n, anc) => {
			if (anc.some((a) => a !== st && (a.type === 'ArrowFunctionExpression' || a.type === 'FunctionExpression' || a.type === 'FunctionDeclaration'))) return;
			if (n.type === 'AwaitExpression' || n.type === 'ReturnStatement' || n.type === 'ThrowStatement') found = true;
		});
		return found;
	};
	for (const st of fn.body.body as Node[]) {
		if (st.type === 'ExpressionStatement' && src.text(st.expression).replace(/\s+/g, '') === `clearTimeout(${timer})`) return null;
		if (ownExit(st)) break;
	}
	return `${fnName} no longer clears ${timer} before its first await`;
}

/**
 * Why a continuation's timer is `timer`: its deferring call must be the whole
 * right-hand side of `timer = …` (round 5 P1-4 — a pin on the clear alone held
 * after the callback was moved onto a timer nobody clears).
 */
function assignedTo(src: AstSource, unit: Unit, timer: string): string | null {
	let ok = false;
	walk(src.script, (n) => {
		if (n.type === 'AssignmentExpression' && n.operator === '=' && n.right === unit.call && n.left.type === 'Identifier' && n.left.name === timer) ok = true;
	});
	return ok ? null : `its ${src.text(unit.call!.callee)} is not assigned to ${timer}`;
}

/** Callbacks passed to deferring calls, in the script. */
const CONTINUATIONS: SignedRow[] = [
	{
		call: /noScroll: true, \}\)\.catch\($/,
		body: /./,
		in: 'reconcileCollectionSegment',
		code: '() => { if (renameOverride === bridge) renameOverride = null; }',
		why: 'rename heal failure: clears only the bridge object this heal installed', reviewed: 'c06db87a69e9',
		may: ['renameOverride'],
	},
	{ call: /^setTimeout\($/, body: /copied = false/, why: 'copy-flag reset: switchedAway', reviewed: '56e147b6d62a' },
	{ call: /api\.items\.get\(wsSlug, itemSlug\)\.catch\($/, body: /./, why: 'loadData item fetch: sets a flag local to that load and re-throws', reviewed: 'e43ea7bc6f6f' },
	{
		call: /^setTimeout\($/,
		body: /staleConnecting = true/,
		in: '$effect(…)',
		code: "() => { if (collabProvider?.state === 'connecting' && !hasEverSynced) { staleConnecting = true; } }",
		why: 'connection state of this instance\'s own provider', reviewed: 'c55e6c96d765',
		may: ['staleConnecting'],
	},
	{ call: /\.get\(refreshCtx\.wsSlug, refreshCtx\.itemId\) \.then\($/, body: /./, why: 'force-refresh fetch: refreshGen against loadGeneration', reviewed: '6d51a6aabfdf' },
	{ call: /forceRefreshNonce \+= 1; \}\) \.catch\($/, body: /./, why: 'force-refresh failure: refreshGen against loadGeneration', reviewed: 'af1556df030e' },
	{
		call: /^setTimeout\($/,
		body: /teardownFlushed/,
		in: 'onBeforeUnload',
		code: '() => { teardownFlushed = false; }',
		why: 're-arms the BUG-3005 teardown latch, itself identity-checked', reviewed: 'ad39db63d644',
		may: ['teardownFlushed'],
	},
	{ call: /^queueMicrotask\($/, body: /./, why: 'collab lazy seed: refuses a retired or re-identified context first', reviewed: '745cd7b681c5' },
	{ call: /^setTimeout\($/, body: /saveStatus/, in: 'showSaved', code: "() => { saveStatus = 'idle'; }", why: 'cosmetic save-indicator reset', reviewed: '90e14d5c11a9', may: ['saveStatus'] },
	{ call: /^tick\(\)\.then\($/, body: /./, why: 'schedules a focus frame; commits nothing itself', reviewed: '7e4e3943afda' },
	{
		call: /^requestAnimationFrame\($/,
		body: /./,
		in: 'tick().then(…)',
		code: '() => editorInstance?.commands.focus()',
		why: 'focuses the editor after a tab switch', reviewed: '113d61cd235e',
		may: ['editorInstance.commands.focus'],
	},
	{
		call: /^setTimeout\($/,
		body: /content: toSave \}\)\.then/,
		in: 'handleContentUpdate',
		why: 'content debounce: loadData clears this timer before its first await, so the callback never runs across a load', reviewed: '0a617316d21e',
		startSafe: true,
		pin: (src, unit) => clearsBeforeFirstAwait(src, 'loadData', 'contentDebounceTimer') ?? assignedTo(src, unit, 'contentDebounceTimer'),
	},
	{ call: /\{ content: toSave \}\)\.then\($/, body: /^\(\) =>/, why: 'content save: switchedAway', reviewed: 'e9e7e1144c7e' },
	{ call: /showSaved\(\); \}\)\.catch\($/, body: /./, why: 'content save failure: switchedAway', reviewed: 'db434fa49d04' },
	{ call: /\{ keepalive: true \}\) \.then\($/, body: /./, why: 'raw keepalive save: genAtSave against loadGeneration', reviewed: 'ab79a6c3c6fe' },
	{ call: /localDirty = false; \} \}\) \.catch\($/, body: /^\(\) => \{\}$/, why: 'raw keepalive failure: empty', reviewed: '33ea742058bb' },
	{ call: /reqItemId, \{ content: toSave \}\)\.then\($/, body: /./, why: 'raw foreground save: genAtSave against loadGeneration', reviewed: 'fa36d2f9eb98' },
	{ call: /content: item\.content \}\); \} \}\)\.catch\($/, body: /./, why: 'raw foreground failure: genAtSave against loadGeneration', reviewed: 'eb764a8ec054' },
];

/**
 * Component-level synchronous functions a tabled unit reaches by name,
 * transitively, with the hash of the code each was last reviewed on (see
 * `coverage`). An inlined helper runs with its caller's state, so an edit to
 * one is an edit to every unit that reaches it.
 */
const HELPERS: Record<string, string> = {
	adoptCollection: 'f7434c6b7e7e',
	adoptServerItem: 'b13b3c2258ef',
	autoResizeTitle: '6ab5c619e2f3',
	captureIdentity: '3169e715940b',
	handleGone: 'e0a6466417a8',
	handleNavigateAway: '65a913aa7ef0',
	identityHeld: 'aa01184970c7',
	navigateToCollectionRoot: '1244fd1b9210',
	refreshPrintMeta: 'dcfe6dacb9b9',
	runTeardownFlush: 'ee41feec3e40',
	showSaved: '54edb673369e',
	switchedAway: 'd06abf38a5cb',
	withInflightTags: 'd1622fc8aa71',
};

/** Deferring calls whose callback is not a function literal. */
const IDENTIFIER_CALLBACKS: Array<{ text: string; count: number; why: string }> = [
	{ text: 'Promise.resolve().then(ensureGraphComp)', count: 2, why: 'ensureGraphComp is itself a unit in the table above' },
	{ text: 'setTimeout(r, 50)', count: 1, why: 'resolves flushRawIfPending\'s re-entrancy waiter' },
];

function unitLabel(src: AstSource, u: Unit): string {
	return u.name ? `${u.name}()` : `${u.kind}${u.inMarkup ? ' in markup' : ''} at line ${u.line}`;
}

const sha = (s: string) => createHash('sha1').update(s).digest('hex').slice(0, 12);

/** A function's name for messages: its declared or bound name, or where it is passed. */
function fnName(src: AstSource, fn: Node): string {
	if (fn.type === 'FunctionDeclaration' && fn.id) return fn.id.name;
	const p = parentsOf(src).get(fn);
	if (p?.type === 'VariableDeclarator' && p.id.type === 'Identifier') return p.id.name;
	if (p?.type === 'CallExpression') return `${collapse(src.text(p.callee))}(…)`;
	if (p?.type === 'Property' && !p.computed && p.key.type === 'Identifier') return `{${p.key.name}}`;
	return `function at line ${src.line(fn.start)}`;
}

interface TopLevel {
	/** Component-level SYNC functions, by name: the helper population draws from these. */
	fns: Map<string, Node>;
	/** Component-level statements declaring each name, functions excluded. */
	declaring: Map<string, Node[]>;
	statements: Node[];
}

function topLevelOf(src: AstSource): TopLevel {
	const fns = new Map<string, Node>();
	const declaring = new Map<string, Node[]>();
	const statements: Node[] = [];
	for (const raw of src.script.body as Node[]) {
		const st = raw.type === 'ExportNamedDeclaration' && raw.declaration ? raw.declaration : raw;
		statements.push(raw);
		if (st.type === 'FunctionDeclaration' && st.id) {
			if (!st.async) fns.set(st.id.name, st);
			continue;
		}
		if (st.type !== 'VariableDeclaration') continue;
		for (const d of st.declarations as Node[]) {
			const fnInit = d.init && (d.init.type === 'ArrowFunctionExpression' || d.init.type === 'FunctionExpression');
			if (d.id.type === 'Identifier' && fnInit) {
				if (!d.init.async) fns.set(d.id.name, d.init);
				continue;
			}
			walk(d.id, (n) => {
				if (n.type === 'Identifier') declaring.set(n.name, [...(declaring.get(n.name) ?? []), raw]);
			});
		}
	}
	return { fns, declaring, statements };
}

/**
 * What a hash covers (lead ruling on checkpoint 69, condition 1 and its
 * refinement 2): the code of `roots`, every component-level declaration of a
 * name that code references, and every component-level statement that
 * assigns a name that code CALLS — a function value rebound outside every
 * function (round 8 G). `helpers` are the component-level sync functions the
 * code names; each is its own HELPERS row. All by syntax, never by the
 * analysis, because the analysis is what the hash does not trust.
 */
function coverage(src: AstSource, tl: TopLevel, roots: Node[]): { text: string; helpers: Set<string> } {
	const refs = new Set<string>();
	const called = new Set<string>();
	for (const r of roots) {
		walk(r, (n) => {
			if (n.type === 'Identifier') refs.add(n.name);
			if (n.type === 'CallExpression' && n.callee.type === 'Identifier') called.add(n.callee.name);
		});
	}
	const helpers = new Set([...refs].filter((n) => tl.fns.has(n) && !roots.includes(tl.fns.get(n)!)));
	const stmts = new Set<Node>();
	for (const n of refs) for (const st of tl.declaring.get(n) ?? []) stmts.add(st);
	for (const st of tl.statements) {
		walk(st, (n) => {
			const target = n.type === 'AssignmentExpression' ? n.left : n.type === 'UpdateExpression' ? n.argument : null;
			if (target?.type === 'Identifier' && called.has(target.name)) stmts.add(st);
		});
	}
	const text = [
		...roots.map((r) => codeOf(src, r)),
		...[...stmts].sort((a, b) => a.start - b.start).map((st) => collapse(src.text(st))),
	].join(' || ');
	return { text, helpers };
}

/** What each unit's row hash covers: the unit, and its outermost enclosing function. */
function unitRoots(u: Unit): Node[] {
	return u.enclosing[0] ? [u.fn, u.enclosing[0]] : [u.fn];
}

interface Resolved {
	src: AstSource;
	units: Unit[];
	rowFor: Map<Unit, Row>;
	/** Units without exactly one row, rows without exactly one unit, untabled deferrals. */
	population: string[];
}

function resolve(code: string, extraNested: SignedRow[]): Resolved | string[] {
	let src: AstSource;
	try {
		src = parseComponent(code);
	} catch (e) {
		return [`does not parse: ${String(e)}`];
	}
	const out: string[] = [];
	const { units, deferringCalls } = enumerateUnits(src);
	const claim = (rows: SignedRow[], members: Unit[], what: string, callText: (u: Unit) => string) => {
		const hits = new Map<SignedRow, Unit[]>();
		for (const u of members) {
			const body = collapse(src.text(u.fn));
			const matched = rows.filter(
				(r) =>
					r.body.test(body) &&
					(!r.call || r.call.test(callText(u))) &&
					(r.in === undefined || r.in === contextOf(src, u)) &&
					(r.code === undefined || r.code === codeOf(src, u.fn))
			);
			if (matched.length !== 1) {
				out.push(`${what} ${unitLabel(src, u)} matches ${matched.length} table rows — disposition it: ${body.slice(0, 80)}`);
				continue;
			}
			hits.set(matched[0]!, [...(hits.get(matched[0]!) ?? []), u]);
		}
		for (const r of rows) {
			const n = hits.get(r)?.length ?? 0;
			if (n !== 1) out.push(`${what} row (${r.why}) matches ${n} units — it must match exactly one`);
		}
		return hits;
	};

	const rowFor = new Map<Unit, Row>();
	const topLevel = units.filter((u) => u.kind === 'async-function' && u.name);
	const names = topLevel.map((u) => u.name!).sort();
	const expected = Object.keys(ASYNC_FUNCTIONS).sort();
	for (const n of names) if (!(n in ASYNC_FUNCTIONS)) out.push(`async function ${n}() is not in the table — disposition it`);
	for (const n of expected) if (!names.includes(n)) out.push(`the table names ${n}(), which is gone`);
	for (const u of topLevel) {
		const row = ASYNC_FUNCTIONS[u.name!];
		if (row) rowFor.set(u, row);
	}
	const callText = (u: Unit) => collapse(code.slice(u.call!.start, u.fn.start));
	for (const [row, us] of claim([...NESTED, ...extraNested], units.filter((u) => u.kind === 'async-function' && !u.name && !u.inMarkup), 'nested async function', callText)) {
		for (const u of us) rowFor.set(u, row);
	}
	for (const [row, us] of claim(MARKUP, units.filter((u) => u.kind === 'async-function' && u.inMarkup), 'markup async function', callText)) {
		for (const u of us) rowFor.set(u, row);
	}
	for (const u of units.filter((x) => x.kind === 'continuation' && x.inMarkup)) {
		out.push(`the markup defers a continuation at line ${u.line} — move it into a script handler the table covers`);
	}
	for (const [row, us] of claim(CONTINUATIONS, units.filter((u) => u.kind === 'continuation' && !u.inMarkup), 'continuation', callText)) {
		for (const u of us) rowFor.set(u, row);
	}
	const idCalls = deferringCalls.filter((c) => !c.arguments.some((a: Node) => a.type.endsWith('FunctionExpression')));
	for (const row of IDENTIFIER_CALLBACKS) {
		const n = idCalls.filter((c) => src.text(c).replace(/\s+/g, '') === row.text.replace(/\s+/g, '')).length;
		if (n !== row.count) out.push(`${row.text} occurs ${n} times, the table says ${row.count}`);
	}
	for (const c of idCalls) {
		const t = src.text(c).replace(/\s+/g, '');
		if (!IDENTIFIER_CALLBACKS.some((r) => r.text.replace(/\s+/g, '') === t)) out.push(`deferred call with a non-literal callback at line ${src.line(c.start)} is not tabled: ${t.slice(0, 80)}`);
	}
	return { src, units, rowFor, population: out };
}

export interface GateOptions {
	/** NESTED rows added by a fixture whose edit comes with the row the gate asks for. */
	extraNested?: SignedRow[];
	/** Hashes to treat as reviewed as well: the control that a bump is all an edit costs. */
	alsoReviewed?: ReadonlySet<string>;
	/** HELPERS rows added by a fixture, by name. */
	extraHelpers?: Readonly<Record<string, string>>;
}

/**
 * THE GATE (lead ruling on BUG-3084 checkpoint 69). Everything it refuses
 * about `code`, as readable lines; empty means clean. Every unit matches one
 * row, every row and every helper those rows reach carries the hash of the
 * code it was last reviewed on, and any difference refuses. The analysis
 * does not decide here: `analysisReport` is what the re-reader reads.
 */
export function refusals(code: string, opts: GateOptions = {}): string[] {
	const r = resolve(code, opts.extraNested ?? []);
	if (Array.isArray(r)) return r;
	const { src, units, rowFor } = r;
	const out = [...r.population];
	const tl = topLevelOf(src);
	const reviewed = (want: string | undefined, now: string) => want === now || !!opts.alsoReviewed?.has(now);
	const reach = new Set<string>();
	const pending: string[] = [];
	const pull = (helpers: Set<string>) => {
		for (const h of helpers) {
			if (!reach.has(h)) {
				reach.add(h);
				pending.push(h);
			}
		}
	};
	for (const u of units) {
		const row = rowFor.get(u);
		if (!row) continue;
		const roots = unitRoots(u);
		const cov = coverage(src, tl, roots);
		pull(cov.helpers);
		const now = sha(cov.text);
		if (row.reviewed === undefined) out.push(`${unitLabel(src, u)}: its row (${row.why}) has no reviewed hash (code ${now})`);
		else if (!reviewed(row.reviewed, now)) {
			out.push(`${unitLabel(src, u)} has changed since its row was reviewed (row: ${row.why}; code ${now}; covers ${roots.map((f) => fnName(src, f)).join(', ')}) — re-read the row, then update reviewed`);
		}
	}
	// Transitive: a helper that names another component-level function pulls
	// that one in too (refinement 1).
	while (pending.length) {
		const h = pending.pop()!;
		pull(coverage(src, tl, [tl.fns.get(h)!]).helpers);
	}
	const helpers: Record<string, string> = { ...HELPERS, ...opts.extraHelpers };
	for (const h of [...reach].sort()) {
		const now = sha(coverage(src, tl, [tl.fns.get(h)!]).text);
		if (!(h in helpers)) out.push(`helper ${h}() is reachable from a tabled unit but not in HELPERS — add it (code ${now})`);
		else if (!reviewed(helpers[h], now)) out.push(`helper ${h}() has changed since it was reviewed (code ${now}) — re-read it, then update HELPERS`);
	}
	for (const h of Object.keys(helpers)) if (!reach.has(h)) out.push(`HELPERS names ${h}(), which no tabled unit reaches`);
	return out;
}

/**
 * THE AID: what the flow analysis in `src/test/identityFenceAst.ts` reports
 * about `code`, for the person re-reading a row before bumping its hash.
 * It keeps its round 4–7 regression fixtures, and it must stay quiet on the
 * committed component so a re-reader starts from an empty report; it no
 * longer decides whether an edit is accepted.
 */
export function analysisReport(code: string, extraNested: SignedRow[] = []): string[] {
	const r = resolve(code, extraNested);
	if (Array.isArray(r)) return r;
	const { src, units, rowFor } = r;
	const out = [...r.population];
	let decls: ReturnType<typeof declarations>;
	try {
		decls = declarations(src);
	} catch (e) {
		return [...out, `declarations: ${String((e as Error).message ?? e)}`];
	}
	out.push(...refusedConstructs(src, decls));
	for (const u of units) {
		const row = rowFor.get(u);
		if (!row) continue;
		if (row.startSafe) {
			const why = row.pin ? row.pin(src, u) : 'a row that starts safe has no pin';
			if (why) out.push(`${unitLabel(src, u)} starts safe, but ${why}`);
		}
		try {
			const v = analyseUnit(src, decls, u, {
				startSafe: asyncUnitStartsSafe(decls, u, units) || !!row.startSafe,
				may: new Set(row.may ?? []),
				callbacks: new Map(Object.entries(row.callbacks ?? {}).map(([k, v]) => [k, new Set(v.may)])),
				bareAwaits: new Set(row.bareAwaits ?? []),
			});
			for (const x of v) out.push(`${unitLabel(src, u)} line ${x.line}: ${x.what} — ${x.text}`);
		} catch (e) {
			out.push(`${unitLabel(src, u)}: ${String((e as Error).message ?? e)}`);
		}
	}
	return out;
}

describe('ItemDetail: every async unit is tabled, and none commits past an unfenced await (AST)', () => {
	it('the component as written passes the gate', () => {
		expect(refusals(SOURCE)).toEqual([]);
	});

	it('the aid is quiet on the component as written, so a re-reader starts from an empty report', () => {
		expect(analysisReport(SOURCE)).toEqual([]);
	});

	it('every row carries the hash it was reviewed on, and the helper population is not empty', () => {
		for (const r of [...Object.values(ASYNC_FUNCTIONS), ...NESTED, ...MARKUP, ...CONTINUATIONS]) {
			expect(r.reviewed, `row (${r.why}) has no reviewed hash`).toMatch(/^[0-9a-f]{12}$/);
		}
		// The fences every unit leans on are inlined helpers, so they are hashed.
		for (const h of ['identityHeld', 'switchedAway', 'captureIdentity', 'showSaved']) expect(HELPERS[h], `${h} is not in HELPERS`).toBeDefined();
	});

	it('a row that may commit or starts safe is pinned to its place, and a row that may commit to its code', () => {
		for (const r of [...NESTED, ...MARKUP, ...CONTINUATIONS]) {
			if (r.may || r.startSafe) expect(r.in, `row (${r.why}) has no \`in\``).toBeDefined();
			if (r.may) expect(r.code, `row (${r.why}) has no \`code\``).toBeDefined();
		}
	});

	it('the analysis is not vacuous: it finds units of every kind, and it reads the fences it relies on', () => {
		const src = parseComponent(SOURCE);
		const { units } = enumerateUnits(src);
		expect(units.filter((u) => u.kind === 'async-function' && u.name).length).toBe(Object.keys(ASYNC_FUNCTIONS).length);
		expect(units.filter((u) => u.kind === 'continuation').length).toBe(CONTINUATIONS.length);
		const decls = declarations(src);
		for (const helper of ['switchedAway', 'stillCurrent', 'stillOnSource', 'isForegroundCurrent']) {
			expect(decls.fences.has(helper), `${helper} is no longer read as a fence`).toBe(true);
		}
		for (const capture of ['gen', 'myGen', 'callbackGen', 'genAtFlush', 'identity']) {
			expect(decls.captures.has(capture), `${capture} is no longer read as a capture`).toBe(true);
		}
	});
});

/** The sync handler's list lookup, with its element renamed to `item`. */
const SYNC_FIND_AS_ITEM: [string, string] = [
	'\t\t\t\tconst updated = result.changes.updated.find(i => i.id === reqItemId);\n',
	'\t\t\t\tconst updated = result.changes.updated.find((item) => item.id === reqItemId);\n',
];
/** The sync handler's full-refresh fallback: fetch, fence, adopt. */
const SYNC_REFRESH_FENCE = '\t\t\t\tif (!item || item.id !== reqItemId || myItemGen !== itemGen) return;\n';
const SYNC_REFRESH_FENCE_AND_ADOPT = SYNC_REFRESH_FENCE + '\t\t\t\titem = adoptServerItem(updated);\n';
const SYNC_REFRESH_FENCED = '\t\t\t\tconst updated = await api.items.get(reqWsSlug, reqItemSlug);\n' + SYNC_REFRESH_FENCE_AND_ADOPT;

/** loadData's unconditional debounce clear, which the debounce row's pin relies on. */
const LOAD_DATA_CLEAR = '\t\tclearTimeout(contentDebounceTimer);\n\t\tcontentDebounceTimer = undefined;\n\t\tcollabFlusher.cancel();\n';

/** saveTitle's capture, made reassignable. */
const TITLE_GEN_LET: [string, string] = [
	"\t\tconst gen = loadGeneration;\n\t\tsaveStatus = 'saving';\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
	"\t\tlet gen = loadGeneration;\n\t\tsaveStatus = 'saving';\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
];

/** saveTitle records an identity stamp at entry, before its await. */
const TITLE_STAMP_AT_ENTRY: [string, string] = [
	"\t\tconst gen = loadGeneration;\n\t\tsaveStatus = 'saving';\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
	"\t\tconst gen = loadGeneration;\n\t\tconst snap = { epoch: captureIdentity() };\n\t\tsaveStatus = 'saving';\n\t\ttry {\n\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title",
];

/** saveTitle's fence and the two commits under it: the anchor most round-5 edits rewrite. */
const TITLE_FENCE_AND_COMMITS =
	"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n";

describe('ItemDetail AST guard: round 4\'s edits are all refused (lead ruling, condition 1)', () => {
	// Without a clean baseline every mutant is "refused" for free.
	const BASELINE = refusals(SOURCE);

	it.each(ROUND4_MUTANTS.map((m) => [m.id, m] as const))('%s is refused', (_id, m) => {
		expect(BASELINE, 'the unmutated component is refused, so no mutant result means anything').toEqual([]);
		expect(SOURCE.split(m.old).length - 1, `${m.id}'s anchor is not in the component exactly once — re-point the fixture`).toBe(1);
		const mutated = SOURCE.replace(m.old, m.new);
		expect(refusals(mutated), `${m.id} (${m.finding}: ${m.what}) passed the guard`).not.toEqual([]);
	});

	/**
	 * Defects found in the ANALYSIS itself by later review rounds. Each is an
	 * edit the analysis accepted when it should not have. `refuses` is the
	 * refusal the edit must produce — every string in one line — so an entry
	 * cannot pass because some OTHER part of the edit happens to be refused
	 * (round 5's E5 was refused only through an inlined helper's call, while
	 * the `item =` write it was about went unreported).
	 */
	const ANALYSIS_DEFECTS: Array<{ id: string; subs: Array<[string, string]>; refuses: string[] }> = [
		{
			// Round 5 on #1387: `?:` was walked as `consequent && alternate`, so an
			// unsafe consequent short-circuited the walk and the alternate's commit
			// was never seen.
			id: 'R5-1 a commit in the alternate of a conditional whose consequent awaits',
			subs: [[
				"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n",
				"{ title: titleDraft.trim() });\n\t\t\tupdated ? await tick() : (item = withInflightTags(updated));\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n",
			]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		// Round 5 P1-1: a fence to the LEFT of an await in one test expression was
		// read as holding after that await.
		{
			id: 'R5 E1 a fence before an await in one && test',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen === loadGeneration && (await dialogs.confirm('Keep the new title?'))) {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t\tshowSaved();\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E1b a fence boolean whose initialiser awaits after the fence',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst keep = gen === loadGeneration && (await dialogs.confirm('Keep the new title?'));\n\t\t\tif (!keep) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E1c the early-return form: fence, then an await, in one || test',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id || !(await dialogs.confirm('Keep the new title?'))) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		// Round 5 P1-2: a callback handed to anything but the deferring names was
		// walked as if it ran inside the call, and a function-valued property was
		// not walked at all.
		{
			id: 'R5 E6 a commit in the onRefetched property, which runs after the refetch await',
			subs: [["\t\t\t\t\tlastServerItem = latest;\n", "\t\t\t\t\tlastServerItem = latest;\n\t\t\t\t\titem = withInflightTags(latest);\n"]],
			refuses: ['updateField()', 'callback submitOrderedOCC({onRefetched})', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E7 updateField\'s open-children confirm callback loses its stillCurrent fence',
			subs: [["\t\t\t\t\t\tif (!stillCurrent()) throw new Error('switched away');\n", '']],
			refuses: ['updateField()', 'callback confirmOpenChildrenOrThrow(…)', 'calls submitOrderedOCC after an unfenced await'],
		},
		{
			id: 'R5 E7b handleMove\'s open-children confirm callback loses its stillOnSource fence',
			subs: [["\t\t\t\t\t\tif (!stillOnSource()) throw new Error('switched away');\n", '']],
			refuses: ['handleMove()', 'callback confirmOpenChildrenOrThrow(…)', 'after an unfenced await'],
		},
		{
			id: 'R5 E8 a commit inside a requestIdleCallback callback',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\trequestIdleCallback(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t\tshowSaved();\n\t\t\t});\n"]],
			refuses: ['saveTitle()', 'callback requestIdleCallback(…)', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E8b a commit inside an event listener',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\twindow.addEventListener('focus', () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t}, { once: true });\n"]],
			refuses: ['saveTitle()', 'callback window.addEventListener(…)', 'assigns item after an unfenced await'],
		},
		{
			// A helper handed over by NAME runs as late as a literal would.
			id: 'R5 P1-2 a commit in a helper passed by name to an event listener',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst applyTitleLater = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\twindow.addEventListener('focus', applyTitleLater);\n"]],
			refuses: ['saveTitle()', 'callback window.addEventListener(…)', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P1-2 a commit in a helper passed by name as an object property',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst applyTitleLater = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\tdialogs.register({ onClose: applyTitleLater });\n"]],
			refuses: ['saveTitle()', 'callback dialogs.register({onClose})', 'assigns item after an unfenced await'],
		},
		{
			// A function value that escapes by any route runs whenever its holder
			// calls it.
			id: 'R5 P1-2 a helper stored by name into component state',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst applyTitleLater = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\trenameOverride = applyTitleLater;\n"]],
			refuses: ['saveTitle()', 'callback applyTitleLater', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P1-2 a function literal stored into component state',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\trenameOverride = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n"]],
			refuses: ['saveTitle()', 'callback renameOverride =', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P1-2 a function declared under a name used twice, passed by name',
			subs: [
				[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tconst onTitleClose = () => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t};\n\t\t\twindow.addEventListener('blur', onTitleClose);\n"],
				['\tfunction showSaved() {\n', '\tfunction showSaved() {\n\t\tconst onTitleClose = () => {};\n\t\tvoid onTitleClose;\n'],
			],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// A literal in a callee runs now; a literal in an ARGUMENT inside that
			// callee does not.
			id: 'R5 P1-2 a literal passed as an argument inside a called expression',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\t(dialogs.register(updated ? () => { item = withInflightTags(updated); } : null) ?? tick)();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// Only an empty Set/Map is an allowed constructor default (lead ruling on
			// checkpoint 63, condition 3).
			id: 'R6 a default value that constructs something with effects',
			subs: [['\tfunction showSaved() {\n', "\tfunction showSaved(stream = new TitleStream()) {\n\t\tvoid stream;\n"]],
			refuses: ['default value calls or assigns'],
		},
		{
			// A fence boolean stops being one when it is written (round 6 ruling B,
			// class 6); here the write, not an await, is what makes it stale.
			id: 'R6 a fence boolean overwritten after it is computed',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tlet titleOk = gen === loadGeneration;\n\t\t\ttitleOk = true;\n\t\t\tif (!titleOk) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// H12b with the operands the other way round.
			id: 'R6 loadGeneration compared against an identity capture',
			subs: [
				[TITLE_GEN_LET[0], TITLE_GEN_LET[0].replace('\t\tsaveStatus', '\t\tconst identity = captureIdentity();\n\t\tsaveStatus')],
				[TITLE_FENCE_AND_COMMITS, '{ title: titleDraft.trim() });\n\t\t\tif (loadGeneration !== identity) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n'],
			],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			// Round 6 class 9: the clear must be reached on every path to the
			// first await. An early return before it skips it on one path.
			id: 'R6 loadData can return before it clears the debounce',
			subs: [[LOAD_DATA_CLEAR, '\t\tif (!item) return;\n' + LOAD_DATA_CLEAR]],
			refuses: ['loadData no longer clears contentDebounceTimer before its first await'],
		},
		{
			id: 'R6 loadData awaits before it clears the debounce',
			subs: [[LOAD_DATA_CLEAR, '\t\tawait tick();\n' + LOAD_DATA_CLEAR]],
			refuses: ['loadData no longer clears contentDebounceTimer before its first await'],
		},
		{
			// Iteration methods count as synchronous only on a receiver the unit
			// declared; anything else could be an object whose `forEach` stores
			// the callback.
			id: 'R5 P1-2 an iteration-named method on a receiver the unit did not declare',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tsseService.forEach(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t});\n"]],
			refuses: ['saveTitle()', 'callback sseService.forEach(…)', 'assigns item after an unfenced await'],
		},
		{
			// A row's `may` covers the unit's own statements, not the callbacks it
			// creates: flushTagSaver may delete its registry entry, a listener it
			// registers may not.
			id: "R5 P1-2 a row's may does not reach a callback the unit creates",
			subs: [["\tasync function flushTagSaver(saver: TagSaver) {\n\t\tsaver.running = true;\n", "\tasync function flushTagSaver(saver: TagSaver) {\n\t\twindow.addEventListener('focus', () => tagSavers.delete(saver.itemId));\n\t\tsaver.running = true;\n"]],
			refuses: ['flushTagSaver()', 'callback window.addEventListener(…)', 'calls tagSavers.delete after an unfenced await'],
		},
		{
			// Round 5 P1-4: the debounce row starts safe because loadData clears
			// contentDebounceTimer; the pin must also prove this callback IS that
			// timer's.
			id: 'R5 E12 the debounce callback moves onto a timer loadData does not clear',
			subs: [['\t\tcontentDebounceTimer = setTimeout(() => {\n', '\t\tlegacyContentTimer = setTimeout(() => {\n']],
			refuses: ['starts safe, but its setTimeout is not assigned to contentDebounceTimer'],
		},
		// Round 5 P2-1..P2-3: loop and switch flow.
		{
			id: 'R5 E2 a commit in a for-await body',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tfor await (const t of api.items.titleStream(wsSlug, targetItem.id)) {\n\t\t\t\ttitleDraft = t;\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns titleDraft after an unfenced await'],
		},
		{
			id: 'R5 P2-1 a commit after a for-await loop',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tfor await (const t of api.items.titleStream(wsSlug, targetItem.id)) void t;\n\t\t\ttitleDraft = '';\n"]],
			refuses: ['saveTitle()', "assigns titleDraft after an unfenced await — titleDraft = ''"],
		},
		{
			id: 'R5 E3 a commit after a for-of over an awaited list whose body always returns',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tfor (const newer of await api.items.newerVersions(wsSlug, targetItem.id)) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 P2-2 a commit after a while whose test awaits and whose body always returns',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\twhile (await api.items.hasNewer(wsSlug, targetItem.id)) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		{
			id: 'R5 E4 a commit in the case whose test awaits',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tswitch (titleDraft) {\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\ttitleDraft = '';\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns titleDraft after an unfenced await'],
		},
		{
			id: 'R5 P2-3 a commit in the default case, reached only after an awaiting test',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + "\t\t\tswitch (titleDraft) {\n\t\t\t\tdefault:\n\t\t\t\t\ttitleDraft = '';\n\t\t\t\t\tbreak;\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\tbreak;\n\t\t\t}\n"]],
			refuses: ['saveTitle()', 'assigns titleDraft after an unfenced await'],
		},
		{
			id: 'R5 P2-3 a commit after a switch with no default whose test awaits',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tswitch (titleDraft) {\n\t\t\t\tcase await api.items.canonicalTitle(wsSlug):\n\t\t\t\t\treturn;\n\t\t\t}\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await'],
		},
		// Round 5 P2-4: an identity comparison between two STAMPS proves nothing.
		{
			id: 'R5 E9 flushTagSaver compares two stale identity stamps',
			subs: [['\t\t\t\tif (!identityHeld(saver.epoch)) {\n', '\t\t\t\tif (tagSavers.get(saver.itemId)!.epoch !== saver.epoch) {\n']],
			refuses: ['flushTagSaver()', 'after an unfenced await'],
		},
		{
			id: 'R5 P2-4 flushTagSaver compares two live identity reads',
			subs: [['\t\t\t\tif (!identityHeld(saver.epoch)) {\n', '\t\t\t\tif (captureIdentity() !== authStore.identityEpoch) {\n']],
			refuses: ['flushTagSaver()', 'after an unfenced await'],
		},
		{
			id: 'R5 P2-4 flushTagSaver compares a live identity read against something that is not a stamp',
			subs: [['\t\t\t\tif (!identityHeld(saver.epoch)) {\n', '\t\t\t\tif (authStore.identityEpoch !== saver.itemId.length) {\n']],
			refuses: ['flushTagSaver()', 'after an unfenced await'],
		},
		// Round 5 P2-5: a capture refreshed after an await makes every later check
		// against it pass, whatever the refresh is spelled as.
		{
			id: 'R5 E10 saveTitle refreshes its capture through a cast',
			subs: [TITLE_GEN_LET, ['{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration', '{ title: titleDraft.trim() });\n\t\t\tgen = loadGeneration as number;\n\t\t\tif (gen !== loadGeneration']],
			refuses: ['saveTitle()', 'rewrites capture gen after an unfenced await'],
		},
		{
			id: 'R5 E10b saveTitle refreshes its capture through ??',
			subs: [TITLE_GEN_LET, ['{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration', '{ title: titleDraft.trim() });\n\t\t\tgen = loadGeneration ?? gen;\n\t\t\tif (gen !== loadGeneration']],
			refuses: ['saveTitle()', 'rewrites capture gen after an unfenced await'],
		},
		{
			id: 'R5 P2-5 saveTitle bumps its capture',
			subs: [TITLE_GEN_LET, ['{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration', '{ title: titleDraft.trim() });\n\t\t\tgen++;\n\t\t\tif (gen !== loadGeneration']],
			refuses: ['saveTitle()', 'rewrites capture gen after an unfenced await'],
		},
		// Round 5 P2-6: every param of every function in or around the unit was a
		// capture, so any of them compared to loadGeneration was a fence.
		{
			id: "R5 E11 updateField compares the onRefetched param to loadGeneration",
			subs: [['\t\t\tconst fresh = await submitWithOCC(false);\n\t\t\tif (!stillCurrent()) return;\n', '\t\t\tconst fresh = await submitWithOCC(false);\n\t\t\tif (latest !== loadGeneration) return;\n']],
			refuses: ['updateField()', 'assigns item after an unfenced await'],
		},
		// Round 5 P2-7 / P2-8: a row's allowance covered more than its reason, and
		// a row could re-point by a move.
		{
			id: 'R5 E13 the rename heal clears the override without its bridge compare',
			subs: [['\t\t\t\tif (renameOverride === bridge) renameOverride = null;\n', '\t\t\t\trenameOverride = null;\n']],
			refuses: ['matches 0 table rows'],
		},
		{
			id: "R5 P2-8 the rename heal's catch moves onto the SSE rename navigation",
			subs: [
				[
					"\t\t\t\tnoScroll: true,\n\t\t\t}).catch(() => {\n\t\t\t\t// A failed/cancelled navigation must not leave the override\n\t\t\t\t// bridging to a URL we never reached. Identity compare (this\n\t\t\t\t// heal's own bridge object, round 4 P2) so a cancelled older\n\t\t\t\t// navigation can't clear a newer bridge to the same slug.\n\t\t\t\tif (renameOverride === bridge) renameOverride = null;\n\t\t\t});\n",
					'\t\t\t\tnoScroll: true,\n\t\t\t});\n',
				],
				[
					'\t\t\t\t\t\t\tnoScroll: true,\n\t\t\t\t\t\t});\n',
					'\t\t\t\t\t\t\tnoScroll: true,\n\t\t\t\t\t\t}).catch(() => {\n\t\t\t\t\t\t\tif (renameOverride === bridge) renameOverride = null;\n\t\t\t\t\t\t});\n',
				],
			],
			refuses: ['matches 0 table rows'],
		},
		{
			// A callback allowance covers what its reason covers: the refetch may
			// read, not commit.
			id: 'R5 P1-2 the refetch callback commits beyond its allowance',
			subs: [["\t\t\t\trefetch: () => api.items.get(targetWs, targetItem.id),\n", "\t\t\t\trefetch: () => ((saveStatus = 'saving'), api.items.get(targetWs, targetItem.id)),\n"]],
			refuses: ['updateField()', 'callback submitOrderedOCC({refetch})', 'assigns saveStatus after an unfenced await'],
		},
		// Round 5 P1-3: every binding anywhere in a unit, nested functions
		// included, made that name local for the whole unit.
		{
			id: 'R5 E5 a nested arrow param named item excuses an unfenced item write (adopting)',
			subs: [SYNC_FIND_AS_ITEM, [SYNC_REFRESH_FENCED, SYNC_REFRESH_FENCED.replace(SYNC_REFRESH_FENCE_AND_ADOPT, '\t\t\t\titem = adoptServerItem(updated);\n' + SYNC_REFRESH_FENCE)]],
			refuses: ['assigns item after an unfenced await — item = adoptServerItem(updated)'],
		},
		{
			id: 'R5 E5b a nested arrow param named item excuses an unfenced item write (verbatim)',
			subs: [SYNC_FIND_AS_ITEM, [SYNC_REFRESH_FENCED, SYNC_REFRESH_FENCED.replace(SYNC_REFRESH_FENCE_AND_ADOPT, '\t\t\t\titem = updated;\n' + SYNC_REFRESH_FENCE)]],
			refuses: ['assigns item after an unfenced await — item = updated'],
		},
		{
			// A block's bindings hold only inside that block: a `const item` in
			// one branch excuses no `item =` outside it.
			id: 'R5 P1-3 a block-scoped declaration excuses a write outside its block',
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (updated) {\n\t\t\t\tconst item = updated;\n\t\t\t\tvoid item;\n\t\t\t}\n\t\t\titem = withInflightTags(updated);\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved();\n"]],
			refuses: ['saveTitle()', 'assigns item after an unfenced await — item = withInflightTags(updated)'],
		},
		{
			// An inlined helper resolves names in ITS scopes: a caller's local that
			// shares a name with the state the helper writes excuses nothing.
			id: "R5 P1-3 a caller's local does not excuse the same name inside an inlined helper",
			subs: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst saveStatus = updated.title;\n\t\t\tshowSaved();\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n"]],
			refuses: ['saveTitle()', 'showSaved() -> assigns saveStatus after an unfenced await'],
		},
	];
	it('a callback allowance that names no callback the unit creates is refused', () => {
		const src = parseComponent(SOURCE);
		const unit = enumerateUnits(src).units.find((u) => u.name === 'updateField')!;
		expect(() =>
			analyseUnit(src, declarations(src), unit, { startSafe: true, callbacks: new Map([['submitOrderedOCC({sendd})', new Set(['api.items.update'])]]) })
		).toThrow(/names no callback this unit creates/);
	});

	/**
	 * Round 6 on #1387: edits the guard accepted at 31f3b5e9, from the
	 * reviewer's probe (checkpoint 63), each tagged with its class. Every one
	 * is either closed here or listed in KNOWN_GAPS below.
	 */
	const ROUND6 = (round6.mutants as Array<{ id: string; class: number; subs: string[][]; refuses: string[]; gate?: boolean }>).map((m) => ({
		id: `${m.id} (class ${m.class})`,
		subs: m.subs as Array<[string, string]>,
		refuses: m.refuses,
		// Class 8 was the first hash; its refusal is the gate's now.
		gate: m.gate,
	}));

	/**
	 * Round 7 on #1387 (checkpoint 67): edits the guard accepted at 6e1ba4af.
	 * An F1 edit arrives with the NESTED row the guard asks for, since without
	 * the row it is refused for being untabled rather than for what it commits.
	 */
	interface Round7Nested {
		body: string;
		why: string;
		callbacks?: Record<string, { may: string[]; why: string }>;
	}
	const ROUND7 = (round7.mutants as Array<{ id: string; class: string; subs: string[][]; nested?: Round7Nested[]; refuses: string[] }>).map((m) => ({
		id: `${m.id} (class ${m.class})`,
		subs: m.subs as Array<[string, string]>,
		nested: (m.nested ?? []).map((r): SignedRow => ({ ...r, body: new RegExp(r.body) })),
		refuses: m.refuses,
	}));

	const applySubs = (subs: Array<[string, string]>) => {
		let code = SOURCE;
		for (const [from, to] of subs) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		return code;
	};

	const DEFECTS: Array<{ id: string; subs: Array<[string, string]>; nested?: SignedRow[]; refuses: string[]; gate?: boolean }> = [...ANALYSIS_DEFECTS, ...ROUND6, ...ROUND7];
	it.each(DEFECTS.map((d) => [d.id, d] as const))('analysis defect stays closed: %s', (_id, d) => {
		expect(BASELINE).toEqual([]);
		const code = applySubs(d.subs);
		// The gate refuses every one of these: each edits hashed code.
		expect(refusals(code, { extraNested: d.nested }), 'the gate accepted an edit to a fenced unit').not.toEqual([]);
		// The aid still reports it, which is what these fixtures regress.
		const got = d.gate ? refusals(code) : analysisReport(code, d.nested ?? []);
		expect(
			got.some((line) => d.refuses.every((s) => line.includes(s))),
			`no refusal carries ${JSON.stringify(d.refuses)}; got ${JSON.stringify(got, null, 1)}`
		).toBe(true);
	});

	/**
	 * AID GAPS (checkpoint 63 condition 4; checkpoint 69 condition 4): edits
	 * the ANALYSIS accepts although they reintroduce a stale commit, each with
	 * the sentence that says why. Each is listed in the "does not see" part of
	 * `identityFenceAst.ts`'s header. The gate refuses every one of them by a
	 * hash mismatch. When the aid starts reporting one, move it to
	 * ANALYSIS_DEFECTS.
	 */
	const KNOWN_GAPS: Array<{ id: string; model: string; subs: Array<[string, string]> }> = [
		{
			id: 'a non-array object aliased to a local, whose forEach defers its callback',
			model:
				'trusted synchronous callee: iteration methods on a local receiver are taken to be array methods; binding a service to a local to call a deferring method named forEach is evasion, not an ordinary edit',
			subs: [[TITLE_FENCE_AND_COMMITS, TITLE_FENCE_AND_COMMITS + '\t\t\tconst later = sseService;\n\t\t\tlater.forEach(() => {\n\t\t\t\titem = withInflightTags(updated);\n\t\t\t});\n']],
		},
		{
			id: 'a stamp minted after the await and compared at once',
			model:
				'contrived fence: a stamp is trusted to have been recorded earlier; building one from a live read right before comparing it to a live read is a fence that cannot fail, which no author trying to comply writes',
			subs: [[TITLE_FENCE_AND_COMMITS, '{ title: titleDraft.trim() });\n\t\t\tconst fresh = { epoch: captureIdentity() };\n\t\t\tif (authStore.identityEpoch !== fresh.epoch) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n']],
		},
		// Round 7 (checkpoint 67): the reviewer's out-of-model findings.
		...(round7.gaps as Array<{ id: string; class: string; model: string; subs: string[][] }>).map((g) => ({
			id: `${g.id} (class ${g.class})`,
			model: g.model,
			subs: g.subs as Array<[string, string]>,
		})),
		// Round 8 (checkpoint 69): recorded, not fixed, under the lead's ruling.
		...(round8.aidGaps as Array<{ id: string; model: string; subs: string[][] }>).map((g) => ({
			id: `R8 ${g.id}`,
			model: g.model,
			subs: g.subs as Array<[string, string]>,
		})),
	];
	it.each(KNOWN_GAPS.map((g) => [g.id, g] as const))('aid gap, which the gate refuses: %s', (_id, g) => {
		expect(BASELINE).toEqual([]);
		const code = applySubs(g.subs);
		expect(analysisReport(code), 'the aid now reports this: move it to ANALYSIS_DEFECTS').toEqual([]);
		expect(
			refusals(code).some((l) => l.includes('has changed since')),
			'the gate does not refuse this aid gap by a hash mismatch'
		).toBe(true);
	});

	/**
	 * Controls for the AID: an analysis that reports everything also reports
	 * every mutant. These edits commit nothing, and the aid must stay quiet on
	 * them. (The gate refuses them all, since each edits a hashed function; its
	 * own controls are at the end of this file.)
	 */
	const ACCEPTED: Array<{ id: string; old: string; new: string; also?: Array<[string, string]> }> = [
		{
			id: 'a comment after an await, before the fence',
			old: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n",
			new: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n\t\t\t// a note\n",
		},
		{
			id: 'a pure local computed after an await, before the fence',
			old: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n",
			new: "\t\t\tconst updated = await api.items.update(wsSlug, targetItem.id, { title: titleDraft.trim() });\n\t\t\tconst snapshot = JSON.stringify(updated);\n",
		},
		{
			// A helper's own params are captures inside its own body, whatever
			// they are called: switchedAway stays a fence with its param renamed.
			id: "switchedAway's generation param renamed",
			old: '\tfunction switchedAway(targetItem: Item, gen: number): boolean {\n\t\treturn gen !== loadGeneration || item?.id !== targetItem.id;\n',
			new: '\tfunction switchedAway(targetItem: Item, expected: number): boolean {\n\t\treturn expected !== loadGeneration || item?.id !== targetItem.id;\n',
		},
		{
			// A catch param is a local of the handler, even though the handler is
			// entered after an await.
			id: 'a write to the catch param in an unfenced handler',
			old: "\t\t} catch {\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tsaveStatus = 'idle';\n\t\t\ttoastStore.show('Failed to update title'",
			new: "\t\t} catch (err) {\n\t\t\terr = null;\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tsaveStatus = 'idle';\n\t\t\ttoastStore.show('Failed to update title'",
		},
		{
			// An inlined helper declared inside a block sees that block's names.
			id: "a helper writing a local of the block it is declared in",
			old: '{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration',
			new: '{ title: titleDraft.trim() });\n\t\t\tlet bumped = 0;\n\t\t\tconst bumpTitleCount = () => {\n\t\t\t\tbumped = 1;\n\t\t\t};\n\t\t\tbumpTitleCount();\n\t\t\tif (gen !== loadGeneration',
		},
		{
			// A loop head's names are locals of the loop, after an await too.
			id: 'a write to a loop-head binding after an await',
			old: TITLE_FENCE_AND_COMMITS,
			new: TITLE_FENCE_AND_COMMITS + '\t\t\tfor (let attempt = 0; attempt < 1; attempt++) {\n\t\t\t\tawait tick();\n\t\t\t\tattempt = 2;\n\t\t\t}\n',
		},
		{
			id: 'a commit moved BELOW its fence stays accepted',
			old: "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n",
			new: "{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved();\n\t\t\titem = withInflightTags(updated);\n",
		},
		{
			// A stamp rooted in a local of the unit is a stamp inside a fence
			// boolean's initialiser too (round 7 F4: the initialiser is read in its
			// own scope).
			id: 'a fence boolean over a stamp the unit recorded before its await',
			old: TITLE_STAMP_AT_ENTRY[0],
			new: TITLE_STAMP_AT_ENTRY[1],
			also: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tconst held = authStore.identityEpoch === snap.epoch;\n\t\t\tif (!held || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
		},
		{
			// ...and inside a helper's body, read in the helper's scope.
			id: 'a fence helper over a stamp the unit recorded before its await',
			old: TITLE_STAMP_AT_ENTRY[0],
			new: TITLE_STAMP_AT_ENTRY[1].replace('\t\tsaveStatus', '\t\tconst snapHeld = () => authStore.identityEpoch === snap.epoch;\n\t\tsaveStatus'),
			also: [[TITLE_FENCE_AND_COMMITS, "{ title: titleDraft.trim() });\n\t\t\tif (!snapHeld() || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"]],
		},
	];
	it.each(ACCEPTED.map((c) => [c.id, c] as const))('control accepted: %s', (_id, c) => {
		expect(BASELINE).toEqual([]);
		expect(analysisReport(applySubs([[c.old, c.new], ...(c.also ?? [])]))).toEqual([]);
	});
});

describe('ItemDetail identity gate: a fenced unit cannot change without a re-read (lead ruling on checkpoint 69)', () => {
	const GATE_BASELINE = refusals(SOURCE);
	const changedHashes = (lines: string[]) => new Set(lines.flatMap((l) => (l.includes('has changed since') ? [l.match(/code ([0-9a-f]{12})/)![1]!] : [])));

	const ROUND8 = round8.gate as Array<{ id: string; subs: string[][]; refuses: string[] }>;
	it.each(ROUND8.map((g) => [g.id, g] as const))('round 8 edit refused by a hash mismatch: %s', (_id, g) => {
		expect(GATE_BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of g.subs as Array<[string, string]>) {
			expect(code.split(from).length - 1, `anchor ${JSON.stringify(from.slice(0, 60))} is not in the component exactly once`).toBe(1);
			code = code.replace(from, to);
		}
		const got = refusals(code);
		expect(got.some((l) => g.refuses.every((s) => l.includes(s))), `no refusal carries ${JSON.stringify(g.refuses)}; got ${JSON.stringify(got, null, 1)}`).toBe(true);
	});

	/**
	 * GATE GAPS: edits the gate accepts, each outside what it hashes, with the
	 * sentence that says why. When one starts being refused, it has closed.
	 */
	const GATE_GAPS = round8.gateGaps as Array<{ id: string; model: string; subs: string[][] }>;
	it.each(GATE_GAPS.map((g) => [g.id, g] as const))('gate gap, still outside what is hashed: %s', (_id, g) => {
		expect(GATE_BASELINE).toEqual([]);
		let code = SOURCE;
		for (const [from, to] of g.subs as Array<[string, string]>) {
			expect(code.split(from).length - 1).toBe(1);
			code = code.replace(from, to);
		}
		expect(refusals(code), 'this gap is now refused: move it to the round 8 gate fixtures').toEqual([]);
	});

	it('CONTROL: an edit outside every hashed function is accepted', () => {
		const anchor = '<script lang="ts">\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		expect(refusals(SOURCE.replace(anchor, anchor + '\t// a note outside every fenced unit\n'))).toEqual([]);
	});

	it('CONTROL: an edit to a fenced unit costs exactly one hash bump', () => {
		const code = SOURCE.replace(
			TITLE_FENCE_AND_COMMITS,
			"{ title: titleDraft.trim() });\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\tshowSaved();\n\t\t\titem = withInflightTags(updated);\n"
		);
		const first = refusals(code);
		expect(first).toHaveLength(1);
		expect(first[0]).toContain('saveTitle() has changed since its row was reviewed');
		expect(refusals(code, { alsoReviewed: changedHashes(first) })).toEqual([]);
	});

	/** Reviews `base` as a fixture: its changed hashes and new helpers become accepted. */
	const reviewAs = (base: string): GateOptions => {
		const lines = refusals(base);
		const extraHelpers = Object.fromEntries(
			lines.flatMap((l) => {
				const m = l.match(/^helper (\w+)\(\) is reachable .*\(code ([0-9a-f]{12})\)$/);
				return m ? [[m[1]!, m[2]!]] : [];
			})
		);
		const opts = { alsoReviewed: changedHashes(lines), extraHelpers };
		expect(refusals(base, opts), 'the base does not pass once reviewed').toEqual([]);
		return opts;
	};

	it('a component-level declaration a unit reads is part of that unit\'s hash (refinement 2)', () => {
		const decl = '\tlet loadGeneration = 0;\n';
		expect(SOURCE.split(decl).length - 1).toBe(1);
		const got = refusals(SOURCE.replace(decl, '\tlet loadGeneration = $state(0);\n'));
		expect(got.some((l) => l.includes('saveTitle() has changed since its row was reviewed'))).toBe(true);
	});

	it('a helper reached only through another helper is hashed too (refinement 1)', () => {
		// switchedAway encloses no unit, so only a second hop reaches noteSaved.
		const anchor = '\tfunction switchedAway(targetItem: Item, gen: number): boolean {\n';
		expect(SOURCE.split(anchor).length - 1).toBe(1);
		const base = SOURCE.replace(anchor, '\tfunction noteSaved() {\n\t\tvoid 0;\n\t}\n\n' + anchor + '\t\tnoteSaved();\n');
		const opts = reviewAs(base);
		expect(opts.extraHelpers).toHaveProperty('noteSaved');
		const edited = base.replace('\tfunction noteSaved() {\n\t\tvoid 0;\n', "\tfunction noteSaved() {\n\t\titem = null;\n");
		expect(refusals(edited, opts).some((l) => l.includes('helper noteSaved() has changed since it was reviewed'))).toBe(true);
	});

	it('a function value rebound outside every unit changes the hash of the unit that calls it (refinement 2)', () => {
		// Step 1: a reviewed base where saveTitle calls a no-op `let` helper.
		const noop = '\tlet applyTitle = (next: Item): void => {\n\t\tvoid next;\n\t};\n\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\tvoid next;\n\t\t};\n\t});\n\n\tfunction showSaved() {\n';
		const base = SOURCE.replace('\tfunction showSaved() {\n', noop).replace(
			TITLE_FENCE_AND_COMMITS,
			"{ title: titleDraft.trim() });\n\t\t\tapplyTitle(updated);\n\t\t\tif (gen !== loadGeneration || item?.id !== targetItem.id) return;\n\t\t\titem = withInflightTags(updated);\n\t\t\tshowSaved();\n"
		);
		const opts = reviewAs(base);
		// Step 2: only the rebinding inside the $effect changes.
		const rebind = '\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\tvoid next;\n';
		expect(base.split(rebind).length - 1).toBe(1);
		const edited = base.replace(rebind, '\t$effect(() => {\n\t\tapplyTitle = (next: Item) => {\n\t\t\titem = withInflightTags(next);\n');
		expect(refusals(edited, opts).some((l) => l.includes('saveTitle() has changed since its row was reviewed'))).toBe(true);
	});
});
