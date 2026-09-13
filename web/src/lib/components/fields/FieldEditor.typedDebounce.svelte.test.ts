// BUG-3039 — a debounced typed edit is cancelled and lost when the PREVIOUS
// keystroke's write echoes back.
//
// The sequence, all inside one 500ms window's worth of typing:
//
//   1. Type `a`. The debounce fires and the parent's onchange sends a PATCH.
//   2. Before it answers, type `b`. The field holds `ab` and arms a new timer.
//   3. The `a` response lands and the parent re-props `value` as `a`.
//   4. The value-track $effect sees `hasPending` and cancels the `ab` timer.
//
// `ab` is never sent and never shown. No ticket is taken for it, so the write
// ordering added in PLAN-2857 U4 cannot see it either — that model orders
// writes that were DISPATCHED, and this one never is.
//
// The effect exists for a real reason: a value arriving from SSE, a collab peer,
// or the parent's own 409 refetch must replace what the field shows. What it
// cannot currently do is tell that case apart from its OWN write echoing back,
// and the two want opposite answers.
//
// This suite mounts FieldEditor DIRECTLY. ItemDetail wraps its fields in
// `{#key itemSlug}` and so remounts them on an item switch — a fact BUG-3039
// originally got wrong, from a stale comment in the component itself — which
// means the item-switch legs below pin the component's own behaviour for a
// caller that does not remount, not a path the pane can reach today.
//
// Written before the fix, per CONVE-29, and confirmed RED against the unfixed
// tree — see the BUG-3039 trail for the run.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$lib/api/client', () => ({ api: { search: vi.fn(), items: { create: vi.fn() } } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrapStateFor: vi.fn(() => 'ready'),
		findByIdOrSlug: vi.fn(),
		getByCollection: vi.fn(() => []),
		cursorFor: vi.fn(),
		upsert: vi.fn(),
		scopeEpochFor: vi.fn(),
		pendingResyncFor: vi.fn(),
		resetGenerationFor: vi.fn(),
	},
}));
vi.mock('$lib/stores/localSearch.svelte', () => ({
	localSearch: { search: vi.fn(() => []), epoch: vi.fn(() => 0) },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [], collectionsAreFreshFor: vi.fn(() => true) },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditCollection: vi.fn(() => true) },
}));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: { show: vi.fn() } }));

import FieldEditor from './FieldEditor.svelte';

const DEBOUNCE_MS = 500;
const field = { key: 'component', label: 'Component', type: 'text' as const };

beforeEach(() => {
	vi.useFakeTimers();
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
	vi.restoreAllMocks();
});

/** Type into the field's input the way a user does — one `input` event. */
function type(input: HTMLInputElement, text: string) {
	input.value = text;
	input.dispatchEvent(new Event('input', { bubbles: true }));
}

function mount(initial: string, onchange: (v: unknown) => void) {
	const result = render(FieldEditor, {
		props: { field, value: initial, onchange, itemId: 'item-A' },
	});
	const input = result.container.querySelector('input') as HTMLInputElement;
	expect(input, 'text field should render an input').toBeTruthy();
	return { ...result, input };
}

describe('a typed edit survives the previous keystroke echoing back (BUG-3039)', () => {
	it('sends the LATER text, not just the earlier one', async () => {
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		// 1. Type `a` and let the debounce fire: the parent gets the write.
		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: the first write must actually go out').toHaveBeenCalledWith(
			'a',
		);

		// 2. Type `b` while that write is still in flight.
		type(input, 'ab');
		await tick();

		// 3. The first write's response lands and the parent re-props.
		await rerender({ field, value: 'a', onchange, itemId: 'item-A' });
		await tick();

		// 4. The second write must still be pending, and must still fire.
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith('ab');
		expect(onchange).toHaveBeenCalledTimes(2);
	});

	it('keeps the typed text on screen while our own write echoes back', async () => {
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await tick();

		await rerender({ field, value: 'a', onchange, itemId: 'item-A' });
		await tick();

		// The user is still looking at this field. Snapping it back to `a` loses
		// the character they typed, whether or not the write eventually lands.
		expect(input.value).toBe('ab');
	});
});

describe('a change from ELSEWHERE still wins (the guard this must not break)', () => {
	it('drops the pending edit when the new value is not what we sent', async () => {
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith('a');

		type(input, 'ab');
		await tick();

		// An SSE / collab peer sets the field to something nobody here typed.
		await rerender({ field, value: 'peer wrote this', onchange, itemId: 'item-A' });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(
			onchange,
			'the peer edit is newer than our pending one; ours must not overwrite it',
		).toHaveBeenCalledTimes(1);
	});

	it('drops the pending edit on an item switch, even to the value we just sent', async () => {
		// The Codex round 3 [P1] this effect was built for: an editor pointed at
		// another row must not flush item A's text onto item B, and an echo check
		// on the VALUE ALONE cannot see the difference, since item B's value can
		// equal what we sent for item A.
		//
		// NOT reachable from ItemDetail, which remounts under `{#key itemSlug}`
		// rather than retargeting — this suite mounts the component directly, so
		// it pins the COMPONENT's behaviour for any caller that does not remount.
		// An earlier version of this comment asserted the pane reuses it, which
		// was false and is what BUG-3039 was built on.
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await tick();

		await rerender({ field, value: 'a', onchange, itemId: 'item-B' });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'item B must not receive item A\'s typing').toHaveBeenCalledTimes(1);
	});
});

describe('the fence is the item, not the text', () => {
	it('drops the pending edit on a retarget even when the two values are EQUAL', async () => {
		// Item B's field happens to hold the same text item A's did, so the value
		// alone shows nothing at all.
		//
		// This leg does NOT prove the effect re-runs in that situation in a real
		// parent: `rerender` assigns every prop, and measurement showed an equal
		// assignment re-runs a tracking effect, so the run this depends on may be
		// the harness's rather than the pane's. What it pins is the DECISION the
		// component makes once it does run. The stronger claim is not needed —
		// ItemDetail remounts — and is not made.
		const onchange = vi.fn();
		const { input, rerender } = mount('start', onchange);

		type(input, 'starta');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith('starta');

		type(input, 'startab');
		await tick();

		// Same value, different item.
		await rerender({ field, value: 'start', onchange, itemId: 'item-B' });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, "item B must not receive item A's typing").toHaveBeenCalledTimes(1);
		expect(input.value, 'and the box must show item B, not the abandoned draft').toBe('start');
	});

	it('still recognises its own echo for a caller that gives no itemId', async () => {
		// CopyItemDialog and FieldSaveProbe mount this without an item. They have
		// nothing to retarget BETWEEN, so `undefined === undefined` is a true
		// answer to "same subject?" rather than a missing one, and the echo check
		// is as sound for them as for the pane. What they do not get is retarget
		// DETECTION — there is no id to notice changing — which costs them
		// nothing they can reach.
		//
		// The first draft of this leg asserted the opposite and passed without
		// ever re-propping `value`: the effect never ran, so it proved only that
		// nothing happens when nothing happens.
		const onchange = vi.fn();
		const { container, rerender } = render(FieldEditor, {
			props: { field, value: '', onchange },
		});
		const input = container.querySelector('input') as HTMLInputElement;

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: the first write goes out').toHaveBeenCalledWith('a');

		type(input, 'ab');
		await tick();

		// The precondition the first draft was missing: the prop MUST change, or
		// the effect under test never runs.
		await rerender({ field, value: 'a', onchange });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith('ab');
		expect(onchange).toHaveBeenCalledTimes(2);
	});

	it('drops a pending edit for a no-itemId caller when the change is NOT its echo', async () => {
		const onchange = vi.fn();
		const { container, rerender } = render(FieldEditor, {
			props: { field, value: '', onchange },
		});
		const input = container.querySelector('input') as HTMLInputElement;

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await tick();

		await rerender({ field, value: 'someone else', onchange });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledTimes(1);
	});
});

describe('the echo answers for exactly one write', () => {
	it('a LATER change equal to the same text is an outside change, and drops', async () => {
		// D5 in the mutation matrix. `awaitingEcho` is consumed when it matches, so
		// it cannot go on vouching for every future value that happens to look like
		// what we once sent. Without the consume, a peer writing our old text back
		// an hour later would read as our own write coming home.
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await tick();

		// The echo. Consumed here.
		await rerender({ field, value: 'a', onchange, itemId: 'item-A' });
		await tick();

		// Someone else sets it back to `a`. Not ours, however familiar it looks.
		await rerender({ field, value: 'a', onchange, itemId: 'item-A' });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledTimes(1);
	});

	it('releases the display hold once the prop has caught up', async () => {
		// D8. With nothing pending the prop is the truth again, so the hold must be
		// dropped — otherwise the field goes on showing what we last typed and a
		// later peer edit is invisible.
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		await rerender({ field, value: 'a', onchange, itemId: 'item-A' });
		await tick();
		expect(input.value, 'precondition: our own write is on screen').toBe('a');

		await rerender({ field, value: 'peer wrote this', onchange, itemId: 'item-A' });
		await tick();
		expect(input.value).toBe('peer wrote this');
	});

	it('reads an empty field echoed back as null as OUR echo', async () => {
		// D11. A cleared text field is sent as '' and comes back from the pane as
		// null. Strict equality calls that an outside change and throws away the
		// keystrokes typed since.
		const onchange = vi.fn();
		const { input, rerender } = mount('x', onchange);

		type(input, '');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: clearing the field sends something').toHaveBeenCalledTimes(1);

		type(input, 'z');
		await tick();

		await rerender({ field, value: null, onchange, itemId: 'item-A' });
		await tick();

		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith('z');
	});
});

describe('a write that never comes home (codex round 1, finding 3)', () => {
	it('releases the display hold when the consumer reports the write FAILED', async () => {
		// The R8-2 failure in the typed path. Agreement alone cannot answer for a
		// REFUSED write, because the value never comes back — so without a second
		// release signal the rejected text sits on screen forever, over a server
		// that holds something else. An earlier version of the source comment
		// claimed nothing was pinned here; it was defending the design I had just
		// written, and it was wrong.
		let reject: (e: unknown) => void = () => {};
		const onchange = vi.fn(() => new Promise((_, rj) => { reject = rj; }));
		const { input } = mount('server text', onchange);

		type(input, 'rejected text');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: the write went out').toHaveBeenCalledTimes(1);
		expect(input.value, 'precondition: our text is on screen while it is in flight').toBe(
			'rejected text',
		);

		reject(new Error('refused'));
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(input.value, 'the server never accepted this; stop showing it').toBe('server text');
	});

	it('a settlement does NOT take away text typed since', async () => {
		let resolve: (v: unknown) => void = () => {};
		const onchange = vi.fn(() => new Promise((rs) => { resolve = rs; }));
		const { input } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await tick();

		resolve(undefined);
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(input.value, 'the newer keystrokes outlive the older write\'s outcome').toBe('ab');
	});

	it('an OLDER write settling does not release a NEWER write\'s record', async () => {
		const settles: ((v: unknown) => void)[] = [];
		const onchange = vi.fn(() => new Promise((rs) => { settles.push(rs); }));
		const { input } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: two writes are outstanding').toHaveBeenCalledTimes(2);

		// The FIRST write settles. The second is still in flight.
		settles[0](undefined);
		await vi.advanceTimersByTimeAsync(0);
		await tick();

		expect(input.value, 'still showing the newer write, not the older outcome').toBe('ab');
	});
});

describe('an older echo does not release a newer hold (findings 2 and 4)', () => {
	it('keeps showing the newest send when an EARLIER value arrives', async () => {
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'a');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		type(input, 'ab');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledTimes(2);

		// The FIRST write's response arrives after the second was sent.
		await rerender({ field, value: 'a', onchange, itemId: 'item-A' });
		await tick();

		expect(input.value, 'the older response must not paint over the newer send').toBe('ab');

		// And the newer one landing does release it.
		await rerender({ field, value: 'ab', onchange, itemId: 'item-A' });
		await tick();
		expect(input.value).toBe('ab');
	});
});

describe('the number field (finding 1)', () => {
	const numberField = { key: 'estimate', label: 'Estimate', type: 'number' as const };

	function mountNumber(initial: unknown, onchange: (v: unknown) => void) {
		const result = render(FieldEditor, {
			props: { field: numberField, value: initial, onchange, itemId: 'item-A' },
		});
		const input = result.container.querySelector('input.number-input') as HTMLInputElement;
		expect(input, 'number field should render its input').toBeTruthy();
		return { ...result, input };
	}

	it('steps from the value we SENT, not the prop that has not caught up', async () => {
		// `value` lags anything sent but not echoed, so stepping from it turns
		// "type 5, then +1" into oldValue + 1 and the typed 5 is gone.
		const onchange = vi.fn();
		const { container, input } = mountNumber(2, onchange);

		type(input, '5');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledWith(5);

		// The echo has NOT arrived: the prop is still 2.
		const step = container.querySelectorAll('button');
		const plus = Array.from(step).find((b) => (b.textContent ?? '').includes('+'))
			?? step[step.length - 1];
		plus.click();
		await tick();

		expect(onchange).toHaveBeenLastCalledWith(6);
	});

	it('keeps a half-typed number on screen while our own write echoes back', async () => {
		// The hold keeps the raw typed STRING, not the parsed number, so an echo
		// landing mid-keystroke does not rewrite `5.` to `5` under the cursor.
		//
		// The first draft of this leg asserted that an unrelated prop change to
		// `2` left `1.` alone, on the premise that nothing had been sent. Wrong
		// twice: `Number('1.')` is 1, so it schedules like any other edit, and a
		// prop arriving as `2` is an outside change the guard is SUPPOSED to win.
		// The code was right and the test was wrong.
		const onchange = vi.fn();
		const { input, rerender } = mountNumber(2, onchange);

		type(input, '5');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: the first write goes out').toHaveBeenCalledWith(5);

		type(input, '5.');
		await tick();

		// Our own echo for the earlier write.
		await rerender({ field: numberField, value: 5, onchange, itemId: 'item-A' });
		await tick();

		expect(input.value).toBe('5.');
	});
});

describe('the subject can change without the row changing (finding 5)', () => {
	// THE INTERLEAVING MATTERS, and my first draft of both legs got it wrong: they
	// swapped the subject while an edit was still PENDING, and a pending edit is
	// dropped by the ordinary "this is not our echo" branch whether or not the
	// subject check exists. Both mutants survived. The window only the subject
	// check covers is AFTER a flush — `hasPending` is false, but the display hold
	// and the echo stamp are still live, and that branch deliberately KEEPS
	// showing what we sent. Without a subject check the old field's text is then
	// painted over the new field's value.

	it('drops a live display hold when the FIELD is swapped under the editor', async () => {
		const onchange = vi.fn();
		const { input, rerender } = mount('', onchange);

		type(input, 'typed into component');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange, 'precondition: flushed, so the hold is live and nothing is pending')
			.toHaveBeenCalledTimes(1);
		expect(input.value).toBe('typed into component');

		const other = { key: 'owner', label: 'Owner', type: 'text' as const };
		await rerender({ field: other, value: 'someone', onchange, itemId: 'item-A' });
		await tick();

		expect(input.value, "the new field must not wear the old one's draft").toBe('someone');
	});

	it('drops a live display hold when the editor flips to readonly and back', async () => {
		const onchange = vi.fn();
		const { input, rerender } = mount('server', onchange);

		type(input, 'typed then locked');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS);
		expect(onchange).toHaveBeenCalledTimes(1);

		// Locked: the user may no longer edit this, so their draft is not ours to
		// keep showing.
		await rerender({ field, value: 'server', onchange, itemId: 'item-A', readonly: true });
		await tick();
		await rerender({ field, value: 'server', onchange, itemId: 'item-A', readonly: false });
		await tick();

		const after = document.querySelector('input.field-input') as HTMLInputElement | null;
		expect(after?.value ?? '', 'the abandoned draft must not come back').toBe('server');
	});
});
