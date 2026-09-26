// Node-project SOURCE guard: every async commit point on the workspace settings
// page is fenced on the signed-in identity (BUG-3006).
//
// THE GATE BESIDE THIS (TASK-3097): `settings/settingsIdentityGate.test.ts` tables every async unit on
// this page with the hash of the code it was reviewed on, and refuses any
// edit to one until its row is re-read. That catches BUG-3084's round-4
// classes, which a source scanner cannot see. This file catches what the
// gate does not: the RULE on a NEW handler, which the gate accepts with any
// row. Keep both. Neither is a duplicate of the other.
//
// WHY SOURCE. The page is a SvelteKit route with fourteen independent async
// commit points across six handler families, a global toast callback and two
// deferred timers. The behaviour of individual handlers is measured by
// `settingsIdentityFence.svelte.test.ts` next to this file, which flips the
// epoch mid-await and asserts no commit lands. What that cannot do is tell you
// a FIFTEENTH handler was added unfenced — the population is the property here,
// and a behavioural suite only ever covers the members someone thought to
// write a case for. This guard owns the population; that one owns the semantics.
//
// HOW IT FAILS. It ENUMERATES the async functions in the script block and the
// inline async arrows in the markup, and requires each to be either fenced or
// on an explicit exempt list with a reason. An unrecognised handler FAILS the
// test rather than being skipped — the BUG-3030 lesson re-learned here: a guard
// anchored on a position window or on the names that existed when it was
// written keeps passing while covering less and less. If you add a handler, you
// add it to one of the two lists and say why.
//
// WHAT A SOURCE GUARD CANNOT DO, so this is not mistaken for proof: it checks
// spellings. It would not catch a fence comparing the wrong two values, one
// made unreachable by an earlier return, or one placed after the commit it is
// supposed to guard. Those are the behavioural suite's job.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const SOURCE = readFileSync(
	fileURLToPath(new URL('./settings/+page.svelte', import.meta.url)),
	'utf8'
);

/**
 * Comments in this page quote the very identifiers asserted below — the fence's
 * own doc comment names `identityHeld` several times — so a guard that did not
 * strip them would pass on the documentation after the code was deleted.
 */
const CODE = SOURCE.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|\s)\/\/[^\n]*/g, '$1');

const SCRIPT_END = CODE.indexOf('</script>');
const SCRIPT = CODE.slice(0, SCRIPT_END);
const MARKUP = CODE.slice(SCRIPT_END);

/**
 * Async functions declared at the top level of the script block, with their
 * bodies. FAILS CLOSED on a body it cannot delimit rather than slicing to the
 * end of the file — `itemDetailIdentityDiscardsTeardownWrites.test.ts` documents
 * what an unbounded slice costs: assertions satisfied by unrelated occurrences
 * hundreds of lines away.
 */
function asyncFunctions(): Map<string, string> {
	const out = new Map<string, string>();
	const re = /\n\tasync function (\w+)\s*\(/g;
	let m: RegExpExecArray | null;
	while ((m = re.exec(SCRIPT)) !== null) {
		const name = m[1];
		const start = m.index;
		const end = SCRIPT.indexOf('\n\t}', start);
		if (end <= start) {
			throw new Error(`could not delimit ${name}() — re-point this guard rather than widening it`);
		}
		out.set(name, SCRIPT.slice(start, end));
	}
	return out;
}

/**
 * Inline async arrows in the MARKUP. These are the population the
 * function-level enumeration cannot see, and the reason this guard has two
 * lists: the first version of the instrument that built BUG-3006's table
 * attributed a markup arrow to the last function DECLARED above it — a name on
 * the covered list — and reported the one site it existed to find as already
 * covered.
 */
function markupAsyncArrows(): string[] {
	return MARKUP.split('\n').filter((line) => /async\s*\(/.test(line));
}

/**
 * Handlers that legitimately need no fence, each with the reason. A handler
 * lands here only when it has NO await and NO deferred commit — not when
 * fencing it merely looks unnecessary.
 */
const EXEMPT: Record<string, string> = {
	load: 'the page-level load, fenced by BUG-2991 on (user, workspace) and the site that RE-STAMPS the epoch'
};

/**
 * Handlers exempt from the ENTRY-CAPTURE rule specifically, because their
 * capture is taken by their CALLER and handed in. They are still required to
 * check the fence — only the capture moves.
 */
const CAPTURED_BY_CALLER: Record<string, string> = {
	undoDeleteWorkspace:
		'runs when the Undo toast is clicked, possibly minutes later and in a different page; ' +
		'capturing at its own entry would capture the clicker and compare it with itself'
};

describe('the workspace settings page fences every async commit point', () => {
	it('declares exactly one page-level fence helper, stamped at load', () => {
		expect(CODE).toMatch(/let\s+identityEpochAtLoad\s*=\s*authStore\.identityEpoch/);
		// TWO helpers, answering two different questions. `identityHeld` takes
		// the caller's own capture; `pageIdentityHeld` asks whether the PAGE
		// still belongs to the signed-in user, which is the only one that can
		// speak for a control rendered under a previous identity.
		expect(CODE).toMatch(/function captureIdentity\(\)\s*:\s*number\s*\{[\s\S]*?return authStore\.identityEpoch/);
		expect(CODE).toMatch(/function identityHeld\(captured: number\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === captured/);
		expect(CODE).toMatch(/function pageIdentityHeld\(\)\s*:\s*boolean\s*\{[\s\S]*?authStore\.identityEpoch === identityEpochAtLoad/);

		// RE-STAMPED before load()'s first await, or a page that survives an
		// identity change without remounting compares against a dead epoch for
		// the rest of its life.
		const load = asyncFunctions().get('load');
		expect(load, 'load() was renamed — re-point this guard').toBeDefined();
		const beforeFirstAwait = load!.slice(0, load!.indexOf('await'));
		expect(beforeFirstAwait).toContain('identityEpochAtLoad = authStore.identityEpoch');
	});

	it('captures the identity at ENTRY in every async handler, before its first await', () => {
		// The CAPTURE is the half a "does it mention the fence" check misses,
		// and it is where this guard's first version was wrong (codex round 1
		// [Medium]): an occurrence anywhere in the body satisfied it, including
		// one after the write it was supposed to guard.
		//
		// Entry, specifically, because the page's load epoch is RE-STAMPED by a
		// concurrent load — so a handler comparing against that would compare
		// the new value with the new value and commit. The capture has to be
		// taken before the handler's first await or it is not the handler's own.
		const missing: string[] = [];
		for (const [name, body] of asyncFunctions()) {
			if (name in EXEMPT || name in CAPTURED_BY_CALLER) continue;
			if (!body.includes('await')) continue;
			const beforeFirstAwait = body.slice(0, body.indexOf('await'));
			if (!beforeFirstAwait.includes('captureIdentity()')) missing.push(name);
		}
		// The exempted one still has to CHECK — only its capture moves — which the
		// per-await count below enforces for it like everything else.
		expect(
			missing,
			`these async handlers do not capture the identity before their first await. ` +
				`A fence comparing against anything else can be defeated by a concurrent load re-stamping it.`
		).toEqual([]);
	});

	it('never lets a handler compare against the PAGE load epoch', () => {
		// Lead ruling, day 67, carrying codex round 1's [High] into the
		// enumeration: `identityEpochAtLoad` is RE-STAMPED by `load()`, so a
		// handler comparing against it can be defeated by a concurrent load
		// setting it to the very value the handler is about to see. Exactly one
		// function may read it — `pageIdentityHeld`, whose whole question is
		// about the page rather than about a unit of work.
		const readers: string[] = [];
		for (const [name, body] of asyncFunctions()) {
			// `load` is the RE-STAMPER, so it is the one handler that writes the
			// value; it is already exempt from the capture rule for that reason.
			if (name in EXEMPT) continue;
			if (body.includes('identityEpochAtLoad')) readers.push(name);
		}
		expect(
			readers,
			`these handlers read the page load epoch directly. It is re-stamped by load(), so a handler ` +
				`comparing against it commits when a concurrent load moves it to the current value. ` +
				`Capture at the handler's own entry instead.`
		).toEqual([]);

		// And the one legitimate reader is a plain function, not a handler, so
		// it is asserted positively rather than by the absence above.
		expect(CODE).toMatch(/function pageIdentityHeld\(\)[\s\S]*?identityEpochAtLoad/);
	});

	it('checks the fence at least once per await in every async handler', () => {
		// COUNTED, not merely present. One check in a handler with three awaits
		// leaves two continuations unguarded, and "the body mentions the fence"
		// cannot tell those apart. This is a lower bound rather than a proof —
		// it cannot see WHERE the checks sit — and the behavioural suite is what
		// establishes that they sit before the commits.
		const short: string[] = [];
		for (const [name, body] of asyncFunctions()) {
			if (name in EXEMPT) continue;
			const awaits = body.split('await ').length - 1;
			if (awaits === 0) continue;
			const checks = body.split('identityHeld(').length - 1;
			if (checks < awaits) short.push(`${name} (${checks} checks for ${awaits} awaits)`);
		}
		expect(
			short,
			`these async handlers have fewer identity checks than awaits, so at least one continuation commits unguarded`
		).toEqual([]);
	});

	it('fences the inline async arrows in the markup too', () => {
		const arrows = markupAsyncArrows();
		// The count is asserted so a NEW arrow cannot arrive unnoticed: an added
		// one fails here and has to be looked at, which is the whole point of
		// enumerating rather than pattern-matching.
		expect(arrows).toHaveLength(1);
		// WHAT THE FENCE COVERS IN THE ONE THAT EXISTS, stated because the
		// obvious reading is wrong: the arrow awaits `copyToClipboard` and the
		// clipboard write IS that await, so a check cannot come before it — the
		// click is synchronous with the handler's first line and no identity can
		// have moved yet. The commit the fence guards is the TOAST after it,
		// which is what would otherwise report one session's copy to the next.
		// `handleInvite` is the opposite case and is fenced accordingly: there
		// the clipboard write happens AFTER an await, so the check precedes it.
		for (const arrow of arrows) {
			// `pageIdentityHeld`, not an entry capture. By the time this click
			// arrives the epoch has ALREADY moved — that is the whole scenario —
			// so a capture taken at entry is the new value and can detect
			// nothing. The question here is whether the page this button was
			// rendered into still belongs to whoever is signed in.
			expect(arrow, `an inline async arrow in the markup commits without a fence: ${arrow.slice(0, 120)}`)
				.toContain('pageIdentityHeld()');
		}
	});

	it('fences both deferred timers, which no await-adjacent check covers', () => {
		// A setTimeout body runs two seconds after its await returned. These are
		// the commit points the await-site fences do NOT reach, and they are
		// invisible to any instrument that enumerates awaits.
		const timers = SCRIPT.split('setTimeout(').slice(1);
		expect(timers).toHaveLength(2);
		for (const timer of timers) {
			const body = timer.slice(0, timer.indexOf('}, 2000)'));
			expect(body, `a deferred timer commits without an identity fence: ${body.slice(0, 80)}`)
				.toContain('identityHeld(');
		}
	});

	it('gives the Undo toast its OWN captured epoch rather than the page helper', () => {
		// The toast outlives the page, so `identityHeld` is the wrong instrument
		// for it twice over: the variable it reads may belong to a dead page, and
		// a later load re-stamps it. The callback captures at DELETE time and
		// compares at CLICK time.
		const del = asyncFunctions().get('handleDeleteWorkspace');
		expect(del, 'handleDeleteWorkspace was renamed — re-point this guard').toBeDefined();
		expect(del!).toMatch(/const epochAtDelete = captureIdentity\(\)/);
		expect(del!).toMatch(/onAction: \(\) => \{[\s\S]*?if \(authStore\.identityEpoch !== epochAtDelete\) return;/);

		// And the restore itself re-checks after ITS await — the identity can
		// move between the click and the response.
		const undo = asyncFunctions().get('undoDeleteWorkspace');
		expect(undo, 'undoDeleteWorkspace was renamed — re-point this guard').toBeDefined();
		expect(undo!).toContain('epochAtDelete');
	});
});
