/**
 * BUG-3084 round 4, lead ruling on checkpoint 51: the identity-fence guard for
 * ItemDetail reads an AST instead of spellings, and FAILS CLOSED.
 *
 * WHY NOT THE SCANNER (`identityFenceSource.ts`). Four review rounds on #1387
 * each returned new members of one class — shapes a regex does not model:
 * unlisted committers (`void req()`, `+=`, `obj[k] =`, `.push`), unlisted async
 * shapes (nested named functions, annotated arrows, object methods, markup
 * `async function`), a text window that borrowed the NEXT callback's fence, and
 * helpers trusted by name. Per team CONVE-35 that measured the instrument, not
 * the code. The route-surface guards still use the scanner; their port is
 * TASK-3097.
 *
 * WHAT THIS DOES
 *
 * 1. POPULATION by node type. Every `async` function node anywhere in the
 *    script or the markup is a UNIT, and so is every callback passed to a
 *    deferring call (`.then` / `.catch` / `.finally`, `setTimeout`,
 *    `setInterval`, `queueMicrotask`, `requestAnimationFrame`). That list IS
 *    a spelling list, so it is not trusted to be complete: inside a unit,
 *    every other function literal passed to a call, and every function-valued
 *    object property, is a CALLBACK walked from an unsafe start, since it may
 *    run after any await (round 5 P1-2). Only callees on
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
 *    whatever the body does; `for await` starts every pass unsafe. A `switch`
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
 * 4. FENCES by what they compare, never by name. An atom is a comparison of a
 *    CAPTURED variable against `loadGeneration` / `itemGen`, a call to
 *    `identityHeld(x)`, or a comparison of a LIVE identity read
 *    (`authStore.identityEpoch`, `captureIdentity()`) against a stamp. `!`, `&&`, `||` and
 *    `?:` combine atoms with their polarity, so `if (gen !== loadGeneration)
 *    return;` fences what follows and `if (gen === loadGeneration) { … }`
 *    fences only its consequent. A helper (`const stillCurrent = () => …`,
 *    `function switchedAway(…)`) is a fence exactly when its OWN body has a
 *    polarity; a local boolean initialised from a fence is one too.
 *
 * 5. SCOPES. Whether a write targets a local is decided by the lexical scopes
 *    enclosing the write: the unit's chain, plus each nested function while
 *    its body is walked. An inlined helper sees its own chain, not the
 *    caller's (round 5 P1-3). Block scoping is flattened to the function.
 *
 * WHAT THIS CANNOT DO. It proves that a check with the right SHAPE dominates
 * every commit; it does not prove the check reads the right item. On the
 * CHECK side it still trusts a captured variable by name, component-wide (a
 * shadowing declaration of a capture's name defeats it). The mount suite is
 * the other half.
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
 * again. Keys are the callee's source text with `?.` read as `.`; a `*.name`
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

export interface Declarations {
	/** Names declared anywhere whose initialiser is a generation or identity capture. */
	captures: Set<string>;
	/** Helper and boolean names whose value is a fence, with its polarity. */
	fences: Map<string, Polarity>;
	/**
	 * Synchronous functions declared exactly once, by name. A call to one is
	 * walked INLINE at the call site, so what it does is judged where it runs
	 * instead of being trusted or refused by its name.
	 */
	helpers: Map<string, Node>;
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
	const fnBodies = new Map<string, Node[]>();
	const boolInits = new Map<string, Node[]>();
	const note = (m: Map<string, Node[]>, k: string, v: Node) => m.set(k, [...(m.get(k) ?? []), v]);
	const visit = (n: Node) => {
		if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier' && n.init) {
			if (isGenerationRead(n.init) || isIdentityRead(src, n.init)) captures.add(n.id.name);
			else if (isFn(n.init) && !n.init.async) note(fnBodies, n.id.name, n.init);
			else note(boolInits, n.id.name, n.init);
		}
		if (n.type === 'FunctionDeclaration' && !n.async && n.id) note(fnBodies, n.id.name, n);
	};
	// The markup declares captures too (the mode toggles' `startGen`).
	walk(src.script, visit);
	walk(src.fragment, visit);
	const decls: Declarations = { captures, fences: new Map(), helpers: new Map() };
	for (const [name, fns] of fnBodies) if (fns.length === 1) decls.helpers.set(name, fns[0]!);
	// Two passes so a helper built on another helper (`stillOnSource` on
	// `switchedAway`) resolves. A name declared twice with different polarities
	// is refused rather than guessed.
	for (let pass = 0; pass < 2; pass++) {
		for (const [name, fns] of fnBodies) {
			const pols = fns.map((f) => returnPolarity(src, f, decls));
			settle(decls, name, pols);
		}
		for (const [name, inits] of boolInits) {
			const pols = inits.map((i) => polarity(src, i, decls));
			if (pols.some((p) => p.t || p.f)) settle(decls, name, pols);
		}
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

function returnPolarity(src: AstSource, fn: Node, decls: Declarations): Polarity {
	if (fn.body.type !== 'BlockStatement') return polarity(src, fn.body, decls);
	const stmts = fn.body.body;
	if (stmts.length === 1 && stmts[0].type === 'ReturnStatement' && stmts[0].argument) return polarity(src, stmts[0].argument, decls);
	return NONE;
}

// ---------------------------------------------------------------------------
// Polarity of a test expression
// ---------------------------------------------------------------------------

function isCaptureRef(n: Node, decls: Declarations, params: Set<string>): boolean {
	return n.type === 'Identifier' && (decls.captures.has(n.name) || params.has(n.name));
}

function isIdentityOperand(src: AstSource, n: Node): boolean {
	return isIdentityRead(src, n) || (n.type === 'MemberExpression' && !n.computed && ['identityEpoch', 'epoch'].includes(n.property.name));
}

export function polarity(src: AstSource, n: Node, decls: Declarations, params: Set<string> = new Set()): Polarity {
	switch (n.type) {
		case 'ChainExpression':
		case 'ParenthesizedExpression':
			return polarity(src, n.expression, decls, params);
		case 'UnaryExpression':
			if (n.operator !== '!') return NONE;
			{
				const p = polarity(src, n.argument, decls, params);
				return { t: p.f, f: p.t };
			}
		case 'LogicalExpression': {
			const a = polarity(src, n.left, decls, params);
			const b = polarity(src, n.right, decls, params);
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
				(l.type === 'Identifier' && GENERATIONS.has(l.name) && isCaptureRef(r, decls, params)) ||
				(r.type === 'Identifier' && GENERATIONS.has(r.name) && isCaptureRef(l, decls, params));
			// An identity atom compares a live read against a stamp. Two stamps can
			// both be stale and still agree (round 5 P2-4); two live reads always
			// agree.
			const ident =
				(isIdentityRead(src, l) && isIdentityOperand(src, r) && !isIdentityRead(src, r)) ||
				(isIdentityRead(src, r) && isIdentityOperand(src, l) && !isIdentityRead(src, l));
			if (!gen && !ident) return NONE;
			return eq ? { t: true, f: false } : { t: false, f: true };
		}
		case 'Identifier':
			return decls.fences.get(n.name) ?? NONE;
		case 'CallExpression': {
			const key = calleeKey(src, n.callee);
			if (key === 'identityHeld') {
				const a = n.arguments[0];
				const ok = a && (isCaptureRef(a, decls, params) || isIdentityOperand(src, a));
				return ok ? { t: true, f: false } : NONE;
			}
			const helper = n.callee.type === 'Identifier' ? decls.fences.get(n.callee.name) : undefined;
			if (!helper) return NONE;
			// A helper taking arguments is a fence only when one of them is a
			// capture: `switchedAway(item, gen)` with a fresh `gen` proves nothing.
			if (n.arguments.length > 0 && !n.arguments.some((a: Node) => isCaptureRef(a, decls, params))) return NONE;
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

function patternNames(p: Node, out: Set<string>) {
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

/**
 * The names a function's OWN scope binds: its params, its name if it is a
 * named expression, and every declaration in its body outside nested
 * functions (a nested function declaration binds its name here, and nothing
 * else). Block scoping is flattened to the function, which can only make a
 * name local in more of the function than it is — never in another function.
 */
export function scopeBindings(fn: Node): Set<string> {
	const out = new Set<string>();
	for (const p of fn.params) patternNames(p, out);
	if (fn.type === 'FunctionExpression' && fn.id) out.add(fn.id.name);
	const visit = (n: Node) => {
		if (isFn(n)) {
			if (n.type === 'FunctionDeclaration' && n.id) out.add(n.id.name);
			return;
		}
		if (n.type === 'VariableDeclarator') patternNames(n.id, out);
		if (n.type === 'CatchClause' && n.param) patternNames(n.param, out);
		for (const c of children(n)) visit(c);
	};
	visit(fn.body);
	return out;
}

const fnAncestorCache = new WeakMap<AstSource, Map<Node, Node[]>>();

/** The functions lexically enclosing `fn`, outermost first. */
function fnAncestors(src: AstSource, fn: Node): Node[] {
	let m = fnAncestorCache.get(src);
	if (!m) {
		const map = new Map<Node, Node[]>();
		const visit = (n: Node, anc: Node[]) => {
			if (isFn(n)) map.set(n, anc.filter(isFn));
		};
		walk(src.script, visit);
		walk(src.fragment, visit);
		fnAncestorCache.set(src, map);
		m = map;
	}
	const chain = m.get(fn);
	if (!chain) throw new Error(`function at line ${src.line(fn.start)} is not in the component`);
	return chain;
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
}

const EXIT = 'exit' as const;
type State = boolean | typeof EXIT;

const and = (a: State, b: State): State => (a === EXIT ? b : b === EXIT ? a : a && b);

class Analyser {
	violations: Violation[] = [];
	locals = new Set<string>();
	params = new Set<string>();
	private breaks: State[][] = [];
	private continues: State[][] = [];
	private usedBare = new Set<string>();

	constructor(
		private src: AstSource,
		private decls: Declarations,
		private opts: AnalyseOptions
	) {}

	run(fn: Node, enclosing: Node[] = []): Violation[] {
		this.may = this.opts.may;
		for (const e of [...enclosing, fn]) {
			for (const name of scopeBindings(e)) this.locals.add(name);
			this.declareParams(e);
		}
		if (fn.body.type === 'BlockStatement') this.block(fn.body.body, this.opts.startSafe);
		else this.expr(fn.body, this.opts.startSafe);
		for (const b of this.opts.bareAwaits ?? []) {
			if (!this.usedBare.has(b) && this.src.text(fn).includes(b)) {
				throw new Error(`bare-await entry ${JSON.stringify(b)} is in this unit but was never reached as a statement`);
			}
		}
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

	/** Every param of `fn` and of every function nested in it. */
	private declareParams(fn: Node) {
		walk(fn, (n) => {
			if (isFn(n)) for (const p of n.params) walk(p, (i) => i.type === 'Identifier' && this.params.add(i.name));
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
		return new Set([...this.locals, ...scopeBindings(fn)]);
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
	private callback(fn: Node, key: string) {
		if (fn.async) return; // an async function is a unit of its own
		this.callbacksSeen.add(key);
		const saved = { may: this.may, breaks: this.breaks, continues: this.continues };
		this.may = this.opts.callbacks?.get(key) ?? new Set();
		this.breaks = [];
		this.continues = [];
		this.path.push(`callback ${key}`);
		this.withLocals(this.nested(fn), () => {
			if (fn.body.type === 'BlockStatement') this.block(fn.body.body, false);
			else this.expr(fn.body, false);
		});
		this.path.pop();
		({ may: this.may, breaks: this.breaks, continues: this.continues } = saved);
	}

	private objectProperties(n: Node, s: boolean, keyOf: (prop: string) => string): boolean {
		for (const p of n.properties) {
			if (p.type === 'SpreadElement') s = this.expr(p.argument, s);
			else {
				if (p.computed) s = this.expr(p.key, s);
				const name = !p.computed && p.key.type === 'Identifier' ? p.key.name : p.key.type === 'Literal' ? String(p.key.value) : '?';
				if (isFn(p.value)) this.callback(p.value, keyOf(name));
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

	block(stmts: Node[], s: State): State {
		for (const st of stmts) {
			if (s === EXIT) return EXIT;
			s = this.stmt(st, s);
		}
		return s;
	}

	private test(n: Node, s: boolean): { t: boolean; f: boolean } {
		const after = this.expr(n, s);
		const p = polarity(this.src, n, this.decls, this.params);
		return { t: after || p.t, f: after || p.f };
	}

	stmt(n: Node, s: State): State {
		if (s === EXIT) return EXIT;
		switch (n.type) {
			case 'EmptyStatement':
			case 'FunctionDeclaration':
				return s;
			case 'BlockStatement':
				return this.block(n.body, s);
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
					s = this.expr(d.init, s as boolean);
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
				if (n.handler) c = this.stmt(n.handler.body, awaits ? false : s);
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
				return this.loop(n, s);
			case 'BreakStatement':
				this.breaks.at(-1)?.push(s);
				return EXIT;
			case 'ContinueStatement':
				this.continues.at(-1)?.push(s);
				return EXIT;
			case 'SwitchStatement': {
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
			default:
				throw new Error(`statement type ${n.type} at line ${this.src.line(n.start)} is not modelled — teach the guard rather than skip it`);
		}
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
				if (n.type === 'ForOfStatement' && n.await) s = false;
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
			case 'Identifier':
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
				const p = polarity(this.src, n.left, this.decls, this.params);
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
				s = this.expr(n.right, s);
				if (n.left.type !== 'Identifier') {
					s = this.lhs(n.left, s);
					if (!s) this.flag(n, `writes ${this.src.text(n.left)} after an unfenced await`, this.targetKey(n.left));
				} else if (!this.locals.has(n.left.name) && !s) {
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
				// A function value that is not called here is a definition.
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

	private call(n: Node, s: boolean): boolean {
		const callee = n.callee.type === 'ChainExpression' ? n.callee.expression : n.callee;
		const key = calleeKey(this.src, callee);
		const deferred = n.type === 'CallExpression' && !!deferringCallee(this.src, n);
		if (callee.type === 'MemberExpression') {
			s = this.expr(callee.object, s);
			if (callee.computed) s = this.expr(callee.property, s);
		} else if (callee.type !== 'Identifier') {
			s = this.expr(callee, s);
		}
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
			} else if (a.type === 'ObjectExpression' && !deferred) {
				s = this.objectProperties(a, s, (prop) => `${key}({${prop}})`);
			} else {
				s = this.expr(a, s);
			}
		}
		// Scheduling a continuation commits nothing: the continuation is a unit.
		if (deferred) return s;
		if (n.type === 'CallExpression') {
			const p = polarity(this.src, n, this.decls, this.params);
			if (p.t || p.f) return s;
		}
		// A helper declared once in this component runs its body HERE.
		const helper = callee.type === 'Identifier' ? this.decls.helpers.get(callee.name) : undefined;
		if (helper && n.type === 'CallExpression') {
			if (this.inlining.has(helper)) return s;
			this.inlining.add(helper);
			// The helper sees ITS lexical scopes, not the caller's: a caller's
			// local that shares a name with component state the helper writes
			// must not excuse that write.
			const lexical = new Set<string>();
			for (const e of [...fnAncestors(this.src, helper), helper]) for (const name of scopeBindings(e)) lexical.add(name);
			this.declareParams(helper);
			this.path.push(`${key}()`);
			const body = helper.body;
			this.withLocals(lexical, () => {
				if (body.type === 'BlockStatement') this.block(body.body, s);
				else this.expr(body, s);
			});
			this.path.pop();
			this.inlining.delete(helper);
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
		if (c.type === 'AwaitExpression' && !anc.some((a) => isFn(a) && a !== n)) found = true;
	});
	return found;
}

export function analyseUnit(src: AstSource, decls: Declarations, unit: Unit, opts: AnalyseOptions): Violation[] {
	return new Analyser(src, decls, opts).run(unit.fn, unit.enclosing);
}

export function declarations(src: AstSource): Declarations {
	return collectDeclarations(src);
}
