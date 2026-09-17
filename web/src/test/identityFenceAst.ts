/**
 * BUG-3084: the identity-fence guard for ItemDetail. Round 4 moved it from a
 * scanner to this AST (lead ruling on checkpoint 51). Rounds 3–8 then showed
 * that an analysis of what the code DOES does not converge: every review
 * round found edits it accepted, and round 8 found rows missing from the
 * population table itself (team CONVE-35). So the lead ruled on checkpoint 69:
 *
 * THE GATE IS A CHANGE DETECTOR, and it lives in
 * `itemDetailIdentityFenceAst.test.ts` (`refusals`). Every async unit the
 * POPULATION finds, and every deferring call it recognises, matches exactly
 * one table row. It does NOT say every deferred callback has a row: the
 * population is the spelling list in 1 below, so a deferral spelled some
 * other way — a listener registration, `requestIdleCallback` — has no row to
 * match and is not seen at all (round 9 F5). Every row carries the hash of
 * the code it was last reviewed on, and so does every component-level sync
 * function a row's code reaches by name, transitively (HELPERS). What a
 * row's hash covers:
 *   - the unit, its HEAD included — `async`, `*`, the type parameters, the
 *     params and the return type, each of which changes what a call DOES
 *     while leaving every statement of the body untouched (round 9 F1);
 *   - its outermost enclosing function;
 *   - component-level declarations of names the unit references, IMPORTS
 *     among them (round 9 F2);
 *   - component-level statements that REBIND a name it calls, or a
 *     component-level function it merely NAMES — a name it hands to someone
 *     else is a function it will run, just later (round 9 F4, and round 8 G
 *     for the callee half). Destructuring patterns and for-in/of heads count
 *     as rebindings (round 9 F3); a bare member write does not; any other
 *     target shape is REFUSED by line rather than read as binding nothing.
 * A deferring call whose callback is not a literal (`IDENTIFIER_CALLBACKS`)
 * carries its own hash over the statements that declare or rebind the names
 * it passes, because such a call site may sit outside every hashed unit —
 * both `.then(ensureGraphComp)` sites do (round 9 F4). The population's own
 * vocabulary (`DEFERRING_METHODS`, `DEFERRING_FUNCTIONS`,
 * `SYNC_CALLBACK_CALLEES`, all below) is hashed BY VALUE for the same
 * reason: nothing in the component covers it, and shrinking it would drop
 * units from the population with no row changing.
 * All of that is found by syntax, never by this analysis. ANY difference
 * refuses, naming the row. The claim is bounded and checkable: a fenced unit,
 * or anything it inlines, cannot change without someone re-reading its row.
 *
 * THE COST, stated plainly: an edit to a fenced unit or a helper it reaches
 * costs one hash bump, and the bump IS the act of re-reading the row. The
 * refusal prints the new hash. A comment INSIDE a statement of one of those
 * functions costs a bump too, because the hash reads statement text; a
 * comment between statements does not. So does a SIGNATURE change with no
 * body change — a return type, a param, `async` — since round 9 F1.
 *
 * THIS FILE IS THE AID (`analysisReport`): the flow analysis a re-reader runs
 * before bumping a hash. It keeps its round 4–7 regression fixtures, and it
 * must stay quiet on the committed component. It no longer decides whether
 * an edit is accepted, and nothing below is a guarantee.
 *
 * WHAT THE AID DOES
 *
 * 1. POPULATION by node type. Every `async` function node anywhere in the
 *    script or the markup is a UNIT, and so is every callback passed to a
 *    deferring call (`.then` / `.catch` / `.finally`, `setTimeout`,
 *    `setInterval`, `queueMicrotask`, `requestAnimationFrame`). That list IS
 *    a spelling list, so it is not trusted to be complete: inside a unit,
 *    every other function literal passed to a call, and every function-valued
 *    object property, is a CALLBACK walked from an unsafe start, since it may
 *    run after any await (round 5 P1-2). An `async` literal is its own unit,
 *    and one defined inside a unit or a helper a unit inlines starts unsafe
 *    for the same reason (`asyncUnitStartsSafe`, round 7 F1); an async unit
 *    defined anywhere else starts safe. Only callees on
 *    `SYNC_CALLBACK_CALLEES`, and array iteration methods on a receiver the
 *    unit declared, run their callback inline with the caller's state. A
 *    callback commits nothing unless its unit's row names it.
 *
 * 2. FLOW. Each unit is walked in evaluation order carrying one boolean,
 *    `safe`: no await has completed on this path since the last fence. An
 *    `await` (and the start of a deferred callback) makes it false; passing a
 *    fence makes it true. Branches merge by AND; `return` / `throw` end a path.
 *    A `try` handler is entered unsafe if the try block awaits anything, since
 *    a rejection can come after any statement. Loops run their body twice, the
 *    second time from the merge of the entry and the first pass's end, and
 *    leave from the state after their test or iterator (plus every `break`),
 *    whatever the body does; `for await` starts every pass unsafe, and lapses
 *    fence booleans as an await does (round 7 F3). A `switch`
 *    evaluates its case tests in order, so a case is entered from the state
 *    after its own test and `default` from the state after all of them. A
 *    statement type the walker does not know THROWS — it is not skipped.
 *
 * 3. COMMITS fail closed. While unsafe, each of these is a violation:
 *    - an assignment or update whose target is not a local of the unit (any
 *      operator, any member or bracket target — a member write is always a
 *      commit, since a local can alias component state);
 *    - any call or `new` that is not a fence and not on `PURE_CALLS` below —
 *      including the call inside an `await`, which is issued BEFORE the
 *      suspension and so belongs to the previous await's window;
 *    - a CAPTURE of a generation or an identity (`= loadGeneration`,
 *      `++itemGen`, `captureIdentity()`): a capture taken after an unfenced
 *      await records the new load, and every later check against it passes;
 *      so does ANY later write to a capture's name, whatever its value.
 *    `delete`, tagged templates and dynamic `import()` are commits too.
 *
 * 4. FENCES by what they compare. An atom is a comparison of a
 *    CAPTURED variable against `loadGeneration` / `itemGen`, a call to
 *    `identityHeld(x)`, or a comparison of a LIVE identity read
 *    (`authStore.identityEpoch`, `captureIdentity()`) against a stamp. `!`,
 *    `&&` and `||` combine atoms with their polarity, and when the right
 *    operand awaits only its own atoms survive (round 5 P1-1). So
 *    `if (gen !== loadGeneration) return;` fences what follows and
 *    `if (gen === loadGeneration) { … }` fences only its consequent. A `?:`
 *    is not a fence at all (its arms are walked, its value is not read). A
 *    helper (`const stillCurrent = () => …`, `function switchedAway(…)`) is a
 *    fence exactly when its OWN body has a polarity, with its own params
 *    counted as captures there and nowhere else; a CALL to it fences only when
 *    each param that polarity needs receives a capture of the kind it
 *    compares, in that position, and its name must be declared once and never
 *    for a fence boolean too (round 7 F8). Naming a helper without calling it
 *    is never a fence (round 7 F2). A local boolean initialised from a fence
 *    is one too, until the next suspension or a write to it. Captures are
 *    typed: `generation` (from `loadGeneration` / `++itemGen`) or `identity`
 *    (from `captureIdentity()`); the type is recorded per NAME, so a name
 *    captured as both is refused (round 7 F5); only a snapshot declared inside
 *    a function is one. A stamp is `.epoch` / `.identityEpoch` read through
 *    plain names whose root is bound in the scopes around the check —
 *    component state is re-stamped by the identity change (round 7 F4).
 *
 * 5. SCOPES. Whether a write targets a local is decided by the lexical scopes
 *    enclosing the write: every function, block, loop head, switch body and
 *    catch clause around the unit, plus each one the walk enters. An inlined
 *    helper sees its own chain, not the caller's (round 5 P1-3). A `var` is
 *    treated as scoped to its block, which can only refuse more.
 *
 * 6. REFUSED OUTRIGHT, anywhere inside a function, because the walk does not
 *    model them (`refusedConstructs`): a declaration shadowing a trusted name
 *    (`trustedNames`) or a capture; a default value that calls or assigns
 *    (an empty `new Set()` / `new Map()` excepted); a generator; a tagged
 *    template; a for-of/in target that is not a declaration; a call whose
 *    callee builds a function in any shape but `(literal)(…)` or
 *    `(name OP literal)(…)`, where OP is any logical operator and `name` a
 *    plain name or member chain.
 *
 * WHAT THE AID DOES NOT SEE. Each item is trusted without being checked; the
 * test file's KNOWN_GAPS keeps each one executable.
 *
 * - It proves a check with the right SHAPE dominates every commit, not that
 *   the check reads the right item. A callback allowance covers its callee,
 *   not which item the call names (round 8 J).
 * - `identityHeld`, `captureIdentity` and `authStore.identityEpoch` are
 *   recognised BY NAME. The site pins in `itemDetailIdentityFence.test.ts`
 *   hold the two function bodies (round 8 E).
 * - Captures are known by name. A declaration or catch param shadowing one
 *   is refused. A function PARAM is not, because helpers such as
 *   `switchedAway` need the name, so a callback param named like a capture
 *   is read as that capture.
 * - A stamp rooted in a local is trusted to have been recorded earlier. That
 *   includes a local assigned from component state after the await
 *   (round 8 H), and a stamp minted after the await and compared at once.
 * - Fence booleans:
 *   - one read inside a helper's body never lapses;
 *   - one initialised from a fence boolean that has already lapsed is
 *     marked live anyway (round 8 D).
 * - Callbacks:
 *   - `SYNC_CALLBACK_CALLEES`, and array iteration methods on a receiver in
 *     scope, are trusted to call synchronously;
 *   - a row's `callbacks` allowance trusts the callee to re-check before
 *     calling, AND trusts the values the unit hands the callee for that
 *     re-check (round 8 C);
 *   - a helper already being inlined is not walked again as the callback it
 *     hands itself to (round 8 B).
 * - Helpers:
 *   - a helper is declared once, not bound once: a `let` rebound outside
 *     every unit is inlined with its declared body (round 8 G);
 *   - a row's `may` keys also excuse writes inside the helpers the unit
 *     inlines (round 8 F).
 * - A `startSafe` row trusts its pin's premises. The debounce pin does not
 *   prove the timer variable holds the only live handle (round 8 A). The
 *   mount suite holds A and C semantically.
 * - `var` is treated as block-scoped, which only ever refuses more.
 *
 * WHAT NEITHER SEES (the gate's own boundary, and its GATE_GAPS table):
 * - synchronous code no row reaches: markup handlers, callback props on
 *   child components (round 8 I), `$effect` / `onMount` / `onDestroy` bodies
 *   that enclose no unit, and component-level statements outside the hashed
 *   set. Dropping `destroyed = true` from the `onDestroy` body is accepted
 *   although `reconcileCollectionSegment` reads it (round 9 O1);
 * - deferrals the population does not recognise, because it recognises them
 *   by spelling: listener registrations (`authStore.onIdentityChange`,
 *   `addEventListener`, service subscriptions) and callees outside the lists
 *   in 1 — `requestIdleCallback(() => …)` inside an `$effect` that encloses
 *   no unit is accepted (round 9 F5). The site pins hold the identity
 *   listener;
 * - a write to a name a unit only READS. By design: the hash covers the
 *   names a unit CALLS or names as a function VALUE, not every name it
 *   touches, because the second would pull most of the component into every
 *   row;
 * - imported modules (`fieldWriteOrder.ts`, the auth store), which have
 *   their own tests;
 * - `<script module>`, because only the instance script is parsed.
 *
 * The site pins cover the named sites among these; the rest is covered only
 * by review.
 */
import { parse } from 'svelte/compiler';

// ---------------------------------------------------------------------------
// Minimal ESTree typing: every node has `type`, `start`, `end`.
// ---------------------------------------------------------------------------
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type Node = { type: string; start: number; end: number; [k: string]: any };

const SKIP_KEYS = new Set(['type', 'start', 'end', 'loc', 'range', 'metadata', 'parent', 'leadingComments', 'trailingComments']);

function children(node: Node): Node[] {
	const out: Node[] = [];
	for (const key of Object.keys(node)) {
		if (SKIP_KEYS.has(key)) continue;
		const v = node[key];
		if (Array.isArray(v)) {
			for (const c of v) if (c && typeof c === 'object' && typeof c.type === 'string') out.push(c);
		} else if (v && typeof v === 'object' && typeof v.type === 'string') {
			out.push(v);
		}
	}
	return out;
}

export function walk(node: Node, visit: (n: Node, ancestors: Node[]) => void, ancestors: Node[] = []): void {
	visit(node, ancestors);
	ancestors.push(node);
	for (const c of children(node)) walk(c, visit, ancestors);
	ancestors.pop();
}

const isFn = (n: Node | undefined | null): n is Node =>
	!!n && (n.type === 'ArrowFunctionExpression' || n.type === 'FunctionExpression' || n.type === 'FunctionDeclaration');

// ---------------------------------------------------------------------------
// Tables the analysis consults. Each entry carries its reason.
// ---------------------------------------------------------------------------

/** Counters `loadData` bumps. A fence compares a CAPTURE against one of these. */
export const GENERATIONS = new Set(['loadGeneration', 'itemGen']);

/** Calls that deliver their callback LATER: each callback is its own unit. */
export const DEFERRING_METHODS = new Set(['then', 'catch', 'finally']);
export const DEFERRING_FUNCTIONS = new Set(['setTimeout', 'setInterval', 'queueMicrotask', 'requestAnimationFrame']);

/**
 * Callees known to run a function-literal argument NOW, inside the call, so
 * the walk inlines it with the caller's state (round 5 P1-2). Every other
 * function literal passed to a call, and every function-valued object
 * property, is a CALLBACK that may run later: it is walked starting unsafe,
 * and what it may commit is the unit row's `callbacks` entry for it.
 */
export const SYNC_CALLBACK_CALLEES: Record<string, string> = {
	untrack: 'Svelte: calls its argument synchronously and returns its value',
};
/**
 * Array iteration methods run their callback synchronously — but only an
 * array's do, so they count only on a receiver the unit itself declared.
 */
export const SYNC_ITERATION_METHODS = new Set(['find', 'findIndex', 'findLast', 'filter', 'map', 'flatMap', 'some', 'every', 'forEach', 'reduce', 'sort']);

/**
 * Calls that commit nothing, by callee. SHORT on purpose (lead ruling,
 * condition 2): a new callee is a commit until it is listed here with its
 * reason, and a list that grows to absorb every refusal is the regex problem
 * again. Keys are the callee's source text with `?.` read as `.`.
 * There are no method-name wildcards: `*.get` once admitted `api.items.get`,
 * a request (round-4 mutant G6), so every entry names its receiver.
 */
export const PURE_CALLS: Record<string, string> = {
	untrack: 'Svelte: reads its callback without tracking; the callback is walked inline',
	tick: 'Svelte: resolves after the pending flush; schedules nothing of ours',
	captureIdentity: 'reads the identity epoch; as a CAPTURE it is checked by the capture rule, not here',
	'Date.now': 'reads the clock',
	'JSON.parse': 'pure',
	'JSON.stringify': 'pure',
	'Object.keys': 'pure read',
	'Array.isArray': 'pure read',
	String: 'pure conversion',
	Error: 'constructs a value',
	'console.warn': 'diagnostic output; no state',
	'console.error': 'diagnostic output; no state',
	'fieldWrites.superseded': 'read-only query on the write-order ticket (fieldWriteOrder.ts superseded() returns a boolean)',
	isOpenChildrenError: 'imported predicate over an error value',
	isUpdateConflictError: 'imported predicate over an error value',
};

// ---------------------------------------------------------------------------
// Source + parse
// ---------------------------------------------------------------------------

export interface AstSource {
	readonly code: string;
	readonly script: Node;
	/** Top-level expression-bearing markup nodes. */
	readonly fragment: Node;
	/** First index of the markup (after `</script>`), for classifying units. */
	readonly markupStart: number;
	text(n: Node): string;
	line(pos: number): number;
}

export function parseComponent(code: string): AstSource {
	const ast = parse(code, { modern: true }) as unknown as { instance: { content: Node }; fragment: Node };
	if (!ast.instance) throw new Error('no instance <script> — re-point this guard');
	const markupStart = ast.instance.content.end;
	return {
		code,
		script: ast.instance.content,
		fragment: ast.fragment,
		markupStart,
		text: (n) => code.slice(n.start, n.end),
		line: (pos) => code.slice(0, pos).split('\n').length,
	};
}

// ---------------------------------------------------------------------------
// Declarations: captures, fence helpers, fence booleans
// ---------------------------------------------------------------------------

function collapseText(src: AstSource, n: Node): string {
	return src.text(n).replace(/\s+/g, ' ');
}

function calleeKey(src: AstSource, callee: Node): string {
	return src.text(callee).replace(/\s+/g, '').replace(/\?\./g, '.');
}

function isGenerationRead(n: Node): boolean {
	if (n.type === 'Identifier') return GENERATIONS.has(n.name);
	if (n.type === 'UpdateExpression' && n.operator === '++' && n.prefix) return isGenerationRead(n.argument);
	return false;
}

function isIdentityRead(src: AstSource, n: Node): boolean {
	if (n.type === 'CallExpression') {
		const key = calleeKey(src, n.callee);
		if (key === 'captureIdentity') return true;
		if (key === 'untrack' && isFn(n.arguments[0]) && n.arguments[0].expression) return isIdentityRead(src, n.arguments[0].body);
	}
	return n.type === 'MemberExpression' && src.text(n).replace(/\s+/g, '') === 'authStore.identityEpoch';
}

export type CaptureKind = 'generation' | 'identity';

export interface Declarations {
	/** Names declared inside a function whose initialiser is a generation or identity capture. */
	captures: Set<string>;
	/** The same names, split by what they captured (round 6: `identityHeld(gen)` is not a fence). */
	kinds: Map<CaptureKind, Set<string>>;
	/** Local booleans initialised from a fence; each is a fence only until the next await. */
	fenceBooleans: Set<string>;
	/** For each fence helper, the params its polarity needs and the capture kind each must receive. */
	fenceParams: Map<string, Array<{ index: number; kind: CaptureKind }>>;
	/** Helper and boolean names whose value is a fence, with its polarity. */
	fences: Map<string, Polarity>;
	/**
	 * Synchronous functions declared exactly once, by name. A call to one is
	 * walked INLINE at the call site, so what it does is judged where it runs
	 * instead of being trusted or refused by its name.
	 */
	helpers: Map<string, Node>;
	/**
	 * Helper names referenced anywhere as a VALUE rather than called — passed,
	 * stored, returned, spread. Such a helper runs whenever its holder calls
	 * it, so its body is also walked as a callback where it is defined.
	 */
	escapes: Set<string>;
}

export interface Polarity {
	/** The expression being TRUE implies the load is current. */
	t: boolean;
	/** The expression being FALSE implies the load is current. */
	f: boolean;
}
const NONE: Polarity = { t: false, f: false };

function collectDeclarations(src: AstSource): Declarations {
	const captures = new Set<string>();
	const kinds = new Map<CaptureKind, Set<string>>([
		['generation', new Set()],
		['identity', new Set()],
	]);
	const fnBodies = new Map<string, Node[]>();
	const boolInits = new Map<string, Node[]>();
	/** The names bound around each boolean initialiser, for its stamps (round 7 F4). */
	const initScope = new Map<Node, Set<string>>();
	const note = (m: Map<string, Node[]>, k: string, v: Node) => m.set(k, [...(m.get(k) ?? []), v]);
	const visit = (n: Node, anc: Node[]) => {
		if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier' && n.init) {
			const capture = isGenerationRead(n.init) ? 'generation' : isIdentityRead(src, n.init) ? 'identity' : null;
			// A capture is a snapshot taken inside a function. A component-level
			// variable is state that other code re-stamps (round 6 H40).
			if (capture && anc.some(isFn)) {
				captures.add(n.id.name);
				kinds.get(capture)!.add(n.id.name);
			} else if (capture) {
				// component state: neither a capture nor a fence boolean
			} else if (isFn(n.init) && !n.init.async) note(fnBodies, n.id.name, n.init);
			else {
				note(boolInits, n.id.name, n.init);
				initScope.set(n.init, chainBindings(src, anc));
			}
		}
		if (n.type === 'FunctionDeclaration' && !n.async && n.id) note(fnBodies, n.id.name, n);
	};
	// The markup declares captures too (the mode toggles' `startGen`).
	walk(src.script, visit);
	walk(src.fragment, visit);
	// Kinds are recorded by name, so one name must mean one kind everywhere
	// (round 7 F5).
	for (const name of kinds.get('generation')!) {
		if (kinds.get('identity')!.has(name)) throw new Error(`'${name}' is captured as both a generation and an identity — rename one; the guard types captures by name`);
	}
	const decls: Declarations = { captures, kinds, fenceBooleans: new Set(), fenceParams: new Map(), fences: new Map(), helpers: new Map(), escapes: new Set() };
	for (const [name, fns] of fnBodies) if (fns.length === 1) decls.helpers.set(name, fns[0]!);
	const reference = (n: Node, anc: Node[]) => {
		if (n.type !== 'Identifier' || !decls.helpers.has(n.name)) return;
		const p = anc.at(-1);
		if (!p || p.type.startsWith('TS')) return;
		if ((p.type === 'VariableDeclarator' || isFn(p)) && p.id === n) return;
		if (isFn(p) && p.params.includes(n)) return;
		if (p.type === 'CallExpression' && p.callee === n) return;
		if (p.type === 'MemberExpression' && p.property === n && !p.computed) return;
		if (p.type === 'Property' && p.key === n && !p.computed && !p.shorthand) return;
		decls.escapes.add(n.name);
	};
	walk(src.script, reference);
	walk(src.fragment, reference);
	// Two passes so a helper built on another helper (`stillOnSource` on
	// `switchedAway`) resolves. A name declared twice with different polarities
	// is refused rather than guessed.
	for (let pass = 0; pass < 2; pass++) {
		for (const [name, fns] of fnBodies) {
			const rets = fns.map((f) => returnPolarity(src, f, decls));
			settle(decls, name, rets.map((r) => r.polarity));
			if (fns.length === 1 && decls.fences.has(name)) decls.fenceParams.set(name, rets[0]!.required);
		}
		for (const [name, inits] of boolInits) {
			const pols = inits.map((i) => polarity(src, i, decls, { locals: initScope.get(i)! }));
			if (pols.some((p) => p.t || p.f)) {
				settle(decls, name, pols);
				decls.fenceBooleans.add(name);
			}
		}
	}
	// A helper's required params are known only for a name declared once, and
	// a bare name is read as a fence boolean: a fence helper's name must be
	// unique, and never also a boolean's (round 7 F8).
	for (const [name, fns] of fnBodies) {
		if (fns.length > 1 && decls.fences.has(name)) throw new Error(`'${name}' is declared more than once as a fence helper — rename one; the guard checks a helper's params only when it has one declaration`);
		if (decls.fenceBooleans.has(name)) throw new Error(`'${name}' is declared both as a function and as a fence boolean — rename one`);
	}
	return decls;
}

function settle(decls: Declarations, name: string, pols: Polarity[]) {
	const first = pols[0]!;
	if (pols.some((p) => p.t !== first.t || p.f !== first.f)) {
		throw new Error(`'${name}' is declared more than once with different fence meanings — rename one; the guard will not guess`);
	}
	if (first.t || first.f) decls.fences.set(name, first);
	else decls.fences.delete(name);
}

/**
 * A helper's polarity. Its OWN params count as captures here, whatever they
 * are called, and nowhere else (round 5 P2-6). Each param the polarity rests
 * on is recorded with the capture kind it needs, so a call site must pass a
 * capture of that kind IN THAT POSITION (round 6 H11).
 */
function returnPolarity(src: AstSource, fn: Node, decls: Declarations): { polarity: Polarity; required: Array<{ index: number; kind: CaptureKind }> } {
	let body: Node | null = null;
	if (fn.body.type !== 'BlockStatement') body = fn.body;
	else if (fn.body.body.length === 1 && fn.body.body[0].type === 'ReturnStatement' && fn.body.body[0].argument) body = fn.body.body[0].argument;
	if (!body) return { polarity: NONE, required: [] };
	const required: Array<{ index: number; kind: CaptureKind }> = [];
	// Every param shadows any capture of the same name; one at a time, each is
	// tried as a capture of each kind.
	const none = new Map<string, CaptureKind | null>();
	const names = new Set<string>();
	for (const p of fn.params) patternNames(p, names);
	for (const name of names) none.set(name, null);
	const all = new Map(none);
	const locals = new Set([...visibleAt(src, fn), ...scopeBindings(fn)]);
	fn.params.forEach((p: Node, index: number) => {
		if (p.type !== 'Identifier') return;
		for (const kind of ['generation', 'identity'] as const) {
			const alone = polarity(src, body!, decls, { locals, params: new Map([...none, [p.name, kind]]) });
			if (alone.t || alone.f) {
				required.push({ index, kind });
				all.set(p.name, kind);
				break;
			}
		}
	});
	return { polarity: polarity(src, body, decls, { locals, params: all }), required };
}

// ---------------------------------------------------------------------------
// Polarity of a test expression
// ---------------------------------------------------------------------------

function isCaptureRef(n: Node, decls: Declarations, params: ReadonlyMap<string, CaptureKind | null>, kind: CaptureKind): boolean {
	return n.type === 'Identifier' && (params.has(n.name) ? params.get(n.name) === kind : decls.kinds.get(kind)!.has(n.name));
}

/**
 * A stamp: an identity recorded earlier, read as `x.epoch` / `x.a.identityEpoch`
 * through plain names only — never a live read, never an object built on the
 * spot (round 6 H37) — whose root is a binding of the scopes around the
 * check. A root in component state is re-stamped by the identity change
 * itself, so it compares equal after one (round 7 F4, the H40 rule for
 * stamps).
 */
function isStamp(src: AstSource, n: Node, locals: ReadonlySet<string>): boolean {
	if (n.type !== 'MemberExpression' || n.computed || !['identityEpoch', 'epoch'].includes(n.property.name)) return false;
	if (isIdentityRead(src, n)) return false;
	let o = n.object;
	while (o.type === 'MemberExpression' && !o.computed) o = o.object;
	return o.type === 'Identifier' && locals.has(o.name);
}

/** Where a test expression is read. */
export interface PolarityScope {
	/** Names bound around the expression; a stamp's root must be one. */
	locals: ReadonlySet<string>;
	/** A helper's own params, each typed as the capture kind it is tried as. */
	params?: ReadonlyMap<string, CaptureKind | null>;
	/**
	 * When given, the fence booleans declared since the last suspension; any
	 * other fence boolean has gone stale (round 6 H10, H39).
	 */
	live?: ReadonlySet<string>;
}

export function polarity(src: AstSource, n: Node, decls: Declarations, scope: PolarityScope): Polarity {
	const params = scope.params ?? new Map<string, CaptureKind | null>();
	const { live, locals } = scope;
	switch (n.type) {
		case 'ChainExpression':
		case 'ParenthesizedExpression':
			return polarity(src, n.expression, decls, scope);
		case 'UnaryExpression':
			if (n.operator !== '!') return NONE;
			{
				const p = polarity(src, n.argument, decls, scope);
				return { t: p.f, f: p.t };
			}
		case 'LogicalExpression': {
			const a = polarity(src, n.left, decls, scope);
			const b = polarity(src, n.right, decls, scope);
			// The left operand is evaluated first. When the right one awaits, a
			// fence on the left is stale by the time the whole expression has a
			// value, so only the right operand's atoms survive (round 5 P1-1). On
			// the short-circuit path no await ran, and there the left atom's own
			// verdict is already the other half of the polarity.
			const awaitsRight = containsAwait(n.right);
			if (n.operator === '&&') return { t: awaitsRight ? b.t : a.t || b.t, f: a.f && b.f };
			if (n.operator === '||') return { t: a.t && b.t, f: awaitsRight ? b.f : a.f || b.f };
			return NONE;
		}
		case 'BinaryExpression': {
			const eq = n.operator === '===' || n.operator === '==';
			const ne = n.operator === '!==' || n.operator === '!=';
			if (!eq && !ne) return NONE;
			const [l, r] = [n.left, n.right];
			const gen =
				(l.type === 'Identifier' && GENERATIONS.has(l.name) && isCaptureRef(r, decls, params, 'generation')) ||
				(r.type === 'Identifier' && GENERATIONS.has(r.name) && isCaptureRef(l, decls, params, 'generation'));
			// An identity atom compares a live read against a stamp. Two stamps can
			// both be stale and still agree (round 5 P2-4); two live reads always
			// agree.
			const ident =
				(isIdentityRead(src, l) && isStamp(src, r, locals)) || (isIdentityRead(src, r) && isStamp(src, l, locals));
			if (!gen && !ident) return NONE;
			return eq ? { t: true, f: false } : { t: false, f: true };
		}
		case 'Identifier':
			// Only a fence BOOLEAN is a fence by its bare name. A fence helper named
			// without a call is a function value, always truthy, so `!helper`
			// never holds (round 7 F2).
			if (!decls.fenceBooleans.has(n.name)) return NONE;
			if (live && !live.has(n.name)) return NONE;
			return decls.fences.get(n.name) ?? NONE;
		case 'CallExpression': {
			const key = calleeKey(src, n.callee);
			if (key === 'identityHeld') {
				const a = n.arguments[0];
				const ok = a && (isCaptureRef(a, decls, params, 'identity') || isStamp(src, a, locals));
				return ok ? { t: true, f: false } : NONE;
			}
			const helper = n.callee.type === 'Identifier' ? decls.fences.get(n.callee.name) : undefined;
			if (!helper) return NONE;
			// Every param the helper's polarity rests on must receive a capture of
			// the kind it compares: `switchedAway(item, gen)` with a fresh `gen`,
			// or with the capture in another position, proves nothing.
			const required = decls.fenceParams.get(n.callee.name) ?? [];
			if (!required.every((r) => n.arguments[r.index] && isCaptureRef(n.arguments[r.index], decls, params, r.kind))) return NONE;
			return helper;
		}
		default:
			return NONE;
	}
}

// ---------------------------------------------------------------------------
// Units
// ---------------------------------------------------------------------------

export type UnitKind = 'async-function' | 'continuation';

export interface Unit {
	kind: UnitKind;
	/** The function node whose body is analysed. */
	fn: Node;
	/** For a continuation, the deferring call. */
	call?: Node;
	/** A top-level `async function` declaration's name. */
	name?: string;
	inMarkup: boolean;
	/** Functions enclosing this unit, innermost last; their locals are this unit's closure. */
	enclosing: Node[];
	/** Source text used to match a table row: the call for a continuation, the function otherwise. */
	signatureText: string;
	line: number;
}

function deferringCallee(src: AstSource, call: Node): string | null {
	const c = call.callee.type === 'ChainExpression' ? call.callee.expression : call.callee;
	if (c.type === 'MemberExpression' && !c.computed && DEFERRING_METHODS.has(c.property.name)) return c.property.name;
	if (c.type === 'Identifier' && DEFERRING_FUNCTIONS.has(c.name)) return c.name;
	if (c.type === 'MemberExpression' && !c.computed && DEFERRING_FUNCTIONS.has(c.property.name)) return c.property.name;
	// Bracket spellings are refused rather than read.
	if (c.type === 'MemberExpression' && c.computed && c.property.type === 'Literal' && (DEFERRING_METHODS.has(c.property.value) || DEFERRING_FUNCTIONS.has(c.property.value))) {
		throw new Error(`bracket-called ${c.property.value} at line ${src.line(call.start)} — write it as a plain call`);
	}
	return null;
}

export function enumerateUnits(src: AstSource): { units: Unit[]; deferringCalls: Node[] } {
	const units: Unit[] = [];
	const deferringCalls: Node[] = [];
	const topLevel = new Set<Node>(src.script.body.filter((s: Node) => s.type === 'FunctionDeclaration'));
	const visit = (inMarkup: boolean) => (n: Node, anc: Node[]) => {
		const enclosing = anc.filter(isFn);
		if (isFn(n) && n.async) {
			units.push({
				kind: 'async-function',
				fn: n,
				enclosing,
				name: n.type === 'FunctionDeclaration' && topLevel.has(n) ? n.id.name : undefined,
				inMarkup,
				signatureText: src.text(n),
				line: src.line(n.start),
			});
		}
		if (n.type === 'CallExpression' && deferringCallee(src, n)) {
			deferringCalls.push(n);
			for (const a of n.arguments) {
				if (isFn(a)) {
					units.push({
						kind: 'continuation',
						fn: a,
						call: n,
						enclosing,
						inMarkup,
						signatureText: src.code.slice(Math.max(0, n.start - 120), a.start) + src.text(a),
						line: src.line(n.start),
					});
				}
			}
		}
	};
	walk(src.script, visit(false));
	walk(src.fragment, visit(true));
	return { units, deferringCalls };
}

// ---------------------------------------------------------------------------
// Scopes
// ---------------------------------------------------------------------------

export function patternNames(p: Node, out: Set<string>) {
	switch (p.type) {
		case 'Identifier':
			out.add(p.name);
			return;
		case 'ObjectPattern':
			for (const prop of p.properties) patternNames(prop.type === 'RestElement' ? prop.argument : prop.value, out);
			return;
		case 'ArrayPattern':
			for (const e of p.elements) if (e) patternNames(e, out);
			return;
		case 'AssignmentPattern':
			patternNames(p.left, out);
			return;
		case 'RestElement':
			patternNames(p.argument, out);
			return;
		default:
			throw new Error(`binding pattern ${p.type} is not modelled — teach the guard rather than skip it`);
	}
}

/** Names declared directly in a statement list (not inside nested blocks). */
function declaredIn(stmts: Node[], out: Set<string> = new Set()): Set<string> {
	for (const st of stmts) {
		if (st.type === 'VariableDeclaration') for (const d of st.declarations) patternNames(d.id, out);
		if (st.type === 'FunctionDeclaration' && st.id) out.add(st.id.name);
	}
	return out;
}

/**
 * The names a function's OWN top-level scope binds: its params, its name if it
 * is a named expression, and the declarations directly in its body. A
 * declaration inside a nested block binds only while that block is walked
 * (`Analyser.stmt`); a `var` there is therefore treated as block-scoped, which
 * can only make a write MORE likely to be refused.
 */
export function scopeBindings(fn: Node): Set<string> {
	const out = new Set<string>();
	for (const p of fn.params) patternNames(p, out);
	if (fn.type === 'FunctionExpression' && fn.id) out.add(fn.id.name);
	if (fn.body.type === 'BlockStatement') declaredIn(fn.body.body, out);
	return out;
}

/** The names a loop's own head declares (`for (let i …)`, `for (const x of …)`). */
function loopBindings(n: Node): Set<string> {
	const out = new Set<string>();
	for (const head of [n.init, n.left]) {
		if (head?.type === 'VariableDeclaration') for (const d of head.declarations) patternNames(d.id, out);
	}
	return out;
}

/** The names a switch body declares: its cases share one block. */
function switchBindings(n: Node): Set<string> {
	const out = new Set<string>();
	for (const c of n.cases) declaredIn(c.consequent, out);
	return out;
}

const ancestorCache = new WeakMap<AstSource, Map<Node, Node[]>>();

/**
 * Names bound, at `fn`, by the scopes around it: every enclosing function,
 * block, loop head, switch body and catch clause. The component's own script
 * scope is NOT included — those names are component state.
 */
function visibleAt(src: AstSource, fn: Node): Set<string> {
	let m = ancestorCache.get(src);
	if (!m) {
		const map = new Map<Node, Node[]>();
		const visit = (n: Node, anc: Node[]) => {
			if (isFn(n)) map.set(n, anc.slice());
		};
		walk(src.script, visit);
		walk(src.fragment, visit);
		ancestorCache.set(src, map);
		m = map;
	}
	const chain = m.get(fn);
	if (!chain) throw new Error(`function at line ${src.line(fn.start)} is not in the component`);
	return chainBindings(src, chain);
}

/** Names bound by the scopes in an ancestor chain (see `visibleAt`). */
function chainBindings(src: AstSource, chain: Node[]): Set<string> {
	const out = new Set<string>();
	const add = (names: Set<string>) => names.forEach((x) => out.add(x));
	for (const a of chain) {
		if (a === src.script) continue;
		if (isFn(a)) {
			add(scopeBindings(a));
			continue;
		}
		// `isFn` narrows `a` to never here; every ancestor is still a Node.
		const b = a as Node;
		if (b.type === 'BlockStatement') add(declaredIn(b.body));
		else if (b.type === 'SwitchStatement') add(switchBindings(b));
		else if (b.type === 'ForStatement' || b.type === 'ForOfStatement' || b.type === 'ForInStatement') add(loopBindings(b));
		else if (b.type === 'CatchClause' && b.param) patternNames(b.param, out);
	}
	return out;
}

// ---------------------------------------------------------------------------
// Constructs refused outright (lead ruling on BUG-3084 checkpoint 63, (B))
// ---------------------------------------------------------------------------

/**
 * Names the analysis resolves by spelling. A function-level declaration that
 * reuses one would silently change what a call or a fence means, so it is
 * refused rather than modelled.
 */
export function trustedNames(): Set<string> {
	const out = new Set<string>([...GENERATIONS, ...DEFERRING_FUNCTIONS, ...Object.keys(SYNC_CALLBACK_CALLEES), 'identityHeld', 'captureIdentity', 'authStore']);
	for (const k of Object.keys(PURE_CALLS)) out.add(k.split('.')[0]!);
	return out;
}

const CALL_OR_WRITE = new Set(['CallExpression', 'NewExpression', 'AssignmentExpression', 'UpdateExpression', 'AwaitExpression', 'TaggedTemplateExpression', 'ImportExpression']);

const EMPTY_COLLECTIONS = new Set(['Set', 'Map']);

function unwrap(n: Node): Node {
	while (n.type === 'ParenthesizedExpression' || n.type === 'TSAsExpression' || n.type === 'TSNonNullExpression' || n.type === 'TSSatisfiesExpression' || n.type === 'ChainExpression') n = n.expression;
	return n;
}

/**
 * Constructs the analysis does not model and refuses wherever they sit inside
 * a function: a declaration shadowing a trusted name or a capture, a default
 * value that calls or assigns, a generator, a tagged template, a for-of/in
 * target that is not a declaration, and a call whose callee builds a function
 * in any shape but `(literal)(…)` or `(name ?? literal)(…)`.
 */
export function refusedConstructs(src: AstSource, decls: Declarations): string[] {
	const trusted = trustedNames();
	const out: string[] = [];
	const at = (n: Node) => `line ${src.line(n.start)}`;
	const binding = (p: Node, kind: 'param' | 'catch' | 'declarator', captureInit: boolean) => {
		const names = new Set<string>();
		patternNames(p, names);
		for (const name of names) {
			if (trusted.has(name)) out.push(`${at(p)}: declares ${name}, which shadows the trusted name ${name}`);
			else if (kind !== 'param' && !captureInit && decls.captures.has(name)) out.push(`${at(p)}: declares ${name}, which shadows the capture ${name}`);
		}
	};
	const visit = (n: Node, anc: Node[]) => {
		const inFn = anc.some(isFn);
		if (isFn(n)) {
			if (n.generator) out.push(`${at(n)}: generator function — the guard does not model when its body runs`);
			for (const p of n.params) binding(p, 'param', false);
			if (n.type !== 'ArrowFunctionExpression' && n.id && inFn && trusted.has(n.id.name)) out.push(`${at(n)}: declares ${n.id.name}, which shadows the trusted name ${n.id.name}`);
		}
		if (!inFn) return;
		if (n.type === 'VariableDeclarator') binding(n.id, 'declarator', !!n.init && (isGenerationRead(n.init) || isIdentityRead(src, n.init)));
		if (n.type === 'CatchClause' && n.param) binding(n.param, 'catch', false);
		if (n.type === 'AssignmentPattern') {
			let bad = false;
			walk(n.right, (c) => {
				// An empty collection is the one constructor default the component
				// uses (`excludeChildIds = new Set()`); it builds a value and runs
				// nothing of ours.
				const emptyCollection = c.type === 'NewExpression' && c.arguments.length === 0 && c.callee.type === 'Identifier' && EMPTY_COLLECTIONS.has(c.callee.name);
				if (CALL_OR_WRITE.has(c.type) && !emptyCollection) bad = true;
			});
			if (bad) out.push(`${at(n)}: default value calls or assigns — the guard does not walk defaults`);
		}
		if (n.type === 'TaggedTemplateExpression') out.push(`${at(n)}: tagged template — the guard does not model the tag`);
		if ((n.type === 'ForOfStatement' || n.type === 'ForInStatement') && n.left.type !== 'VariableDeclaration') {
			out.push(`${at(n)}: for-of/for-in target is not a declaration`);
		}
		if (n.type === 'CallExpression') {
			const c = unwrap(n.callee);
			if (c.type === 'Identifier' || c.type === 'MemberExpression' || c.type === 'Super' || c.type === 'Import') return;
			// `isFn` narrows its argument; read the logical shape through an unnarrowed alias.
			const callee: Node = c;
			const literalShape =
				isFn(c) || (callee.type === 'LogicalExpression' && ['Identifier', 'MemberExpression'].includes(unwrap(callee.left).type) && isFn(unwrap(callee.right)));
			let buildsFn = false;
			walk(c, (x) => {
				if (isFn(x)) buildsFn = true;
			});
			if (buildsFn && !literalShape) out.push(`${at(n)}: calls a function built in its callee`);
		}
	};
	walk(src.script, visit);
	walk(src.fragment, visit);
	return out;
}

// ---------------------------------------------------------------------------
// Flow analysis
// ---------------------------------------------------------------------------

export interface Violation {
	line: number;
	what: string;
	text: string;
}

export interface AnalyseOptions {
	/** Commits a `none`-style unit may make while unsafe, by target or callee key. */
	may?: ReadonlySet<string>;
	/** Exact statement texts treated as a bare await: their argument is not walked. */
	bareAwaits?: ReadonlySet<string>;
	/** Start the unit unsafe (a deferred callback) or safe (a function body). */
	startSafe: boolean;
	/**
	 * What each callback the unit creates may commit, by callback key
	 * (`callee(…)` for a function argument, `callee({prop})` for a property of
	 * an object argument, `{prop}` for any other object property). A callback
	 * with no entry may commit nothing.
	 */
	callbacks?: ReadonlyMap<string, ReadonlySet<string>>;
	/** When given, receives the key of every callback the walk reached (for population tables). */
	seen?: Set<string>;
}

const EXIT = 'exit' as const;
type State = boolean | typeof EXIT;

const and = (a: State, b: State): State => (a === EXIT ? b : b === EXIT ? a : a && b);

class Analyser {
	violations: Violation[] = [];
	locals = new Set<string>();
	/** Fence booleans declared since the last await on the walk. */
	private liveBools = new Set<string>();
	private breaks: State[][] = [];
	private continues: State[][] = [];
	private usedBare = new Set<string>();

	constructor(
		private src: AstSource,
		private decls: Declarations,
		private opts: AnalyseOptions
	) {}

	run(fn: Node): Violation[] {
		this.may = this.opts.may;
		this.locals = new Set([...visibleAt(this.src, fn), ...scopeBindings(fn)]);
		if (fn.body.type === 'BlockStatement') this.block(fn.body.body, this.opts.startSafe);
		else this.expr(fn.body, this.opts.startSafe);
		for (const b of this.opts.bareAwaits ?? []) {
			if (!this.usedBare.has(b) && this.src.text(fn).includes(b)) {
				throw new Error(`bare-await entry ${JSON.stringify(b)} is in this unit but was never reached as a statement`);
			}
		}
		for (const k of this.callbacksSeen) this.opts.seen?.add(k);
		for (const k of this.opts.callbacks?.keys() ?? []) {
			if (!this.callbacksSeen.has(k)) throw new Error(`callback entry ${k} names no callback this unit creates`);
		}
		const seen = new Set<string>();
		return this.violations.filter((v) => {
			const k = `${v.line}|${v.what}`;
			if (seen.has(k)) return false;
			seen.add(k);
			return true;
		});
	}

	/**
	 * Walks a nested function's body with `locals` as the names that resolve to
	 * a binding of the unit rather than to component state (round 5 P1-3: a
	 * nested arrow's `item` param used to make every `item =` in the unit
	 * local).
	 */
	private withLocals<T>(locals: Set<string>, body: () => T): T {
		const saved = this.locals;
		this.locals = locals;
		try {
			return body();
		} finally {
			this.locals = saved;
		}
	}

	private nested(fn: Node): Set<string> {
		return this.plus(scopeBindings(fn));
	}

	private plus(names: Set<string>): Set<string> {
		return new Set([...this.locals, ...names]);
	}

	/** The commits the code being walked may make: the row's, or a callback's. */
	private may: ReadonlySet<string> | undefined;
	/** Inlined helpers and callbacks being walked, outermost first, for messages. */
	private path: string[] = [];
	/** Callback keys the walk reached, so a row cannot list one that is gone. */
	readonly callbacksSeen = new Set<string>();

	private flag(n: Node, what: string, key?: string) {
		if (key && this.may?.has(key)) return;
		const where = this.path.map((k) => `${k} -> `).join('');
		this.violations.push({ line: this.src.line(n.start), what: where + what, text: this.src.text(n).replace(/\s+/g, ' ').slice(0, 100) });
	}

	/**
	 * A function literal that may run LATER: walked from an unsafe start, with
	 * only its own allowance. What it does never changes the caller's state.
	 */
	private callback(fn: Node, key: string, locals: Set<string> = this.nested(fn)) {
		if (fn.async) return; // an async function is a unit of its own
		this.callbacksSeen.add(key);
		if (this.inlining.has(fn)) return;
		this.inlining.add(fn);
		const saved = { may: this.may, breaks: this.breaks, continues: this.continues, liveBools: this.liveBools };
		this.liveBools = new Set();
		this.may = this.opts.callbacks?.get(key) ?? new Set();
		this.breaks = [];
		this.continues = [];
		this.path.push(`callback ${key}`);
		this.withLocals(locals, () => {
			if (fn.body.type === 'BlockStatement') this.block(fn.body.body, false);
			else this.expr(fn.body, false);
		});
		this.path.pop();
		this.inlining.delete(fn);
		({ may: this.may, breaks: this.breaks, continues: this.continues, liveBools: this.liveBools } = saved);
	}

	/**
	 * Walks a helper's body HERE, with the caller's state. The helper sees ITS
	 * lexical scopes, not the caller's: a caller's local that shares a name with
	 * component state the helper writes must not excuse that write.
	 */
	private inline(helper: Node, s: boolean, key: string) {
		if (this.inlining.has(helper)) return;
		this.inlining.add(helper);
		this.path.push(`${key}()`);
		const body = helper.body;
		this.withLocals(this.lexicalOf(helper), () => {
			if (body.type === 'BlockStatement') this.block(body.body, s);
			else this.expr(body, s);
		});
		this.path.pop();
		this.inlining.delete(helper);
	}

	/**
	 * A function defined inside the unit. A helper that is only ever CALLED is
	 * checked at each call site instead (`inline`); anything else — a helper
	 * also used as a value, a name declared twice, a literal stored or returned
	 * — may run at any time, so it is walked here as a callback (round 5 P1-2).
	 */
	private definition(fn: Node, key: string) {
		if (fn.async) return;
		const name = fn.type === 'FunctionDeclaration' ? fn.id?.name : key;
		const isHelper = name !== undefined && this.decls.helpers.get(name) === fn;
		if (isHelper && !this.decls.escapes.has(name)) return;
		this.callback(fn, key, isHelper ? this.lexicalOf(fn) : this.nested(fn));
	}

	/** A helper declared once in the component, named by `n`; it may be handed over by name. */
	private helperNamed(n: Node): Node | undefined {
		return n.type === 'Identifier' ? this.decls.helpers.get(n.name) : undefined;
	}

	/** The names a helper's body resolves locally: its own lexical chain, never the caller's. */
	private lexicalOf(helper: Node): Set<string> {
		return new Set([...visibleAt(this.src, helper), ...scopeBindings(helper)]);
	}

	private objectProperties(n: Node, s: boolean, keyOf: (prop: string) => string): boolean {
		for (const p of n.properties) {
			if (p.type === 'SpreadElement') s = this.expr(p.argument, s);
			else {
				if (p.computed) s = this.expr(p.key, s);
				const name = !p.computed && p.key.type === 'Identifier' ? p.key.name : p.key.type === 'Literal' ? String(p.key.value) : '?';
				const named = this.helperNamed(p.value);
				if (isFn(p.value)) this.callback(p.value, keyOf(name));
				else if (named) this.callback(named, keyOf(name), this.lexicalOf(named));
				else s = this.expr(p.value, s);
			}
		}
		return s;
	}

	private runsNow(callee: Node, key: string): boolean {
		if (key in SYNC_CALLBACK_CALLEES) return true;
		if (callee.type !== 'MemberExpression' || callee.computed || !SYNC_ITERATION_METHODS.has(callee.property.name)) return false;
		let root = callee.object;
		while (root.type === 'MemberExpression' || root.type === 'ChainExpression' || root.type === 'TSNonNullExpression') root = root.type === 'MemberExpression' ? root.object : root.expression;
		return root.type === 'Identifier' && this.locals.has(root.name);
	}

	private scope(): PolarityScope {
		return { locals: this.locals, live: this.liveBools };
	}

	block(stmts: Node[], s: State): State {
		for (const st of stmts) {
			if (s === EXIT) return EXIT;
			s = this.stmt(st, s);
		}
		return s;
	}

	private test(n: Node, s: boolean): { t: boolean; f: boolean } {
		const after = this.expr(n, s);
		const p = polarity(this.src, n, this.decls, this.scope());
		return { t: after || p.t, f: after || p.f };
	}

	stmt(n: Node, s: State): State {
		if (s === EXIT) return EXIT;
		switch (n.type) {
			case 'EmptyStatement':
				return s;
			case 'FunctionDeclaration':
				this.definition(n, n.id.name);
				return s;
			case 'BlockStatement':
				return this.withLocals(this.plus(declaredIn(n.body)), () => this.block(n.body, s));
			case 'ExpressionStatement': {
				const text = this.src.text(n);
				if (this.opts.bareAwaits?.has(text) && n.expression.type === 'AwaitExpression') {
					this.usedBare.add(text);
					return false;
				}
				return this.expr(n.expression, s);
			}
			case 'VariableDeclaration': {
				for (const d of n.declarations) {
					if (!d.init) continue;
					if (isFn(d.init) && d.id.type === 'Identifier') {
						this.definition(d.init, d.id.name);
						continue;
					}
					s = this.expr(d.init, s as boolean);
					if (d.id.type === 'Identifier' && this.decls.fenceBooleans.has(d.id.name)) this.liveBools.add(d.id.name);
					if (!s && d.id.type === 'Identifier' && this.decls.captures.has(d.id.name)) {
						this.flag(d, 'captures a generation or identity after an unfenced await');
					}
				}
				return s;
			}
			case 'ReturnStatement':
				if (n.argument) this.expr(n.argument, s);
				return EXIT;
			case 'ThrowStatement':
				this.expr(n.argument, s);
				return EXIT;
			case 'IfStatement': {
				const t = this.test(n.test, s);
				const a = this.stmt(n.consequent, t.t);
				const b = n.alternate ? this.stmt(n.alternate, t.f) : t.f;
				return a === EXIT && b === EXIT ? EXIT : and(a, b);
			}
			case 'TryStatement': {
				const awaits = containsAwait(n.block);
				const t = this.stmt(n.block, s);
				let c: State = EXIT;
				if (n.handler) {
					const param = new Set<string>();
					if (n.handler.param) patternNames(n.handler.param, param);
					c = this.withLocals(this.plus(param), () => this.stmt(n.handler.body, awaits ? false : s));
				}
				const normal: State = n.handler ? (t === EXIT && c === EXIT ? EXIT : and(t, c)) : t;
				if (!n.finalizer) return normal;
				const fin = this.stmt(n.finalizer, awaits || (n.handler && containsAwait(n.handler.body)) ? false : s);
				if (fin === EXIT) return EXIT;
				return normal === EXIT ? EXIT : normal && fin;
			}
			case 'WhileStatement':
			case 'DoWhileStatement':
			case 'ForStatement':
			case 'ForOfStatement':
			case 'ForInStatement':
			{
				const entry = s;
				return this.withLocals(this.plus(loopBindings(n)), () => this.loop(n, entry));
			}
			case 'BreakStatement':
				this.breaks.at(-1)?.push(s);
				return EXIT;
			case 'ContinueStatement':
				this.continues.at(-1)?.push(s);
				return EXIT;
			case 'SwitchStatement':
				return this.withLocals(this.plus(switchBindings(n)), () => this.switchStmt(n, s as boolean));
			default:
				throw new Error(`statement type ${n.type} at line ${this.src.line(n.start)} is not modelled — teach the guard rather than skip it`);
		}
	}

	private switchStmt(n: Node, s: boolean): State {
		s = this.expr(n.discriminant, s);
		// Case tests run in source order, skipping `default`, until one
		// matches: a case is entered after its own test, and `default`
		// only once every test has run (round 5 P2-3).
		const entries: boolean[] = [];
		let tested = s;
		for (const c of n.cases) {
			if (c.test) tested = this.expr(c.test, tested);
			entries.push(tested);
		}
		this.breaks.push([]);
		let fall: State = EXIT;
		let hasDefault = false;
		n.cases.forEach((c: Node, i: number) => {
			if (!c.test) hasDefault = true;
			fall = this.block(c.consequent, and(fall, c.test ? entries[i]! : tested));
		});
		const brk = this.breaks.pop()!;
		let out: State = fall;
		for (const b of brk) out = and(out, b);
		if (!hasDefault) out = and(out, tested);
		return out;
	}

	private loop(n: Node, entry: boolean): State {
		if (n.init) entry = this.stmtOrExpr(n.init, entry);
		// One pass: `exit` is the state on every way out of the loop, `next` the
		// state carried into the following pass. A loop leaves NORMALLY right
		// after its test (or its iterator) says stop, so that state is an exit
		// even when the body itself always returns (round 5 P2-2).
		const once = (s: boolean): { exit: State; next: State } => {
			this.breaks.push([]);
			this.continues.push([]);
			let exit: State = EXIT;
			if (n.type === 'WhileStatement' || n.type === 'ForStatement') {
				if (n.test) {
					s = this.expr(n.test, s);
					exit = s;
				}
			}
			if (n.type === 'ForOfStatement' || n.type === 'ForInStatement') {
				s = this.expr(n.right, s);
				// `for await` awaits before every iteration, and once more to find
				// the end (round 5 P2-1).
				if (n.type === 'ForOfStatement' && n.await) {
					s = false;
					// A fence boolean lapses there too (round 7 F3). The try block is
					// walked before its handler and finalizer, so this and the await
					// case also lapse them for a handler entered after a suspension.
					this.liveBools.clear();
				}
				exit = s;
			}
			let next = this.stmt(n.body, s);
			for (const c of this.continues.pop()!) next = and(next, c);
			if (next !== EXIT && n.update) next = this.expr(n.update, next);
			if (next !== EXIT && n.type === 'DoWhileStatement') {
				next = this.expr(n.test, next);
				exit = next;
			}
			for (const b of this.breaks.pop()!) exit = and(exit, b);
			return { exit, next };
		};
		const first = once(entry);
		const second = once(first.next === EXIT ? entry : entry && first.next);
		const exit = and(first.exit, second.exit);
		return exit;
	}

	private stmtOrExpr(n: Node, s: boolean): boolean {
		const r = n.type === 'VariableDeclaration' ? this.stmt(n, s) : this.expr(n, s);
		return r === EXIT ? s : r;
	}

	/** Walks an expression in evaluation order; returns `safe` after it. */
	expr(n: Node, s: boolean): boolean {
		switch (n.type) {
			case 'Identifier': {
				// A helper named in a value position is handed to whoever holds the
				// value, whatever wraps it (`as`, `!`, `?:`): walked as a callback, or
				// inline when this value is being called right here (round 6 H17,
				// H36).
				const named = this.helperNamed(n);
				if (named && this.calleeDepth > 0) this.inline(named, s, n.name);
				else if (named) this.callback(named, n.name, this.lexicalOf(named));
				return s;
			}
			case 'Literal':
			case 'ThisExpression':
			case 'Super':
			case 'MetaProperty':
				return s;
			case 'TemplateLiteral':
				for (const e of n.expressions) s = this.expr(e, s);
				return s;
			case 'ChainExpression':
			case 'ParenthesizedExpression':
				return this.expr(n.expression, s);
			case 'TSAsExpression':
			case 'TSNonNullExpression':
			case 'TSSatisfiesExpression':
			case 'TSTypeAssertion':
				return this.expr(n.expression, s);
			case 'ArrayExpression':
				for (const e of n.elements) if (e) s = this.expr(e, s);
				return s;
			case 'ObjectExpression':
				// A function-valued property is handed to whoever reads the object,
				// which may call it at any time.
				return this.objectProperties(n, s, (prop) => `{${prop}}`);
			case 'SpreadElement':
				return this.expr(n.argument, s);
			case 'MemberExpression':
				s = this.expr(n.object, s);
				if (n.computed) s = this.expr(n.property, s);
				return s;
			case 'UnaryExpression':
				s = this.expr(n.argument, s);
				if (n.operator === 'delete' && !s) this.flag(n, 'deletes a property after an unfenced await');
				return s;
			case 'BinaryExpression':
				return this.expr(n.right, this.expr(n.left, s));
			case 'LogicalExpression': {
				const l = this.expr(n.left, s);
				const p = polarity(this.src, n.left, this.decls, this.scope());
				const rhsSafe = l || (n.operator === '&&' ? p.t : n.operator === '||' ? p.f : false);
				const r = this.expr(n.right, rhsSafe);
				// After the whole expression, only the short-circuit that skipped the
				// right side and the path through it are both possible.
				return l && r;
			}
			case 'ConditionalExpression': {
				const t = this.test(n.test, s);
				// Both arms are walked: `a && b` here would skip the alternate
				// whenever the consequent ends unsafe (round 5 on #1387).
				const a = this.expr(n.consequent, t.t);
				const b = this.expr(n.alternate, t.f);
				return a && b;
			}
			case 'SequenceExpression':
				for (const e of n.expressions) s = this.expr(e, s);
				return s;
			case 'AssignmentExpression': {
				if (isFn(n.right)) this.definition(n.right, `${collapseText(this.src, n.left)} =`);
				else s = this.expr(n.right, s);
				if (n.left.type !== 'Identifier') {
					s = this.lhs(n.left, s);
					if (!s) this.flag(n, `writes ${this.src.text(n.left)} after an unfenced await`, this.targetKey(n.left));
				} else if ((this.liveBools.delete(n.left.name), !this.locals.has(n.left.name) && !s)) {
					this.flag(n, `assigns ${n.left.name} after an unfenced await`, n.left.name);
				}
				// Any rewrite of a capture after an unfenced await makes every later
				// check against it pass, however the new value is spelled (round 5
				// P2-5: `gen = loadGeneration as number`, `gen = loadGeneration ?? gen`).
				if (n.left.type === 'Identifier' && this.decls.captures.has(n.left.name) && !s) {
					this.flag(n, `rewrites capture ${n.left.name} after an unfenced await`);
				}
				return s;
			}
			case 'UpdateExpression': {
				const a = n.argument;
				if (a.type !== 'Identifier') s = this.lhs(a, s);
				const local = a.type === 'Identifier' && this.locals.has(a.name);
				if (!local && !s) this.flag(n, `updates ${this.src.text(a)} after an unfenced await`, this.targetKey(a));
				if (a.type === 'Identifier' && this.decls.captures.has(a.name) && !s) {
					this.flag(n, `rewrites capture ${a.name} after an unfenced await`);
				}
				return s;
			}
			case 'AwaitExpression':
				this.expr(n.argument, s);
				this.liveBools.clear();
				return false;
			case 'CallExpression':
			case 'NewExpression':
				return this.call(n, s);
			case 'TaggedTemplateExpression':
				s = this.expr(n.quasi, s);
				if (!s) this.flag(n, 'tagged template after an unfenced await');
				return s;
			case 'ImportExpression':
				s = this.expr(n.source, s);
				if (!s) this.flag(n, 'dynamic import after an unfenced await', 'import');
				return s;
			case 'ArrowFunctionExpression':
			case 'FunctionExpression':
				// In the callee of a call (`(f ?? ((u) => goto(u)))(url)`) the literal
				// is invoked right here, with this state.
				if (this.calleeDepth > 0 && !n.async) {
					// Its body is ordinary code: a function it returns or stores is a
					// value again, not something invoked here (round 6 H1).
					const depth = this.calleeDepth;
					this.calleeDepth = 0;
					try {
						this.withLocals(this.nested(n), () => (n.body.type === 'BlockStatement' ? this.block(n.body.body, s) : this.expr(n.body, s)));
					} finally {
						this.calleeDepth = depth;
					}
					return s;
				}
				// Any other function VALUE (returned, in an array, in a conditional):
				// whoever holds it may call it at any time.
				this.definition(n, 'function value');
				return s;
			case 'ClassExpression':
			case 'YieldExpression':
			default:
				throw new Error(`expression type ${n.type} at line ${this.src.line(n.start)} is not modelled — teach the guard rather than skip it`);
		}
	}

	private lhs(n: Node, s: boolean): boolean {
		if (n.type === 'MemberExpression') {
			s = this.expr(n.object, s);
			if (n.computed) s = this.expr(n.property, s);
			return s;
		}
		if (n.type === 'Identifier') return s;
		// Destructuring targets: every leaf is a write; the walk above flags the whole.
		return s;
	}

	private targetKey(n: Node): string {
		let r = n;
		while (r.type === 'MemberExpression') r = r.object;
		return r.type === 'Identifier' ? r.name : this.src.text(n);
	}

	private inlining = new Set<Node>();
	/** Above zero while walking a call's callee expression (see the function-literal case). */
	private calleeDepth = 0;

	private args(n: Node, s: boolean, key: string, callee: Node, deferred: boolean): boolean {
		const now = this.runsNow(callee, key);
		for (const a of n.arguments) {
			if (isFn(a)) {
				// A deferred or async callback is its own unit. A callback to a
				// callee known to call it synchronously runs HERE; any other may
				// run later, so it is walked from an unsafe start.
				if (deferred || a.async) continue;
				if (now) {
					const inner = this.withLocals(this.nested(a), () =>
						a.body.type === 'BlockStatement' ? this.block(a.body.body, s) : this.expr(a.body, s)
					);
					if (inner === false) s = false;
				} else {
					this.callback(a, `${key}(…)`);
				}
			} else if (this.helperNamed(a) && !deferred) {
				// A helper handed over by NAME runs whenever the callee calls it,
				// exactly as a literal would.
				const named = this.helperNamed(a)!;
				if (now) this.inline(named, s, a.name);
				else this.callback(named, `${key}(…)`, this.lexicalOf(named));
			} else if (now && !deferred && unwrap(a).type !== 'ArrowFunctionExpression' && unwrap(a).type !== 'FunctionExpression') {
				// A synchronous callee runs whatever it is given; a value the guard
				// cannot follow is a call to an unknown function (round 6 H7).
				s = this.expr(a, s);
				if (!s) this.flag(a, `calls ${key}(${collapseText(this.src, a)}) after an unfenced await`, `${key}(${collapseText(this.src, a)})`);
			} else if (a.type === 'ObjectExpression' && !deferred) {
				s = this.objectProperties(a, s, (prop) => `${key}({${prop}})`);
			} else {
				s = this.expr(a, s);
			}
		}
		return s;
	}

	private call(n: Node, s: boolean): boolean {
		const callee = n.callee.type === 'ChainExpression' ? n.callee.expression : n.callee;
		const key = calleeKey(this.src, callee);
		const deferred = n.type === 'CallExpression' && !!deferringCallee(this.src, n);
		if (callee.type === 'MemberExpression') {
			s = this.expr(callee.object, s);
			if (callee.computed) s = this.expr(callee.property, s);
		} else if (callee.type !== 'Identifier') {
			this.calleeDepth++;
			try {
				s = this.expr(callee, s);
			} finally {
				this.calleeDepth--;
			}
		}
		const depth = this.calleeDepth;
		this.calleeDepth = 0;
		try {
			s = this.args(n, s, key, callee, deferred);
		} finally {
			this.calleeDepth = depth;
		}
		// Scheduling a continuation commits nothing: the continuation is a unit.
		if (deferred) return s;
		if (n.type === 'CallExpression') {
			const p = polarity(this.src, n, this.decls, this.scope());
			if (p.t || p.f) {
				// A fence helper's body still runs here, and may commit (round 6 H25).
				const fenceHelper = callee.type === 'Identifier' ? this.decls.helpers.get(callee.name) : undefined;
				if (fenceHelper) this.inline(fenceHelper, s, key);
				return s;
			}
		}
		// A helper declared once in this component runs its body HERE.
		const helper = callee.type === 'Identifier' ? this.decls.helpers.get(callee.name) : undefined;
		if (helper && n.type === 'CallExpression') {
			this.inline(helper, s, key);
			return s;
		}
		if (s) return s;
		if (key in PURE_CALLS) return s;
		this.flag(n, `${n.type === 'NewExpression' ? 'constructs' : 'calls'} ${key} after an unfenced await`, key);
		return s;
	}
}

function containsAwait(n: Node): boolean {
	let found = false;
	walk(n, (c, anc) => {
		// `for await` suspends too, and is not an AwaitExpression (round 6 H15).
		const suspends = c.type === 'AwaitExpression' || (c.type === 'ForOfStatement' && c.await);
		if (suspends && !anc.some((a) => isFn(a) && a !== n)) found = true;
	});
	return found;
}

/**
 * Whether an async unit starts safe. One called from outside any unit starts
 * with whatever state is current when it is called. One DEFINED inside a
 * unit, or inside a helper a unit may inline, is a callback: the callee it is
 * handed to may call it after any await, exactly as with a synchronous
 * literal, so it starts unsafe and commits only what its row allows
 * (round 7 F1).
 */
export function asyncUnitStartsSafe(decls: Declarations, unit: Unit, units: readonly Unit[]): boolean {
	if (unit.kind !== 'async-function') return false;
	const inside = new Set<Node>([...units.map((u) => u.fn), ...decls.helpers.values()]);
	return !unit.enclosing.some((f) => inside.has(f));
}

export function analyseUnit(src: AstSource, decls: Declarations, unit: Unit, opts: AnalyseOptions): Violation[] {
	return new Analyser(src, decls, opts).run(unit.fn);
}

export function declarations(src: AstSource): Declarations {
	return collectDeclarations(src);
}
