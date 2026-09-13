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
// or an item switch must replace what the field shows. What it cannot currently
// do is tell that case apart from its OWN write echoing back, and the two want
// opposite answers.
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
		// The Codex round 3 [P1] this effect was built for: the pane reuses this
		// component for the next item, and flushing here would write item A's text
		// onto item B. An echo check on the VALUE ALONE cannot see this — item B's
		// value can equal what we sent for item A — so the fence has to be the
		// item, not the text.
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
		// The case the value alone cannot show, and the reason the effect tracks
		// `itemId` rather than only `value`. Item B's field happens to hold the
		// same text item A's did, so the prop does not change at all — and an
		// effect watching only `value` never runs, leaving item A's pending
		// keystrokes armed to flush onto item B.
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
